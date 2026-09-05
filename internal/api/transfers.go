package api

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/dibin/tdrive/internal/database"
)

// The transfer panel shows one list. Uploads and downloads are stored apart
// because their state machines have nothing in common, but a person watching
// their transfers does not care which table a row came from — they care what
// is moving, how fast, and what happened to the thing they started an hour
// ago. So the filtering, the date range and the deletion all operate on the
// merged view, and the merge happens here rather than in the browser.

// uploadJobBody is the wire shape of an upload, aligned field-for-field with
// downloadJobBody wherever the two mean the same thing.
type uploadJobBody struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	FileID        string `json:"fileId,omitempty"`
	DirID         string `json:"dirId,omitempty"`
	Name          string `json:"name"`
	TotalSize     int64  `json:"totalSize"`
	SegmentSize   int64  `json:"segmentSize"`
	SegmentCount  int    `json:"segmentCount"`
	UploadedBytes int64  `json:"uploadedBytes"`
	Status        string `json:"status"`
	Error         string `json:"error,omitempty"`
	Source        string `json:"source,omitempty"`
	SourceURL     string `json:"sourceUrl,omitempty"`
	CreatedAt     int64  `json:"createdAt"`
	UpdatedAt     int64  `json:"updatedAt"`
	StartedAt     int64  `json:"startedAt,omitempty"`
	FinishedAt    int64  `json:"finishedAt,omitempty"`
	// AvgSpeed is bytes per second over the window the transfer was actually
	// moving. It is computed here rather than in the browser because only the
	// server knows when the job left the queue and started sending.
	AvgSpeed float64 `json:"avgSpeed,omitempty"`
	// Speed is the current rate of a transfer this process is driving. A
	// browser upload reports its own and leaves this zero; a WebDAV write or a
	// VPS-local upload has no browser to ask, which is why the server measures
	// it.
	Speed float64 `json:"speed,omitempty"`
}

func uploadBody(job database.UploadJob) uploadJobBody {
	body := uploadJobBody{
		ID:            job.ID,
		Kind:          "upload",
		FileID:        job.FileID,
		DirID:         job.DirID,
		Name:          job.Name,
		TotalSize:     job.TotalSize,
		SegmentSize:   job.SegmentSize,
		SegmentCount:  job.SegmentCount,
		UploadedBytes: job.UploadedBytes,
		Status:        string(job.Status),
		Error:         job.Error,
		Source:        job.Source,
		SourceURL:     job.SourceURL,
		CreatedAt:     job.CreatedAt.UnixMilli(),
		UpdatedAt:     job.UpdatedAt.UnixMilli(),
		AvgSpeed: averageSpeed(job.UploadedBytes, job.StartedAt,
			measuredUntil(job.FinishedAt, job.Status == database.JobPending || job.Status == database.JobRunning)),
	}
	if !job.StartedAt.IsZero() {
		body.StartedAt = job.StartedAt.UnixMilli()
	}
	if !job.FinishedAt.IsZero() {
		body.FinishedAt = job.FinishedAt.UnixMilli()
	}
	return body
}

// transferRow is one entry of the merged list. Exactly one of Upload and
// Download is set; the shared fields are lifted out so the client can sort and
// group without unwrapping.
type transferRow struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	CreatedAt int64  `json:"createdAt"`

	Upload   *uploadJobBody   `json:"upload,omitempty"`
	Download *downloadJobBody `json:"download,omitempty"`
}

type transferListBody struct {
	Transfers []transferRow `json:"transfers"`
	// Sources and statuses actually present in the unfiltered history, so the
	// filter bar can show only the chips that would match something.
	Total int `json:"total"`
}

// transferFilterFrom parses the query string into a database filter.
func (s *Server) transferFilterFrom(r *http.Request) database.TransferFilter {
	query := r.URL.Query()
	user := currentUser(r)

	filter := database.TransferFilter{
		UserID:   user.ID,
		Statuses: splitList(query.Get("status")),
		Sources:  splitList(query.Get("source")),
		Query:    query.Get("q"),
	}
	// Only an administrator can widen the view past their own transfers, and
	// only by asking for it.
	if user.Role == database.RoleAdmin && query.Get("all") == "1" {
		filter.AllUsers = true
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
	return filter
}

func (s *Server) handleListTransfers(w http.ResponseWriter, r *http.Request) {
	filter := s.transferFilterFrom(r)
	kind := r.URL.Query().Get("kind")

	rows := make([]transferRow, 0, 64)

	if kind == "" || kind == "upload" {
		uploads, err := s.db.FilterUploads(r.Context(), filter)
		if err != nil {
			s.fail(w, err, "list transfers")
			return
		}
		for i := range uploads {
			// The live snapshot fills the gap between two segment completions,
			// so a refresh does not briefly show progress going backwards.
			s.progress.merge(&uploads[i])
			body := uploadBody(uploads[i])
			body.Speed = s.progress.speed(body.ID)
			rows = append(rows, transferRow{
				ID: body.ID, Kind: body.Kind, Name: body.Name,
				Status: body.Status, CreatedAt: body.CreatedAt, Upload: &body,
			})
		}
	}

	if kind == "" || kind == "download" {
		downloads, err := s.db.FilterDownloads(r.Context(), filter)
		if err != nil {
			s.fail(w, err, "list transfers")
			return
		}
		for _, job := range downloads {
			body := s.downloadBody(job)
			rows = append(rows, transferRow{
				ID: body.ID, Kind: body.Kind, Name: body.Name,
				Status: body.Status, CreatedAt: body.CreatedAt, Download: &body,
			})
		}
	}

	// Both tables come back newest-first; merging them needs one more sort.
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].CreatedAt > rows[j].CreatedAt
	})

	writeJSON(w, http.StatusOK, transferListBody{Transfers: rows, Total: len(rows)})
}

type deleteTransfersRequest struct {
	// IDs names specific rows. With none given, everything matching the
	// filter is removed, which is how "clear all finished" is expressed.
	IDs  []string `json:"ids,omitempty"`
	Kind string   `json:"kind,omitempty"`
	// Statuses and Before narrow a bulk delete. Before is Unix milliseconds.
	Statuses []string `json:"statuses,omitempty"`
	Before   int64    `json:"before,omitempty"`
	// All lets an administrator clear history that belongs to other accounts.
	All bool `json:"all,omitempty"`
}

// handleDeleteTransfers removes history rows.
//
// Only terminal rows can go: deleting a running job's record would orphan the
// transfer still writing to it. Staged downloads take their cached bytes with
// them, because a cache entry nothing points at is just leaked disk.
func (s *Server) handleDeleteTransfers(w http.ResponseWriter, r *http.Request) {
	var req deleteTransfersRequest
	if r.ContentLength > 0 && !decodeJSON(w, r, &req) {
		return
	}

	user := currentUser(r)
	filter := database.TransferFilter{
		UserID:   user.ID,
		Statuses: req.Statuses,
		To:       req.Before,
	}
	if req.All && user.Role == database.RoleAdmin {
		filter.AllUsers = true
	}

	var removed int64
	if req.Kind == "" || req.Kind == "upload" {
		n, err := s.db.DeleteFinishedUploads(r.Context(), filter, req.IDs)
		if err != nil {
			s.fail(w, err, "delete transfers")
			return
		}
		removed += n
	}

	if req.Kind == "" || req.Kind == "download" {
		doomed, err := s.db.FinishedDownloadsToDelete(r.Context(), filter, req.IDs)
		if err != nil {
			s.fail(w, err, "delete transfers")
			return
		}
		for _, job := range doomed {
			// DeleteStaged unlinks the cached file first, so the row and the
			// bytes disappear together.
			if err := s.drive.DeleteStaged(r.Context(), job); err != nil {
				s.fail(w, err, "delete transfers")
				return
			}
			removed++
		}
	}

	s.audit(r, database.AuditTransferDelete, "", fmt.Sprintf("removed=%d", removed))
	writeJSON(w, http.StatusOK, map[string]int64{"removed": removed})
}

// handleDeleteTransfer removes one history row, which is the per-row delete
// button in the transfer panel.
func (s *Server) handleDeleteTransfer(w http.ResponseWriter, r *http.Request) {
	kind := chi.URLParam(r, "kind")
	id := chi.URLParam(r, "id")

	user := currentUser(r)
	filter := database.TransferFilter{UserID: user.ID}
	if user.Role == database.RoleAdmin {
		filter.AllUsers = true
	}

	switch kind {
	case "upload":
		n, err := s.db.DeleteFinishedUploads(r.Context(), filter, []string{id})
		if err != nil {
			s.fail(w, err, "delete transfer")
			return
		}
		if n == 0 {
			writeError(w, http.StatusConflict, "仅可删除已结束的传输记录。")
			return
		}
	case "download":
		job, err := s.downloadForUser(r, id)
		if err != nil {
			s.fail(w, err, "delete transfer")
			return
		}
		// downloadForUser lets anyone with access to the file read or join a
		// staged job, which is what makes a shared staged copy work. Deleting
		// the record is not part of that: it is someone else's history, and the
		// bulk endpoint scopes it to the owner too.
		if job.UserID != "" && job.UserID != user.ID && user.Role != database.RoleAdmin {
			s.fail(w, fmt.Errorf("%w: download", database.ErrNotFound), "delete transfer")
			return
		}
		switch job.Status {
		case database.DownloadPending, database.DownloadRunning:
			writeError(w, http.StatusConflict, "该下载正在进行，请先取消再删除。")
			return
		}
		if err := s.drive.DeleteStaged(r.Context(), job); err != nil {
			s.fail(w, err, "delete transfer")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "kind must be upload or download")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
