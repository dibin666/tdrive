package drive

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/dibin/tdrive/internal/database"
)

// Daily transfer quotas are account-local byte budgets. A quota is reserved
// when a whole content transfer gets an account, then committed when that
// transfer's slot is released. Keeping the reservation separate from the
// committed usage is what prevents two simultaneous files from both spending
// the same last few bytes.

type quotaDirection uint8

const (
	quotaUpload quotaDirection = iota + 1
	quotaDownload
)

type quotaKey struct {
	accountID string
	date      string
	direction quotaDirection
}

type quotaDayKey struct {
	accountID string
	date      string
}

type quotaTracker struct {
	db  *database.DB
	log *zap.Logger

	mu         sync.Mutex
	used       map[quotaKey]int64
	reserved   map[quotaKey]int64
	loaded     map[quotaDayKey]bool
	wakeup     chan struct{}
	generation uint64
}

func newQuotaTracker(db *database.DB, log *zap.Logger) *quotaTracker {
	return &quotaTracker{
		db:       db,
		log:      log,
		used:     make(map[quotaKey]int64),
		reserved: make(map[quotaKey]int64),
		loaded:   make(map[quotaDayKey]bool),
		wakeup:   make(chan struct{}),
	}
}

// quotaDate uses UTC so a deployment moved between hosts cannot get a second
// partial quota day merely by changing its local timezone.
func quotaDate() string { return time.Now().UTC().Format("2006-01-02") }

func quotaResetAt() time.Time {
	now := time.Now().UTC()
	next := now.AddDate(0, 0, 1)
	return time.Date(next.Year(), next.Month(), next.Day(), 0, 0, 0, 0, time.UTC)
}

func (t *quotaTracker) ensureLoadedLocked(accountID, date string) {
	day := quotaDayKey{accountID: accountID, date: date}
	if t.loaded[day] {
		return
	}
	if t.db == nil {
		t.loaded[day] = true
		return
	}
	usage, err := t.db.TelegramUsageFor(context.Background(), accountID, date)
	if err != nil {
		if t.log != nil {
			t.log.Warn("could not load telegram daily usage",
				zap.String("account", accountID), zap.String("date", date), zap.Error(err))
		}
		return
	}
	t.loaded[day] = true
	t.used[quotaKey{accountID: accountID, date: date, direction: quotaUpload}] = usage.UploadBytes
	t.used[quotaKey{accountID: accountID, date: date, direction: quotaDownload}] = usage.DownloadBytes
}

func (t *quotaTracker) valuesLocked(accountID string, direction quotaDirection) (used, reserved int64) {
	date := quotaDate()
	t.ensureLoadedLocked(accountID, date)
	key := quotaKey{accountID: accountID, date: date, direction: direction}
	return t.used[key], t.reserved[key]
}

// mayStart reports whether a new content transfer may reserve this account.
// A content whose size fits the remaining budget is admitted normally. If it
// would cross the remaining budget, one such content is still admitted when
// the account has no other content reservation; once it starts, it is allowed
// to finish rather than being moved halfway through to a different Telegram
// login. This is the important distinction between a quota boundary and a
// hard per-file size limit.
func (t *quotaTracker) mayStart(accountID string, direction quotaDirection, limit, size int64) bool {
	if size <= 0 || limit <= 0 {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	used, reserved := t.valuesLocked(accountID, direction)
	if used >= limit || reserved >= limit-used {
		return false
	}
	remaining := limit - used - reserved
	if size <= remaining {
		return true
	}
	// Let one content cross the boundary. A second concurrent content must
	// wait, because this reservation may be the content that consumes the last
	// available bytes and the running content must stay on its account.
	return reserved == 0
}

// fits reports whether a content can be covered by the bytes that remain on
// this account. The scheduler uses it to prefer an account that can fit the
// whole content over one that would cross its quota boundary.
func (t *quotaTracker) fits(accountID string, direction quotaDirection, limit, size int64) bool {
	if size <= 0 || limit <= 0 {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	used, reserved := t.valuesLocked(accountID, direction)
	if used >= limit || reserved >= limit-used {
		return false
	}
	return size <= limit-used-reserved
}

// reserve atomically claims the expected size for a content transfer.
func (t *quotaTracker) reserve(accountID string, direction quotaDirection, limit, size int64) (*quotaReservation, bool) {
	if size < 0 {
		size = 0
	}
	date := quotaDate()
	t.mu.Lock()
	t.ensureLoadedLocked(accountID, date)
	key := quotaKey{accountID: accountID, date: date, direction: direction}
	if limit > 0 && size > 0 {
		used, reserved := t.used[key], t.reserved[key]
		if used >= limit || reserved >= limit-used {
			t.mu.Unlock()
			return nil, false
		}
		remaining := limit - used - reserved
		if size > remaining && reserved != 0 {
			t.mu.Unlock()
			return nil, false
		}
	}
	t.reserved[key] += size
	t.mu.Unlock()
	return &quotaReservation{tracker: t, key: key, expected: size}, true
}

func (t *quotaTracker) signalLocked() {
	t.generation++
	close(t.wakeup)
	t.wakeup = make(chan struct{})
}

func (t *quotaTracker) generationNow() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.generation
}

// wait blocks until a reservation changes or the UTC quota day rolls over.
func (t *quotaTracker) wait(ctx context.Context, observed ...uint64) error {
	if ctx == nil {
		ctx = context.Background()
	}
	t.mu.Lock()
	if len(observed) > 0 && t.generation != observed[0] {
		t.mu.Unlock()
		return nil
	}
	wakeup := t.wakeup
	t.mu.Unlock()

	timer := time.NewTimer(time.Until(quotaResetAt()))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-wakeup:
		return nil
	case <-timer.C:
		return nil
	}
}

type quotaReservation struct {
	tracker  *quotaTracker
	key      quotaKey
	expected int64

	mu        sync.Mutex
	actual    int64
	actualSet bool
	committed bool
}

// setActual lets a transfer report a smaller amount for a cancelled or failed
// content. Callers use it before releasing the account slot; if they do not,
// the expected content size is charged, which is the conservative fallback.
func (r *quotaReservation) setActual(bytes int64) {
	if r == nil {
		return
	}
	if bytes < 0 {
		bytes = 0
	}
	r.mu.Lock()
	if !r.committed {
		r.actual, r.actualSet = bytes, true
	}
	r.mu.Unlock()
}

func (r *quotaReservation) commit() {
	if r == nil || r.tracker == nil {
		return
	}
	r.mu.Lock()
	if r.committed {
		r.mu.Unlock()
		return
	}
	r.committed = true
	actual := r.expected
	if r.actualSet {
		actual = r.actual
	}
	r.mu.Unlock()

	t := r.tracker
	t.mu.Lock()
	if t.reserved[r.key] >= r.expected {
		t.reserved[r.key] -= r.expected
	} else {
		t.reserved[r.key] = 0
	}
	if actual > 0 {
		t.used[r.key] += actual
	}
	t.signalLocked()
	t.mu.Unlock()

	if actual == 0 || t.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var upload, download int64
	if r.key.direction == quotaUpload {
		upload = actual
	} else {
		download = actual
	}
	if err := t.db.AddTelegramUsage(ctx, r.key.accountID, r.key.date, upload, download); err != nil && t.log != nil {
		t.log.Warn("could not persist telegram daily usage",
			zap.String("account", r.key.accountID), zap.String("date", r.key.date), zap.Error(err))
	}
}

// AccountQuotaStatus is the current quota and reservation snapshot shown by
// the account settings API. Remaining bytes exclude content transfers that
// are already in flight.
type AccountQuotaStatus struct {
	Date     string
	ResetAt  time.Time
	Upload   QuotaDirectionStatus
	Download QuotaDirectionStatus
}

type QuotaDirectionStatus struct {
	Limit     int64
	Used      int64
	Reserved  int64
	Remaining int64
}

func quotaDirectionStatus(limit, used, reserved int64) QuotaDirectionStatus {
	remaining := int64(-1)
	if limit > 0 {
		remaining = limit - used - reserved
		if remaining < 0 {
			remaining = 0
		}
	}
	return QuotaDirectionStatus{Limit: limit, Used: used, Reserved: reserved, Remaining: remaining}
}

func (t *quotaTracker) status(accountID string, uploadLimit, downloadLimit int64) AccountQuotaStatus {
	date := quotaDate()
	t.mu.Lock()
	uploadUsed, uploadReserved := t.valuesLocked(accountID, quotaUpload)
	downloadUsed, downloadReserved := t.valuesLocked(accountID, quotaDownload)
	t.mu.Unlock()
	return AccountQuotaStatus{
		Date:     date,
		ResetAt:  quotaResetAt(),
		Upload:   quotaDirectionStatus(uploadLimit, uploadUsed, uploadReserved),
		Download: quotaDirectionStatus(downloadLimit, downloadUsed, downloadReserved),
	}
}

func (s *Service) quotaTracker() *quotaTracker {
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	if s.quotas == nil {
		s.quotas = newQuotaTracker(s.db, s.log)
	}
	return s.quotas
}

// NotifyQuotaChanged wakes transfers waiting for an account whose quota was
// previously exhausted, so an administrator changing a quota takes effect
// immediately instead of at the next UTC midnight.
func (s *Service) NotifyQuotaChanged() {
	t := s.quotaTracker()
	t.mu.Lock()
	t.signalLocked()
	t.mu.Unlock()
}

// AccountQuotaStatus reports today's committed and in-flight bytes.
func (s *Service) AccountQuotaStatus(accountID string, uploadLimit, downloadLimit int64) AccountQuotaStatus {
	return s.quotaTracker().status(accountID, uploadLimit, downloadLimit)
}

func quotaLimit(account Account, upload bool) int64 {
	provider, ok := account.(interface{ DailyQuota(bool) int64 })
	if !ok {
		return 0
	}
	limit := provider.DailyQuota(upload)
	if limit < 0 {
		return 0
	}
	return limit
}

func directionFor(upload bool) quotaDirection {
	if upload {
		return quotaUpload
	}
	return quotaDownload
}

func (s *Service) quotaMayStart(account Account, upload bool, size int64) bool {
	return s.quotaTracker().mayStart(account.ID(), directionFor(upload), quotaLimit(account, upload), size)
}

func (s *Service) quotaFits(account Account, upload bool, size int64) bool {
	return s.quotaTracker().fits(account.ID(), directionFor(upload), quotaLimit(account, upload), size)
}

func (s *Service) reserveQuota(account Account, upload bool, size int64) (*quotaReservation, bool) {
	return s.quotaTracker().reserve(account.ID(), directionFor(upload), quotaLimit(account, upload), size)
}

// recordQuotaBytes changes what the account slot's reservation charges when
// it is eventually released. It is deliberately best-effort bookkeeping: a
// transfer that does not report a final count is charged its expected size.
func (l *lease) recordQuotaBytes(bytes int64) {
	if l != nil && l.quota != nil {
		l.quota.setActual(bytes)
	}
}

func (s *Service) transferSize(ctx context.Context, fileID string) int64 {
	if s.db == nil || fileID == "" {
		return 0
	}
	file, err := s.db.FileByID(ctx, fileID)
	if err != nil || file.Size < 0 {
		return 0
	}
	return file.Size
}

func (s *Service) uploadJobSize(ctx context.Context, jobID string) int64 {
	if s.db == nil || jobID == "" {
		return 0
	}
	job, err := s.db.JobByID(ctx, jobID)
	if err != nil || job.TotalSize < 0 {
		return 0
	}
	return job.TotalSize
}
