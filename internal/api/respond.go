// Package api serves the REST endpoints the WebUI talks to.
package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"go.uber.org/zap"

	"github.com/dibin/tdrive/internal/auth"
	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/drive"
	"github.com/dibin/tdrive/internal/localfs"
	"github.com/dibin/tdrive/internal/tgc"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

type errorBody struct {
	Error string `json:"error"`
	// Code lets the WebUI react to specific conditions — showing the setup
	// wizard, or prompting for a Telegram login — without matching on prose.
	Code string `json:"code,omitempty"`
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorBody{Error: message})
}

// fail maps a domain error onto the right status code so handlers can return
// errors without each one restating the mapping.
func (s *Server) fail(w http.ResponseWriter, err error, action string) {
	switch {
	case errors.Is(err, localfs.ErrDisabled):
		writeJSON(w, http.StatusPreconditionRequired, errorBody{
			Error: err.Error(), Code: "local_unconfigured",
		})
	case errors.Is(err, localfs.ErrInvalidPath):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, localfs.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, localfs.ErrNotFile), errors.Is(err, localfs.ErrNotDir):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, localfs.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, drive.ErrNotFound), errors.Is(err, database.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, drive.ErrExists), errors.Is(err, database.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, drive.ErrLoop), errors.Is(err, drive.ErrIsDir), errors.Is(err, drive.ErrNotDir):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, drive.ErrUploadTaskClosed):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, auth.ErrBadCredentials):
		writeError(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, auth.ErrWeakPassword):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, drive.ErrNoChannel):
		writeJSON(w, http.StatusPreconditionRequired, errorBody{
			Error: err.Error(), Code: "no_channel",
		})
	case errors.Is(err, tgc.ErrNotConfigured):
		writeJSON(w, http.StatusPreconditionRequired, errorBody{
			Error: err.Error(), Code: "telegram_unconfigured",
		})
	case errors.Is(err, tgc.ErrNotReady):
		writeJSON(w, http.StatusPreconditionRequired, errorBody{
			Error: err.Error(), Code: "telegram_unauthorized",
		})
	case errors.Is(err, tgc.ErrInvalidSession):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		var pathErr *drive.PathError
		if errors.As(err, &pathErr) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// Anything unclassified is a bug or an outage. The detail goes to
		// the log; the client gets enough to report it.
		s.log.Error("request failed", zap.String("action", action), zap.Error(err))
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request body: "+err.Error())
		return false
	}
	return true
}

// currentUser is only called from handlers behind RequireAuth, so the absence
// of a user is a routing mistake rather than an authentication failure.
func currentUser(r *http.Request) database.User {
	u, _ := auth.FromContext(r.Context())
	return u
}
