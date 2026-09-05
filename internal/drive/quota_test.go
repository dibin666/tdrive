package drive

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dibin/tdrive/internal/database"
)

func TestDailyQuotaMovesNewContentToAnotherAccount(t *testing.T) {
	svc := limiterTestService(2, 2, time.Millisecond, 8, 2)
	cluster := svc.cluster.(*fakeTelegram)
	cluster.account(0).uploadQuota = 100
	cluster.account(1).uploadQuota = 100

	first, err := svc.acquire(context.Background(), true, "", 100)
	if err != nil {
		t.Fatalf("acquire first content: %v", err)
	}
	second, err := svc.acquire(context.Background(), true, "", 100)
	if err != nil {
		first.release()
		t.Fatalf("acquire second content: %v", err)
	}
	if first.account.ID() == second.account.ID() {
		t.Fatalf("both contents used account %s after its daily quota was reserved", first.account.ID())
	}

	// Releasing a lease commits the content's bytes. Both accounts are now
	// exhausted, so a new content waits rather than exceeding either budget.
	first.release()
	second.release()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := svc.acquire(ctx, true, "", 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire after both quotas were spent = %v, want context deadline", err)
	}
}

// A browser/plugin job carries its size in the persisted job row. The job
// lease must reserve that size before the global slot is released; otherwise a
// new job can choose the same account while the old job is still running.
func TestUploadJobQuotaMovesQueuedContentToAnotherAccount(t *testing.T) {
	h := newHarnessN(t, 1<<20, 2)
	ctx := context.Background()
	h.tg.account(0).uploadQuota = 100
	h.tg.account(1).uploadQuota = 100

	firstJob, _, err := h.svc.Begin(ctx, UploadRequest{
		DirPath: "/", Name: "first.bin", Size: 100,
	})
	if err != nil {
		t.Fatalf("begin first job: %v", err)
	}
	first, firstRelease, err := h.svc.AcquireUploadJob(ctx, firstJob.ID)
	if err != nil {
		t.Fatalf("acquire first job: %v", err)
	}
	defer firstRelease()
	if first.ID() != "acct-1" {
		t.Fatalf("first job used %s, want acct-1", first.ID())
	}

	secondJob, _, err := h.svc.Begin(ctx, UploadRequest{
		DirPath: "/", Name: "second.bin", Size: 1,
	})
	if err != nil {
		t.Fatalf("begin second job: %v", err)
	}
	second, secondRelease, err := h.svc.AcquireUploadJob(ctx, secondJob.ID)
	if err != nil {
		t.Fatalf("acquire second job: %v", err)
	}
	defer secondRelease()
	if second.ID() != "acct-2" {
		t.Fatalf("queued job used %s, want the fallback acct-2 while acct-1 is reserved", second.ID())
	}
	h.svc.ReleaseUploadJob(secondJob.ID)
}

func TestDailyQuotaDoesNotMoveAnActiveContent(t *testing.T) {
	svc := limiterTestService(2, 2, time.Millisecond, 8, 2)
	cluster := svc.cluster.(*fakeTelegram)
	cluster.account(0).uploadQuota = 100
	cluster.account(1).uploadQuota = 100

	first, err := svc.acquire(context.Background(), true, "", 100)
	if err != nil {
		t.Fatalf("acquire content: %v", err)
	}
	defer first.release()

	// The first lease keeps its original account even though that reservation
	// consumes the whole daily budget. A new content is what gets routed to the
	// other account; a running content is never split between logins.
	second, err := svc.acquire(context.Background(), true, "", 1)
	if err != nil {
		t.Fatalf("acquire follow-up content: %v", err)
	}
	if second.account.ID() == first.account.ID() {
		t.Fatalf("follow-up content stayed on exhausted account %s", first.account.ID())
	}
	second.release()
	if first.account.ID() == "" {
		t.Fatal("the active content lost its account assignment")
	}
}

func TestDailyQuotaPrefersAnAccountThatFits(t *testing.T) {
	svc := limiterTestService(2, 2, time.Millisecond, 8, 2)
	cluster := svc.cluster.(*fakeTelegram)
	cluster.account(0).uploadQuota = 100
	cluster.account(1).uploadQuota = 100

	first, err := svc.acquire(context.Background(), true, "", 60)
	if err != nil {
		t.Fatalf("acquire first content: %v", err)
	}
	if first.account.ID() != cluster.account(0).id {
		t.Fatalf("first content used %s, want the primary", first.account.ID())
	}
	first.recordQuotaBytes(60)
	first.release()

	// The primary has only 40 bytes left. The fallback can fit the whole
	// content, so the scheduler must not spend the primary's one allowed
	// boundary-crossing admission and strand a later upload there.
	second, err := svc.acquire(context.Background(), true, "", 50)
	if err != nil {
		t.Fatalf("acquire follow-up content: %v", err)
	}
	if second.account.ID() != cluster.account(1).id {
		t.Fatalf("follow-up content used %s, want the fitting fallback acct-2", second.account.ID())
	}
	second.release()
}

func TestDailyQuotaAllowsAContentToCrossTheBoundary(t *testing.T) {
	svc := limiterTestService(2, 2, time.Millisecond, 8, 1)
	cluster := svc.cluster.(*fakeTelegram)
	cluster.account(0).uploadQuota = 100

	first, err := svc.acquire(context.Background(), true, "", 60)
	if err != nil {
		t.Fatalf("acquire first content: %v", err)
	}
	first.recordQuotaBytes(60)
	first.release()

	// Only 40 bytes remain, but the next 50-byte content is allowed to cross
	// the boundary and stays on this account for its whole upload.
	second, err := svc.acquire(context.Background(), true, "", 50)
	if err != nil {
		t.Fatalf("acquire content that crosses the quota boundary: %v", err)
	}
	if second.account.ID() != cluster.account(0).id {
		t.Fatalf("content was not admitted on the only available account: %s", second.account.ID())
	}
	second.release()

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := svc.acquire(ctx, true, "", 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire after crossing the quota boundary = %v, want context deadline", err)
	}
}

func TestDailyDownloadQuotaMovesNewContentToAnotherAccount(t *testing.T) {
	svc := limiterTestService(2, 2, time.Millisecond, 8, 2)
	cluster := svc.cluster.(*fakeTelegram)
	cluster.account(0).downloadQuota = 100
	cluster.account(1).downloadQuota = 100

	first, err := svc.acquire(context.Background(), false, "", 100)
	if err != nil {
		t.Fatalf("acquire first download: %v", err)
	}
	second, err := svc.acquire(context.Background(), false, "", 100)
	if err != nil {
		first.release()
		t.Fatalf("acquire second download: %v", err)
	}
	if first.account.ID() == second.account.ID() {
		t.Fatalf("both downloads used account %s after its daily quota was reserved", first.account.ID())
	}
	first.release()
	second.release()

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := svc.acquire(ctx, false, "", 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("download after both quotas were spent = %v, want context deadline", err)
	}
}

func TestDailyQuotaUsageIsPersistedAndShownAsRemaining(t *testing.T) {
	h := newHarnessN(t, 1<<20, 2)
	account := database.TGAccount{
		ID:          "acct-1",
		AppID:       1234567,
		AppHash:     "hash",
		SessionFile: "session-quota.json",
		Enabled:     true,
	}
	if err := h.db.InsertAccount(context.Background(), account); err != nil {
		t.Fatalf("InsertAccount: %v", err)
	}
	cluster := h.tg
	cluster.account(0).uploadQuota = 100
	cluster.account(1).unavailable.Store(true)

	lease, err := h.svc.acquire(context.Background(), true, "", 25)
	if err != nil {
		t.Fatalf("acquire quota-limited content: %v", err)
	}
	if lease.account.ID() != "acct-1" {
		t.Fatalf("content used account %s, want acct-1", lease.account.ID())
	}
	lease.recordQuotaBytes(25)
	lease.release()

	usage, err := h.db.TelegramUsageFor(context.Background(), account.ID, quotaDate())
	if err != nil {
		t.Fatalf("TelegramUsageFor: %v", err)
	}
	if usage.UploadBytes != 25 {
		t.Fatalf("persisted upload usage = %d, want 25", usage.UploadBytes)
	}
	status := h.svc.AccountQuotaStatus(account.ID, 100, 0)
	if status.Upload.Used != 25 || status.Upload.Remaining != 75 {
		t.Fatalf("quota status = used %d/remaining %d, want 25/75",
			status.Upload.Used, status.Upload.Remaining)
	}
}
