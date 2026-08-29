package drive

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dibin/tdrive/internal/database"
)

func TestStartLocalUploadsMountedFile(t *testing.T) {
	h := newHarness(t, 1024)
	root := t.TempDir()
	data := randomBytes(2500, 19)
	if err := os.WriteFile(filepath.Join(root, "from-vps.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	h.cfg.Local.Root = root

	job, err := h.svc.StartLocal(context.Background(), LocalRequest{
		SourcePath: "/from-vps.bin",
		DirPath:    "/incoming",
		UserID:     "",
	})
	if err != nil {
		t.Fatalf("StartLocal: %v", err)
	}

	deadline := time.NewTimer(5 * time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		current, err := h.db.JobByID(context.Background(), job.ID)
		if err != nil {
			t.Fatalf("JobByID: %v", err)
		}
		if current.Status == database.JobComplete {
			break
		}
		if current.Status == database.JobFailed {
			t.Fatalf("local transfer failed: %s", current.Error)
		}
		select {
		case <-deadline.C:
			t.Fatalf("local transfer did not finish; status=%s", current.Status)
		case <-ticker.C:
		}
	}

	file, err := h.db.FileByID(context.Background(), job.FileID)
	if err != nil {
		t.Fatalf("FileByID: %v", err)
	}
	got := h.readAll(t, file)
	if string(got) != string(data) {
		t.Fatalf("uploaded local file differs: got %d bytes, want %d", len(got), len(data))
	}
}
