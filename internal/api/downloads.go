package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/drive"
	"github.com/dibin/tdrive/internal/events"
)

// Downloading a file is a decision, not a button.
//
// For a file that fits in one Telegram object there is nothing to decide: the
// browser follows a link and the server streams it. For a file split across a
// dozen objects the choice actually matters, and the honest thing is to say so
// rather than to pick silently and let a 40 GB transfer fail at the ninth
// segment boundary. So the server describes the options and their trade-offs,
// and the client picks.

type downloadModeInfo struct {
	Mode string `json:"mode"`
	// Available is false when the deployment cannot offer this mode — no cache
	// space configured, or the account lacks the permission.
	Available bool `json:"available"`
	// Recommended marks the mode the UI should default to.
	Recommended bool   `json:"recommended"`
	Reason      string `json:"reason,omitempty"`
}

type segmentBound struct {
	Index int   `json:"index"`
	Start int64 `json:"start"`
	Size  int64 `json:"size"`
}

type downloadOptions struct {
	FileID       string `json:"fileId"`
	Name         string `json:"name"`
	Size         int64  `json:"size"`
	MIME         string `json:"mime,omitempty"`
	SegmentCount int    `json:"segmentCount"`
	// SegmentBounds lets the client show what it would be downloading and, in
	// segments mode, drive the per-part requests.
	SegmentBounds []segmentBound     `json:"segmentBounds,omitempty"`
	Modes         []downloadModeInfo `json:"modes"`
	// MaxConnections is the parallelism ceiling the server will accept for one
	// logical download.
	MaxConnections int `json:"maxConnections"`
	// Staged names a ready staged copy, if one already exists, so the client
	// can skip straight to downloading it.
	Staged *downloadJobBody  `json:"staged,omitempty"`
	Cache  drive.CacheStatus `json:"cache"`
}

func (s *Server) handleDownloadOptions(w http.ResponseWriter, r *http.Request) {
	file, err := s.fileForUser(r, chi.URLParam(r, "id"))
	if err != nil {
		s.fail(w, err, "download options")
		return
	}

	settings := s.cfg.RuntimeSettings()
	user := currentUser(r)
	split := file.SegmentCount > 1

	cache, err := s.drive.CacheStatus(r.Context())
	if err != nil {
		s.fail(w, err, "download options")
		return
	}

	stagingAllowed := user.Can(database.PermStage) && settings.CacheLimit > 0
	fitsInCache := file.Size <= settings.CacheLimit

	out := downloadOptions{
		FileID:         file.ID,
		Name:           file.Name,
		Size:           file.Size,
		MIME:           file.MIME,
		SegmentCount:   file.SegmentCount,
		MaxConnections: settings.MaxDownloadConns,
		Cache:          cache,
	}
	for index := 1; index <= file.SegmentCount; index++ {
		out.SegmentBounds = append(out.SegmentBounds, segmentBound{
			Index: index,
			Start: int64(index-1) * file.SegmentSize,
			Size:  segmentBytes(file, index),
		})
	}

	direct := downloadModeInfo{Mode: string(database.DownloadDirect), Available: true}
	staged := downloadModeInfo{
		Mode:      string(database.DownloadStaged),
		Available: stagingAllowed && fitsInCache,
	}
	segments := downloadModeInfo{
		Mode:      string(database.DownloadSegments),
		Available: split,
	}

	switch {
	case !split:
		direct.Recommended = true
		direct.Reason = "文件无需分卷，支持直接下载、多线程与断点续传。"
		staged.Reason = "无需分卷的文件不必暂存，避免占用服务器磁盘。"
		segments.Reason = "文件无需分卷，无分卷可供下载。"
	case staged.Available:
		staged.Recommended = true
		staged.Reason = "由服务器先拼装为完整文件，再多线程下载，传输更稳定。"
		direct.Reason = "流式拼装在跨分卷边界时容易失败，不推荐用于大文件。"
		segments.Reason = "各分卷下载到本地后合并，不占用服务器磁盘。"
	default:
		segments.Recommended = true
		segments.Reason = "各分卷下载到本地后合并，不占用服务器磁盘。"
		direct.Reason = "流式拼装在跨分卷边界时容易失败，不推荐用于大文件。"
		switch {
		case !user.Can(database.PermStage):
			staged.Reason = "当前账号无权使用服务器暂存。"
		case settings.CacheLimit <= 0:
			staged.Reason = "未配置下载暂存磁盘配额。"
		case !fitsInCache:
			staged.Reason = "文件大小超出暂存磁盘配额。"
		}
	}
	out.Modes = []downloadModeInfo{direct, staged, segments}

	if job, err := s.db.StagedDownloadFor(r.Context(), file.ID, nowMillis()); err == nil {
		if _, statErr := os.Stat(job.CachePath); statErr == nil {
			body := s.downloadBody(job)
			out.Staged = &body
		}
	} else if !errors.Is(err, database.ErrNotFound) {
		s.fail(w, err, "download options")
		return
	}

	writeJSON(w, http.StatusOK, out)
}

type startDownloadRequest struct {
	FileID string `json:"fileId"`
	Mode   string `json:"mode"`
}

// downloadJobBody is the wire shape of a download, matching what the transfer
// panel renders.
type downloadJobBody struct {
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	FileID          string `json:"fileId,omitempty"`
	Name            string `json:"name"`
	TotalSize       int64  `json:"totalSize"`
	DownloadedBytes int64  `json:"downloadedBytes"`
	Mode            string `json:"mode"`
	Status          string `json:"status"`
	Error           string `json:"error,omitempty"`
	CreatedAt       int64  `json:"createdAt"`
	UpdatedAt       int64  `json:"updatedAt"`
	StartedAt       int64  `json:"startedAt,omitempty"`
	FinishedAt      int64  `json:"finishedAt,omitempty"`
	ExpiresAt       int64  `json:"expiresAt,omitempty"`
	// AvgSpeed is bytes per second over the time the transfer was actually
	// moving, so time spent queued behind the concurrency limit is excluded.
	AvgSpeed float64 `json:"avgSpeed,omitempty"`
	// Speed is the current rate of a staged download, which the server moves
	// itself and therefore is the only thing able to time.
	Speed float64 `json:"speed,omitempty"`
	// URL is where the finished bytes can be fetched from.
	URL string `json:"url,omitempty"`
}

func (s *Server) downloadBody(job database.DownloadJob) downloadJobBody {
	body := downloadJobBody{
		ID:              job.ID,
		Kind:            "download",
		FileID:          job.FileID,
		Name:            job.Name,
		TotalSize:       job.TotalSize,
		DownloadedBytes: job.DownloadedBytes,
		Mode:            string(job.Mode),
		Status:          string(job.Status),
		Error:           job.Error,
		CreatedAt:       job.CreatedAt.UnixMilli(),
		UpdatedAt:       job.UpdatedAt.UnixMilli(),
		AvgSpeed: averageSpeed(job.DownloadedBytes, job.StartedAt,
			measuredUntil(job.FinishedAt, job.Status == database.DownloadPending || job.Status == database.DownloadRunning)),
		Speed: s.downloadRates.speed(job.ID),
	}
	if !job.StartedAt.IsZero() {
		body.StartedAt = job.StartedAt.UnixMilli()
	}
	if !job.FinishedAt.IsZero() {
		body.FinishedAt = job.FinishedAt.UnixMilli()
	}
	if !job.ExpiresAt.IsZero() {
		body.ExpiresAt = job.ExpiresAt.UnixMilli()
	}
	if job.Status == database.DownloadReady || job.Status == database.DownloadComplete {
		body.URL = fmt.Sprintf("/api/downloads/%s/file", job.ID)
	}
	return body
}

func (s *Server) handleStartDownload(w http.ResponseWriter, r *http.Request) {
	var req startDownloadRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	file, err := s.fileForUser(r, req.FileID)
	if err != nil {
		s.fail(w, err, "start download")
		return
	}

	switch database.DownloadMode(req.Mode) {
	case database.DownloadStaged:
		if err := requirePerm(r, database.PermStage); err != nil {
			s.fail(w, err, "start download")
			return
		}
		job, err := s.drive.StartStaged(r.Context(), drive.StageRequest{
			FileID: file.ID,
			UserID: currentUser(r).ID,
		})
		if err != nil {
			s.fail(w, err, "start download")
			return
		}
		s.audit(r, database.AuditDownloadStage, file.Name, fmt.Sprintf("size=%d", file.Size))
		writeJSON(w, http.StatusAccepted, s.downloadBody(job))

	case database.DownloadDirect, database.DownloadSegments:
		// Direct and split downloads are driven entirely by the client: it
		// requests ranges and the server serves them. Recording a row anyway
		// is what makes them show up in the transfer history alongside
		// everything else, which is the whole point of the history.
		job := database.DownloadJob{
			ID:        database.NewID(),
			UserID:    currentUser(r).ID,
			FileID:    file.ID,
			Name:      file.Name,
			TotalSize: file.Size,
			Mode:      database.DownloadMode(req.Mode),
			Status:    database.DownloadRunning,
		}
		if err := s.db.InsertDownload(r.Context(), job); err != nil {
			s.fail(w, err, "start download")
			return
		}
		if err := s.db.SetDownloadStatus(r.Context(), job.ID, database.DownloadRunning, ""); err != nil {
			s.fail(w, err, "start download")
			return
		}
		fresh, err := s.db.DownloadByID(r.Context(), job.ID)
		if err != nil {
			s.fail(w, err, "start download")
			return
		}
		writeJSON(w, http.StatusCreated, s.downloadBody(fresh))

	default:
		writeError(w, http.StatusBadRequest, "mode must be one of direct, staged or segments")
	}
}

// handleDownloadProgress lets a client-driven download report how far it got,
// so the transfer panel shows real numbers for transfers the server is not
// itself performing.
type downloadProgressRequest struct {
	Downloaded int64  `json:"downloaded"`
	Status     string `json:"status,omitempty"`
	Error      string `json:"error,omitempty"`
}

func (s *Server) handleDownloadProgress(w http.ResponseWriter, r *http.Request) {
	job, err := s.downloadForUser(r, chi.URLParam(r, "id"))
	if err != nil {
		s.fail(w, err, "download progress")
		return
	}

	var req downloadProgressRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Downloaded > 0 {
		if err := s.db.SetDownloadProgress(r.Context(), job.ID, min(req.Downloaded, job.TotalSize)); err != nil {
			s.fail(w, err, "download progress")
			return
		}
	}

	if req.Status != "" {
		status := database.DownloadStatus(req.Status)
		switch status {
		case database.DownloadComplete, database.DownloadFailed, database.DownloadCancelled, database.DownloadRunning:
		default:
			writeError(w, http.StatusBadRequest, "status must be running, complete, failed or cancelled")
			return
		}
		if err := s.db.SetDownloadStatus(r.Context(), job.ID, status, req.Error); err != nil {
			s.fail(w, err, "download progress")
			return
		}
	}

	fresh, err := s.db.DownloadByID(r.Context(), job.ID)
	if err != nil {
		s.fail(w, err, "download progress")
		return
	}
	s.publishDownload(fresh)
	writeJSON(w, http.StatusOK, s.downloadBody(fresh))
}

func (s *Server) handleDownloadJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.downloadForUser(r, chi.URLParam(r, "id"))
	if err != nil {
		s.fail(w, err, "read download")
		return
	}
	writeJSON(w, http.StatusOK, s.downloadBody(job))
}

// handleStagedFile serves the assembled bytes from local disk.
//
// This deliberately does not take a Telegram download slot: the bytes are
// already on this machine, and the limit exists to bound how much is pulled
// out of Telegram at once, not how fast a local file can be read.
func (s *Server) handleStagedFile(w http.ResponseWriter, r *http.Request) {
	job, err := s.downloadForUser(r, chi.URLParam(r, "id"))
	if err != nil {
		s.fail(w, err, "staged download")
		return
	}
	ready, err := s.drive.StagedFile(r.Context(), job.ID)
	if err != nil {
		s.fail(w, err, "staged download")
		return
	}

	f, err := os.Open(ready.CachePath)
	if err != nil {
		s.fail(w, err, "staged download")
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		s.fail(w, err, "staged download")
		return
	}

	// A staged copy is only ever fetched to be saved.
	setDisposition(w, r, ready.Name, true)
	w.Header().Set("Accept-Ranges", "bytes")
	if mime := s.mimeOf(r.Context(), ready.FileID); mime != "" {
		w.Header().Set("Content-Type", mime)
	}
	// http.ServeContent handles ranges, conditional requests and multipart
	// responses correctly for a real file, which is one of the reasons staging
	// is worth doing at all.
	http.ServeContent(w, r, ready.Name, info.ModTime(), f)
}

// mimeOf recovers the stored content type for a staged copy, which the file
// on disk cannot tell us.
func (s *Server) mimeOf(ctx context.Context, fileID string) string {
	if fileID == "" {
		return ""
	}
	file, err := s.db.FileByID(ctx, fileID)
	if err != nil {
		return ""
	}
	return file.MIME
}

func (s *Server) handleCancelDownload(w http.ResponseWriter, r *http.Request) {
	job, err := s.downloadForUser(r, chi.URLParam(r, "id"))
	if err != nil {
		s.fail(w, err, "cancel download")
		return
	}

	if job.Mode == database.DownloadStaged {
		if err := s.drive.CancelStaged(r.Context(), job.ID); err != nil {
			s.fail(w, err, "cancel download")
			return
		}
	} else {
		// A WebDAV read is served by this process, so it can genuinely be
		// stopped: the next read refuses and the client's request ends.
		s.drive.CancelClientDownload(job.ID)
		// Direct and segmented downloads run entirely in the browser, so there
		// is no worker to interrupt — but the row still has to leave the running
		// state. Refusing to cancel it, which is what only handling staged jobs
		// did, stranded every transfer whose tab was closed mid-download:
		// permanently "in progress", uncancellable, and therefore undeletable.
		if _, err := s.db.SetDownloadStatusIf(r.Context(), job.ID,
			database.DownloadCancelled, "cancelled",
			database.DownloadPending, database.DownloadRunning); err != nil {
			s.fail(w, err, "cancel download")
			return
		}
	}
	if fresh, err := s.db.DownloadByID(r.Context(), job.ID); err == nil {
		s.publishDownload(fresh)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCacheStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.drive.CacheStatus(r.Context())
	if err != nil {
		s.fail(w, err, "cache status")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handlePurgeCache(w http.ResponseWriter, r *http.Request) {
	freed, err := s.drive.PurgeCache(r.Context())
	if err != nil {
		s.fail(w, err, "purge cache")
		return
	}
	s.audit(r, database.AuditCachePurge, "", fmt.Sprintf("freed=%d", freed))
	writeJSON(w, http.StatusOK, map[string]int64{"freed": freed})
}

// downloadForUser loads a download. Direct and segmented jobs remain private;
// staged jobs are shared by file after the caller's normal file-access check.
func (s *Server) downloadForUser(r *http.Request, id string) (database.DownloadJob, error) {
	job, err := s.db.DownloadByID(r.Context(), id)
	if err != nil {
		return database.DownloadJob{}, err
	}
	user := currentUser(r)
	if job.UserID != "" && job.UserID != user.ID && user.Role != database.RoleAdmin {
		// Staged jobs are deliberately shared by file, so another authorized
		// user may receive the owner's job id when joining an in-flight or
		// cached copy. Re-check the file access rather than exposing arbitrary
		// direct-download history or another user's transfer metadata.
		if job.Mode != database.DownloadStaged || job.FileID == "" {
			return database.DownloadJob{}, fmt.Errorf("%w: download", database.ErrNotFound)
		}
		if _, fileErr := s.fileForUser(r, job.FileID); fileErr != nil {
			return database.DownloadJob{}, fmt.Errorf("%w: download", database.ErrNotFound)
		}
	}
	return job, nil
}

// measuredUntil closes the window a transfer is measured over.
//
// A transfer still moving has no finish time, and reporting nothing until it
// ends is why the details panel showed a dash for its whole run. Measuring a
// running transfer against now is the honest answer. A transfer that ended
// without recording a finish time keeps its zero: extending its window forever
// would make its average speed fall towards zero long after it stopped.
func measuredUntil(finished time.Time, running bool) time.Time {
	if finished.IsZero() && running {
		return time.Now()
	}
	return finished
}

// averageSpeed is bytes per second across a transfer's active window. It
// returns zero rather than a wild number when the window is too short to
// measure, which is the honest answer for a transfer that finished instantly.
func averageSpeed(bytes int64, started, finished time.Time) float64 {
	if bytes <= 0 || started.IsZero() || finished.IsZero() {
		return 0
	}
	elapsed := finished.Sub(started).Seconds()
	if elapsed < 0.05 {
		return 0
	}
	return float64(bytes) / elapsed
}

// wireDownloadProgress connects staged downloads to the event stream, mirroring
// wireRemoteProgress on the upload side.
func wireDownloadProgress(driveSvc *drive.Service, broker *events.Broker, rates *liveRates) {
	driveSvc.OnDownloadProgress = func(job database.DownloadJob, downloaded, total int64, err error) {
		running := err == nil &&
			(job.Status == database.DownloadPending || job.Status == database.DownloadRunning)
		if running {
			rates.observe(job.ID, downloaded)
		} else {
			rates.forget(job.ID)
		}

		payload := events.DownloadProgress{
			JobID:      job.ID,
			FileID:     job.FileID,
			Name:       job.Name,
			Downloaded: downloaded,
			Total:      total,
			Mode:       string(job.Mode),
			Status:     string(job.Status),
			Speed:      rates.speed(job.ID),
		}
		if err != nil {
			payload.Status = string(database.DownloadFailed)
			payload.Error = err.Error()
		}
		broker.Publish(events.Event{
			Type:   events.TypeDownload,
			UserID: job.UserID,
			Data:   payload,
		})
	}
}

func (s *Server) publishDownload(job database.DownloadJob) {
	s.events.Publish(events.Event{
		Type:   events.TypeDownload,
		UserID: job.UserID,
		Data: events.DownloadProgress{
			JobID:      job.ID,
			FileID:     job.FileID,
			Name:       job.Name,
			Downloaded: job.DownloadedBytes,
			Total:      job.TotalSize,
			Mode:       string(job.Mode),
			Status:     string(job.Status),
			Error:      job.Error,
		},
	})
}

// splitList parses a repeated or comma-joined query parameter, which is how
// the transfer filters arrive.
func splitList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
