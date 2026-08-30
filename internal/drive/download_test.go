package drive

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dibin/tdrive/internal/database"
)

func waitDownload(t *testing.T, db *database.DB, id string, want database.DownloadStatus) database.DownloadJob {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		job, err := db.DownloadByID(context.Background(), id)
		if err == nil && job.Status == want {
			return job
		}
		time.Sleep(5 * time.Millisecond)
	}
	job, err := db.DownloadByID(context.Background(), id)
	if err != nil {
		t.Fatalf("DownloadByID: %v", err)
	}
	t.Fatalf("download %q status = %q, want %q", id, job.Status, want)
	return database.DownloadJob{}
}

func TestStagedCacheUsesOwnedNamespaceAndKeepsUnrelatedData(t *testing.T) {
	h := newHarness(t, 4096)
	cacheDir := t.TempDir()
	h.cfg.Storage.CacheDir = cacheDir
	h.cfg.Storage.CacheLimit = 1 << 20

	file := h.store(t, "/", "staged.bin", bytes.Repeat([]byte("x"), 1024))
	job, err := h.svc.StartStaged(context.Background(), StageRequest{FileID: file.ID})
	if err != nil {
		t.Fatalf("StartStaged: %v", err)
	}
	ready := waitDownload(t, h.db, job.ID, database.DownloadReady)

	namespace := filepath.Dir(filepath.Dir(ready.CachePath))
	if filepath.Base(namespace) != stagedCacheNamespace {
		t.Fatalf("cache path %q is not below the owned namespace", ready.CachePath)
	}
	unrelated := filepath.Join(cacheDir, "keep-me")
	if err := os.Mkdir(unrelated, 0o750); err != nil {
		t.Fatalf("create unrelated directory: %v", err)
	}
	orphan := filepath.Join(namespace, "orphan")
	if err := os.Mkdir(orphan, 0o750); err != nil {
		t.Fatalf("create orphan directory: %v", err)
	}

	h.svc.SweepCache(context.Background())
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("unrelated cache sibling was removed: %v", err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan directory stat = %v, want not exist", err)
	}
	if _, err := os.Stat(ready.CachePath); err != nil {
		t.Fatalf("ready staged file was removed: %v", err)
	}
}

func TestStagedFileRejectsExpiredCopyImmediately(t *testing.T) {
	h := newHarness(t, 4096)
	cacheDir := t.TempDir()
	h.cfg.Storage.CacheDir = cacheDir
	job := database.DownloadJob{
		ID:        database.NewID(),
		Name:      "expired.bin",
		TotalSize: 4,
		Mode:      database.DownloadStaged,
		Status:    database.DownloadReady,
		ExpiresAt: time.Now().Add(-time.Minute),
	}
	job.CachePath = filepath.Join(cacheDir, "old-root", job.ID, "expired.bin")
	if err := h.db.InsertDownload(context.Background(), job); err != nil {
		t.Fatalf("InsertDownload: %v", err)
	}
	if err := h.db.SetDownloadCache(context.Background(), job.ID, job.CachePath, job.ExpiresAt.UnixMilli()); err != nil {
		t.Fatalf("SetDownloadCache: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(job.CachePath), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(job.CachePath, []byte("old!"), 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := h.svc.StagedFile(context.Background(), job.ID)
	if !errors.Is(err, database.ErrNotFound) {
		t.Fatalf("StagedFile error = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(job.CachePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired cache file stat = %v, want not exist", err)
	}
	fresh, err := h.db.DownloadByID(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("DownloadByID: %v", err)
	}
	if fresh.Status != database.DownloadExpired || fresh.CachePath != "" {
		t.Fatalf("expired job = (%q, %q), want (expired, empty path)", fresh.Status, fresh.CachePath)
	}
}

func TestPurgeCacheCancelsActiveStagedRows(t *testing.T) {
	h := newHarness(t, 4096)
	cacheDir := t.TempDir()
	h.cfg.Storage.CacheDir = cacheDir
	if _, err := ensureStagedCacheRoot(cacheDir); err != nil {
		t.Fatalf("ensure cache root: %v", err)
	}
	job := database.DownloadJob{
		ID:        database.NewID(),
		Name:      "active.bin",
		TotalSize: 100,
		Mode:      database.DownloadStaged,
		Status:    database.DownloadPending,
	}
	if err := h.db.InsertDownload(context.Background(), job); err != nil {
		t.Fatalf("InsertDownload: %v", err)
	}
	jobDir := filepath.Join(cacheDir, stagedCacheNamespace, job.ID)
	if err := os.MkdirAll(jobDir, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, "active.part"), []byte("partial"), 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	freed, err := h.svc.PurgeCache(context.Background())
	if err != nil {
		t.Fatalf("PurgeCache: %v", err)
	}
	if freed != job.TotalSize {
		t.Fatalf("PurgeCache freed %d, want %d", freed, job.TotalSize)
	}
	if _, err := h.db.DownloadByID(context.Background(), job.ID); !errors.Is(err, database.ErrNotFound) {
		t.Fatalf("active job lookup error = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(jobDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("active job directory stat = %v, want not exist", err)
	}
}

func TestDeleteStagedUsesPersistedPathAfterCacheDirChange(t *testing.T) {
	h := newHarness(t, 4096)
	oldDir := t.TempDir()
	newDir := t.TempDir()
	h.cfg.Storage.CacheDir = newDir
	job := database.DownloadJob{
		ID:        database.NewID(),
		Name:      "old-root.bin",
		TotalSize: 4,
		Mode:      database.DownloadStaged,
		Status:    database.DownloadReady,
	}
	job.CachePath = filepath.Join(oldDir, job.ID, "old-root.bin")
	if err := h.db.InsertDownload(context.Background(), job); err != nil {
		t.Fatalf("InsertDownload: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(job.CachePath), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(job.CachePath, []byte("old!"), 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := h.svc.DeleteStaged(context.Background(), job); err != nil {
		t.Fatalf("DeleteStaged: %v", err)
	}
	if _, err := os.Stat(job.CachePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old cache file stat = %v, want not exist", err)
	}
}

func TestPurgeDownloadHistoryRemovesRowsAndStagedBytes(t *testing.T) {
	h := newHarness(t, 4096)
	cacheDir := t.TempDir()
	staged := database.DownloadJob{
		ID:        database.NewID(),
		Name:      "history-staged.bin",
		TotalSize: 4,
		Mode:      database.DownloadStaged,
		Status:    database.DownloadReady,
	}
	staged.CachePath = filepath.Join(cacheDir, staged.ID, staged.Name)
	direct := database.DownloadJob{
		ID:        database.NewID(),
		Name:      "history-direct.bin",
		TotalSize: 4,
		Mode:      database.DownloadDirect,
		Status:    database.DownloadComplete,
	}
	for _, job := range []database.DownloadJob{staged, direct} {
		if err := h.db.InsertDownload(context.Background(), job); err != nil {
			t.Fatalf("InsertDownload: %v", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(staged.CachePath), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(staged.CachePath, []byte("old!"), 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := h.db.SetDownloadCache(context.Background(), staged.ID, staged.CachePath,
		time.Now().Add(-time.Hour).UnixMilli()); err != nil {
		t.Fatalf("SetDownloadCache: %v", err)
	}

	removed, err := h.svc.PurgeDownloadHistory(context.Background(), time.Now().Add(time.Hour).UnixMilli())
	if err != nil {
		t.Fatalf("PurgeDownloadHistory: %v", err)
	}
	if removed != 2 {
		t.Fatalf("PurgeDownloadHistory removed %d, want 2", removed)
	}
	if _, err := os.Stat(staged.CachePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("history cache file stat = %v, want not exist", err)
	}
	for _, id := range []string{staged.ID, direct.ID} {
		if _, err := h.db.DownloadByID(context.Background(), id); !errors.Is(err, database.ErrNotFound) {
			t.Fatalf("history job %q lookup error = %v, want ErrNotFound", id, err)
		}
	}
}
