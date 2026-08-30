package api

import (
	"net/http"

	"github.com/dibin/tdrive/internal/drive"
	"github.com/dibin/tdrive/internal/localfs"
)

type localListBody struct {
	Path        string          `json:"path"`
	Entries     []localfs.Entry `json:"entries"`
	Breadcrumbs []crumb         `json:"breadcrumbs"`
}

// handleLocalList exposes only the configured read-only source directory. The
// path is relative to that root, never an absolute path on the VPS.
func (s *Server) handleLocalList(w http.ResponseWriter, r *http.Request) {
	requestPath := r.URL.Query().Get("path")
	if requestPath == "" {
		requestPath = drive.Root
	}

	entries, clean, err := localfs.New(s.cfg.RuntimeSettings().LocalRoot).List(requestPath)
	if err != nil {
		s.fail(w, err, "list local files")
		return
	}
	if entries == nil {
		entries = []localfs.Entry{}
	}
	writeJSON(w, http.StatusOK, localListBody{
		Path:        clean,
		Entries:     entries,
		Breadcrumbs: breadcrumbs(clean),
	})
}

type localUploadRequest struct {
	SourcePath string `json:"sourcePath"`
	Path       string `json:"path"`
	Name       string `json:"name,omitempty"`
	Overwrite  bool   `json:"overwrite,omitempty"`
}

// handleLocalUpload creates a server-side transfer. The VPS file therefore
// never travels through the browser or the public HTTP connection.
func (s *Server) handleLocalUpload(w http.ResponseWriter, r *http.Request) {
	var req localUploadRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.SourcePath == "" {
		writeError(w, http.StatusBadRequest, "sourcePath is required")
		return
	}
	dest, err := s.realPath(r, req.Path)
	if err != nil {
		s.fail(w, err, "local upload")
		return
	}

	job, err := s.drive.StartLocal(r.Context(), drive.LocalRequest{
		SourcePath: req.SourcePath,
		DirPath:    dest,
		Name:       req.Name,
		UserID:     currentUser(r).ID,
		Overwrite:  req.Overwrite,
	})
	if err != nil {
		s.fail(w, err, "local upload")
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}
