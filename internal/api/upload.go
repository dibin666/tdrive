package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/drive"
	"github.com/dibin/tdrive/internal/events"
)

// The browser drives its own uploads segment by segment. It knows the segment
// size from /api/status, slices the local File on exactly those boundaries and
// PUTs each piece, so a segment maps one-to-one onto a Telegram object. That
// buys three things for free: resume after a disconnect (re-PUT the missing
// indices), parallelism the server cannot get from a single stream, and no
// server-side spooling of multi-gigabyte files.

type beginUploadRequest struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	MIME      string `json:"mime,omitempty"`
	Overwrite bool   `json:"overwrite,omitempty"`
}

type uploadPlan struct {
	Job database.UploadJob `json:"job"`
	// SegmentSize and SegmentBounds tell the browser exactly where to slice.
	SegmentSize   int64   `json:"segmentSize"`
	SegmentBounds []bound `json:"segmentBounds"`
	Pending       []int   `json:"pending"`
}

type bound struct {
	Index int   `json:"index"`
	Start int64 `json:"start"`
	Size  int64 `json:"size"`
}

func (s *Server) handleBeginUpload(w http.ResponseWriter, r *http.Request) {
	var req beginUploadRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Size < 0 {
		writeError(w, http.StatusBadRequest, "size must be known before an upload starts")
		return
	}

	dest, err := s.realPath(r, req.Path)
	if err != nil {
		s.fail(w, err, "begin upload")
		return
	}

	job, file, err := s.drive.Begin(r.Context(), drive.UploadRequest{
		DirPath:   dest,
		Name:      req.Name,
		Size:      req.Size,
		MIME:      req.MIME,
		UserID:    currentUser(r).ID,
		Source:    "webui",
		Overwrite: req.Overwrite,
	})
	if err != nil {
		s.fail(w, err, "begin upload")
		return
	}

	writeJSON(w, http.StatusCreated, s.planFor(job, file))
}

func (s *Server) planFor(job database.UploadJob, file database.File) uploadPlan {
	bounds := make([]bound, 0, file.SegmentCount)
	for i := 1; i <= file.SegmentCount; i++ {
		bounds = append(bounds, bound{
			Index: i,
			Start: int64(i-1) * file.SegmentSize,
			Size:  drive.SegmentSize(file.Size, file.SegmentSize, i),
		})
	}
	return uploadPlan{
		Job:           job,
		SegmentSize:   file.SegmentSize,
		SegmentBounds: bounds,
		Pending:       job.PendingSegments(),
	}
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.jobForUser(r, chi.URLParam(r, "id"))
	if err != nil {
		s.fail(w, err, "read job")
		return
	}
	s.progress.merge(&job)
	file, err := s.db.FileByID(r.Context(), job.FileID)
	if err != nil {
		s.fail(w, err, "read job")
		return
	}
	writeJSON(w, http.StatusOK, s.planFor(job, file))
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	jobs, err := s.db.ListJobs(r.Context(), currentUser(r).ID, limit)
	if err != nil {
		s.fail(w, err, "list jobs")
		return
	}
	if jobs == nil {
		jobs = []database.UploadJob{}
	}
	for i := range jobs {
		s.progress.merge(&jobs[i])
	}
	writeJSON(w, http.StatusOK, jobs)
}

// handlePutSegment receives exactly one segment's bytes and streams them
// straight into Telegram. Nothing touches the disk on this path.
func (s *Server) handlePutSegment(w http.ResponseWriter, r *http.Request) {
	job, err := s.jobForUser(r, chi.URLParam(r, "id"))
	if err != nil {
		s.fail(w, err, "put segment")
		return
	}

	index, err := strconv.Atoi(chi.URLParam(r, "index"))
	if err != nil || index < 1 {
		writeError(w, http.StatusBadRequest, "segment index must be a positive integer")
		return
	}

	file, err := s.db.FileByID(r.Context(), job.FileID)
	if err != nil {
		s.fail(w, err, "put segment")
		return
	}
	want := drive.SegmentSize(file.Size, file.SegmentSize, index)

	// The length has to be exact. Telegram commits to a part count in the
	// first upload request, so a body that turns out shorter would be stored
	// silently truncated.
	if r.ContentLength != want {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"segment %d must be exactly %d bytes; the request declared %d",
			index, want, r.ContentLength))
		return
	}

	// Re-sending a segment that already landed is how a resumed upload
	// behaves when the client's record of progress is behind the server's.
	if database.MaskHas(job.DoneMask, index) {
		writeJSON(w, http.StatusOK, map[string]any{"segment": index, "alreadyStored": true})
		return
	}

	// Cancelling an upload has to stop the segments that are already in flight.
	// Aborting the browser's request only closes one direction: the bytes it
	// already handed over would still be pushed into Telegram against a job the
	// panel shows as stopped. Watching the job gives the cancellation something
	// to interrupt on this side too.
	ctx, release := s.drive.WatchUploadJob(r.Context(), job.ID)
	defer release()

	// A browser upload is one task even though its segments are sent by
	// multiple HTTP requests at once. The job lease makes those requests share
	// one global upload slot while still allowing the segments to run in
	// parallel inside that task.
	// The lease also fixes which Telegram account the job runs on, so every
	// segment of this upload lands through the same login.
	_, releaseRequest, err := s.drive.AcquireUploadJob(ctx, job.ID)
	if err != nil {
		s.fail(w, err, "put segment")
		return
	}
	defer releaseRequest()

	// Waiting for a slot is exactly when a transfer gets cancelled, so this
	// transition is conditional: a job that stopped while it queued must not be
	// put back into a running state by the request that was waiting.
	started, err := s.db.SetJobStatusIf(ctx, job.ID, database.JobRunning, "",
		database.JobPending, database.JobRunning)
	if err != nil {
		s.drive.ReleaseUploadJob(job.ID)
		s.fail(w, err, "put segment")
		return
	}
	if !started {
		s.drive.ReleaseUploadJob(job.ID)
		s.fail(w, fmt.Errorf("%w: %q", database.ErrJobFinished, job.Name), "put segment")
		return
	}

	base := int64(index-1) * file.SegmentSize
	throttle := newProgressThrottle()
	err = s.drive.PutSegment(ctx, job, index, r.Body, want, func(uploaded, _ int64) {
		s.progress.update(job.ID, base+uploaded, file.Size, database.JobRunning)
		if !throttle.ready() {
			return
		}
		s.events.Publish(events.Event{
			Type:   events.TypeUpload,
			UserID: job.UserID,
			Data: events.UploadProgress{
				JobID: job.ID, FileID: file.ID, Name: file.Name,
				Uploaded: base + uploaded, Total: file.Size,
				Segment: index, SegmentCount: file.SegmentCount,
				Status: string(database.JobRunning),
				Speed:  s.progress.speed(job.ID),
			},
		})
	})
	if err != nil {
		s.drive.ReleaseUploadJob(job.ID)
		s.progress.clear(job.ID)
		// A segment that stopped because the transfer was cancelled is not a
		// failure, and announcing one would contradict the cancellation the
		// browser has already been told about.
		status, message := database.JobFailed, err.Error()
		if ctx.Err() != nil || errors.Is(err, database.ErrJobFinished) {
			status, message = database.JobCancelled, ""
		}
		s.events.Publish(events.Event{
			Type:   events.TypeUpload,
			UserID: job.UserID,
			Data: events.UploadProgress{
				JobID: job.ID, Name: file.Name, Segment: index,
				SegmentCount: file.SegmentCount,
				Status:       string(status), Error: message,
			},
		})
		s.fail(w, err, "put segment")
		return
	}

	updated, err := s.db.JobByID(r.Context(), job.ID)
	if err != nil {
		s.drive.ReleaseUploadJob(job.ID)
		s.fail(w, err, "put segment")
		return
	}
	if updated.Status == database.JobComplete {
		// All Telegram objects for this content are in place. Release the
		// account lease now rather than waiting for the browser's follow-up
		// /complete request; if that request is lost, the finished content
		// must not hold the account's daily reservation forever.
		s.drive.ReleaseUploadJob(job.ID)
		s.progress.clear(job.ID)
	} else {
		s.progress.update(job.ID, updated.UploadedBytes, updated.TotalSize, updated.Status)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"segment": index,
		"pending": updated.PendingSegments(),
		"done":    updated.Done(),
	})
}

func (s *Server) handleCompleteUpload(w http.ResponseWriter, r *http.Request) {
	job, err := s.jobForUser(r, chi.URLParam(r, "id"))
	if err != nil {
		s.fail(w, err, "complete upload")
		return
	}
	file, err := s.drive.Complete(r.Context(), job.ID)
	if err != nil {
		s.fail(w, err, "complete upload")
		return
	}
	s.progress.clear(job.ID)

	s.events.Publish(events.Event{
		Type:   events.TypeUpload,
		UserID: job.UserID,
		Data: events.UploadProgress{
			JobID: job.ID, FileID: file.ID, Name: file.Name,
			Uploaded: file.Size, Total: file.Size,
			Segment: file.SegmentCount, SegmentCount: file.SegmentCount,
			Status: string(database.JobComplete),
		},
	})
	if dir, err := s.db.DirByID(r.Context(), file.DirID); err == nil {
		s.events.Publish(events.Event{Type: events.TypeTree, Data: events.TreeChanged{Path: dir.Path}})
	} else {
		s.events.Publish(events.Event{Type: events.TypeTree, Data: events.TreeChanged{Path: drive.Root}})
	}
	writeJSON(w, http.StatusOK, file)
}

func (s *Server) handleCancelUpload(w http.ResponseWriter, r *http.Request) {
	job, err := s.jobForUser(r, chi.URLParam(r, "id"))
	if err != nil {
		s.fail(w, err, "cancel upload")
		return
	}
	// CancelUpload rather than Abort: the record is only half the job. The
	// segments already in flight — from this browser, from a server-side worker,
	// from a plugin streaming its own upload — have to be interrupted too, or
	// the transfer keeps running behind a row that says it stopped.
	if err := s.drive.CancelUpload(r.Context(), job.ID); err != nil {
		s.fail(w, err, "cancel upload")
		return
	}
	s.progress.clear(job.ID)
	s.events.Publish(events.Event{
		Type:   events.TypeUpload,
		UserID: job.UserID,
		Data: events.UploadProgress{
			JobID: job.ID, Name: job.Name, Status: string(database.JobCancelled),
		},
	})
	w.WriteHeader(http.StatusNoContent)
}

type remoteUploadRequest struct {
	Path      string `json:"path"`
	Name      string `json:"name,omitempty"`
	URL       string `json:"url"`
	Overwrite bool   `json:"overwrite,omitempty"`
}

// handleRemoteUpload starts a server-side fetch. It returns as soon as the job
// exists, because a multi-gigabyte download should not hold an HTTP request
// open; progress arrives over SSE.
func (s *Server) handleRemoteUpload(w http.ResponseWriter, r *http.Request) {
	var req remoteUploadRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	dest, err := s.realPath(r, req.Path)
	if err != nil {
		s.fail(w, err, "remote upload")
		return
	}

	job, err := s.drive.StartRemote(r.Context(), drive.RemoteRequest{
		URL:       req.URL,
		DirPath:   dest,
		Name:      req.Name,
		UserID:    currentUser(r).ID,
		Overwrite: req.Overwrite,
	})
	if err != nil {
		s.fail(w, err, "remote upload")
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

// jobForUser loads a job and refuses to expose another account's transfer.
func (s *Server) jobForUser(r *http.Request, id string) (database.UploadJob, error) {
	job, err := s.db.JobByID(r.Context(), id)
	if err != nil {
		return database.UploadJob{}, err
	}
	user := currentUser(r)
	if job.UserID != user.ID && user.Role != database.RoleAdmin {
		// Reporting "not found" rather than "forbidden" avoids confirming
		// that someone else's job id exists.
		return database.UploadJob{}, fmt.Errorf("%w: upload job", database.ErrNotFound)
	}
	return job, nil
}

// progressThrottle limits how often progress events go out. gotd reports every
// upload part, which on a fast link is hundreds of events a second per file —
// far more than a browser can use.
//
// The callback runs on every upload thread at once, so the timestamp is
// compare-and-swapped rather than simply assigned.
type progressThrottle struct {
	lastNanos atomic.Int64
}

func newProgressThrottle() *progressThrottle { return &progressThrottle{} }

func (p *progressThrottle) ready() bool {
	const interval = int64(250 * time.Millisecond)
	now := time.Now().UnixNano()
	last := p.lastNanos.Load()
	if now-last < interval {
		return false
	}
	return p.lastNanos.CompareAndSwap(last, now)
}
