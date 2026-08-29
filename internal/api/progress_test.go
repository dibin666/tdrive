package api

import (
	"testing"

	"github.com/dibin/tdrive/internal/database"
)

func TestLiveUploadProgressDoesNotRegress(t *testing.T) {
	progress := newLiveUploadProgress()
	progress.update("job", 800, 1000, database.JobRunning)
	progress.update("job", 200, 1000, database.JobRunning)

	job := database.UploadJob{
		ID:            "job",
		TotalSize:     1000,
		UploadedBytes: 0,
		Status:        database.JobRunning,
	}
	progress.merge(&job)

	if job.UploadedBytes != 800 {
		t.Fatalf("UploadedBytes = %d, want 800", job.UploadedBytes)
	}

	progress.clear("job")
	job.UploadedBytes = 400
	progress.merge(&job)
	if job.UploadedBytes != 400 {
		t.Fatalf("cleared progress changed UploadedBytes to %d", job.UploadedBytes)
	}
}

func TestLiveUploadProgressClampsToTotal(t *testing.T) {
	progress := newLiveUploadProgress()
	progress.update("job", 1200, 1000, database.JobRunning)

	job := database.UploadJob{ID: "job", TotalSize: 1000, Status: database.JobRunning}
	progress.merge(&job)
	if job.UploadedBytes != 1000 {
		t.Fatalf("UploadedBytes = %d, want 1000", job.UploadedBytes)
	}
}
