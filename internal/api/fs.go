package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/dibin/tdrive/internal/auth"
	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/drive"
	"github.com/dibin/tdrive/internal/events"
)

type listBody struct {
	Path    string        `json:"path"`
	Entries []drive.Entry `json:"entries"`
	// Breadcrumbs let the UI render the path without re-splitting it.
	Breadcrumbs []crumb `json:"breadcrumbs"`
}

type crumb struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		path = drive.Root
	}
	clean, err := drive.CleanPath(path)
	if err != nil {
		s.fail(w, err, "list")
		return
	}

	entries, err := s.drive.List(r.Context(), clean)
	if err != nil {
		s.fail(w, err, "list")
		return
	}
	if entries == nil {
		entries = []drive.Entry{}
	}
	writeJSON(w, http.StatusOK, listBody{
		Path:        clean,
		Entries:     entries,
		Breadcrumbs: breadcrumbs(clean),
	})
}

func breadcrumbs(p string) []crumb {
	out := []crumb{{Name: "", Path: drive.Root}}
	acc := ""
	for _, part := range drive.SplitPath(p) {
		acc = drive.Join(acc, part)
		out = append(out, crumb{Name: part, Path: acc})
	}
	return out
}

func (s *Server) handleStat(w http.ResponseWriter, r *http.Request) {
	entry, err := s.drive.Stat(r.Context(), r.URL.Query().Get("path"))
	if err != nil {
		s.fail(w, err, "stat")
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

type pathRequest struct {
	Path string `json:"path"`
}

func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request) {
	var req pathRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	dir, err := s.drive.Mkdir(r.Context(), req.Path)
	if err != nil {
		s.fail(w, err, "mkdir")
		return
	}
	parent, _ := drive.Parent(dir.Path)
	s.events.Publish(events.Event{Type: events.TypeTree, Data: events.TreeChanged{Path: parent}})
	writeJSON(w, http.StatusCreated, dir)
}

type renameRequest struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	var req renameRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	entry, err := s.drive.Rename(r.Context(), req.Path, req.Name)
	if err != nil {
		s.fail(w, err, "rename")
		return
	}
	parent, _ := drive.Parent(entry.Path)
	s.events.Publish(events.Event{Type: events.TypeTree, Data: events.TreeChanged{Path: parent}})
	writeJSON(w, http.StatusOK, entry)
}

type moveRequest struct {
	Path string `json:"path"`
	To   string `json:"to"`
}

func (s *Server) handleMove(w http.ResponseWriter, r *http.Request) {
	var req moveRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	from, _ := drive.Parent(req.Path)
	entry, err := s.drive.Move(r.Context(), req.Path, req.To)
	if err != nil {
		s.fail(w, err, "move")
		return
	}
	s.events.Publish(events.Event{Type: events.TypeTree, Data: events.TreeChanged{Path: from}})
	s.events.Publish(events.Event{Type: events.TypeTree, Data: events.TreeChanged{Path: req.To}})
	writeJSON(w, http.StatusOK, entry)
}

type deleteRequest struct {
	// Paths accepts several targets so a multi-select in the UI is one
	// request rather than a burst of them.
	Paths []string `json:"paths"`
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	var req deleteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Paths) == 0 {
		writeError(w, http.StatusBadRequest, "no paths given")
		return
	}

	touched := map[string]bool{}
	for _, p := range req.Paths {
		if err := s.drive.Delete(r.Context(), p); err != nil {
			s.fail(w, err, "delete")
			return
		}
		parent, _ := drive.Parent(p)
		touched[parent] = true
	}
	for p := range touched {
		s.events.Publish(events.Event{Type: events.TypeTree, Data: events.TreeChanged{Path: p}})
	}
	w.WriteHeader(http.StatusNoContent)
}

// segmentInfo exposes a file's physical layout for the details panel. It is
// informational only: every other endpoint treats the file as one object.
type segmentInfo struct {
	Index int   `json:"index"`
	Size  int64 `json:"size"`
	// MessageID is shown so a user can find the segment in their own
	// Telegram client.
	MessageID int `json:"messageId"`
}

func (s *Server) handleSegments(w http.ResponseWriter, r *http.Request) {
	file, err := s.db.FileByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.fail(w, err, "segments")
		return
	}
	segs, err := s.db.Segments(r.Context(), file.ID)
	if err != nil {
		s.fail(w, err, "segments")
		return
	}

	out := make([]segmentInfo, 0, len(segs))
	for _, seg := range segs {
		out = append(out, segmentInfo{Index: seg.Index, Size: seg.Size, MessageID: seg.TGMsgID})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"file":     file,
		"segments": out,
	})
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.db.ListUsers(r.Context())
	if err != nil {
		s.fail(w, err, "list users")
		return
	}
	writeJSON(w, http.StatusOK, users)
}

type createUserRequest struct {
	Username string        `json:"username"`
	Password string        `json:"password"`
	Role     database.Role `json:"role"`
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Role != database.RoleAdmin && req.Role != database.RoleUser {
		req.Role = database.RoleUser
	}
	user, err := s.auth.CreateUser(r.Context(), req.Username, req.Password, req.Role)
	if err != nil {
		s.fail(w, err, "create user")
		return
	}
	writeJSON(w, http.StatusCreated, user)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == currentUser(r).ID {
		writeError(w, http.StatusBadRequest, "you cannot delete your own account")
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
			writeError(w, http.StatusBadRequest, "the last administrator cannot be removed")
			return
		}
	}

	if err := s.db.DeleteUser(r.Context(), id); err != nil {
		s.fail(w, err, "delete user")
		return
	}
	s.auth.InvalidateUser(target.Username)
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
	if err := s.auth.ChangePassword(r.Context(), chi.URLParam(r, "id"), req.Password); err != nil {
		s.fail(w, err, "set password")
		return
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
				writeError(w, http.StatusBadRequest, "the last administrator cannot be demoted")
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
	}
	w.WriteHeader(http.StatusNoContent)
}
