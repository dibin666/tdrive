package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap/zapcore"

	"github.com/dibin/tdrive/internal/config"
	"github.com/dibin/tdrive/internal/database"
)

// settingsBody is intentionally limited to values that are safe to change
// while the server is running. The local source path is the one exception to
// the usual deployment-only path rule: an administrator can point it at an
// already-mounted VPS directory without restarting the server.
type settingsBody struct {
	AppID               int    `json:"appId"`
	AppHash             string `json:"appHash"`
	LocalRoot           string `json:"localRoot"`
	SegmentSize         int64  `json:"segmentSize"`
	PoolSize            int64  `json:"poolSize"`
	UploadThreads       int    `json:"uploadThreads"`
	UploadPartSize      int64  `json:"uploadPartSize"`
	RateLimitMs         int64  `json:"rateLimitMs"`
	StreamConcurrency   int    `json:"streamConcurrency"`
	UploadConcurrency   int    `json:"uploadConcurrency"`
	DownloadConcurrency int    `json:"downloadConcurrency"`
	WebDAVEnabled       bool   `json:"webdavEnabled"`
	LogLevel            string `json:"logLevel"`
	CacheDir            string `json:"cacheDir"`
	CacheLimit          int64  `json:"cacheLimit"`
	CacheTTLHours       int64  `json:"cacheTtlHours"`
	MaxDownloadConns    int    `json:"maxDownloadConns"`
	DownloadGraceMs     int64  `json:"downloadGraceMs"`
	ShareTTLHours       int64  `json:"shareTtlHours"`

	// The task limits above are per Telegram account, so what an operator
	// actually gets is the limit times the number of accounts able to take
	// work. These three are derived and read-only; the WebUI shows them next to
	// the sliders so the multiplication is not a surprise.
	AccountCount                 int `json:"accountCount"`
	EffectiveUploadConcurrency   int `json:"effectiveUploadConcurrency"`
	EffectiveDownloadConcurrency int `json:"effectiveDownloadConcurrency"`
}

type settingsUpdateRequest struct {
	AppID               *int    `json:"appId"`
	AppHash             *string `json:"appHash"`
	LocalRoot           *string `json:"localRoot"`
	SegmentSize         *int64  `json:"segmentSize"`
	PoolSize            *int64  `json:"poolSize"`
	UploadThreads       *int    `json:"uploadThreads"`
	UploadPartSize      *int64  `json:"uploadPartSize"`
	RateLimitMs         *int64  `json:"rateLimitMs"`
	StreamConcurrency   *int    `json:"streamConcurrency"`
	UploadConcurrency   *int    `json:"uploadConcurrency"`
	DownloadConcurrency *int    `json:"downloadConcurrency"`
	WebDAVEnabled       *bool   `json:"webdavEnabled"`
	LogLevel            *string `json:"logLevel"`
	CacheDir            *string `json:"cacheDir"`
	CacheLimit          *int64  `json:"cacheLimit"`
	CacheTTLHours       *int64  `json:"cacheTtlHours"`
	MaxDownloadConns    *int    `json:"maxDownloadConns"`
	DownloadGraceMs     *int64  `json:"downloadGraceMs"`
	ShareTTLHours       *int64  `json:"shareTtlHours"`
}

func toSettingsBody(s config.RuntimeSettings) settingsBody {
	return settingsBody{
		AppID:               s.AppID,
		AppHash:             s.AppHash,
		LocalRoot:           s.LocalRoot,
		SegmentSize:         s.SegmentSize,
		PoolSize:            s.PoolSize,
		UploadThreads:       s.UploadThreads,
		UploadPartSize:      s.UploadPartSize,
		RateLimitMs:         s.RateLimit.Milliseconds(),
		StreamConcurrency:   s.StreamConcurrency,
		UploadConcurrency:   s.UploadConcurrency,
		DownloadConcurrency: s.DownloadConcurrency,
		WebDAVEnabled:       s.WebDAVEnabled,
		LogLevel:            s.LogLevel,
		CacheDir:            s.CacheDir,
		CacheLimit:          s.CacheLimit,
		CacheTTLHours:       int64(s.CacheTTL / time.Hour),
		MaxDownloadConns:    s.MaxDownloadConns,
		DownloadGraceMs:     s.DownloadGrace.Milliseconds(),
		ShareTTLHours:       int64(s.ShareTTL / time.Hour),
	}
}

// settingsBodyFor fills in the derived per-account totals, which toSettingsBody
// cannot compute on its own because they depend on how many accounts are live.
func (s *Server) settingsBodyFor(settings config.RuntimeSettings) settingsBody {
	body := toSettingsBody(settings)
	accounts, upload, download := s.drive.TransferCapacity()
	body.AccountCount = accounts
	body.EffectiveUploadConcurrency = upload
	body.EffectiveDownloadConcurrency = download
	return body
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.settingsBodyFor(s.cfg.RuntimeSettings()))
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	w.Header().Set("Cache-Control", "no-store")

	var req settingsUpdateRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	current := s.cfg.RuntimeSettings()
	next := current
	if req.AppID != nil {
		next.AppID = *req.AppID
	}
	if req.AppHash != nil {
		next.AppHash = strings.TrimSpace(*req.AppHash)
	}
	if req.LocalRoot != nil {
		localRoot, err := config.NormalizeLocalRoot(*req.LocalRoot)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		next.LocalRoot = localRoot
	}
	if req.SegmentSize != nil {
		next.SegmentSize = *req.SegmentSize
	}
	if req.PoolSize != nil {
		next.PoolSize = *req.PoolSize
	}
	if req.UploadThreads != nil {
		next.UploadThreads = *req.UploadThreads
	}
	if req.UploadPartSize != nil {
		next.UploadPartSize = *req.UploadPartSize
	}
	if req.RateLimitMs != nil {
		if *req.RateLimitMs < 1 || *req.RateLimitMs > int64(time.Minute/time.Millisecond) {
			writeError(w, http.StatusBadRequest, "telegram request interval must be between 1ms and 60000ms")
			return
		}
		next.RateLimit = time.Duration(*req.RateLimitMs) * time.Millisecond
	}
	if req.StreamConcurrency != nil {
		next.StreamConcurrency = *req.StreamConcurrency
	}
	if req.UploadConcurrency != nil {
		next.UploadConcurrency = *req.UploadConcurrency
	}
	if req.DownloadConcurrency != nil {
		next.DownloadConcurrency = *req.DownloadConcurrency
	}
	if req.WebDAVEnabled != nil {
		next.WebDAVEnabled = *req.WebDAVEnabled
	}
	if req.LogLevel != nil {
		next.LogLevel = strings.ToLower(strings.TrimSpace(*req.LogLevel))
	}
	if req.CacheDir != nil {
		// The cache directory follows the same normalisation as the VPS upload
		// root: an absolute path, or empty to fall back to the data directory.
		cacheDir, err := config.NormalizeLocalRoot(*req.CacheDir)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		next.CacheDir = cacheDir
	}
	if req.CacheLimit != nil {
		next.CacheLimit = *req.CacheLimit
	}
	if req.CacheTTLHours != nil {
		if *req.CacheTTLHours < 1 || *req.CacheTTLHours > 24*365 {
			writeError(w, http.StatusBadRequest, "\u6682\u5b58\u4fdd\u7559\u65f6\u957f\u5fc5\u987b\u5728 1 \u5c0f\u65f6\u5230 1 \u5e74\u4e4b\u95f4")
			return
		}
		next.CacheTTL = time.Duration(*req.CacheTTLHours) * time.Hour
	}
	if req.MaxDownloadConns != nil {
		next.MaxDownloadConns = *req.MaxDownloadConns
	}
	if req.DownloadGraceMs != nil {
		next.DownloadGrace = time.Duration(*req.DownloadGraceMs) * time.Millisecond
	}
	if req.ShareTTLHours != nil {
		if *req.ShareTTLHours < 0 {
			writeError(w, http.StatusBadRequest, "\u5206\u4eab\u94fe\u63a5\u6709\u6548\u671f\u4e0d\u80fd\u662f\u8d1f\u6570")
			return
		}
		next.ShareTTL = time.Duration(*req.ShareTTLHours) * time.Hour
	}

	if err := next.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := zapcore.ParseLevel(next.LogLevel); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid log level %q", next.LogLevel))
		return
	}

	credentialsChanged := next.AppID != current.AppID || next.AppHash != current.AppHash
	if credentialsChanged && (next.AppID <= 0 || next.AppHash == "") {
		writeError(w, http.StatusBadRequest, "telegram app id and app hash are required when changing Telegram credentials")
		return
	}

	values := map[string]string{
		config.SettingTGAppID:             strconv.Itoa(next.AppID),
		config.SettingTGAppHash:           next.AppHash,
		config.SettingLocalRoot:           next.LocalRoot,
		config.SettingSegmentSize:         strconv.FormatInt(next.SegmentSize, 10),
		config.SettingTGPoolSize:          strconv.FormatInt(next.PoolSize, 10),
		config.SettingUploadThreads:       strconv.Itoa(next.UploadThreads),
		config.SettingTGUploadPartSize:    strconv.FormatInt(next.UploadPartSize, 10),
		config.SettingTGRateLimit:         next.RateLimit.String(),
		config.SettingStreamConcurrency:   strconv.Itoa(next.StreamConcurrency),
		config.SettingUploadConcurrency:   strconv.Itoa(next.UploadConcurrency),
		config.SettingDownloadConcurrency: strconv.Itoa(next.DownloadConcurrency),
		config.SettingWebDAVEnabled:       strconv.FormatBool(next.WebDAVEnabled),
		config.SettingLogLevel:            next.LogLevel,
		config.SettingCacheDir:            next.CacheDir,
		config.SettingCacheLimit:          strconv.FormatInt(next.CacheLimit, 10),
		config.SettingCacheTTL:            next.CacheTTL.String(),
		config.SettingMaxDownloadConns:    strconv.Itoa(next.MaxDownloadConns),
		config.SettingDownloadGrace:       next.DownloadGrace.String(),
		config.SettingShareTTL:            next.ShareTTL.String(),
	}
	if err := s.db.SetSettings(r.Context(), values); err != nil {
		s.fail(w, err, "save settings")
		return
	}

	s.cfg.SetRuntimeSettings(next)
	s.drive.SetTransferConcurrency(next.UploadConcurrency, next.DownloadConcurrency)
	if s.setLogLevel != nil && next.LogLevel != current.LogLevel {
		if err := s.setLogLevel(next.LogLevel); err != nil {
			s.fail(w, err, "apply log level")
			return
		}
	}

	// A new app credential pair must reconnect the Telegram client. The
	// credentials on this page are the primary account's; the others are
	// managed under /tg/accounts.
	primary, primaryErr := s.primary()
	configureTelegram := primaryErr == nil && (credentialsChanged ||
		(!primary.Ready() && (req.AppID != nil || req.AppHash != nil) && next.AppID > 0 && next.AppHash != ""))

	var reconnectErr error
	switch {
	case configureTelegram:
		reconnectErr = primary.Configure(r.Context(), next.AppID, next.AppHash)
	case next.PoolSize != current.PoolSize || next.RateLimit != current.RateLimit:
		// Pool size and request interval are connection setup, and every account
		// builds its own pool and its own rate limiter from them. Reconnecting
		// only the primary would leave the others on the old numbers, so they
		// all rebuild — each keeping its own session file and login.
		for _, manager := range s.accounts.All() {
			if !manager.Ready() {
				continue
			}
			if err := manager.Reload(r.Context()); err != nil && reconnectErr == nil {
				reconnectErr = err
			}
		}
	}
	if reconnectErr != nil {
		s.fail(w, reconnectErr, "apply telegram settings")
		return
	}

	s.audit(r, database.AuditSettingsUpdate, "", "runtime settings")
	writeJSON(w, http.StatusOK, s.settingsBodyFor(next))
}
