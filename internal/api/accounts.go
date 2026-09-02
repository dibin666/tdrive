package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

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
	// ProxyURL is deliberately masked. Proxy credentials are stored on the
	// server, but must never be sent back to the browser in the accounts list.
	ProxyURL string `json:"proxyUrl,omitempty"`
	// ChannelTitle names the current global storage channel. InChannel and
	// CanPost below say whether this account can actually use it.
	ChannelTitle string `json:"channelTitle,omitempty"`

	Status tgc.Status `json:"status"`
	// CanPost is whether this account has been admitted to the storage channel
	// with posting rights. Without it the account is configured but idle.
	CanPost bool `json:"canPost"`
	// InChannel distinguishes "not a member" from "a member who cannot post".
	InChannel bool `json:"inChannel"`

	// ActiveUploads and ActiveDownloads identify which account currently owns a
	// globally admitted task. They are status counters, not per-account limits.
	ActiveUploads   int `json:"activeUploads"`
	ActiveDownloads int `json:"activeDownloads"`

	// Daily quotas are byte budgets. Zero means unlimited; usage and
	// reservations are for the current UTC calendar day.
	UploadDailyQuota       int64  `json:"uploadDailyQuota"`
	DownloadDailyQuota     int64  `json:"downloadDailyQuota"`
	UploadUsedToday        int64  `json:"uploadUsedToday"`
	DownloadUsedToday      int64  `json:"downloadUsedToday"`
	UploadReservedToday    int64  `json:"uploadReservedToday"`
	DownloadReservedToday  int64  `json:"downloadReservedToday"`
	UploadRemainingToday   int64  `json:"uploadRemainingToday"`
	DownloadRemainingToday int64  `json:"downloadRemainingToday"`
	QuotaDate              string `json:"quotaDate"`
	QuotaResetAt           int64  `json:"quotaResetAt"`
}

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	// Login completion normally triggers this check through Cluster.OnReady.
	// Polling also kicks it here so an account added to the channel manually,
	// or an account that was already running when the channel was changed,
	// becomes usable without requiring a failed transfer first.
	checkContext, cancelCheck := context.WithTimeout(
		context.WithoutCancel(r.Context()), 20*time.Second,
	)
	go func() {
		defer cancelCheck()
		s.accounts.RefreshReadyChannels(checkContext)
	}()

	accountRows, err := s.db.ListAccounts(r.Context())
	if err != nil {
		s.fail(w, err, "list telegram accounts")
		return
	}

	// Channel membership is per account, so the picture is only complete with
	// the default channel in hand. A drive with no channel yet simply reports
	// every account as not admitted.
	access := map[string]database.ChannelAccess{}
	storageChannelTitle := ""
	if channel, err := s.db.DefaultChannel(r.Context()); err == nil {
		storageChannelTitle = channel.Title
		accessRows, err := s.db.ChannelAccesses(r.Context(), channel.ID)
		if err != nil {
			s.fail(w, err, "list telegram accounts")
			return
		}
		for _, row := range accessRows {
			access[row.AccountID] = row
		}
	}

	upload, download := s.drive.ActiveTasksByAccount()
	out := make([]accountBody, 0, len(accountRows))
	for _, row := range accountRows {
		body := accountBody{
			ID:                 row.ID,
			Label:              row.Label,
			AppID:              row.AppID,
			Enabled:            row.Enabled,
			IsPrimary:          row.IsPrimary,
			ProxyURL:           tgc.MaskProxyURL(row.ProxyURL),
			ChannelTitle:       storageChannelTitle,
			Status:             tgc.Status{State: tgc.StateUnconfigured},
			ActiveUploads:      upload[row.ID],
			ActiveDownloads:    download[row.ID],
			UploadDailyQuota:   row.UploadDailyQuota,
			DownloadDailyQuota: row.DownloadDailyQuota,
		}
		quota := s.drive.AccountQuotaStatus(row.ID, row.UploadDailyQuota, row.DownloadDailyQuota)
		body.UploadUsedToday = quota.Upload.Used
		body.DownloadUsedToday = quota.Download.Used
		body.UploadReservedToday = quota.Upload.Reserved
		body.DownloadReservedToday = quota.Download.Reserved
		body.UploadRemainingToday = quota.Upload.Remaining
		body.DownloadRemainingToday = quota.Download.Remaining
		body.QuotaDate = quota.Date
		body.QuotaResetAt = quota.ResetAt.UnixMilli()
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
	Label    string `json:"label"`
	AppID    int    `json:"appId"`
	AppHash  string `json:"appHash"`
	ProxyURL string `json:"proxyUrl"`
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

	manager, err := s.accounts.Add(
		r.Context(), strings.TrimSpace(req.Label), req.AppID,
		strings.TrimSpace(req.AppHash), strings.TrimSpace(req.ProxyURL),
	)
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

type setAccountProxyRequest struct {
	ProxyURL string `json:"proxyUrl"`
}

// handleSetAccountProxy replaces one account's outbound proxy. An empty value
// intentionally means "go direct". The cluster validates and tests the proxy,
// persists it, then redials the account so the very next Telegram request uses
// the new exit address.
func (s *Server) handleSetAccountProxy(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req setAccountProxyRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	proxyURL, err := tgc.NormalizeProxyURL(strings.TrimSpace(req.ProxyURL))
	if err != nil {
		s.failAccount(w, err, "set telegram account proxy")
		return
	}
	if err := s.accounts.SetProxy(r.Context(), id, proxyURL); err != nil {
		s.failAccount(w, err, "set telegram account proxy")
		return
	}
	s.audit(r, database.AuditSettingsUpdate, id, "updated a telegram account proxy")
	writeJSON(w, http.StatusOK, map[string]any{
		"proxyUrl": tgc.MaskProxyURL(proxyURL),
	})
}

type updateAccountRequest struct {
	Label              *string `json:"label"`
	Enabled            *bool   `json:"enabled"`
	UploadDailyQuota   *int64  `json:"uploadDailyQuota"`
	DownloadDailyQuota *int64  `json:"downloadDailyQuota"`
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
	uploadQuota, downloadQuota := row.UploadDailyQuota, row.DownloadDailyQuota
	if req.Label != nil {
		label = strings.TrimSpace(*req.Label)
	}
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	if req.UploadDailyQuota != nil {
		uploadQuota = *req.UploadDailyQuota
	}
	if req.DownloadDailyQuota != nil {
		downloadQuota = *req.DownloadDailyQuota
	}
	if uploadQuota < 0 || downloadQuota < 0 {
		writeError(w, http.StatusBadRequest, "Telegram account daily quotas must not be negative")
		return
	}

	if err := s.accounts.Update(r.Context(), id, label, enabled); err != nil {
		s.failAccount(w, err, "update telegram account")
		return
	}
	if uploadQuota != row.UploadDailyQuota || downloadQuota != row.DownloadDailyQuota {
		if err := s.accounts.SetQuotas(r.Context(), id, uploadQuota, downloadQuota); err != nil {
			s.failAccount(w, err, "update telegram account daily quotas")
			return
		}
		s.drive.NotifyQuotaChanged()
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
//
// An account that was already added by hand in a Telegram client is detected
// here rather than invited again, so pressing this after doing it manually is
// the supported way to finish the job.
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

// handleAccountChannels lists the channels this account can see from its own
// session, so the storage channel can be pointed at by hand when the automatic
// join cannot work — a primary that may not export invites, or an account that
// was added to the channel manually.
func (s *Server) handleAccountChannels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	manager, ok := s.accountFromPath(w, r)
	if !ok {
		return
	}
	channel, err := s.db.DefaultChannel(r.Context())
	if err != nil {
		s.fail(w, err, "list the channels of a telegram account")
		return
	}
	channels, err := manager.ListChannels(r.Context())
	if err != nil {
		s.failAccount(w, err, "list the channels of a telegram account")
		return
	}

	// The storage channel is sent along so the picker can point at the right
	// row instead of asking someone to recognise a channel by name.
	writeJSON(w, http.StatusOK, map[string]any{
		"channels": channels,
		"storage": map[string]any{
			"tgId":  channel.TGID,
			"title": channel.Title,
		},
	})
}

// handleAccountCheckChannel verifies the account's own access to the current
// storage channel without inviting it or changing any membership. This is the
// explicit check an administrator can run immediately after configuring an
// account or changing its proxy.
func (s *Server) handleAccountCheckChannel(w http.ResponseWriter, r *http.Request) {
	manager, ok := s.accountFromPath(w, r)
	if !ok {
		return
	}
	info, err := s.accounts.CheckChannel(r.Context(), manager)
	if err != nil {
		s.failAccount(w, err, "check the storage channel for a telegram account")
		return
	}
	s.audit(r, database.AuditSettingsUpdate, manager.ID(),
		"checked a telegram account's storage channel access")
	writeJSON(w, http.StatusOK, map[string]any{
		"channel": info,
		"usable":  info.CanPost,
	})
}

type linkAccountChannelRequest struct {
	TGID int64 `json:"tgId"`
}

// handleAccountLinkChannel adopts a channel the operator picked as this
// account's view of the storage channel. The account must already be in it;
// this only records what its own session sees.
func (s *Server) handleAccountLinkChannel(w http.ResponseWriter, r *http.Request) {
	manager, ok := s.accountFromPath(w, r)
	if !ok {
		return
	}
	var req linkAccountChannelRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	channel, err := s.db.DefaultChannel(r.Context())
	if err != nil {
		s.fail(w, err, "link the storage channel to a telegram account")
		return
	}
	if err := s.accounts.LinkChannel(r.Context(), manager, channel, req.TGID); err != nil {
		s.failAccount(w, err, "link the storage channel to a telegram account")
		return
	}
	s.audit(r, database.AuditSettingsUpdate, manager.ID(),
		"linked a telegram account to the storage channel by hand")
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
	case errors.Is(err, tgc.ErrPrimaryNotInChannel):
		writeError(w, http.StatusConflict,
			"主账号看不到存储频道，所以没法自动邀请别的账号进去（通常是主账号换过号、或者被移出了频道）。"+
				"请用这个账号在 Telegram 客户端里加入该频道，然后用「手动选择频道」把它对上。")
	case errors.Is(err, tgc.ErrNotInChannel):
		writeError(w, http.StatusConflict,
			"这个账号还不在存储频道里。请先在 Telegram 客户端里用它加入频道，再回来点一次——加入之后这里会自动认出来。")
	case errors.Is(err, tgc.ErrNoPostRights):
		writeError(w, http.StatusConflict,
			"这个账号在频道里，但没有发消息权限。请在 Telegram 客户端里把它设为管理员，"+
				"并勾选发消息、编辑消息和删除消息。")
	case errors.Is(err, tgc.ErrWrongChannel):
		writeError(w, http.StatusBadRequest,
			"选中的频道不是这个网盘正在使用的存储频道，请选列表里标着「存储频道」的那个。")
	default:
		s.fail(w, err, action)
	}
}
