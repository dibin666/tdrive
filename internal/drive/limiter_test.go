package drive

import (
	"context"
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
