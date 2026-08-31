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

// activeCount is how many slots are in use, which is what the scheduler sorts
// accounts by when it looks for the least busy one.
func (l *taskLimiter) activeCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active
}

// tryAcquire takes a slot only if one is free right now. It respects the
// waiter queue: jumping ahead of tasks that are already blocked would let a
// steady trickle of new transfers starve them indefinitely.
func (l *taskLimiter) tryAcquire() (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active < l.limit && len(l.queue) == 0 {
		l.active++
		return l.releaseFunc(), true
	}
	return nil, false
}

// acquireAny blocks on several limiters at once and returns the first slot that
// frees up, along with the index of the limiter it came from.
//
// This is how a busy multi-account drive stays even: rather than committing a
// queued transfer to one account and watching another go idle, every account is
// queued for and the loser waits are cancelled. A waiter that is granted a slot
// at the same moment as its cancellation hands it straight back — taskLimiter's
// own cancel path already covers that race, which is what makes this safe.
func acquireAny(ctx context.Context, limiters []*taskLimiter) (int, func(), error) {
	switch len(limiters) {
	case 0:
		return 0, nil, ErrNoAccount
	case 1:
		release, err := limiters[0].acquire(ctx)
		return 0, release, err
	}

	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		index   int
		release func()
		err     error
	}
	results := make(chan result, len(limiters))
	for i, limiter := range limiters {
		go func() {
			release, err := limiter.acquire(raceCtx)
			results <- result{index: i, release: release, err: err}
		}()
	}

	var (
		winner  = -1
		release func()
		lastErr error
	)
	for range limiters {
		r := <-results
		switch {
		case r.err != nil:
			if lastErr == nil {
				lastErr = r.err
			}
		case winner == -1:
			winner, release = r.index, r.release
			// Stop the others. They either fail with context.Canceled or return
			// their slot themselves.
			cancel()
		default:
			r.release()
		}
	}
	if winner == -1 {
		if lastErr == nil {
			lastErr = ctx.Err()
		}
		return 0, nil, lastErr
	}
	return winner, release, nil
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

	ready  chan struct{}
	cancel context.CancelFunc

	gateRelease func()
	acquireErr  error
	active      int
	closed      bool
}

// SetTransferConcurrency applies new task limits immediately. Existing tasks
// keep their slots; queued tasks are woken when a larger limit makes room.
//
// The limits are per account. An operator asking for one upload at a time on a
// drive with two accounts gets two concurrent uploads, one per login — which is
// the point of configuring a second account, and is spelled out in the WebUI
// next to the slider.
func (s *Service) SetTransferConcurrency(upload, download int) {
	s.limitersMu.Lock()
	defer s.limitersMu.Unlock()
	for _, limiter := range s.uploadLimiters {
		limiter.setLimit(upload)
	}
	for _, limiter := range s.downloadLimiters {
		limiter.setLimit(download)
	}
}

// TransferCapacity reports the configured per-account limits and what they add
// up to across the accounts currently able to take work. The WebUI shows both
// numbers so the per-account semantics are not a surprise.
func (s *Service) TransferCapacity() (accounts, upload, download int) {
	settings := s.cfg.RuntimeSettings()
	n := len(s.cluster.Accounts())
	return n, settings.UploadConcurrency * n, settings.DownloadConcurrency * n
}

// ActiveTasksByAccount reports how many upload and download slots each account
// is using, which is what makes "one at a time, per account" observable in the
// settings page instead of something an operator has to take on trust.
func (s *Service) ActiveTasksByAccount() (upload, download map[string]int) {
	s.limitersMu.Lock()
	defer s.limitersMu.Unlock()

	upload = make(map[string]int, len(s.uploadLimiters))
	for id, limiter := range s.uploadLimiters {
		upload[id] = limiter.activeCount()
	}
	download = make(map[string]int, len(s.downloadLimiters))
	for id, limiter := range s.downloadLimiters {
		download[id] = limiter.activeCount()
	}
	return upload, download
}

// AcquireDownloadTask reserves one whole-file download slot for a caller that
// owns the whole transfer on its own — a staged download assembling a file on
// disk, for instance. Callers serving a client over HTTP want
// AcquireDownloadSession instead, so that the several range requests making up
// one logical download share a slot.
func (s *Service) AcquireDownloadTask(ctx context.Context, prefer string) (Account, func(), error) {
	lease, err := s.leaseDownload(ctx, prefer)
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

	ready  chan struct{}
	cancel context.CancelFunc

	gateRelease func()
	acquireErr  error
	active      int
	// idle is the grace timer armed when active reaches zero.
	idle *time.Timer
}

// AcquireDownloadSession reserves (or joins) the task slot for one logical
// download, and returns the Telegram account that slot belongs to. The returned
// function must be called when the request finishes.
//
// fileID lets the scheduler prefer the account that uploaded the file, which
// already holds usable document handles for it. It is only a hint — any account
// can serve any file by re-resolving its own handles, so a busy uploader never
// blocks a read — and it is only consulted when the session is first created,
// because every later range request joins an account that is already chosen.
func (s *Service) AcquireDownloadSession(ctx context.Context, key, fileID string) (Account, func(), error) {
	if key == "" {
		return s.AcquireDownloadTask(ctx, s.SegmentOwner(ctx, fileID))
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
	acquired, acquireErr := s.leaseDownload(taskCtx, s.SegmentOwner(taskCtx, fileID))
	stopCancel()

	s.downloadSessionsMu.Lock()
	session.acquireErr = acquireErr
	if acquireErr == nil {
		session.account = acquired.account
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
	if session.gateRelease != nil {
		session.gateRelease()
		session.gateRelease = nil
	}
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
	acquired, acquireErr := s.leaseUpload(taskCtx)
	stopCancel()

	var releaseNow func()
	s.uploadJobsMu.Lock()
	if acquireErr == nil && lease.closed {
		acquireErr = ErrUploadTaskClosed
		releaseNow = acquired.release
		acquired = nil
	}
	lease.acquireErr = acquireErr
	if acquireErr == nil {
		lease.account = acquired.account
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
func (s *Service) acquireUploadTask(ctx context.Context) (*lease, error) {
	return s.leaseUpload(ctx)
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
