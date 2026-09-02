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
//
// The account is part of the lease, not chosen per request: every segment of
// one upload goes through the same login. Spreading them would spend several
// accounts' quota on one file and leave the file's segments owned by whichever
// account happened to be idle, which is worse for reading it back.
type uploadJobLease struct {
	id      string
	account Account
	quota   *quotaReservation

	ready  chan struct{}
	cancel context.CancelFunc

	gateRelease func()
	acquireErr  error
	active      int
	closed      bool
}

// SetTransferConcurrency applies the global task limits immediately. Existing
// tasks keep their slots; queued tasks are woken when a larger limit makes room.
// The number of Telegram accounts has no effect on these limits.
func (s *Service) SetTransferConcurrency(upload, download int) {
	s.limitersMu.Lock()
	defer s.limitersMu.Unlock()
	if s.uploadLimiter == nil {
		s.uploadLimiter = newTaskLimiter(upload)
	} else {
		s.uploadLimiter.setLimit(upload)
	}
	if s.downloadLimiter == nil {
		s.downloadLimiter = newTaskLimiter(download)
	} else {
		s.downloadLimiter.setLimit(download)
	}
}

// ActiveTasksByAccount reports which account currently owns each globally
// admitted task. There is still one account per transfer, but these counters are
// informational only; they do not create additional capacity.
func (s *Service) ActiveTasksByAccount() (upload, download map[string]int) {
	s.limitersMu.Lock()
	defer s.limitersMu.Unlock()

	upload = make(map[string]int, len(s.activeUploads))
	for id, active := range s.activeUploads {
		upload[id] = active
	}
	download = make(map[string]int, len(s.activeDownloads))
	for id, active := range s.activeDownloads {
		download[id] = active
	}
	return upload, download
}

// AcquireDownloadTask reserves one whole-file download slot for a caller that
// owns the whole transfer on its own — a staged download assembling a file on
// disk, for instance. Callers serving a client over HTTP want
// AcquireDownloadSession instead, so that the several range requests making up
// one logical download share a slot.
func (s *Service) AcquireDownloadTask(ctx context.Context, _ string, expectedSize ...int64) (Account, func(), error) {
	size := int64(0)
	if len(expectedSize) > 0 {
		size = expectedSize[0]
	}
	lease, err := s.leaseDownload(ctx, "", size)
	if err != nil {
		return nil, nil, err
	}
	return lease.account, lease.release, nil
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
	key     string
	account Account
	quota   *quotaReservation

	ready  chan struct{}
	cancel context.CancelFunc

	gateRelease func()
	acquireErr  error
	active      int
	// transferred is the number of bytes actually served through Telegram
	// during this logical content session. It is used to settle a partial
	// client download without charging the whole file.
	transferred int64
	// idle is the grace timer armed when active reaches zero.
	idle *time.Timer
}

// AcquireDownloadSession reserves (or joins) the task slot for one logical
// download, and returns the Telegram account that slot belongs to. The returned
// function must be called when the request finishes.
//
// fileID supplies the file size for daily quota accounting. Account selection is
// primary-first and does not promote the account that uploaded the file; a
// fallback account can re-resolve its own handles when it takes over.
func (s *Service) AcquireDownloadSession(ctx context.Context, key, fileID string) (Account, func(), error) {
	if key == "" {
		return s.AcquireDownloadTask(ctx, "", s.transferSize(ctx, fileID))
	}
	if ctx == nil {
		ctx = context.Background()
	}

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
			return nil, nil, ctx.Err()
		case <-ready:
		}

		s.downloadSessionsMu.Lock()
		if session.acquireErr != nil {
			err := session.acquireErr
			s.downloadSessionsMu.Unlock()
			return nil, nil, err
		}
		// A session that already released its slot is gone; the caller has to
		// start a fresh one rather than resurrect this record.
		if s.downloadSessions[key] != session {
			s.downloadSessionsMu.Unlock()
			return s.AcquireDownloadSession(ctx, key, fileID)
		}
		if session.active >= maxConns {
			s.downloadSessionsMu.Unlock()
			return nil, nil, fmt.Errorf("%w: limit is %d", ErrTooManyConnections, maxConns)
		}
		session.active++
		account := session.account
		s.downloadSessionsMu.Unlock()
		return account, s.downloadRequestRelease(session), nil
	}

	taskCtx, cancel := context.WithCancel(context.Background())
	session := &downloadSession{key: key, ready: make(chan struct{}), cancel: cancel}
	s.downloadSessions[key] = session
	s.downloadSessionsMu.Unlock()

	// A client that disconnects while queued must not leave the session
	// waiting forever on a slot nobody will use.
	stopCancel := context.AfterFunc(ctx, cancel)
	acquired, acquireErr := s.leaseDownload(
		taskCtx, s.SegmentOwner(taskCtx, fileID), s.transferSize(taskCtx, fileID),
	)
	stopCancel()

	s.downloadSessionsMu.Lock()
	session.acquireErr = acquireErr
	if acquireErr == nil {
		session.account = acquired.account
		session.quota = acquired.quota
		session.gateRelease = acquired.release
		session.active = 1
	} else if s.downloadSessions[key] == session {
		delete(s.downloadSessions, key)
	}
	close(session.ready)
	s.downloadSessionsMu.Unlock()

	if acquireErr != nil {
		return nil, nil, acquireErr
	}
	return acquired.account, s.downloadRequestRelease(session), nil
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
	if session.quota != nil {
		session.quota.setActual(session.transferred)
	}
	if session.gateRelease != nil {
		session.gateRelease()
		session.gateRelease = nil
	}
}

// RecordDownloadSessionBytes attributes bytes served by a range request to
// the logical content session that owns the Telegram account reservation.
// Requests are grouped by key, so parallel ranges update one counter.
func (s *Service) RecordDownloadSessionBytes(key string, bytes int64) {
	if key == "" || bytes <= 0 {
		return
	}
	s.downloadSessionsMu.Lock()
	if session, ok := s.downloadSessions[key]; ok {
		session.transferred += bytes
	}
	s.downloadSessionsMu.Unlock()
}

// AcquireUploadJob reserves (or joins) the task slot for one browser upload and
// returns the Telegram account that slot belongs to. Every segment request gets
// a small request lease; ReleaseUploadJob closes the job-level lease once the
// complete/cancel endpoint finishes.
func (s *Service) AcquireUploadJob(ctx context.Context, jobID string) (Account, func(), error) {
	if jobID == "" {
		return nil, nil, errors.New("drive: upload job id is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	s.uploadJobsMu.Lock()
	if lease, ok := s.uploadJobs[jobID]; ok {
		if lease.closed {
			s.uploadJobsMu.Unlock()
			return nil, nil, ErrUploadTaskClosed
		}
		ready := lease.ready
		s.uploadJobsMu.Unlock()

		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-ready:
		}

		s.uploadJobsMu.Lock()
		if lease.acquireErr != nil {
			err := lease.acquireErr
			s.uploadJobsMu.Unlock()
			return nil, nil, err
		}
		if lease.closed {
			s.uploadJobsMu.Unlock()
			return nil, nil, ErrUploadTaskClosed
		}
		lease.active++
		account := lease.account
		s.uploadJobsMu.Unlock()
		return account, s.uploadRequestRelease(lease), nil
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
	acquired, acquireErr := s.leaseUpload(taskCtx, s.uploadJobSize(taskCtx, jobID))
	stopCancel()

	var releaseNow func()
	s.uploadJobsMu.Lock()
	if acquireErr == nil && lease.closed {
		acquireErr = ErrUploadTaskClosed
		// The job was closed before the caller received a request lease, so
		// no bytes could have reached Telegram through this reservation.
		// Return the slot without charging the expected content size.
		acquired.recordQuotaBytes(0)
		releaseNow = acquired.release
		acquired = nil
	}
	lease.acquireErr = acquireErr
	if acquireErr == nil {
		lease.account = acquired.account
		lease.quota = acquired.quota
		lease.gateRelease = acquired.release
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
		return nil, nil, acquireErr
	}
	return acquired.account, s.uploadRequestRelease(lease), nil
}

// uploadJobAccount is the account an upload was admitted on, so that every
// segment of that job stores its bytes through the same login.
func (s *Service) uploadJobAccount(jobID string) (Account, bool) {
	s.uploadJobsMu.Lock()
	defer s.uploadJobsMu.Unlock()
	if lease, ok := s.uploadJobs[jobID]; ok && lease.account != nil {
		return lease.account, true
	}
	if account, ok := s.jobAccounts[jobID]; ok && account != nil {
		return account, true
	}
	return nil, false
}

// bindUploadAccount pins a server-side upload to the account whose slot the
// caller already holds. The returned function unbinds it and must be called
// when the transfer ends, or the map grows for the life of the process.
func (s *Service) bindUploadAccount(jobID string, account Account) func() {
	if jobID == "" || account == nil {
		return func() {}
	}
	s.uploadJobsMu.Lock()
	s.jobAccounts[jobID] = account
	s.uploadJobsMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.uploadJobsMu.Lock()
			delete(s.jobAccounts, jobID)
			s.uploadJobsMu.Unlock()
		})
	}
}

// acquireUploadTask reserves an upload slot on some account for a transfer the
// server drives end to end.
func (s *Service) acquireUploadTask(ctx context.Context, expectedSize ...int64) (*lease, error) {
	return s.leaseUpload(ctx, expectedSize...)
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
func (s *Service) ReleaseUploadJob(jobID string, actualBytes ...int64) {
	s.uploadJobsMu.Lock()
	lease, ok := s.uploadJobs[jobID]
	if !ok {
		s.uploadJobsMu.Unlock()
		return
	}
	lease.closed = true
	lease.cancel()
	if lease.quota != nil {
		actual := int64(0)
		if len(actualBytes) > 0 {
			actual = actualBytes[0]
		} else if s.db != nil {
			// Only terminal closure commits a browser job's bytes. A segment
			// request can release the job lease after a transient error and a
			// later retry must not pay for the same earlier segments twice.
			if job, err := s.db.JobByID(context.Background(), jobID); err == nil {
				if job.Status.Terminal() {
					actual = job.UploadedBytes
				}
			}
		}
		lease.quota.setActual(actual)
	}

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
