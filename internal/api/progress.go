package api

import (
	"sync"
	"time"

	"github.com/dibin/tdrive/internal/database"
)

// liveUploadProgress keeps the most recent in-process progress snapshot. The
// database deliberately records completed segments, while this cache fills the
// small gap between two segment completions so a refreshed transfer page does
// not briefly jump back to zero.
type liveUploadProgress struct {
	mu   sync.RWMutex
	jobs map[string]uploadProgressSnapshot
	// rates answers "how fast is this going right now". The stored row cannot:
	// it holds bytes, not time.
	rates *liveRates
}

type uploadProgressSnapshot struct {
	uploaded int64
	total    int64
	status   database.JobStatus
}

func newLiveUploadProgress() *liveUploadProgress {
	return &liveUploadProgress{
		jobs:  make(map[string]uploadProgressSnapshot),
		rates: newLiveRates(),
	}
}

// speed is the current bytes per second of a server-driven upload, or zero for
// one this process is not moving.
func (p *liveUploadProgress) speed(id string) float64 {
	if p == nil {
		return 0
	}
	return p.rates.speed(id)
}

// update is monotonic for one job. Upload callbacks can arrive out of order
// when several Telegram parts are in flight, so an older callback must not make
// the transfer page regress.
func (p *liveUploadProgress) update(id string, uploaded, total int64, status database.JobStatus) {
	if p == nil || id == "" {
		return
	}
	if status != database.JobPending && status != database.JobRunning {
		p.clear(id)
		return
	}
	if uploaded < 0 {
		uploaded = 0
	}
	p.rates.observe(id, uploaded)
	if total < 0 {
		total = 0
	}
	if total > 0 && uploaded > total {
		uploaded = total
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	previous, ok := p.jobs[id]
	if ok {
		if uploaded < previous.uploaded {
			uploaded = previous.uploaded
		}
		if total < previous.total {
			total = previous.total
		}
		if previous.status == database.JobRunning {
			status = database.JobRunning
		}
	}
	p.jobs[id] = uploadProgressSnapshot{uploaded: uploaded, total: total, status: status}
}

func (p *liveUploadProgress) clear(id string) {
	if p == nil || id == "" {
		return
	}
	p.mu.Lock()
	delete(p.jobs, id)
	p.mu.Unlock()
	p.rates.forget(id)
}

// merge applies a live snapshot to a job read from SQLite without allowing an
// older database value to overwrite progress that is already in memory.
func (p *liveUploadProgress) merge(job *database.UploadJob) {
	if p == nil || job == nil {
		return
	}
	if job.Status != database.JobPending && job.Status != database.JobRunning {
		p.clear(job.ID)
		return
	}

	p.mu.RLock()
	snapshot, ok := p.jobs[job.ID]
	p.mu.RUnlock()
	if !ok {
		return
	}

	if snapshot.uploaded > job.UploadedBytes {
		job.UploadedBytes = snapshot.uploaded
	}
	if snapshot.total > job.TotalSize {
		job.TotalSize = snapshot.total
	}
	if snapshot.status == database.JobRunning {
		job.Status = database.JobRunning
	}
}

// A transfer the browser drives reports its own speed, because it is the thing
// holding the socket. Everything else — a WebDAV write, a VPS-local upload, a
// remote URL fetch, a staged download — is moved by this process on nobody's
// behalf, and the only record of it is a byte count in SQLite. Timing those
// byte counts here is what puts a speed on those rows instead of a dash.
const (
	// A sample is only folded in once this much time has passed. gotd reports
	// every upload part, which on a fast link is hundreds of callbacks a
	// second; dividing across such a short window measures jitter, not speed.
	rateSampleInterval = 350 * time.Millisecond
	// Past this long without a sample the transfer is stalled, and its last
	// known rate is no longer a description of anything.
	rateStale = 6 * time.Second
	// Weight of the newest sample. Low enough that one slow chunk does not
	// make the number jump, high enough to follow a real change within a
	// couple of seconds.
	rateSmoothing = 0.4
)

// rateMeter turns a series of cumulative byte counts into a current speed.
type rateMeter struct {
	bytes int64
	at    time.Time
	rate  float64
	// seen is when the rate was last recomputed, which is what decides whether
	// it is still worth reporting.
	seen time.Time
}

func (m *rateMeter) observe(bytes int64, now time.Time) {
	// A count that went backwards means a different transfer is reusing the
	// id, or a resumed one restarted its accounting. Either way the previous
	// window describes nothing.
	if m.at.IsZero() || bytes < m.bytes {
		*m = rateMeter{bytes: bytes, at: now, seen: now}
		return
	}
	elapsed := now.Sub(m.at)
	if elapsed < rateSampleInterval {
		return
	}
	instant := float64(bytes-m.bytes) / elapsed.Seconds()
	if m.rate == 0 {
		m.rate = instant
	} else {
		m.rate = m.rate*(1-rateSmoothing) + instant*rateSmoothing
	}
	m.bytes, m.at, m.seen = bytes, now, now
}

func (m rateMeter) speed(now time.Time) float64 {
	if m.seen.IsZero() || now.Sub(m.seen) > rateStale {
		return 0
	}
	return m.rate
}

// liveRates holds one meter per in-flight transfer.
type liveRates struct {
	mu     sync.Mutex
	meters map[string]*rateMeter
}

func newLiveRates() *liveRates {
	return &liveRates{meters: make(map[string]*rateMeter)}
}

func (l *liveRates) observe(id string, bytes int64) {
	if l == nil || id == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	meter, ok := l.meters[id]
	if !ok {
		meter = &rateMeter{}
		l.meters[id] = meter
	}
	meter.observe(bytes, time.Now())
}

func (l *liveRates) speed(id string) float64 {
	if l == nil || id == "" {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	meter, ok := l.meters[id]
	if !ok {
		return 0
	}
	return meter.speed(time.Now())
}

// forget drops a finished transfer's meter. Without it the map would grow by
// one entry per transfer for the lifetime of the process.
func (l *liveRates) forget(id string) {
	if l == nil || id == "" {
		return
	}
	l.mu.Lock()
	delete(l.meters, id)
	l.mu.Unlock()
}
