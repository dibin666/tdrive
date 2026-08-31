package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/tgc"
)

// The Telegram accounts a deployment holds.
//
// The endpoints under /tg (without /accounts) act on the primary account and
// are what the setup wizard drives; these manage the rest. The split keeps the
// first-run path exactly as it was for someone who only ever wants one account.

type accountBody struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	AppID int    `json:"appId"`
	// AppHash is never returned. It is a credential, and the accounts list is
	// polled by every open settings tab.
	Enabled   bool `json:"enabled"`
	IsPrimary bool `json:"isPrimary"`

	Status tgc.Status `json:"status"`
	// CanPost is whether this account has been admitted to the storage channel
	// with posting rights. Without it the account is configured but idle.
	CanPost bool `json:"canPost"`
	// InChannel distinguishes "not a member" from "a member who cannot post".
	InChannel bool `json:"inChannel"`

	// ActiveUploads and ActiveDownloads are the task slots this account is
	// currently using, which is what makes the per-account limits legible.
	ActiveUploads   int `json:"activeUploads"`
	ActiveDownloads int `json:"activeDownloads"`
}

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	rows, err := s.db.ListAccounts(r.Context())
	if err != nil {
		s.fail(w, err, "list telegram accounts")
		return
	}

	// Channel membership is per account, so the picture is only complete with
	// the default channel in hand. A drive with no channel yet simply reports
	// every account as not admitted.
	access := map[string]database.ChannelAccess{}
	if channel, err := s.db.DefaultChannel(r.Context()); err == nil {
		rows, err := s.db.ChannelAccesses(r.Context(), channel.ID)
		if err != nil {
			s.fail(w, err, "list telegram accounts")
			return
		}
		for _, row := range rows {
			access[row.AccountID] = row
		}
	}

	upload, download := s.drive.ActiveTasksByAccount()
	out := make([]accountBody, 0, len(rows))
	for _, row := range rows {
		body := accountBody{
			ID:              row.ID,
			Label:           row.Label,
			AppID:           row.AppID,
			Enabled:         row.Enabled,
			IsPrimary:       row.IsPrimary,
			Status:          tgc.Status{State: tgc.StateUnconfigured},
			ActiveUploads:   upload[row.ID],
			ActiveDownloads: download[row.ID],
		}
		if manager, ok := s.accounts.Manager(row.ID); ok {
			body.Status = manager.Status()
		}
		if a, ok := access[row.ID]; ok {
			body.InChannel = true
			body.CanPost = a.CanPost
		}
		out = append(out, body)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

type createAccountRequest struct {
	Label   string `json:"label"`
	AppID   int    `json:"appId"`
	AppHash string `json:"appHash"`
}

// handleCreateAccount registers another Telegram login. It is not usable yet:
// it still has to sign in, and then be admitted to the storage channel.
func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	var req createAccountRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.AppID <= 0 {
		writeError(w, http.StatusBadRequest, "Telegram app id must be positive")
		return
	}
	if strings.TrimSpace(req.AppHash) == "" {
		writeError(w, http.StatusBadRequest, "Telegram app hash is required")
		return
	}

	manager, err := s.accounts.Add(r.Context(), strings.TrimSpace(req.Label), req.AppID, strings.TrimSpace(req.AppHash))
	if err != nil {
		s.fail(w, err, "add telegram account")
		return
	}
	s.audit(r, database.AuditSettingsUpdate, manager.ID(), "added a telegram account")
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":     manager.ID(),
		"status": manager.Status(),
	})
}

type updateAccountRequest struct {
	Label   *string `json:"label"`
	Enabled *bool   `json:"enabled"`
}

func (s *Server) handleUpdateAccount(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	row, err := s.db.AccountByID(r.Context(), id)
	if err != nil {
		s.fail(w, err, "update telegram account")
		return
	}

	var req updateAccountRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	label, enabled := row.Label, row.Enabled
	if req.Label != nil {
		label = strings.TrimSpace(*req.Label)
	}
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	if err := s.accounts.Update(r.Context(), id, label, enabled); err != nil {
		s.failAccount(w, err, "update telegram account")
		return
	}
	s.audit(r, database.AuditSettingsUpdate, id, "updated a telegram account")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.accounts.Remove(r.Context(), id); err != nil {
		s.failAccount(w, err, "remove telegram account")
		return
	}
	s.audit(r, database.AuditSettingsUpdate, id, "removed a telegram account")
	w.WriteHeader(http.StatusNoContent)
}

// handleAccountJoinChannel admits an account to the storage channel and gives
// it the rights the drive needs. It is the step between "signed in" and
// "actually carrying transfers".
func (s *Server) handleAccountJoinChannel(w http.ResponseWriter, r *http.Request) {
	manager, ok := s.accounts.Manager(chi.URLParam(r, "id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no such Telegram account")
		return
	}
	channel, err := s.db.DefaultChannel(r.Context())
	if err != nil {
		s.fail(w, err, "join storage channel")
		return
	}
	if err := s.accounts.JoinChannel(r.Context(), manager, channel); err != nil {
		s.failAccount(w, err, "join storage channel")
		return
	}
	s.audit(r, database.AuditSettingsUpdate, manager.ID(), "admitted a telegram account to the storage channel")
	writeJSON(w, http.StatusOK, map[string]any{"canPost": true})
}

// Per-account login. The primary signs in through /tg/login/*, which the setup
// wizard already drives; these are the same three steps aimed at one account.

func (s *Server) handleAccountSendCode(w http.ResponseWriter, r *http.Request) {
	manager, ok := s.accountFromPath(w, r)
	if !ok {
		return
	}
	var req phoneRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	res, err := manager.SendCode(r.Context(), req.Phone)
	if err != nil {
		s.fail(w, err, "send login code")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleAccountSignIn(w http.ResponseWriter, r *http.Request) {
	manager, ok := s.accountFromPath(w, r)
	if !ok {
		return
	}
	var req codeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	res, err := manager.SignIn(r.Context(), req.Code)
	if err != nil {
		s.fail(w, err, "telegram sign in")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleAccountPassword(w http.ResponseWriter, r *http.Request) {
	manager, ok := s.accountFromPath(w, r)
	if !ok {
		return
	}
	var req tgPasswordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := manager.SubmitPassword(r.Context(), req.Password); err != nil {
		s.fail(w, err, "telegram password")
		return
	}
	writeJSON(w, http.StatusOK, manager.Status())
}

func (s *Server) accountFromPath(w http.ResponseWriter, r *http.Request) (*tgc.Manager, bool) {
	manager, ok := s.accounts.Manager(chi.URLParam(r, "id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no such Telegram account")
		return nil, false
	}
	return manager, true
}

// failAccount maps the account-management errors that are the caller's fault
// onto 4xx, so the WebUI can show them as guidance rather than as a crash.
func (s *Server) failAccount(w http.ResponseWriter, err error, action string) {
	switch {
	case errors.Is(err, tgc.ErrLastAccount):
		writeError(w, http.StatusConflict,
			"这是最后一个启用的 Telegram 账号，删除或停用它会让整个网盘无法访问")
	case errors.Is(err, tgc.ErrCannotPromote):
		writeError(w, http.StatusConflict,
			"主账号无权在这个频道里授予管理员权限（通常是因为它不是频道创建者）。"+
				"请在 Telegram 客户端里手动把这个账号设为管理员，并勾选发消息、编辑消息和删除消息。")
	case errors.Is(err, tgc.ErrNotReady):
		writeError(w, http.StatusConflict, "请先让这个账号登录 Telegram，再把它加入存储频道")
	default:
		s.fail(w, err, action)
	}
}
