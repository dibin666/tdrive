package drive

import (
	"context"
	"testing"
	"time"

	"github.com/dibin/tdrive/internal/database"
)

// waitForStatus polls the row until the session's grace timer has settled it,
// because settling is deliberately asynchronous.
func waitForStatus(t *testing.T, h *harness, jobID string, want database.DownloadStatus) database.DownloadJob {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last database.DownloadJob
	for time.Now().Before(deadline) {
		job, err := h.db.DownloadByID(context.Background(), jobID)
		if err != nil {
			t.Fatalf("DownloadByID: %v", err)
		}
		last = job
		if job.Status == want {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("download settled as %q, want %q", last.Status, want)
	return last
}

// onlyDownload returns the single download row, failing if there is not exactly
// one — which is the property that matters most here.
func onlyDownload(t *testing.T, h *harness) database.DownloadJob {
	t.Helper()
	jobs, err := h.db.FilterDownloads(context.Background(), database.TransferFilter{AllUsers: true})
	if err != nil {
		t.Fatalf("FilterDownloads: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d download rows, want 1", len(jobs))
	}
	return jobs[0]
}

// A WebDAV client fetches one file in many range requests. That has to leave
// one row in the transfer history, not one per request.
func TestClientDownloadGroupsRequestsIntoOneRow(t *testing.T) {
	h := newHarness(t, 1<<20)
	// A grace period long enough that the session is still open while the test
	// is making its assertions about it.
	h.cfg.Transfer.DownloadGrace = 300 * time.Millisecond
	ctx := context.Background()

	file := h.store(t, "/", "movie.mkv", randomBytes(4096, 7))

	first, err := h.svc.TrackClientDownload(ctx, "dav:u1:"+file.ID, "", file, database.DownloadWebDAV)
	if err != nil {
		t.Fatalf("TrackClientDownload: %v", err)
	}
	second, err := h.svc.TrackClientDownload(ctx, "dav:u1:"+file.ID, "", file, database.DownloadWebDAV)
	if err != nil {
		t.Fatalf("TrackClientDownload (join): %v", err)
	}

	job := onlyDownload(t, h)
	if job.Mode != database.DownloadWebDAV || job.Status != database.DownloadRunning {
		t.Fatalf("row = mode %q status %q, want webdav/running", job.Mode, job.Status)
	}

	if err := first.Add(2048); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := second.Add(2048); err != nil {
		t.Fatalf("Add: %v", err)
	}
	first.Close()
	second.Close()

	settled := waitForStatus(t, h, job.ID, database.DownloadComplete)
	if settled.DownloadedBytes != 4096 {
		t.Fatalf("DownloadedBytes = %d, want 4096", settled.DownloadedBytes)
	}
	if settled.StartedAt.IsZero() || settled.FinishedAt.IsZero() {
		t.Fatal("a settled download has no timing bracket, so it can report no duration")
	}
	onlyDownload(t, h)
}

// A client that stops partway did not download the file, and recording that as
// complete would make the history answer the wrong question.
func TestClientDownloadThatStopsEarlyIsNotComplete(t *testing.T) {
	h := newHarness(t, 1<<20)
	h.cfg.Transfer.DownloadGrace = 50 * time.Millisecond
	ctx := context.Background()

	file := h.store(t, "/", "movie.mkv", randomBytes(4096, 11))

	read, err := h.svc.TrackClientDownload(ctx, "dav:u1:"+file.ID, "", file, database.DownloadWebDAV)
	if err != nil {
		t.Fatalf("TrackClientDownload: %v", err)
	}
	if err := read.Add(1000); err != nil {
		t.Fatalf("Add: %v", err)
	}
	read.Close()

	settled := waitForStatus(t, h, onlyDownload(t, h).ID, database.DownloadCancelled)
	if settled.DownloadedBytes != 1000 {
		t.Fatalf("DownloadedBytes = %d, want 1000", settled.DownloadedBytes)
	}
}

// The cancel button in the transfer panel has to actually stop a mounted
// client, which it can only do by refusing to serve the next read.
func TestCancelClientDownloadStopsFurtherReads(t *testing.T) {
	h := newHarness(t, 1<<20)
	h.cfg.Transfer.DownloadGrace = 50 * time.Millisecond
	ctx := context.Background()

	file := h.store(t, "/", "movie.mkv", randomBytes(4096, 13))

	read, err := h.svc.TrackClientDownload(ctx, "dav:u1:"+file.ID, "", file, database.DownloadWebDAV)
	if err != nil {
		t.Fatalf("TrackClientDownload: %v", err)
	}
	job := onlyDownload(t, h)

	if !h.svc.CancelClientDownload(job.ID) {
		t.Fatal("CancelClientDownload did not find the live download")
	}
	if err := read.Add(1); err != ErrDownloadCancelled {
		t.Fatalf("Add after cancel = %v, want ErrDownloadCancelled", err)
	}
	read.Close()

	waitForStatus(t, h, job.ID, database.DownloadCancelled)

	if h.svc.CancelClientDownload(job.ID) {
		t.Fatal("a settled download is still registered as live")
	}
}
