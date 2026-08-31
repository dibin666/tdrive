package api

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"

	"github.com/dibin/tdrive/internal/auth"
	"github.com/dibin/tdrive/internal/config"
	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/drive"
	"github.com/dibin/tdrive/internal/events"
	"github.com/dibin/tdrive/internal/indexer"
	"github.com/dibin/tdrive/internal/plugin"
	"github.com/dibin/tdrive/internal/tgc"
)

// Server wires the HTTP surface together.
type Server struct {
	cfg   *config.Config
	db    *database.DB
	auth  *auth.Service
	drive *drive.Service
	// accounts holds every Telegram login. Handlers that manage the drive's
	// identity — the setup wizard, the channel picker, credential changes —
	// act on the primary account through primary(); the ones under
	// /tg/accounts address a specific one.
	accounts    *tgc.Cluster
	index       *indexer.Indexer
	events      *events.Broker
	log         *zap.Logger
	setLogLevel func(string) error
	settingsMu  sync.Mutex
	progress    *liveUploadProgress
	plugins     *plugin.Manager
	// downloadRates times the staged downloads this process is copying, which
	// is the only place their speed can be measured.
	downloadRates *liveRates
}

// SetPluginManager attaches the optional plugin management surface. It is a
// setter rather than a required constructor argument so existing embedders and
// API tests keep the no-plugin construction path.
func (s *Server) SetPluginManager(manager *plugin.Manager) {
	s.plugins = manager
}

func New(
	cfg *config.Config,
	db *database.DB,
	authSvc *auth.Service,
	driveSvc *drive.Service,
	accounts *tgc.Cluster,
	idx *indexer.Indexer,
	broker *events.Broker,
	log *zap.Logger,
	setLogLevel ...func(string) error,
) *Server {
	var applyLogLevel func(string) error
	if len(setLogLevel) > 0 {
		applyLogLevel = setLogLevel[0]
	}
	server := &Server{
		cfg: cfg, db: db, auth: authSvc, drive: driveSvc,
		accounts: accounts, index: idx, events: broker, log: log, setLogLevel: applyLogLevel,
		progress:      newLiveUploadProgress(),
		downloadRates: newLiveRates(),
	}
	wireRemoteProgress(driveSvc, broker, server.progress)
	wireDownloadProgress(driveSvc, broker, server.downloadRates)
	return server
}

// primary is the account the drive's identity belongs to: the one the setup
// wizard signs in, the one whose access hash the channels table holds, and the
// one the top-level /tg endpoints act on. Accounts added afterwards are managed
// individually under /tg/accounts.
func (s *Server) primary() (*tgc.Manager, error) {
	return s.accounts.Primary()
}

// telegramStatus reports the primary account without failing when there is no
// account at all, which is precisely the state the setup wizard exists for.
func (s *Server) telegramStatus() tgc.Status {
	manager, err := s.primary()
	if err != nil {
		return tgc.Status{State: tgc.StateUnconfigured}
	}
	return manager.Status()
}

// Routes builds the /api subtree. WebDAV and the static UI are mounted by the
// caller, because they are not part of the JSON API.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	if s.plugins != nil {
		r.Use(s.plugins.HTTPMiddleware)
	}
	// No blanket request timeout: a single upload segment or a long download
	// legitimately runs for many minutes, and the handlers that do need a
	// deadline set their own.

	// Open endpoints: enough for the WebUI to decide between the setup
	// wizard and a login form.
	r.Get("/status", s.handleStatus)
	r.Post("/setup", s.handleSetup)
	r.Post("/auth/login", s.handleLogin)
	r.Post("/auth/refresh", s.handleRefresh)
	r.Post("/auth/logout", s.handleLogout)

	// Serving bytes accepts either normal credentials or a per-file media
	// token, because a <video> element and a browser download cannot send an
	// Authorization header.
	r.Group(func(r chi.Router) {
		r.Use(s.requireFileAccess)
		r.Get("/files/{id}/raw", s.handleRaw)
		r.Head("/files/{id}/raw", s.handleRaw)
		// One segment as a standalone object, which is what the split
		// download mode fetches.
		r.Get("/files/{id}/segments/{index}/raw", s.handleSegmentRaw)
		r.Head("/files/{id}/segments/{index}/raw", s.handleSegmentRaw)
	})

	r.Group(func(r chi.Router) {
		r.Use(s.auth.RequireAuth)

		r.Get("/me", s.handleMe)
		r.Post("/me/password", s.handleChangeOwnPassword)
		r.Get("/me/sessions", s.handleMySessions)
		r.Delete("/me/sessions/{sid}", s.handleRevokeMySession)
		r.Get("/events", s.handleEvents)
		r.Get("/stats", s.handleStats)

		r.With(s.auth.RequirePerm(database.PermUploadLocal)).
			Get("/local/list", s.handleLocalList)

		// The filesystem routes carry a permission each. Listing needs only
		// read; everything that changes the tree needs its own bit, so an
		// account can be given a read-only or an append-only view without
		// inventing a second role for it.
		r.Route("/fs", func(r chi.Router) {
			r.With(s.auth.RequirePerm(database.PermRead)).Get("/list", s.handleList)
			r.With(s.auth.RequirePerm(database.PermRead)).Get("/stat", s.handleStat)
			r.With(s.auth.RequirePerm(database.PermMkdir)).Post("/mkdir", s.handleMkdir)
			r.With(s.auth.RequirePerm(database.PermRename)).Post("/rename", s.handleRename)
			r.With(s.auth.RequirePerm(database.PermRename)).Post("/batch-rename", s.handleBatchRename)
			r.With(s.auth.RequirePerm(database.PermMove)).Post("/move", s.handleMove)
			r.With(s.auth.RequirePerm(database.PermDelete)).Post("/delete", s.handleDelete)
		})

		r.Route("/files/{id}", func(r chi.Router) {
			r.Get("/segments", s.handleSegments)
			r.With(s.auth.RequirePerm(database.PermDownload)).Get("/link", s.handleMediaLink)
			r.With(s.auth.RequirePerm(database.PermDownload)).
				Get("/download-options", s.handleDownloadOptions)
			r.With(s.auth.RequirePerm(database.PermShare)).Post("/share", s.handleCreateShare)
		})

		r.Get("/shares", s.handleListShares)
		r.Delete("/shares/{id}", s.handleRevokeShare)

		r.Route("/downloads", func(r chi.Router) {
			r.Use(s.auth.RequirePerm(database.PermDownload))
			r.Post("/", s.handleStartDownload)
			r.Get("/{id}", s.handleDownloadJob)
			r.Post("/{id}/progress", s.handleDownloadProgress)
			r.Get("/{id}/file", s.handleStagedFile)
			r.Head("/{id}/file", s.handleStagedFile)
			r.Delete("/{id}", s.handleCancelDownload)
		})

		// The merged transfer history. Uploads and downloads are listed,
		// filtered and deleted together because that is how they are read.
		r.Get("/transfers", s.handleListTransfers)
		r.Delete("/transfers", s.handleDeleteTransfers)
		r.Delete("/transfers/{kind}/{id}", s.handleDeleteTransfer)

		r.Route("/uploads", func(r chi.Router) {
			r.Get("/", s.handleListJobs)
			r.With(s.auth.RequirePerm(database.PermUpload)).Post("/", s.handleBeginUpload)
			r.With(s.auth.RequirePerm(database.PermUploadLocal)).Post("/local", s.handleLocalUpload)
			r.With(s.auth.RequirePerm(database.PermRemoteFetch)).Post("/remote", s.handleRemoteUpload)
			r.Get("/{id}", s.handleJob)
			r.With(s.auth.RequirePerm(database.PermUpload)).
				Put("/{id}/segments/{index}", s.handlePutSegment)
			r.Post("/{id}/complete", s.handleCompleteUpload)
			r.Delete("/{id}", s.handleCancelUpload)
		})

		r.Route("/tg", func(r chi.Router) {
			r.Get("/status", s.handleTelegramStatus)
			r.Group(func(r chi.Router) {
				r.Use(s.auth.RequireAdmin)
				r.Post("/configure", s.handleTelegramConfigure)
				r.Post("/login/code", s.handleSendCode)
				r.Post("/login/signin", s.handleSignIn)
				r.Post("/login/password", s.handleSubmitPassword)
				r.Post("/logout", s.handleTelegramLogout)
				r.Get("/account/export", s.handleTelegramAccountExport)
				r.Post("/account/import", s.handleTelegramAccountImport)
				r.Get("/channels", s.handleListChannels)
				r.Post("/channels", s.handleCreateChannel)
				r.Post("/channels/select", s.handleSelectChannel)

				// Additional Telegram accounts. The endpoints above act on the
				// primary one, which is what the setup wizard configures.
				r.Route("/accounts", func(r chi.Router) {
					r.Get("/", s.handleListAccounts)
					r.Post("/", s.handleCreateAccount)
					r.Patch("/{id}", s.handleUpdateAccount)
					r.Delete("/{id}", s.handleDeleteAccount)
					r.Put("/{id}/proxy", s.handleSetAccountProxy)
					r.Post("/{id}/join-channel", s.handleAccountJoinChannel)
					// The channels one account can see, and the pick that
					// links it to the storage channel by hand when the
					// automatic join cannot do it.
					r.Get("/{id}/channels", s.handleAccountChannels)
					r.Post("/{id}/channel/check", s.handleAccountCheckChannel)
					r.Post("/{id}/channel", s.handleAccountLinkChannel)
					r.Post("/{id}/login/code", s.handleAccountSendCode)
					r.Post("/{id}/login/signin", s.handleAccountSignIn)
					r.Post("/{id}/login/password", s.handleAccountPassword)
				})
			})
		})

		r.Group(func(r chi.Router) {
			r.Use(s.auth.RequireAdmin)
			r.Get("/settings", s.handleSettings)
			r.Put("/settings", s.handleUpdateSettings)

			r.Get("/users", s.handleListUsers)
			r.Post("/users", s.handleCreateUser)
			r.Get("/users/permissions", s.handlePermissionCatalog)
			r.Patch("/users/{id}", s.handleUpdateUser)
			r.Delete("/users/{id}", s.handleDeleteUser)
			r.Post("/users/{id}/password", s.handleSetUserPassword)
			r.Post("/users/{id}/role", s.handleSetUserRole)
			r.Get("/users/{id}/sessions", s.handleUserSessions)
			r.Delete("/users/{id}/sessions/{sid}", s.handleRevokeUserSession)
			r.Post("/users/{id}/sessions/revoke-all", s.handleRevokeUserSessions)

			r.Get("/audit", s.handleAudit)
			r.Get("/cache", s.handleCacheStatus)
			r.Post("/cache/purge", s.handlePurgeCache)

			r.Post("/index/rebuild", s.handleRebuildIndex)
			r.Get("/index/status", s.handleIndexStatus)

			s.pluginRoutes(r)
		})
	})

	return r
}

// secureCookies decides whether to mark session cookies Secure. Behind a
// reverse proxy the request itself is plaintext, so the configured base URL is
// the honest signal.
func (s *Server) secureCookies(r *http.Request) bool {
	if s.cfg.Server.BaseURL != "" {
		return strings.HasPrefix(s.cfg.Server.BaseURL, "https://")
	}
	if r.TLS != nil {
		return true
	}
	return r.Header.Get("X-Forwarded-Proto") == "https"
}

// statusBody is what an unauthenticated client polls to decide what to render.
type statusBody struct {
	NeedsSetup bool       `json:"needsSetup"`
	Telegram   tgc.Status `json:"telegram"`
	// HasChannel is false until a storage channel is chosen, which is the
	// last step of the wizard.
	HasChannel   bool   `json:"hasChannel"`
	ChannelTitle string `json:"channelTitle,omitempty"`
	Version      string `json:"version"`
	// SegmentSize lets the browser slice a file on exactly the boundaries
	// the server will store it on.
	SegmentSize  int64  `json:"segmentSize"`
	LocalEnabled bool   `json:"localEnabled"`
	WebDAVPath   string `json:"webdavPath,omitempty"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	needsSetup, err := s.auth.NeedsSetup(r.Context())
	if err != nil {
		s.fail(w, err, "status")
		return
	}

	body := statusBody{
		NeedsSetup:   needsSetup,
		Telegram:     s.telegramStatus(),
		Version:      tgc.Version,
		SegmentSize:  s.cfg.RuntimeSettings().SegmentSize,
		LocalEnabled: s.cfg.RuntimeSettings().LocalRoot != "",
	}
	if s.cfg.RuntimeSettings().WebDAVEnabled {
		body.WebDAVPath = s.cfg.WebDAV.Prefix
	}
	if channel, err := s.db.DefaultChannel(r.Context()); err == nil {
		body.HasChannel = true
		body.ChannelTitle = channel.Title
	}
	writeJSON(w, http.StatusOK, body)
}

type setupRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleSetup creates the first administrator. It is open by necessity and
// closes itself the moment any account exists.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	needsSetup, err := s.auth.NeedsSetup(r.Context())
	if err != nil {
		s.fail(w, err, "setup")
		return
	}
	if !needsSetup {
		writeError(w, http.StatusConflict, "this drive has already been set up")
		return
	}

	var req setupRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	user, err := s.auth.CreateUser(r.Context(), req.Username, req.Password, database.RoleAdmin)
	if err != nil {
		s.fail(w, err, "setup")
		return
	}

	tokens, _, err := s.auth.Login(r.Context(), req.Username, req.Password, auth.ClientFrom(r))
	if err != nil {
		s.fail(w, err, "setup login")
		return
	}
	auth.SetRefreshCookie(w, tokens, s.secureCookies(r))
	writeJSON(w, http.StatusCreated, loginBody{Tokens: tokens, User: user})
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginBody struct {
	Tokens auth.Tokens   `json:"tokens"`
	User   database.User `json:"user"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	tokens, user, err := s.auth.Login(r.Context(), req.Username, req.Password, auth.ClientFrom(r))
	if err != nil {
		s.fail(w, err, "login")
		return
	}
	auth.SetRefreshCookie(w, tokens, s.secureCookies(r))
	writeJSON(w, http.StatusOK, loginBody{Tokens: tokens, User: user})
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(auth.RefreshCookie)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "no session cookie")
		return
	}
	tokens, user, err := s.auth.Refresh(r.Context(), cookie.Value, auth.ClientFrom(r))
	if err != nil {
		auth.ClearRefreshCookie(w, s.secureCookies(r))
		s.fail(w, err, "refresh")
		return
	}
	auth.SetRefreshCookie(w, tokens, s.secureCookies(r))
	writeJSON(w, http.StatusOK, loginBody{Tokens: tokens, User: user})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(auth.RefreshCookie); err == nil {
		_ = s.auth.Logout(r.Context(), cookie.Value)
	}
	auth.ClearRefreshCookie(w, s.secureCookies(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.db.Stats(r.Context())
	if err != nil {
		s.fail(w, err, "stats")
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// handleEvents streams progress over SSE.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported by this server")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Reverse proxies otherwise buffer the stream and defeat the point.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, cancel := s.events.Subscribe(currentUser(r).ID)
	defer cancel()

	// A periodic comment keeps idle proxies from closing the connection.
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case payload, ok := <-ch:
			if !ok {
				return
			}
			if _, err := w.Write(append(append([]byte("data: "), payload...), '\n', '\n')); err != nil {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
