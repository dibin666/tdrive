package drive

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"
)

// The two promises multiple accounts make: the configured task limits are per
// account rather than global, and a file stored by one account can be read by
// any of them. Both are easy to get subtly wrong in a way that only shows up in
// production — the first as a drive that mysteriously runs no faster after an
// account is added, the second as a download that fails only for files
// uploaded before that account existed.

func TestTaskLimitsArePerAccount(t *testing.T) {
	// One upload at a time, two accounts: two uploads at a time in total.
	svc := limiterTestService(1, 1, time.Millisecond, 8, 2)

	firstAccount, first, err := svc.AcquireUploadJob(context.Background(), "job-a")
	if err != nil {
		t.Fatalf("acquire the first upload: %v", err)
	}
	secondAccount, second, err := svc.AcquireUploadJob(context.Background(), "job-b")
	if err != nil {
		t.Fatalf("a second upload was refused even though a second account was idle: %v", err)
	}
	if firstAccount.ID() == secondAccount.ID() {
		t.Fatalf("both uploads were put on account %s, so they share one rate-limit budget",
			firstAccount.ID())
	}

	// Both accounts are now at their limit, so a third upload has to queue.
	waiting := make(chan func(), 1)
	go func() {
		_, release, err := svc.AcquireUploadJob(context.Background(), "job-c")
		if err == nil {
			waiting <- release
		}
	}()
	select {
	case <-waiting:
		t.Fatal("a third upload started while both accounts were already at their limit")
	case <-time.After(30 * time.Millisecond):
	}

	first()
	svc.ReleaseUploadJob("job-a")
	select {
	case release := <-waiting:
		release()
	case <-time.After(time.Second):
		t.Fatal("the queued upload did not start after a slot was freed")
	}
	second()
	svc.ReleaseUploadJob("job-b")
}

// A throttled account must not be handed new work while another one is idle.
// This is the whole point of watching for FLOOD_WAIT rather than just letting
// the waiter middleware sleep.
func TestThrottledAccountIsSkipped(t *testing.T) {
	svc := limiterTestService(4, 4, time.Millisecond, 8, 2)
	cluster := svc.cluster.(*fakeTelegram)
	cluster.account(0).unavailable.Store(true)

	for i := range 3 {
		account, release, err := svc.AcquireUploadJob(context.Background(), "job-"+string(rune('a'+i)))
		if err != nil {
			t.Fatalf("acquire upload %d: %v", i, err)
		}
		if account.ID() == cluster.account(0).id {
			t.Fatalf("upload %d went to the throttled account %s", i, account.ID())
		}
		release()
		svc.ReleaseUploadJob("job-" + string(rune('a'+i)))
	}
}

// An account reading a segment it uploaded already holds a working handle, so
// it must not spend a round trip re-resolving one.
func TestOwningAccountReadsWithoutResolving(t *testing.T) {
	h := newHarnessN(t, 1<<20, 2)
	data := randomBytes(3<<20, 91)
	file := h.store(t, "/", "owned.bin", data)

	owner := h.ownerAccount(t, file.ID)
	before := owner.refreshes.Load()

	r, err := h.svc.OpenFile(context.Background(), file, owner)
	if err != nil {
		t.Fatalf("open through the uploading account: %v", err)
	}
	defer r.Close()
	got := readAllOrFail(t, r)

	if !bytes.Equal(got, data) {
		t.Fatal("the uploading account read back the wrong bytes")
	}
	if spent := owner.refreshes.Load() - before; spent != 0 {
		t.Fatalf("the uploading account re-resolved %d handles it already had", spent)
	}
}

// Telegram mints access hashes per account, so a second account cannot use the
// pair stored on the segment row. It has to resolve its own — and must not
// write that back over the owner's, which would break the owner's next read.
func TestOtherAccountResolvesItsOwnHandle(t *testing.T) {
	h := newHarnessN(t, 1<<20, 2)
	data := randomBytes(3<<20, 92)
	file := h.store(t, "/", "shared.bin", data)

	owner := h.ownerAccount(t, file.ID)
	other := h.otherAccount(t, owner)

	stored, err := h.db.Segments(context.Background(), file.ID)
	if err != nil {
		t.Fatalf("read segments: %v", err)
	}

	before := other.refreshes.Load()
	r, err := h.svc.OpenFile(context.Background(), file, other)
	if err != nil {
		t.Fatalf("open through a non-uploading account: %v", err)
	}
	defer r.Close()
	got := readAllOrFail(t, r)

	if !bytes.Equal(got, data) {
		t.Fatal("a non-uploading account read back the wrong bytes")
	}
	if spent := other.refreshes.Load() - before; spent < 1 {
		t.Fatal("a non-uploading account read without resolving a handle of its own, " +
			"which cannot work against the real Telegram")
	}

	// The owner's stored handle must be exactly as it was.
	after, err := h.db.Segments(context.Background(), file.ID)
	if err != nil {
		t.Fatalf("re-read segments: %v", err)
	}
	for i := range stored {
		if stored[i].AccessHash != after[i].AccessHash ||
			!bytes.Equal(stored[i].FileReference, after[i].FileReference) {
			t.Fatalf("segment %d's stored handle was overwritten by another account's",
				stored[i].Index)
		}
		if after[i].AccountID != owner.ID() {
			t.Fatalf("segment %d changed owner from %s to %s",
				stored[i].Index, owner.ID(), after[i].AccountID)
		}
	}

	// And the owner can still read, which is what the overwrite would have
	// broken.
	ownerRead, err := h.svc.OpenFile(context.Background(), file, owner)
	if err != nil {
		t.Fatalf("reopen through the uploading account: %v", err)
	}
	defer ownerRead.Close()
	if !bytes.Equal(readAllOrFail(t, ownerRead), data) {
		t.Fatal("the uploading account could no longer read its own file")
	}
}

// A segment whose owner is unknown — written before accounts existed, or
// recovered by an index rebuild on a different account — must be treated as
// unreadable-as-stored rather than gambled on.
func TestUnownedSegmentIsResolvedNotTrusted(t *testing.T) {
	h := newHarnessN(t, 1<<20, 2)
	data := randomBytes(2<<20, 93)
	file := h.store(t, "/", "legacy.bin", data)

	ctx := context.Background()
	segs, err := h.db.Segments(ctx, file.ID)
	if err != nil {
		t.Fatalf("read segments: %v", err)
	}
	// Rewrite the rows the way an upgraded database has them: coordinates
	// present, owner unknown.
	for _, seg := range segs {
		seg.AccountID = ""
		if err := h.db.UpsertSegment(ctx, seg); err != nil {
			t.Fatalf("clear segment ownership: %v", err)
		}
	}

	for i := range 2 {
		account := h.tg.account(i)
		r, err := h.svc.OpenFile(ctx, file, account)
		if err != nil {
			t.Fatalf("open an unowned file through %s: %v", account.ID(), err)
		}
		got := readAllOrFail(t, r)
		r.Close()
		if !bytes.Equal(got, data) {
			t.Fatalf("account %s read back the wrong bytes for an unowned file", account.ID())
		}
	}
}

func readAllOrFail(t *testing.T, r io.Reader) []byte {
	t.Helper()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return got
}

// ownerAccount is the account that stored a file's segments.
func (h *harness) ownerAccount(t *testing.T, fileID string) *fakeAccount {
	t.Helper()
	id := h.svc.SegmentOwner(context.Background(), fileID)
	if id == "" {
		t.Fatal("no account was recorded as the owner of the uploaded segments")
	}
	for _, account := range h.tg.accounts {
		if account.id == id {
			return account
		}
	}
	t.Fatalf("segments name account %q, which is not in the cluster", id)
	return nil
}

func (h *harness) otherAccount(t *testing.T, not *fakeAccount) *fakeAccount {
	t.Helper()
	for _, account := range h.tg.accounts {
		if account.id != not.id {
			return account
		}
	}
	t.Fatal("the cluster has only one account")
	return nil
}
