package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/plugin"
)

type pluginInspectRequest struct {
	ManifestURL string `json:"manifestUrl"`
	// ManifestDigest is supplied by the store so the manifest tdrive fetches
	// is the one the store curator reviewed. It is absent when an
	// administrator pastes a URL directly.
	ManifestDigest string `json:"manifestDigest,omitempty"`
}

type pluginInstallRequest struct {
	InspectionID string `json:"inspectionId"`
	Confirm      bool   `json:"confirm"`
}

type pluginEnabledRequest struct {
	Enabled bool `json:"enabled"`
}

// pluginRoutes is mounted inside the authenticated group rather than the
// administrator one, because plugins are installed per account: every endpoint
// here acts on the caller's own installations and can see nobody else's.
//
// Which accounts may install is a permission (PermPlugins, held by the admin
// role by default) rather than a role. Reading and configuring what you
// already own needs nothing extra — the list is yours by construction, and the
// plugin's own UI writes the same state through the host data bridge anyway.
// Everything that reaches the network or starts a child process is gated.
//
// The installation endpoint deliberately has one boolean confirmation and no
// capability array: the plugin trust model is explicit and all-or-nothing.
func (s *Server) pluginRoutes(r chi.Router) {
	if s.plugins == nil {
		return
	}
	r.Route("/plugins", func(r chi.Router) {
		r.Get("/", s.handleListPlugins)
		r.Get("/{id}/settings", s.handlePluginSettings)
		r.Put("/{id}/settings", s.handleUpdatePluginSettings)

		r.Group(func(r chi.Router) {
			r.Use(s.auth.RequirePerm(database.PermPlugins))
			r.Get("/updates", s.handlePluginUpdates)
			r.Get("/store", s.handlePluginStore)
			r.Post("/inspect", s.handleInspectPlugin)
			r.Post("/install", s.handleInstallPlugin)
			r.Post("/{id}/enable", s.handleSetPluginEnabled)
			r.Delete("/{id}", s.handleUninstallPlugin)
		})
	})
}

func (s *Server) handleListPlugins(w http.ResponseWriter, r *http.Request) {
	plugins, err := s.plugins.List(r.Context(), currentUser(r).ID)
	if err != nil {
		s.fail(w, err, "list plugins")
		return
	}
	writeJSON(w, http.StatusOK, plugins)
}

// handlePluginUpdates reports which installed plugins have a newer release.
//
// It only reports. Installing what it finds goes back through inspect and
// install, so an update is reviewed and confirmed exactly like a first
// installation — the plugin trust model does not soften because the plugin is
// already there. refresh=1 bypasses the cached answer.
func (s *Server) handlePluginUpdates(w http.ResponseWriter, r *http.Request) {
	report, err := s.plugins.CheckUpdates(r.Context(), currentUser(r).ID, r.URL.Query().Get("refresh") == "1")
	if err != nil {
		s.fail(w, err, "check plugin updates")
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// handlePluginStore returns the store index marked up with what the caller has
// already installed. The index itself is a public document and the same for
// everybody; only the "installed" flag is personal.
func (s *Server) handlePluginStore(w http.ResponseWriter, r *http.Request) {
	items, err := s.plugins.StoreWithStatus(r.Context(), currentUser(r).ID, r.URL.Query().Get("q"))
	if err != nil {
		s.fail(w, err, "plugin store")
		return
	}
	writeJSON(w, http.StatusOK, pluginStoreResponse{Plugins: items})
}

// pluginStoreResponse keeps the wire shape the WebUI already reads. The
// per-user installed markers ride along inside each entry.
type pluginStoreResponse struct {
	Plugins []plugin.StoreStatus `json:"plugins"`
}

func (s *Server) handleInspectPlugin(w http.ResponseWriter, r *http.Request) {
	var request pluginInspectRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	inspection, err := s.plugins.Inspect(r.Context(), currentUser(r).ID, request.ManifestURL, request.ManifestDigest)
	if err != nil {
		s.fail(w, err, "inspect plugin")
		return
	}
	s.audit(r, database.AuditPluginInspect, inspection.Manifest.ID, request.ManifestURL)
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
	installed, err := s.plugins.Install(r.Context(), currentUser(r).ID, request.InspectionID)
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
	status, err := s.plugins.SetEnabled(r.Context(), currentUser(r).ID, id, request.Enabled)
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
	if err := s.plugins.Uninstall(r.Context(), currentUser(r).ID, id); err != nil {
		s.fail(w, err, "uninstall plugin")
		return
	}
	s.audit(r, database.AuditPluginUninstall, id, "uninstalled")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePluginSettings(w http.ResponseWriter, r *http.Request) {
	value, err := s.plugins.Settings(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"))
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
	if err := s.plugins.UpdateSettings(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"), value); err != nil {
		s.fail(w, err, "update plugin settings")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
