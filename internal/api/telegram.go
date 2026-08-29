package api

import (
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/dibin/tdrive/internal/events"
)

func (s *Server) handleTelegramStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.tg.Status())
}

// publishTelegram pushes the connection state so the setup wizard advances
// without the browser polling.
func (s *Server) publishTelegram() {
	s.events.Publish(events.Event{Type: events.TypeTelegram, Data: s.tg.Status()})
}

type configureRequest struct {
	AppID   int    `json:"appId"`
	AppHash string `json:"appHash"`
}

func (s *Server) handleTelegramConfigure(w http.ResponseWriter, r *http.Request) {
	var req configureRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.tg.Configure(r.Context(), req.AppID, req.AppHash); err != nil {
		s.fail(w, err, "configure telegram")
		return
	}
	s.publishTelegram()
	writeJSON(w, http.StatusOK, s.tg.Status())
}

type phoneRequest struct {
	Phone string `json:"phone"`
}

func (s *Server) handleSendCode(w http.ResponseWriter, r *http.Request) {
	var req phoneRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	res, err := s.tg.SendCode(r.Context(), req.Phone)
	if err != nil {
		s.fail(w, err, "send login code")
		return
	}
	s.publishTelegram()
	writeJSON(w, http.StatusOK, res)
}

type codeRequest struct {
	Code string `json:"code"`
}

func (s *Server) handleSignIn(w http.ResponseWriter, r *http.Request) {
	var req codeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	res, err := s.tg.SignIn(r.Context(), req.Code)
	if err != nil {
		s.fail(w, err, "telegram sign in")
		return
	}
	s.publishTelegram()
	writeJSON(w, http.StatusOK, res)
}

type tgPasswordRequest struct {
	Password string `json:"password"`
}

func (s *Server) handleSubmitPassword(w http.ResponseWriter, r *http.Request) {
	var req tgPasswordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.tg.SubmitPassword(r.Context(), req.Password); err != nil {
		s.fail(w, err, "telegram password")
		return
	}
	s.publishTelegram()
	writeJSON(w, http.StatusOK, s.tg.Status())
}

func (s *Server) handleTelegramLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.tg.LogOut(r.Context()); err != nil {
		s.fail(w, err, "telegram logout")
		return
	}
	s.publishTelegram()
	writeJSON(w, http.StatusOK, s.tg.Status())
}

const telegramAccountExportVersion = 1

type telegramAccountExport struct {
	Format  string `json:"format"`
	Version int    `json:"version"`
	AppID   int    `json:"appId"`
	AppHash string `json:"appHash"`
	Session string `json:"session"`
}

// handleTelegramAccountExport returns only the Telegram login session and the
// app credentials needed to open it. WebUI users, channels and the local index
// are intentionally not part of this portable account package.
func (s *Server) handleTelegramAccountExport(w http.ResponseWriter, r *http.Request) {
	appID, appHash, err := s.tg.Credentials(r.Context())
	if err != nil {
		s.fail(w, err, "export telegram account")
		return
	}
	sessionData, err := s.tg.ExportSession(r.Context())
	if err != nil {
		s.fail(w, err, "export telegram account")
		return
	}

	w.Header().Set("Content-Disposition", `attachment; filename="tdrive-telegram-account.json"`)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, telegramAccountExport{
		Format:  "tdrive-telegram-account",
		Version: telegramAccountExportVersion,
		AppID:   appID,
		AppHash: appHash,
		Session: base64.StdEncoding.EncodeToString(sessionData),
	})
}

// handleTelegramAccountImport accepts a package produced by the export
// endpoint and reconnects the server with that Telegram account.
func (s *Server) handleTelegramAccountImport(w http.ResponseWriter, r *http.Request) {
	var req telegramAccountExport
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Format != "tdrive-telegram-account" || req.Version != telegramAccountExportVersion {
		writeError(w, http.StatusBadRequest, "unsupported Telegram account file")
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
	sessionData, err := base64.StdEncoding.DecodeString(req.Session)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Telegram account session is not valid base64")
		return
	}
	if err := s.tg.ImportSession(r.Context(), req.AppID, req.AppHash, sessionData); err != nil {
		s.fail(w, err, "import telegram account")
		return
	}
	s.publishTelegram()
	writeJSON(w, http.StatusOK, s.tg.Status())
}

// handleListChannels offers the account's existing channels so a drive can
// reuse one rather than always creating a new channel.
func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	channels, err := s.tg.ListChannels(r.Context())
	if err != nil {
		s.fail(w, err, "list channels")
		return
	}

	// Mark the one currently in use so the picker can show it as selected.
	selected := int64(0)
	if def, err := s.db.DefaultChannel(r.Context()); err == nil {
		selected = def.TGID
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"channels": channels,
		"selected": selected,
	})
}

type createChannelRequest struct {
	Title string `json:"title"`
	About string `json:"about"`
}

// handleCreateChannel makes a dedicated private channel and adopts it as the
// storage target in one step, which is the normal path through the wizard.
func (s *Server) handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	var req createChannelRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Title == "" {
		req.Title = "TDrive"
	}
	if req.About == "" {
		req.About = "Storage channel for TDrive. Messages here are file records; editing them by hand will confuse the index."
	}

	info, err := s.tg.CreateChannel(r.Context(), req.Title, req.About)
	if err != nil {
		s.fail(w, err, "create channel")
		return
	}
	channel, err := s.db.UpsertChannel(r.Context(), info.TGID, info.AccessHash, info.Title)
	if err != nil {
		s.fail(w, err, "create channel")
		return
	}
	if err := s.db.SetDefaultChannel(r.Context(), channel.ID); err != nil {
		s.fail(w, err, "create channel")
		return
	}
	writeJSON(w, http.StatusCreated, channel)
}

type selectChannelRequest struct {
	TGID       int64 `json:"tgId"`
	AccessHash int64 `json:"accessHash"`
}

// handleSelectChannel adopts an existing channel. The channel is re-resolved
// first: a stale access hash would let the selection succeed and then break
// every subsequent upload and download.
func (s *Server) handleSelectChannel(w http.ResponseWriter, r *http.Request) {
	var req selectChannelRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	info, err := s.tg.ResolveChannel(r.Context(), req.TGID, req.AccessHash)
	if err != nil {
		s.fail(w, err, "select channel")
		return
	}
	if !info.CanPost {
		writeError(w, http.StatusBadRequest,
			"this account cannot post to that channel, so it cannot be used for storage")
		return
	}

	channel, err := s.db.UpsertChannel(r.Context(), info.TGID, info.AccessHash, info.Title)
	if err != nil {
		s.fail(w, err, "select channel")
		return
	}
	if err := s.db.SetDefaultChannel(r.Context(), channel.ID); err != nil {
		s.fail(w, err, "select channel")
		return
	}
	writeJSON(w, http.StatusOK, channel)
}
