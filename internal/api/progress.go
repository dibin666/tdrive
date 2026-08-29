package api

import (
	"sync"

	"github.com/dibin/tdrive/internal/database"
)

// liveUploadProgress keeps the most recent in-process progress snapshot. The
// database deliberately records completed segments, while this cache fills the
// small gap between two segment completions so a refreshed transfer page does
// not briefly jump back to zero.
type liveUploadProgress struct {
	mu   sync.RWMutex
	jobs map[string]uploadProgressSnapshot
}

type uploadProgressSnapshot struct {
	uploaded int64
	total    int64
	status   database.JobStatus
}

func newLiveUploadProgress() *liveUploadProgress {
	return &liveUploadProgress{jobs: make(map[string]uploadProgressSnapshot)}
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
