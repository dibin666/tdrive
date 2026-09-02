package drive

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/dibin/tdrive/internal/database"
)

// Bookkeeping for a download the client pulls itself.
//
// A WebDAV client never announces a download. It opens a file, asks for some
// ranges, and eventually stops asking — so unlike a browser transfer there is
// nothing that starts a job, reports progress, or declares itself finished.
// The transfer panel therefore had nothing to show for what is often the
// largest transfer on the deployment.
//
// The unit here is the logical download, not the range request: reads are
// grouped under the same session key the concurrency limiter already uses, and
// the row is settled once that session has been idle for the same grace
// period. Without the grouping, a client that fetches a file in two hundred
// ranges would leave two hundred rows behind.

// ErrDownloadCancelled is returned to a client whose download was stopped from
// the transfer panel. Returning it mid-read is what actually ends the transfer:
// a mounted client cannot be asked to stop, only refused.
var ErrDownloadCancelled = errors.New("drive: download cancelled")

const (
	// How often the byte count is written back. The read loop reports every
	// buffer it fills, which is thousands of updates for a single file.
	clientProgressInterval = 500 * time.Millisecond
	// Grace period used when the configured download grace is zero, so a
	// client pausing between two ranges does not close its own transfer.
	clientIdleGrace = 5 * time.Second
)

// clientDownload is one logical download in progress. Every field is guarded
// by Service.clientDownloadsMu, including the map memberships, so that joining
// a session and settling one cannot interleave.
type clientDownload struct {
	key string
	job database.DownloadJob

	received   int64
	lastReport time.Time
	active     int
	idle       *time.Timer
	cancelled  bool
	settled    bool
}

// ClientRead is one request's participation in a client-driven download.
// Close must be called when the request finishes.
type ClientRead struct {
	svc   *Service
	entry *clientDownload
	once  sync.Once
}

// TrackClientDownload opens, or joins, the transfer record for a download the
// client is pulling itself. A nil handle is safe to use and simply records
// nothing, so a caller never has to guard its read loop.
func (s *Service) TrackClientDownload(
	ctx context.Context,
	key, userID string,
	file database.File,
	mode database.DownloadMode,
) (*ClientRead, error) {
	if key == "" {
		return nil, errors.New("drive: client download needs a session key")
	}

	// The lock is held across the insert on purpose. It is a local SQLite
	// write, and holding it removes the race where two range requests arriving
	// together both decide they are the first and open two rows for one
	// download.
	s.clientDownloadsMu.Lock()
	defer s.clientDownloadsMu.Unlock()

	if entry, ok := s.clientDownloads[key]; ok && !entry.settled {
		if entry.idle != nil {
			entry.idle.Stop()
			entry.idle = nil
		}
		entry.active++
		return &ClientRead{svc: s, entry: entry}, nil
	}

	job := database.DownloadJob{
		ID:        database.NewID(),
		UserID:    userID,
		FileID:    file.ID,
		Name:      file.Name,
		TotalSize: file.Size,
		Mode:      mode,
		Status:    database.DownloadRunning,
	}
	if err := s.db.InsertDownload(ctx, job); err != nil {
		return nil, err
	}
	// InsertDownload writes the row; the status transition is what stamps
	// started_at, which is where the panel's elapsed time is measured from.
	if err := s.db.SetDownloadStatus(ctx, job.ID, database.DownloadRunning, ""); err != nil {
		return nil, err
	}

	entry := &clientDownload{key: key, job: job, active: 1, lastReport: time.Now()}
	s.clientDownloads[key] = entry
	s.clientJobs[job.ID] = entry

	s.notifyDownload(job, 0, file.Size, nil)
	return &ClientRead{svc: s, entry: entry}, nil
}

// Add records bytes served to the client. It reports a cancellation from the
// transfer panel by returning ErrDownloadCancelled.
func (r *ClientRead) Add(n int64) error {
	if r == nil || r.entry == nil || n <= 0 {
		return nil
	}

	r.svc.clientDownloadsMu.Lock()
	if r.entry.cancelled {
		r.svc.clientDownloadsMu.Unlock()
		return ErrDownloadCancelled
	}
	r.entry.received += n
	received := r.entry.received
	job := r.entry.job
	report := time.Since(r.entry.lastReport) >= clientProgressInterval
	if report {
		r.entry.lastReport = time.Now()
	}
	r.svc.clientDownloadsMu.Unlock()
	r.svc.RecordDownloadSessionBytes(r.entry.key, n)

	if report {
		r.svc.recordClientProgress(job, received)
	}
	return nil
}

// Close ends this request's participation. The record itself survives until
// the whole session has been idle for the grace period, because a client
// between two ranges has not finished downloading.
func (r *ClientRead) Close() {
	if r == nil || r.entry == nil {
		return
	}
	r.once.Do(func() { r.svc.releaseClientRead(r.entry) })
}

func (s *Service) releaseClientRead(entry *clientDownload) {
	grace := s.cfg.RuntimeSettings().DownloadGrace
	if grace <= 0 {
		grace = clientIdleGrace
	}

	s.clientDownloadsMu.Lock()
	defer s.clientDownloadsMu.Unlock()

	if entry.active > 0 {
		entry.active--
	}
	if entry.active > 0 || entry.idle != nil || entry.settled {
		return
	}
	entry.idle = time.AfterFunc(grace, func() { s.settleClientDownload(entry) })
}

// CancelClientDownload stops a download the client is pulling, which is how the
// transfer panel's cancel button reaches a WebDAV read. It reports whether a
// live download was found.
func (s *Service) CancelClientDownload(jobID string) bool {
	s.clientDownloadsMu.Lock()
	entry, ok := s.clientJobs[jobID]
	if ok {
		entry.cancelled = true
	}
	s.clientDownloadsMu.Unlock()
	return ok
}

func (s *Service) settleClientDownload(entry *clientDownload) {
	s.clientDownloadsMu.Lock()
	// A request that arrived during the grace period stops this timer, but it
	// may already have fired and be waiting on the mutex, so the count is
	// rechecked here.
	if entry.active > 0 || entry.settled {
		entry.idle = nil
		s.clientDownloadsMu.Unlock()
		return
	}
	entry.settled = true
	entry.idle = nil
	received, cancelled, job := entry.received, entry.cancelled, entry.job
	if s.clientDownloads[entry.key] == entry {
		delete(s.clientDownloads, entry.key)
	}
	delete(s.clientJobs, job.ID)
	s.clientDownloadsMu.Unlock()

	// A client that stopped early — a player that scrubbed and moved on, a
	// copy the user interrupted, a mount that went away — did not finish the
	// download, and recording it as complete would make the history useless
	// for the one question it is asked: did that file come across intact?
	status := database.DownloadComplete
	if cancelled || received < job.TotalSize {
		status = database.DownloadCancelled
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := s.db.SetDownloadProgress(ctx, job.ID, received); err != nil {
		s.log.Debug("could not record a client download's final progress",
			zap.String("job", job.ID), zap.Error(err))
	}
	// Conditional, so a cancellation already written by the API is not
	// overwritten by this late completion.
	if _, err := s.db.SetDownloadStatusIf(ctx, job.ID, status, "",
		database.DownloadPending, database.DownloadRunning); err != nil {
		s.log.Warn("could not settle a client download",
			zap.String("job", job.ID), zap.Error(err))
	}

	final := job
	if fresh, err := s.db.DownloadByID(ctx, job.ID); err == nil {
		final = fresh
	} else {
		final.Status = status
	}
	s.notifyDownload(final, received, final.TotalSize, nil)
}

func (s *Service) recordClientProgress(job database.DownloadJob, received int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.db.SetDownloadProgress(ctx, job.ID, received); err != nil {
		s.log.Debug("could not record client download progress",
			zap.String("job", job.ID), zap.Error(err))
	}
	s.notifyDownload(job, received, job.TotalSize, nil)
}
