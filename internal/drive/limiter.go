package drive

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrUploadTaskClosed is returned when a browser segment arrives after its
// upload job has completed or been cancelled.
var ErrUploadTaskClosed = errors.New("drive: upload task is no longer accepting segments")

// taskLimiter is a dynamically-sized, context-aware semaphore. It keeps a FIFO
// waiter queue so a newly-arriving task cannot repeatedly jump ahead of tasks
// that are already waiting. Changing the limit never interrupts a task that
// already owns a slot.
type taskLimiter struct {
	mu     sync.Mutex
	limit  int
	active int
	queue  []*taskWaiter
}

type taskWaiter struct {
	ready   chan struct{}
	granted bool
	cancel  bool
}

func newTaskLimiter(limit int) *taskLimiter {
	if limit < 1 {
		limit = 1
	}
	return &taskLimiter{limit: limit}
}

func (l *taskLimiter) setLimit(limit int) {
	if limit < 1 {
		limit = 1
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.limit == limit {
		return
	}
	l.limit = limit
	l.dispatchLocked()
}

func (l *taskLimiter) acquire(ctx context.Context) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	waiter := &taskWaiter{ready: make(chan struct{})}
	l.mu.Lock()
	if l.active < l.limit && len(l.queue) == 0 {
		l.active++
		l.mu.Unlock()
		return l.releaseFunc(), nil
	}
	l.queue = append(l.queue, waiter)
	l.mu.Unlock()

	select {
	case <-waiter.ready:
		return l.releaseFunc(), nil
	case <-ctx.Done():
		l.mu.Lock()
		if waiter.granted {
			// The task was granted at the same time as cancellation. Return
			// the slot immediately rather than leaking it.
			l.active--
			l.dispatchLocked()
		} else if !waiter.cancel {
			waiter.cancel = true
			for i, queued := range l.queue {
				if queued == waiter {
					copy(l.queue[i:], l.queue[i+1:])
					l.queue = l.queue[:len(l.queue)-1]
					break
				}
			}
			l.dispatchLocked()
		}
		l.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (l *taskLimiter) releaseFunc() func() {
	var once sync.Once
	return func() {
		once.Do(func() { l.release() })
	}
}

func (l *taskLimiter) release() {
	l.mu.Lock()
	if l.active > 0 {
		l.active--
	}
	l.dispatchLocked()
	l.mu.Unlock()
}

func (l *taskLimiter) dispatchLocked() {
	for l.active < l.limit && len(l.queue) > 0 {
		waiter := l.queue[0]
		l.queue = l.queue[1:]
		if waiter.cancel {
			continue
		}
		waiter.granted = true
		l.active++
		close(waiter.ready)
	}
}

// uploadJobLease is shared by all segment HTTP requests belonging to one
// browser upload. The underlying task slot remains occupied until the job is
// completed/cancelled, while active counts keep a terminal request from
// releasing a slot underneath a segment that is still writing.
type uploadJobLease struct {
	id string

	ready  chan struct{}
	cancel context.CancelFunc

	gateRelease func()
	acquireErr  error
	active      int
	closed      bool
}

// SetTransferConcurrency applies new task limits immediately. Existing tasks
// keep their slots; queued tasks are woken when a larger limit makes room.
func (s *Service) SetTransferConcurrency(upload, download int) {
	s.uploadLimiter.setLimit(upload)
	s.downloadLimiter.setLimit(download)
}

func (s *Service) syncTransferConcurrency() {
	settings := s.cfg.RuntimeSettings()
	s.SetTransferConcurrency(settings.UploadConcurrency, settings.DownloadConcurrency)
}

func (s *Service) acquireUploadTask(ctx context.Context) (func(), error) {
	s.syncTransferConcurrency()
	return s.uploadLimiter.acquire(ctx)
}

// AcquireDownloadTask reserves one whole-file download slot for a caller that
// owns the whole transfer on its own — a staged download assembling a file on
// disk, for instance. Callers serving a client over HTTP want
// AcquireDownloadSession instead, so that the several range requests making up
// one logical download share a slot.
func (s *Service) AcquireDownloadTask(ctx context.Context) (func(), error) {
	s.syncTransferConcurrency()
	return s.downloadLimiter.acquire(ctx)
}

// ErrTooManyConnections is returned when one logical download opens more
// parallel range requests than the configured limit allows.
var ErrTooManyConnections = errors.New("drive: too many parallel connections for one download")

// downloadSession is the download-side counterpart of uploadJobLease: every
// range request belonging to one logical download shares a single task slot.
//
// A parallel downloader is the whole point of a reusable link, and counting
// each of its eight connections as a separate task would mean a deployment
// with the default limit of two could never serve one. Grouping them also
// makes the limit mean what an operator expects it to mean — "two files at a
// time", not "two sockets at a time".
//
// The slot is not returned the instant the last request finishes. A parallel
// downloader routinely has a moment with nothing in flight while it decides
// which range to ask for next, and handing its slot to a queued transfer in
// that gap would strand it mid-file behind someone else's upload.
type downloadSession struct {
	key string

	ready  chan struct{}
	cancel context.CancelFunc

	gateRelease func()
	acquireErr  error
	active      int
	// idle is the grace timer armed when active reaches zero.
	idle *time.Timer
}

// AcquireDownloadSession reserves (or joins) the task slot for one logical
// download. The returned function must be called when the request finishes.
func (s *Service) AcquireDownloadSession(ctx context.Context, key string) (func(), error) {
	if key == "" {
		return s.AcquireDownloadTask(ctx)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	s.syncTransferConcurrency()
	maxConns := s.cfg.RuntimeSettings().MaxDownloadConns

	s.downloadSessionsMu.Lock()
	if session, ok := s.downloadSessions[key]; ok {
		// Joining a session that is being torn down would take a slot the
		// grace timer is about to return, so cancel the teardown first.
		if session.idle != nil {
			session.idle.Stop()
			session.idle = nil
		}
		ready := session.ready
		s.downloadSessionsMu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ready:
		}

		s.downloadSessionsMu.Lock()
		if session.acquireErr != nil {
			err := session.acquireErr
			s.downloadSessionsMu.Unlock()
			return nil, err
		}
		// A session that already released its slot is gone; the caller has to
		// start a fresh one rather than resurrect this record.
		if s.downloadSessions[key] != session {
			s.downloadSessionsMu.Unlock()
			return s.AcquireDownloadSession(ctx, key)
		}
		if session.active >= maxConns {
			s.downloadSessionsMu.Unlock()
			return nil, fmt.Errorf("%w: limit is %d", ErrTooManyConnections, maxConns)
		}
		session.active++
		s.downloadSessionsMu.Unlock()
		return s.downloadRequestRelease(session), nil
	}

	taskCtx, cancel := context.WithCancel(context.Background())
	session := &downloadSession{key: key, ready: make(chan struct{}), cancel: cancel}
	s.downloadSessions[key] = session
	s.downloadSessionsMu.Unlock()

	// A client that disconnects while queued must not leave the session
	// waiting forever on a slot nobody will use.
	stopCancel := context.AfterFunc(ctx, cancel)
	gateRelease, acquireErr := s.downloadLimiter.acquire(taskCtx)
	stopCancel()

	s.downloadSessionsMu.Lock()
	session.acquireErr = acquireErr
	if acquireErr == nil {
		session.gateRelease = gateRelease
		session.active = 1
	} else if s.downloadSessions[key] == session {
		delete(s.downloadSessions, key)
	}
	close(session.ready)
	s.downloadSessionsMu.Unlock()

	if acquireErr != nil {
		return nil, acquireErr
	}
	return s.downloadRequestRelease(session), nil
}

func (s *Service) downloadRequestRelease(session *downloadSession) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			grace := s.cfg.RuntimeSettings().DownloadGrace

			s.downloadSessionsMu.Lock()
			if session.active > 0 {
				session.active--
			}
			if session.active > 0 || session.idle != nil {
				s.downloadSessionsMu.Unlock()
				return
			}
			if grace <= 0 {
				s.finishDownloadSessionLocked(session)
				s.downloadSessionsMu.Unlock()
				return
			}
			session.idle = time.AfterFunc(grace, func() {
				s.downloadSessionsMu.Lock()
				defer s.downloadSessionsMu.Unlock()
				// A request that arrived during the grace period stops this
				// timer, but it may already have fired and be waiting on the
				// mutex, so the count is re-checked here.
				if session.active > 0 {
					session.idle = nil
					return
				}
				s.finishDownloadSessionLocked(session)
			})
			s.downloadSessionsMu.Unlock()
		})
	}
}

// finishDownloadSessionLocked returns the slot and forgets the session. The
// caller must hold downloadSessionsMu.
func (s *Service) finishDownloadSessionLocked(session *downloadSession) {
	if s.downloadSessions[session.key] == session {
		delete(s.downloadSessions, session.key)
	}
	session.idle = nil
	session.cancel()
	if session.gateRelease != nil {
		session.gateRelease()
		session.gateRelease = nil
	}
}

// AcquireUploadJob reserves (or joins) the task slot for one browser upload.
// Every segment request gets a small request lease; ReleaseUploadJob closes
// the job-level lease once the complete/cancel endpoint finishes.
func (s *Service) AcquireUploadJob(ctx context.Context, jobID string) (func(), error) {
	if jobID == "" {
		return nil, errors.New("drive: upload job id is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	s.syncTransferConcurrency()

	s.uploadJobsMu.Lock()
	if lease, ok := s.uploadJobs[jobID]; ok {
		if lease.closed {
			s.uploadJobsMu.Unlock()
			return nil, ErrUploadTaskClosed
		}
		ready := lease.ready
		s.uploadJobsMu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ready:
		}

		s.uploadJobsMu.Lock()
		if lease.acquireErr != nil {
			err := lease.acquireErr
			s.uploadJobsMu.Unlock()
			return nil, err
		}
		if lease.closed {
			s.uploadJobsMu.Unlock()
			return nil, ErrUploadTaskClosed
		}
		lease.active++
		s.uploadJobsMu.Unlock()
		return s.uploadRequestRelease(lease), nil
	}

	taskCtx, cancel := context.WithCancel(context.Background())
	lease := &uploadJobLease{
		id:     jobID,
		ready:  make(chan struct{}),
		cancel: cancel,
	}
	s.uploadJobs[jobID] = lease
	s.uploadJobsMu.Unlock()

	// A disconnected browser request must not leave a queued lease waiting
	// forever. A later retry can create a fresh lease for the same job.
	stopCancel := context.AfterFunc(ctx, cancel)
	gateRelease, acquireErr := s.uploadLimiter.acquire(taskCtx)
	stopCancel()

	var releaseNow func()
	s.uploadJobsMu.Lock()
	if acquireErr == nil && lease.closed {
		acquireErr = ErrUploadTaskClosed
		releaseNow = gateRelease
		gateRelease = nil
	}
	lease.acquireErr = acquireErr
	if acquireErr == nil {
		lease.gateRelease = gateRelease
		lease.active = 1
	}
	close(lease.ready)
	if acquireErr != nil && s.uploadJobs[jobID] == lease {
		delete(s.uploadJobs, jobID)
	}
	s.uploadJobsMu.Unlock()
	if releaseNow != nil {
		releaseNow()
	}

	if acquireErr != nil {
		return nil, acquireErr
	}
	return s.uploadRequestRelease(lease), nil
}

func (s *Service) uploadRequestRelease(lease *uploadJobLease) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			var release func()
			s.uploadJobsMu.Lock()
			if lease.active > 0 {
				lease.active--
			}
			if lease.closed && lease.active == 0 {
				if s.uploadJobs[lease.id] == lease {
					delete(s.uploadJobs, lease.id)
				}
				release = lease.gateRelease
				lease.gateRelease = nil
			}
			s.uploadJobsMu.Unlock()
			if release != nil {
				release()
			}
		})
	}
}

// ReleaseUploadJob closes a browser upload's job-level lease. It is safe to
// call when the job was never admitted or when it was already released.
func (s *Service) ReleaseUploadJob(jobID string) {
	s.uploadJobsMu.Lock()
	lease, ok := s.uploadJobs[jobID]
	if !ok {
		s.uploadJobsMu.Unlock()
		return
	}
	lease.closed = true
	lease.cancel()

	var release func()
	if lease.active == 0 && lease.gateRelease != nil {
		delete(s.uploadJobs, jobID)
		release = lease.gateRelease
		lease.gateRelease = nil
	}
	s.uploadJobsMu.Unlock()
	if release != nil {
		release()
	}
}
