package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"go.uber.org/zap/zapcore"

	"github.com/dibin/tdrive/internal/config"
)

// settingsBody is intentionally limited to values that are safe to change
// while the server is running. Paths, the listen address and bootstrap account
// stay deployment-only environment settings.
type settingsBody struct {
	AppID             int    `json:"appId"`
	AppHash           string `json:"appHash"`
	SegmentSize       int64  `json:"segmentSize"`
	PoolSize          int64  `json:"poolSize"`
	UploadThreads     int    `json:"uploadThreads"`
	StreamConcurrency int    `json:"streamConcurrency"`
	WebDAVEnabled     bool   `json:"webdavEnabled"`
	LogLevel          string `json:"logLevel"`
}

type settingsUpdateRequest struct {
	AppID             *int    `json:"appId"`
	AppHash           *string `json:"appHash"`
	SegmentSize       *int64  `json:"segmentSize"`
	PoolSize          *int64  `json:"poolSize"`
	UploadThreads     *int    `json:"uploadThreads"`
	StreamConcurrency *int    `json:"streamConcurrency"`
	WebDAVEnabled     *bool   `json:"webdavEnabled"`
	LogLevel          *string `json:"logLevel"`
}

func toSettingsBody(s config.RuntimeSettings) settingsBody {
	return settingsBody{
		AppID:             s.AppID,
		AppHash:           s.AppHash,
		SegmentSize:       s.SegmentSize,
		PoolSize:          s.PoolSize,
		UploadThreads:     s.UploadThreads,
		StreamConcurrency: s.StreamConcurrency,
		WebDAVEnabled:     s.WebDAVEnabled,
		LogLevel:          s.LogLevel,
	}
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, toSettingsBody(s.cfg.RuntimeSettings()))
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
	if req.SegmentSize != nil {
		next.SegmentSize = *req.SegmentSize
	}
	if req.PoolSize != nil {
		next.PoolSize = *req.PoolSize
	}
	if req.UploadThreads != nil {
		next.UploadThreads = *req.UploadThreads
	}
	if req.StreamConcurrency != nil {
		next.StreamConcurrency = *req.StreamConcurrency
	}
	if req.WebDAVEnabled != nil {
		next.WebDAVEnabled = *req.WebDAVEnabled
	}
	if req.LogLevel != nil {
		next.LogLevel = strings.ToLower(strings.TrimSpace(*req.LogLevel))
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
		config.SettingTGAppID:           strconv.Itoa(next.AppID),
		config.SettingTGAppHash:         next.AppHash,
		config.SettingSegmentSize:       strconv.FormatInt(next.SegmentSize, 10),
		config.SettingTGPoolSize:        strconv.FormatInt(next.PoolSize, 10),
		config.SettingUploadThreads:     strconv.Itoa(next.UploadThreads),
		config.SettingStreamConcurrency: strconv.Itoa(next.StreamConcurrency),
		config.SettingWebDAVEnabled:     strconv.FormatBool(next.WebDAVEnabled),
		config.SettingLogLevel:          next.LogLevel,
	}
	if err := s.db.SetSettings(r.Context(), values); err != nil {
		s.fail(w, err, "save settings")
		return
	}

	s.cfg.SetRuntimeSettings(next)
	if s.setLogLevel != nil && next.LogLevel != current.LogLevel {
		if err := s.setLogLevel(next.LogLevel); err != nil {
			s.fail(w, err, "apply log level")
			return
		}
	}

	// A new app credential pair must reconnect the Telegram client. Pool size
	// also belongs to connection setup, so changing it rebuilds the pool while
	// preserving the existing session file.
	var reconnectErr error
	configureTelegram := credentialsChanged ||
		(!s.tg.Ready() && (req.AppID != nil || req.AppHash != nil) && next.AppID > 0 && next.AppHash != "")
	if configureTelegram {
		reconnectErr = s.tg.Configure(r.Context(), next.AppID, next.AppHash)
	} else if next.PoolSize != current.PoolSize && s.tg.Ready() {
		reconnectErr = s.tg.Reload(r.Context())
	}
	if reconnectErr != nil {
		s.fail(w, reconnectErr, "apply telegram settings")
		return
	}

	writeJSON(w, http.StatusOK, toSettingsBody(next))
}
