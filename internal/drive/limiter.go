package drive

import (
	"context"
	"errors"
	"sync"
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

// AcquireDownloadTask reserves one whole-file download slot. The caller must
// invoke the returned function after the response/reader is closed.
func (s *Service) AcquireDownloadTask(ctx context.Context) (func(), error) {
	s.syncTransferConcurrency()
	return s.downloadLimiter.acquire(ctx)
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
