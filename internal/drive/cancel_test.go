package drive

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dibin/tdrive/internal/database"
)

// Cancelling a transfer has to reach the work already in flight. Recording the
// row as cancelled and leaving the worker running is what kept an upload going
// against a job the transfer panel showed as stopped, and the two then
// disagreed for as long as the transfer lasted.
func TestCancelUploadInterruptsWorkAlreadyInFlight(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 1024)

	job, _, err := h.svc.Begin(ctx, UploadRequest{DirPath: "/", Name: "big.mkv", Size: 4096})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	// Stand in for a browser segment request, a fetch worker or a plugin's
	// segment stream: something moving bytes outside the request that cancels.
	workerCtx, release := h.svc.WatchUploadJob(ctx, job.ID)
	defer release()

	if err := h.svc.CancelUpload(ctx, job.ID); err != nil {
		t.Fatalf("CancelUpload: %v", err)
	}

	select {
	case <-workerCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling the upload left the work in flight running")
	}

	stopped, err := h.db.JobByID(ctx, job.ID)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	if stopped.Status != database.JobCancelled {
		t.Errorf("Status = %q, want %q", stopped.Status, database.JobCancelled)
	}

	// A segment that was already on its way arrives after the cancellation. It
	// must be refused rather than stored against a file Abort has deleted.
	err = h.svc.PutSegment(ctx, job, 1, bytes.NewReader(make([]byte, 1024)), 1024, nil)
	if !errors.Is(err, database.ErrJobFinished) {
		t.Fatalf("PutSegment after cancel = %v, want ErrJobFinished", err)
	}
}

// The transfer panel and the browser can both ask to cancel the same upload,
// and a second cancellation arriving is not an error the user should see.
func TestCancelUploadIsIdempotent(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 1024)

	job, _, err := h.svc.Begin(ctx, UploadRequest{DirPath: "/", Name: "twice.bin", Size: 512})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := h.svc.CancelUpload(ctx, job.ID); err != nil {
		t.Fatalf("first CancelUpload: %v", err)
	}
	if err := h.svc.CancelUpload(ctx, job.ID); err != nil {
		t.Fatalf("second CancelUpload: %v", err)
	}
}

// Abort deletes the file along with the transfer, so cancelling a job whose
// bytes have all landed would take the finished file with it.
func TestCancelUploadRefusesAFinishedTransfer(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, 1024)

	data := randomBytes(2048, 7)
	file := h.store(t, "/", "done.bin", data)

	jobs, err := h.db.ListJobs(ctx, "", 10)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	var jobID string
	for _, job := range jobs {
		if job.FileID == file.ID {
			jobID = job.ID
		}
	}
	if jobID == "" {
		t.Fatal("no job recorded for the stored file")
	}

	if err := h.svc.CancelUpload(ctx, jobID); !errors.Is(err, database.ErrJobFinished) {
		t.Fatalf("CancelUpload of a finished transfer = %v, want ErrJobFinished", err)
	}
	if _, err := h.db.FileByID(ctx, file.ID); err != nil {
		t.Fatalf("the finished file was removed by a late cancellation: %v", err)
	}
}
