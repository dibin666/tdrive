package api

import (
	"testing"
	"time"

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

func TestRateMeterMeasuresBytesPerSecond(t *testing.T) {
	base := time.Now()
	var meter rateMeter

	meter.observe(0, base)
	// Below the sample interval nothing is folded in: dividing by a few
	// milliseconds measures jitter rather than throughput.
	meter.observe(1_000_000, base.Add(50*time.Millisecond))
	if got := meter.speed(base.Add(50 * time.Millisecond)); got != 0 {
		t.Fatalf("speed after a sub-interval sample = %v, want 0", got)
	}

	meter.observe(2_000_000, base.Add(time.Second))
	if got := meter.speed(base.Add(time.Second)); got < 1_900_000 || got > 2_100_000 {
		t.Fatalf("speed = %v, want about 2 MB/s", got)
	}
}

func TestRateMeterGoesQuietWhenStalled(t *testing.T) {
	base := time.Now()
	var meter rateMeter

	meter.observe(0, base)
	meter.observe(1_000_000, base.Add(time.Second))
	if meter.speed(base.Add(time.Second)) == 0 {
		t.Fatal("speed is zero immediately after a sample")
	}
	// A transfer nobody has reported on for a while is not moving, and its last
	// known rate would be a lie rather than an estimate.
	if got := meter.speed(base.Add(time.Second + rateStale + time.Second)); got != 0 {
		t.Fatalf("stale speed = %v, want 0", got)
	}
}

func TestRateMeterRestartsWhenBytesGoBackwards(t *testing.T) {
	base := time.Now()
	var meter rateMeter

	meter.observe(0, base)
	meter.observe(4_000_000, base.Add(time.Second))

	// A count that went backwards means a different transfer is reusing the id.
	meter.observe(100, base.Add(2*time.Second))
	if got := meter.speed(base.Add(2 * time.Second)); got != 0 {
		t.Fatalf("speed after a restart = %v, want 0", got)
	}

	meter.observe(500_100, base.Add(3*time.Second))
	if got := meter.speed(base.Add(3 * time.Second)); got < 450_000 || got > 550_000 {
		t.Fatalf("speed = %v, want about 500 kB/s", got)
	}
}

func TestLiveRatesForgetsFinishedTransfers(t *testing.T) {
	rates := newLiveRates()
	rates.observe("job", 0)
	rates.observe("job", 1000)
	rates.forget("job")

	if got := rates.speed("job"); got != 0 {
		t.Fatalf("speed of a forgotten transfer = %v, want 0", got)
	}
}
