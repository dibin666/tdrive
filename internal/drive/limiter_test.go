package drive

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dibin/tdrive/internal/config"
)

func TestTaskLimiterWaitsUntilAJobFinishes(t *testing.T) {
	limiter := newTaskLimiter(1)
	first, err := limiter.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire first task: %v", err)
	}

	started := make(chan struct{})
	second := make(chan func(), 1)
	go func() {
		release, err := limiter.acquire(context.Background())
		if err != nil {
			return
		}
		close(started)
		second <- release
	}()

	select {
	case <-started:
		t.Fatal("second task bypassed the concurrency limit")
	case <-time.After(30 * time.Millisecond):
	}

	first()
	select {
	case release := <-second:
		release()
	case <-time.After(time.Second):
		t.Fatal("second task did not start after the first task released its slot")
	}
}

func TestTaskLimiterContextCanCancelWaitingTask(t *testing.T) {
	limiter := newTaskLimiter(1)
	first, err := limiter.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire first task: %v", err)
	}
	defer first()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := limiter.acquire(ctx); err == nil {
		t.Fatal("a cancelled waiter acquired a task slot")
	}
}

func TestTaskLimiterLimitChangeWakesWaiters(t *testing.T) {
	limiter := newTaskLimiter(1)
	first, err := limiter.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire first task: %v", err)
	}
	defer first()

	started := make(chan func(), 1)
	go func() {
		release, err := limiter.acquire(context.Background())
		if err == nil {
			started <- release
		}
	}()

	select {
	case <-started:
		t.Fatal("waiter started before the limit changed")
	case <-time.After(30 * time.Millisecond):
	}

	limiter.setLimit(2)
	select {
	case release := <-started:
		release()
	case <-time.After(time.Second):
		t.Fatal("increasing the limit did not wake the waiter")
	}
}

func TestUploadJobSegmentsShareOneTaskSlot(t *testing.T) {
	cfg := &config.Config{Transfer: config.Transfer{UploadConcurrency: 1, DownloadConcurrency: 1}}
	svc := &Service{
		cfg:             cfg,
		uploadLimiter:   newTaskLimiter(1),
		downloadLimiter: newTaskLimiter(1),
		uploadJobs:      make(map[string]*uploadJobLease),
	}

	first, err := svc.AcquireUploadJob(context.Background(), "job-a")
	if err != nil {
		t.Fatalf("acquire first segment: %v", err)
	}
	second, err := svc.AcquireUploadJob(context.Background(), "job-a")
	if err != nil {
		t.Fatalf("join second segment: %v", err)
	}
	first()
	second()

	waiting := make(chan func(), 1)
	go func() {
		release, err := svc.AcquireUploadJob(context.Background(), "job-b")
		if err == nil {
			waiting <- release
		}
	}()

	select {
	case <-waiting:
		t.Fatal("another upload job acquired a slot before job-a was closed")
	case <-time.After(30 * time.Millisecond):
	}

	svc.ReleaseUploadJob("job-a")
	select {
	case release := <-waiting:
		release()
		svc.ReleaseUploadJob("job-b")
	case <-time.After(time.Second):
		t.Fatal("next upload job did not start after job-a was released")
	}
}

// downloadTestService builds a Service with only the fields the download
// session code touches, so the limiter can be exercised without a database or
// a Telegram connection.
//
// grace must be positive: a zero duration means "unset" everywhere runtime
// settings are read, and withRuntimeDefaults would substitute the 15 second
// production default.
func downloadTestService(limit int, grace time.Duration, maxConns int) *Service {
	cfg := &config.Config{
		Transfer: config.Transfer{
			UploadConcurrency:   1,
			DownloadConcurrency: limit,
			MaxDownloadConns:    maxConns,
			DownloadGrace:       grace,
		},
	}
	return &Service{
		cfg:              cfg,
		uploadLimiter:    newTaskLimiter(1),
		downloadLimiter:  newTaskLimiter(limit),
		uploadJobs:       make(map[string]*uploadJobLease),
		downloadSessions: make(map[string]*downloadSession),
	}
}

// A multi-connection download is one transfer. This is the property that lets
// the whole-file concurrency limit and parallel downloading coexist: with a
// limit of one, eight range requests for the same file must all proceed.
func TestDownloadSessionSharesOneTaskSlot(t *testing.T) {
	svc := downloadTestService(1, time.Millisecond, 8)

	releases := make([]func(), 0, 4)
	for i := range 4 {
		release, err := svc.AcquireDownloadSession(context.Background(), "file-a")
		if err != nil {
			t.Fatalf("acquire connection %d of the same download: %v", i+1, err)
		}
		releases = append(releases, release)
	}

	// A different file is a different transfer and must wait.
	waiting := make(chan func(), 1)
	go func() {
		release, err := svc.AcquireDownloadSession(context.Background(), "file-b")
		if err == nil {
			waiting <- release
		}
	}()

	select {
	case <-waiting:
		t.Fatal("a second download bypassed the concurrency limit")
	case <-time.After(30 * time.Millisecond):
	}

	for _, release := range releases {
		release()
	}

	select {
	case release := <-waiting:
		release()
	case <-time.After(time.Second):
		t.Fatal("the queued download did not start after the first one finished")
	}
}

// The slot is held through the gap a parallel downloader leaves between
// ranges, so it cannot lose its place mid-file to a queued transfer.
func TestDownloadSessionHoldsSlotThroughIdleGrace(t *testing.T) {
	svc := downloadTestService(1, 200*time.Millisecond, 8)

	release, err := svc.AcquireDownloadSession(context.Background(), "file-a")
	if err != nil {
		t.Fatalf("acquire download: %v", err)
	}
	release()

	// Nothing is in flight, but the grace period has not elapsed, so another
	// transfer must not be admitted yet.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := svc.AcquireDownloadSession(ctx, "file-b"); err == nil {
		t.Fatal("another download took the slot during the grace period")
	}

	// The same download coming back for its next range reuses the session.
	again, err := svc.AcquireDownloadSession(context.Background(), "file-a")
	if err != nil {
		t.Fatalf("rejoin the same download during grace: %v", err)
	}
	again()

	// Once the grace really does expire, the slot is returned.
	deadline := time.Now().Add(2 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		next, err := svc.AcquireDownloadSession(ctx, "file-b")
		cancel()
		if err == nil {
			next()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the download slot was never returned after the grace period")
		}
	}
}

// One client must not be able to open unlimited sockets against one file.
func TestDownloadSessionCapsConnections(t *testing.T) {
	svc := downloadTestService(4, time.Millisecond, 2)

	first, err := svc.AcquireDownloadSession(context.Background(), "file-a")
	if err != nil {
		t.Fatalf("acquire first connection: %v", err)
	}
	defer first()
	second, err := svc.AcquireDownloadSession(context.Background(), "file-a")
	if err != nil {
		t.Fatalf("acquire second connection: %v", err)
	}
	defer second()

	if _, err := svc.AcquireDownloadSession(context.Background(), "file-a"); !errors.Is(err, ErrTooManyConnections) {
		t.Fatalf("expected a too-many-connections error, got %v", err)
	}
}
