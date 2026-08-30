package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/dibin/tdrive/internal/auth"
	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/drive"
)

// Account administration.
//
// The shape here is deliberately a PATCH rather than a PUT: an administrator
// toggling one switch must not silently reset the four fields the form did not
// send, and a WebUI from a different build must not be able to blank a setting
// it has never heard of. Every mutable field is therefore a pointer, and
// absent means "leave alone".

type userBody struct {
	database.User
	// Perms is the effective permission set, which is what the UI renders and
	// what the server actually enforces. The stored mask is not exposed:
	// "inherits the role" is a fact about storage, not about access.
	Perms []string `json:"perms"`
	// PermsInherited says whether those permissions come from the role rather
	// than from an explicit per-account mask, so the form can show the
	// difference.
	PermsInherited bool  `json:"permsInherited"`
	UsedBytes      int64 `json:"usedBytes"`
	FileCount      int64 `json:"fileCount"`
	LastLoginAt    int64 `json:"lastLoginAt,omitempty"`
	// Sessions is how many live sessions the account has, so an admin can see
	// at a glance whether anyone is signed in as it.
	Sessions int `json:"sessions"`
}

func toUserBody(u database.User, usage database.Usage, sessions int) userBody {
	body := userBody{
		User:           u,
		Perms:          database.PermNames(u.Effective()),
		PermsInherited: u.Perms == 0,
		UsedBytes:      usage.Bytes,
		FileCount:      usage.Files,
		Sessions:       sessions,
	}
	if !u.LastLoginAt.IsZero() {
		body.LastLoginAt = u.LastLoginAt.UnixMilli()
	}
	return body
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.db.ListUsers(r.Context())
	if err != nil {
		s.fail(w, err, "list users")
		return
	}
	// One grouped query rather than one per row: a deployment with fifty
	// accounts should not issue fifty sums to draw a table.
	usage, err := s.db.UsageByOwner(r.Context())
	if err != nil {
		s.fail(w, err, "list users")
		return
	}

	out := make([]userBody, 0, len(users))
	for _, u := range users {
		sessions, err := s.db.ListSessions(r.Context(), u.ID)
		if err != nil {
			s.fail(w, err, "list users")
			return
		}
		out = append(out, toUserBody(u, usage[u.ID], len(sessions)))
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePermissionCatalog lets the WebUI render the permission form without
// hardcoding a list that would drift out of step with the server.
func (s *Server) handlePermissionCatalog(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"all":         database.AllPermNames(),
		"userDefault": database.PermNames(database.DefaultUserPerms),
		"adminNote":   "管理员始终拥有全部权限，无法逐项关闭",
	})
}

type createUserRequest struct {
	Username   string        `json:"username"`
	Password   string        `json:"password"`
	Role       database.Role `json:"role"`
	Perms      []string      `json:"perms,omitempty"`
	ScopePath  string        `json:"scopePath,omitempty"`
	QuotaBytes int64         `json:"quotaBytes,omitempty"`
	Note       string        `json:"note,omitempty"`
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Role != database.RoleAdmin && req.Role != database.RoleUser {
		req.Role = database.RoleUser
	}

	scope, err := normalizeScope(req.ScopePath)
	if err != nil {
		s.fail(w, err, "create user")
		return
	}
	if req.QuotaBytes < 0 {
		writeError(w, http.StatusBadRequest, "配额不能是负数")
		return
	}

	user, err := s.auth.CreateUser(r.Context(), req.Username, req.Password, req.Role)
	if err != nil {
		s.fail(w, err, "create user")
		return
	}

	// The optional fields are applied as a follow-up update so that account
	// creation has exactly one code path, whether or not the caller supplied
	// them.
	profile := database.UserProfile{}
	if len(req.Perms) > 0 {
		perms := database.PermsFromNames(req.Perms)
		profile.Perms = &perms
	}
	if scope != "" {
		profile.ScopePath = &scope
	}
	if req.QuotaBytes > 0 {
		profile.QuotaBytes = &req.QuotaBytes
	}
	if req.Note != "" {
		profile.Note = &req.Note
	}
	if err := s.db.UpdateUserProfile(r.Context(), user.ID, profile); err != nil {
		s.fail(w, err, "create user")
		return
	}

	fresh, err := s.db.UserByID(r.Context(), user.ID)
	if err != nil {
		s.fail(w, err, "create user")
		return
	}
	// A scoped account with no directory to sit in would meet an error on its
	// very first listing, so the subtree is created up front.
	if err := s.drive.EnsureScopeRoot(r.Context(), drive.ScopeOf(fresh)); err != nil {
		s.log.Warn("could not create a new account's home directory",
			zap.String("user", fresh.Username), zap.Error(err))
	}

	s.audit(r, database.AuditUserCreate, fresh.Username, "role="+string(fresh.Role))
	writeJSON(w, http.StatusCreated, toUserBody(fresh, database.Usage{}, 0))
}

type updateUserRequest struct {
	Enabled    *bool     `json:"enabled,omitempty"`
	Perms      *[]string `json:"perms,omitempty"`
	ScopePath  *string   `json:"scopePath,omitempty"`
	QuotaBytes *int64    `json:"quotaBytes,omitempty"`
	Note       *string   `json:"note,omitempty"`
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	target, err := s.db.UserByID(r.Context(), id)
	if err != nil {
		s.fail(w, err, "update user")
		return
	}

	var req updateUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	profile := database.UserProfile{}
	var changes []string

	if req.Enabled != nil {
		if !*req.Enabled {
			if id == currentUser(r).ID {
				writeError(w, http.StatusBadRequest, "不能停用自己的账号")
				return
			}
			// The same reasoning as refusing to delete the last admin: a
			// deployment with no usable administrator cannot be repaired from
			// inside the WebUI.
			if target.Role == database.RoleAdmin {
				admins, err := s.db.CountAdmins(r.Context())
				if err != nil {
					s.fail(w, err, "update user")
					return
				}
				if admins <= 1 {
					writeError(w, http.StatusBadRequest, "不能停用最后一个可用的管理员")
					return
				}
			}
		}
		profile.Enabled = req.Enabled
		changes = append(changes, "enabled="+strconv.FormatBool(*req.Enabled))
	}

	if req.Perms != nil {
		perms := database.PermsFromNames(*req.Perms)
		profile.Perms = &perms
		changes = append(changes, "perms="+strings.Join(database.PermNames(perms), "|"))
	}

	if req.ScopePath != nil {
		scope, err := normalizeScope(*req.ScopePath)
		if err != nil {
			s.fail(w, err, "update user")
			return
		}
		profile.ScopePath = &scope
		changes = append(changes, "scope="+scope)
	}

	if req.QuotaBytes != nil {
		if *req.QuotaBytes < 0 {
			writeError(w, http.StatusBadRequest, "配额不能是负数")
			return
		}
		profile.QuotaBytes = req.QuotaBytes
		changes = append(changes, "quota="+strconv.FormatInt(*req.QuotaBytes, 10))
	}

	if req.Note != nil {
		profile.Note = req.Note
	}

	if err := s.db.UpdateUserProfile(r.Context(), id, profile); err != nil {
		s.fail(w, err, "update user")
		return
	}
	// Cached Basic credentials carry the old permissions and the old enabled
	// flag, so WebDAV would keep honouring them for the cache's lifetime.
	s.auth.InvalidateUser(target.Username)

	fresh, err := s.db.UserByID(r.Context(), id)
	if err != nil {
		s.fail(w, err, "update user")
		return
	}
	if !fresh.Enabled {
		// Disabling has to end the sessions that are already open, otherwise
		// an account stays usable in whatever browsers had it loaded.
		if err := s.db.RevokeUserTokens(r.Context(), id); err != nil {
			s.fail(w, err, "update user")
			return
		}
	}
	if err := s.drive.EnsureScopeRoot(r.Context(), drive.ScopeOf(fresh)); err != nil {
		s.log.Warn("could not create an account's home directory",
			zap.String("user", fresh.Username), zap.Error(err))
	}

	action := database.AuditUserUpdate
	if req.Enabled != nil {
		if *req.Enabled {
			action = database.AuditUserEnable
		} else {
			action = database.AuditUserDisable
		}
	}
	s.audit(r, action, fresh.Username, strings.Join(changes, " "))

	usage, err := s.db.UsedBytesByOwner(r.Context(), id)
	if err != nil {
		s.fail(w, err, "update user")
		return
	}
	writeJSON(w, http.StatusOK, toUserBody(fresh, database.Usage{Bytes: usage}, 0))
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == currentUser(r).ID {
		writeError(w, http.StatusBadRequest, "不能删除自己的账号")
		return
	}

	target, err := s.db.UserByID(r.Context(), id)
	if err != nil {
		s.fail(w, err, "delete user")
		return
	}
	// Removing the last administrator would lock everyone out of the
	// Telegram settings with no way back in.
	if target.Role == database.RoleAdmin {
		admins, err := s.db.CountAdmins(r.Context())
		if err != nil {
			s.fail(w, err, "delete user")
			return
		}
		if admins <= 1 {
			writeError(w, http.StatusBadRequest, "不能删除最后一个管理员")
			return
		}
	}

	if err := s.db.DeleteUser(r.Context(), id); err != nil {
		s.fail(w, err, "delete user")
		return
	}
	s.auth.InvalidateUser(target.Username)
	s.audit(r, database.AuditUserDelete, target.Username, "")
	w.WriteHeader(http.StatusNoContent)
}

type passwordRequest struct {
	Password string `json:"password"`
}

func (s *Server) handleSetUserPassword(w http.ResponseWriter, r *http.Request) {
	var req passwordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	id := chi.URLParam(r, "id")
	if err := s.auth.ChangePassword(r.Context(), id, req.Password); err != nil {
		s.fail(w, err, "set password")
		return
	}
	if target, err := s.db.UserByID(r.Context(), id); err == nil {
		s.audit(r, database.AuditUserPassword, target.Username, "")
	}
	w.WriteHeader(http.StatusNoContent)
}

type changeOwnPasswordRequest struct {
	Current string `json:"current"`
	New     string `json:"new"`
}

func (s *Server) handleChangeOwnPassword(w http.ResponseWriter, r *http.Request) {
	var req changeOwnPasswordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	user := currentUser(r)
	// Proving knowledge of the current password stops a stolen access token
	// from being upgraded into permanent control of the account.
	if err := auth.VerifyPassword(user.PasswordHash, req.Current); err != nil {
		writeError(w, http.StatusUnauthorized, "the current password is not correct")
		return
	}
	if err := s.auth.ChangePassword(r.Context(), user.ID, req.New); err != nil {
		s.fail(w, err, "change password")
		return
	}
	auth.ClearRefreshCookie(w, s.secureCookies(r))
	w.WriteHeader(http.StatusNoContent)
}

type roleRequest struct {
	Role database.Role `json:"role"`
}

func (s *Server) handleSetUserRole(w http.ResponseWriter, r *http.Request) {
	var req roleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Role != database.RoleAdmin && req.Role != database.RoleUser {
		writeError(w, http.StatusBadRequest, "role must be admin or user")
		return
	}

	id := chi.URLParam(r, "id")
	if req.Role == database.RoleUser {
		target, err := s.db.UserByID(r.Context(), id)
		if err != nil {
			s.fail(w, err, "set role")
			return
		}
		if target.Role == database.RoleAdmin {
			admins, err := s.db.CountAdmins(r.Context())
			if err != nil {
				s.fail(w, err, "set role")
				return
			}
			if admins <= 1 {
				writeError(w, http.StatusBadRequest, "不能降级最后一个管理员")
				return
			}
		}
	}

	if err := s.db.SetUserRole(r.Context(), id, req.Role); err != nil {
		s.fail(w, err, "set role")
		return
	}
	if target, err := s.db.UserByID(r.Context(), id); err == nil {
		s.auth.InvalidateUser(target.Username)
		s.audit(r, database.AuditUserRole, target.Username, "role="+string(req.Role))
	}
	w.WriteHeader(http.StatusNoContent)
}

// sessionBody adds a "this is the session you are using right now" marker, so
// the UI can keep someone from signing themselves out by accident.
type sessionBody struct {
	database.Session
	Current bool `json:"current"`
}

func (s *Server) sessionsFor(w http.ResponseWriter, r *http.Request, userID string) {
	sessions, err := s.db.ListSessions(r.Context(), userID)
	if err != nil {
		s.fail(w, err, "list sessions")
		return
	}

	currentID := s.currentSessionID(r)
	out := make([]sessionBody, 0, len(sessions))
	for _, session := range sessions {
		out = append(out, sessionBody{Session: session, Current: session.ID == currentID})
	}
	writeJSON(w, http.StatusOK, out)
}

// currentSessionID resolves the refresh cookie to its stored row, so the
// session list can point at "this device".
func (s *Server) currentSessionID(r *http.Request) string {
	cookie, err := r.Cookie(auth.RefreshCookie)
	if err != nil {
		return ""
	}
	_, tokenID, err := s.db.LookupRefreshToken(r.Context(), auth.HashRefreshToken(cookie.Value))
	if err != nil {
		return ""
	}
	return tokenID
}

func (s *Server) handleMySessions(w http.ResponseWriter, r *http.Request) {
	s.sessionsFor(w, r, currentUser(r).ID)
}

func (s *Server) handleUserSessions(w http.ResponseWriter, r *http.Request) {
	s.sessionsFor(w, r, chi.URLParam(r, "id"))
}

func (s *Server) handleRevokeMySession(w http.ResponseWriter, r *http.Request) {
	if err := s.db.RevokeSessionOf(r.Context(), currentUser(r).ID, chi.URLParam(r, "sid")); err != nil {
		s.fail(w, err, "revoke session")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRevokeUserSession(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.db.RevokeSessionOf(r.Context(), id, chi.URLParam(r, "sid")); err != nil {
		s.fail(w, err, "revoke session")
		return
	}
	if target, err := s.db.UserByID(r.Context(), id); err == nil {
		s.audit(r, database.AuditSessionRevoke, target.Username, "one session")
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRevokeUserSessions(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.db.RevokeUserTokens(r.Context(), id); err != nil {
		s.fail(w, err, "revoke sessions")
		return
	}
	if target, err := s.db.UserByID(r.Context(), id); err == nil {
		s.auth.InvalidateUser(target.Username)
		s.audit(r, database.AuditSessionRevoke, target.Username, "all sessions")
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filter := database.AuditFilter{
		ActorID: query.Get("actor"),
		Action:  query.Get("action"),
		Query:   query.Get("q"),
	}
	if v, err := strconv.ParseInt(query.Get("from"), 10, 64); err == nil {
		filter.From = v
	}
	if v, err := strconv.ParseInt(query.Get("to"), 10, 64); err == nil {
		filter.To = v
	}
	if v, err := strconv.Atoi(query.Get("limit")); err == nil {
		filter.Limit = v
	}

	entries, err := s.db.ListAudit(r.Context(), filter)
	if err != nil {
		s.fail(w, err, "list audit log")
		return
	}
	if entries == nil {
		entries = []database.AuditEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

// normalizeScope validates a directory scope. An empty value means the whole
// drive, which is the default and has to remain expressible.
func normalizeScope(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == drive.Root {
		return "", nil
	}
	clean, err := drive.CleanPath(trimmed)
	if err != nil {
		return "", err
	}
	if clean == drive.Root {
		return "", nil
	}
	return clean, nil
}

// handleMyProfile answers what the signed-in account may do, so the WebUI can
// hide what it cannot use rather than offering buttons that 403.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	used, err := s.db.UsedBytesByOwner(r.Context(), user.ID)
	if err != nil {
		s.fail(w, err, "read profile")
		return
	}
	writeJSON(w, http.StatusOK, toUserBody(user, database.Usage{Bytes: used}, 0))
}
