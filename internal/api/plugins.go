package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/dibin/tdrive/internal/database"
)

type pluginInspectRequest struct {
	SourceURL    string `json:"sourceUrl"`
	Ref          string `json:"ref,omitempty"`
	SourceDigest string `json:"sourceDigest,omitempty"`
}

type pluginInstallRequest struct {
	InspectionID string `json:"inspectionId"`
	Confirm      bool   `json:"confirm"`
}

type pluginEnabledRequest struct {
	Enabled bool `json:"enabled"`
}

// pluginRoutes is mounted only inside the authenticated administrator group.
// The installation endpoint deliberately has one boolean confirmation and no
// capability array: the plugin trust model is explicit and all-or-nothing.
func (s *Server) pluginRoutes(r chi.Router) {
	if s.plugins == nil {
		return
	}
	r.Route("/plugins", func(r chi.Router) {
		r.Get("/", s.handleListPlugins)
		r.Get("/store", s.handlePluginStore)
		r.Post("/inspect", s.handleInspectPlugin)
		r.Post("/install", s.handleInstallPlugin)
		r.Post("/{id}/enable", s.handleSetPluginEnabled)
		r.Get("/{id}/settings", s.handlePluginSettings)
		r.Put("/{id}/settings", s.handleUpdatePluginSettings)
		r.Delete("/{id}", s.handleUninstallPlugin)
	})
}

func (s *Server) handleListPlugins(w http.ResponseWriter, r *http.Request) {
	plugins, err := s.plugins.List(r.Context())
	if err != nil {
		s.fail(w, err, "list plugins")
		return
	}
	writeJSON(w, http.StatusOK, plugins)
}

func (s *Server) handlePluginStore(w http.ResponseWriter, r *http.Request) {
	index, err := s.plugins.Store(r.Context(), r.URL.Query().Get("q"))
	if err != nil {
		s.fail(w, err, "plugin store")
		return
	}
	writeJSON(w, http.StatusOK, index)
}

func (s *Server) handleInspectPlugin(w http.ResponseWriter, r *http.Request) {
	var request pluginInspectRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	inspection, err := s.plugins.Inspect(r.Context(), request.SourceURL, request.Ref, request.SourceDigest)
	if err != nil {
		s.fail(w, err, "inspect plugin")
		return
	}
	s.audit(r, database.AuditPluginInspect, inspection.Manifest.ID, request.SourceURL)
	writeJSON(w, http.StatusOK, inspection)
}

func (s *Server) handleInstallPlugin(w http.ResponseWriter, r *http.Request) {
	var request pluginInstallRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	if request.InspectionID == "" {
		writeError(w, http.StatusBadRequest, "inspectionId is required")
		return
	}
	if !request.Confirm {
		writeError(w, http.StatusBadRequest, "plugin installation requires one confirmation")
		return
	}
	installed, err := s.plugins.Install(r.Context(), request.InspectionID)
	if err != nil {
		s.fail(w, err, "install plugin")
		return
	}
	s.audit(r, database.AuditPluginInstall, installed.ID, installed.Manifest.Version)
	writeJSON(w, http.StatusCreated, installed)
}

func (s *Server) handleSetPluginEnabled(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var request pluginEnabledRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	status, err := s.plugins.SetEnabled(r.Context(), id, request.Enabled)
	if err != nil {
		s.fail(w, err, "set plugin state")
		return
	}
	if request.Enabled {
		s.audit(r, database.AuditPluginEnable, id, "enabled")
	} else {
		s.audit(r, database.AuditPluginDisable, id, "disabled")
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleUninstallPlugin(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.plugins.Uninstall(r.Context(), id); err != nil {
		s.fail(w, err, "uninstall plugin")
		return
	}
	s.audit(r, database.AuditPluginUninstall, id, "uninstalled")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePluginSettings(w http.ResponseWriter, r *http.Request) {
	value, err := s.plugins.Settings(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.fail(w, err, "read plugin settings")
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (s *Server) handleUpdatePluginSettings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	value, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("read plugin settings: %v", err))
		return
	}
	if len(value) == 0 {
		value = []byte("{}")
	}
	if !json.Valid(value) {
		writeError(w, http.StatusBadRequest, "plugin settings must be valid JSON")
		return
	}
	if err := s.plugins.UpdateSettings(r.Context(), chi.URLParam(r, "id"), value); err != nil {
		s.fail(w, err, "update plugin settings")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
