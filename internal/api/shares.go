package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/dibin/tdrive/internal/auth"
	"github.com/dibin/tdrive/internal/database"
)

// Share links.
//
// The media token signed by internal/auth solves a different problem: it lets
// a <video> element inside an authenticated page fetch bytes without a header.
// It is short-lived and unrevocable by design, which makes it exactly wrong
// for the thing people actually want — a URL they can paste into aria2, IDM or
// a download manager on another machine, that still works tomorrow, and that
// they can turn off when it should stop working.
//
// So a share link is a stored capability: 256 bits of entropy, hashed at rest,
// with an expiry and a revoke switch. It lives outside /api so it looks and
// behaves like an ordinary file URL to every downloader that will ever see it.

// SharePrefix is where the public download routes are mounted.
const SharePrefix = "/d"

type shareRequest struct {
	// TTLSeconds overrides the configured default. Zero uses the default; a
	// negative value means "never expires", which is spelled explicitly so it
	// cannot happen by leaving a field out.
	TTLSeconds int64 `json:"ttlSeconds,omitempty"`
	// Segments mints one link per stored segment in addition to the whole-file
	// link, which is what the split-download mode needs.
	Segments bool   `json:"segments,omitempty"`
	Label    string `json:"label,omitempty"`
}

type shareLinkBody struct {
	ID string `json:"id"`
	// URL is absolute so it can be copied straight into another program.
	URL       string `json:"url"`
	Kind      string `json:"kind"`
	Index     int    `json:"index,omitempty"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	ExpiresAt int64  `json:"expiresAt,omitempty"`
}

type shareResponse struct {
	File shareLinkBody `json:"file"`
	// Segments is populated only when the caller asked for per-segment links.
	Segments []shareLinkBody `json:"segments,omitempty"`
}

func (s *Server) handleCreateShare(w http.ResponseWriter, r *http.Request) {
	var req shareRequest
	if r.ContentLength > 0 && !decodeJSON(w, r, &req) {
		return
	}

	file, err := s.fileForUser(r, chi.URLParam(r, "id"))
	if err != nil {
		s.fail(w, err, "create share link")
		return
	}
	if err := requirePerm(r, database.PermShare); err != nil {
		s.fail(w, err, "create share link")
		return
	}
	if file.Status == database.StatusPending {
		writeError(w, http.StatusConflict, "this file is still uploading")
		return
	}

	expiry := s.shareExpiry(req.TTLSeconds)
	user := currentUser(r)
	origin := s.externalOrigin(r)

	whole, err := s.mintShare(r, file, database.ShareFile, 0, req.Label, expiry, user.ID, origin)
	if err != nil {
		s.fail(w, err, "create share link")
		return
	}

	out := shareResponse{File: whole}
	if req.Segments && file.SegmentCount > 1 {
		for index := 1; index <= file.SegmentCount; index++ {
			link, err := s.mintShare(r, file, database.ShareSegment, index, req.Label, expiry, user.ID, origin)
			if err != nil {
				s.fail(w, err, "create share link")
				return
			}
			out.Segments = append(out.Segments, link)
		}
	}

	s.audit(r, database.AuditShareCreate, file.Name,
		fmt.Sprintf("kind=file segments=%d", len(out.Segments)))
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) shareExpiry(ttlSeconds int64) time.Time {
	switch {
	case ttlSeconds < 0:
		// Explicitly permanent.
		return time.Time{}
	case ttlSeconds > 0:
		return time.Now().Add(time.Duration(ttlSeconds) * time.Second)
	}
	if ttl := s.cfg.RuntimeSettings().ShareTTL; ttl > 0 {
		return time.Now().Add(ttl)
	}
	return time.Time{}
}

func (s *Server) mintShare(
	r *http.Request,
	file database.File,
	kind database.ShareKind,
	index int,
	label string,
	expiry time.Time,
	userID, origin string,
) (shareLinkBody, error) {
	token, err := database.NewShareToken()
	if err != nil {
		return shareLinkBody{}, err
	}

	// The segment index is carried in the label rather than a column of its
	// own: it is the only per-segment fact a link needs, and adding a column
	// for one integer that only one of two kinds uses is not worth the
	// migration.
	storedLabel := label
	if kind == database.ShareSegment {
		storedLabel = strconv.Itoa(index)
	}

	share, err := s.db.CreateShare(r.Context(), database.ShareLink{
		UserID:    userID,
		FileID:    file.ID,
		Kind:      kind,
		Label:     storedLabel,
		ExpiresAt: expiry,
	}, token)
	if err != nil {
		return shareLinkBody{}, err
	}

	body := shareLinkBody{
		ID:   share.ID,
		Kind: string(kind),
		Name: file.Name,
		Size: file.Size,
	}
	if !expiry.IsZero() {
		body.ExpiresAt = expiry.UnixMilli()
	}
	if kind == database.ShareSegment {
		body.Index = index
		body.Name = fmt.Sprintf("%s.part%03d", file.Name, index)
		body.Size = segmentBytes(file, index)
		body.URL = fmt.Sprintf("%s%s/%s/part/%d/%s",
			origin, SharePrefix, token, index, url.PathEscape(file.Name))
	} else {
		body.URL = fmt.Sprintf("%s%s/%s/%s", origin, SharePrefix, token, url.PathEscape(file.Name))
	}
	return body, nil
}

func (s *Server) handleListShares(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	// An administrator sees every link, because revoking one they cannot see
	// is not a thing they can do.
	scope := user.ID
	if user.Role == database.RoleAdmin && r.URL.Query().Get("all") == "1" {
		scope = ""
	}

	shares, err := s.db.ListShares(r.Context(), scope, r.URL.Query().Get("revoked") == "1")
	if err != nil {
		s.fail(w, err, "list share links")
		return
	}
	if shares == nil {
		shares = []database.ShareLink{}
	}
	writeJSON(w, http.StatusOK, shares)
}

func (s *Server) handleRevokeShare(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	share, err := s.db.ShareByID(r.Context(), id)
	if err != nil {
		s.fail(w, err, "revoke share link")
		return
	}

	user := currentUser(r)
	if share.UserID != user.ID && user.Role != database.RoleAdmin {
		// Reporting "not found" rather than "forbidden" avoids confirming that
		// somebody else's link id exists.
		s.fail(w, fmt.Errorf("%w: share link", database.ErrNotFound), "revoke share link")
		return
	}
	if err := s.db.RevokeShare(r.Context(), id); err != nil {
		s.fail(w, err, "revoke share link")
		return
	}
	s.audit(r, database.AuditShareRevoke, id, "")
	w.WriteHeader(http.StatusNoContent)
}

// ShareRoutes is the public download subtree. It is mounted outside /api and
// carries no authentication middleware: the token in the path is the
// credential, which is what makes the URL usable from a download manager.
func (s *Server) ShareRoutes() http.Handler {
	mux := chi.NewRouter()
	mux.Get("/{token}/part/{index}/*", s.handleShareDownload)
	mux.Head("/{token}/part/{index}/*", s.handleShareDownload)
	mux.Get("/{token}/part/{index}", s.handleShareDownload)
	mux.Head("/{token}/part/{index}", s.handleShareDownload)
	mux.Get("/{token}/*", s.handleShareDownload)
	mux.Head("/{token}/*", s.handleShareDownload)
	mux.Get("/{token}", s.handleShareDownload)
	mux.Head("/{token}", s.handleShareDownload)
	return mux
}

func (s *Server) handleShareDownload(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	share, err := s.db.ShareByToken(r.Context(), token)
	if err != nil {
		// Everything unresolvable answers the same way, so the response cannot
		// be used to tell a wrong token from a revoked or expired one.
		writeError(w, http.StatusNotFound, "this download link is not valid, or it has expired")
		return
	}

	file, err := s.db.FileByID(r.Context(), share.FileID)
	if err != nil {
		writeError(w, http.StatusNotFound, "the file behind this link is gone")
		return
	}

	// Counting a use is bookkeeping; failing the download over it would be
	// absurd.
	_ = s.db.TouchShare(r.Context(), share.ID)

	sessionKey := s.downloadSessionKey(r, file.ID, share.ID)
	if share.Kind == database.ShareSegment {
		index, err := strconv.Atoi(share.Label)
		if err != nil {
			// A segment link with an unreadable index is corrupt; refusing is
			// better than guessing which part it meant.
			writeError(w, http.StatusNotFound, "this download link is not valid")
			return
		}
		window, err := segmentWindow(file, index, sessionKey)
		if err != nil {
			writeError(w, http.StatusNotFound, "this download link is not valid")
			return
		}
		window.attachment = true
		s.serveBytes(w, r, window)
		return
	}
	window := wholeFileWindow(file, sessionKey)
	window.attachment = true
	s.serveBytes(w, r, window)
}

// segmentBytes is the size of one segment of a file.
func segmentBytes(file database.File, index int) int64 {
	start := int64(index-1) * file.SegmentSize
	if start >= file.Size {
		return 0
	}
	return min(file.SegmentSize, file.Size-start)
}

// audit records an administrative action, attributing it to the requesting
// account. Failures are logged and swallowed: an unwritten audit line is worse
// than nothing, but refusing the action the operator asked for is worse still.
func (s *Server) audit(r *http.Request, action, target, detail string) {
	user, _ := auth.FromContext(r.Context())
	err := s.db.AppendAudit(r.Context(), database.AuditEntry{
		ActorID:   user.ID,
		ActorName: user.Username,
		Action:    action,
		Target:    target,
		Detail:    detail,
		IP:        auth.ClientFrom(r).IP,
	})
	if err != nil && !errors.Is(err, database.ErrNotFound) {
		s.log.Warn("could not write an audit entry",
			zap.String("action", action), zap.Error(err))
	}
}
