package drive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"

	"github.com/dibin/tdrive/internal/database"
)

// Staged downloads.
//
// A file stored as one Telegram object streams fine straight through the
// server: a parallel downloader's range requests all land in the same
// document, and internal/reader turns each into a handful of concurrent chunk
// fetches. A file split across a dozen objects is a different animal. Every
// range that straddles a segment boundary makes the reader open a new source,
// eight parallel connections mean eight independent walks through those
// sources, and a single expired file reference anywhere in the set fails the
// range that touched it. It works, but it is the least robust thing this
// program does, and it is exactly the case where the file is largest and a
// failure costs the most.
//
// Staging sidesteps all of it: the server reads the file once, sequentially,
// with the retry and reference-refresh machinery it already has, and writes it
// to local disk. What the client then downloads is an ordinary file that
// supports ranges natively, so parallelism and resume become the operating
// system's problem rather than Telegram's.
//
// The cost is disk, which is why the cache is bounded, swept and evictable.

// ErrStagingDisabled is returned when no disk has been set aside for staging.
var ErrStagingDisabled = errors.New("drive: staged downloads are disabled because no cache space is configured")

// ErrCacheFull is returned when a staged download cannot fit even after
// evicting everything eligible.
var ErrCacheFull = errors.New("drive: the download cache does not have room for this file")

// DownloadProgress is notified as a staged download advances, so the browser
// can watch it the same way it watches an upload.
type DownloadProgress func(job database.DownloadJob, downloaded, total int64, err error)

// StageRequest asks for a file to be assembled on the server's disk.
type StageRequest struct {
	FileID string
	UserID string
}

// StartStaged begins (or joins) a staged download.
//
// Two people asking for the same file get one transfer: the second joins the
// first rather than pulling the same bytes out of Telegram twice, which is
// both faster for them and cheaper on the rate limit.
func (s *Service) StartStaged(ctx context.Context, req StageRequest) (database.DownloadJob, error) {
	file, err := s.db.FileByID(ctx, req.FileID)
	if err != nil {
		return database.DownloadJob{}, err
	}
	if file.Status == database.StatusPending {
		return database.DownloadJob{}, fmt.Errorf("%q is still uploading", file.Name)
	}

	settings := s.cfg.RuntimeSettings()
	if settings.CacheLimit <= 0 {
		return database.DownloadJob{}, ErrStagingDisabled
	}
	if file.Size > settings.CacheLimit {
		return database.DownloadJob{}, fmt.Errorf(
			"%w: %q is %d bytes and the cache limit is %d",
			ErrCacheFull, file.Name, file.Size, settings.CacheLimit)
	}

	// An existing ready copy is the whole point of a cache.
	if existing, err := s.db.StagedDownloadFor(ctx, file.ID, time.Now().UnixMilli()); err == nil {
		if _, statErr := os.Stat(existing.CachePath); statErr == nil {
			_ = s.db.TouchDownload(ctx, existing.ID)
			return existing, nil
		}
		// The row outlived its file, which means something outside tdrive
		// removed it. Drop the row and stage again.
		_ = s.db.DeleteDownload(ctx, existing.ID)
	} else if !errors.Is(err, database.ErrNotFound) {
		return database.DownloadJob{}, err
	}

	s.stageMu.Lock()
	defer s.stageMu.Unlock()

	if active, err := s.db.ActiveStagedFor(ctx, file.ID); err == nil {
		return active, nil
	} else if !errors.Is(err, database.ErrNotFound) {
		return database.DownloadJob{}, err
	}

	if err := s.makeRoomFor(ctx, file.Size); err != nil {
		return database.DownloadJob{}, err
	}

	job := database.DownloadJob{
		ID:        database.NewID(),
		UserID:    req.UserID,
		FileID:    file.ID,
		Name:      file.Name,
		TotalSize: file.Size,
		Mode:      database.DownloadStaged,
		Status:    database.DownloadPending,
	}
	if err := s.db.InsertDownload(ctx, job); err != nil {
		return database.DownloadJob{}, err
	}

	go s.runStaged(context.WithoutCancel(ctx), job, file)
	return job, nil
}

// ResumeStaged picks up staged downloads interrupted by a restart. Unlike an
// upload, nothing has to wait for a client to reconnect: the server holds
// everything it needs to finish the job on its own.
func (s *Service) ResumeStaged(ctx context.Context) {
	jobs, err := s.db.ResumableDownloads(ctx)
	if err != nil {
		s.log.Warn("could not list staged downloads to resume", zap.Error(err))
		return
	}
	for _, job := range jobs {
		file, err := s.db.FileByID(ctx, job.FileID)
		if err != nil {
			_ = s.db.SetDownloadStatus(ctx, job.ID, database.DownloadFailed,
				"the file this download referred to is gone")
			continue
		}
		s.log.Info("resuming a staged download",
			zap.String("job", job.ID), zap.String("name", job.Name))
		go s.runStaged(context.WithoutCancel(ctx), job, file)
	}
}

// StagedFile returns the on-disk path of a ready staged copy, refreshing its
// LRU position. It reports ErrNotFound when the copy is not usable, so callers
// fall through to streaming from Telegram.
func (s *Service) StagedFile(ctx context.Context, jobID string) (database.DownloadJob, error) {
	job, err := s.db.DownloadByID(ctx, jobID)
	if err != nil {
		return database.DownloadJob{}, err
	}
	if job.CachePath == "" || (job.Status != database.DownloadReady && job.Status != database.DownloadComplete) {
		return database.DownloadJob{}, fmt.Errorf("%w: staged copy is not ready", database.ErrNotFound)
	}
	if _, err := os.Stat(job.CachePath); err != nil {
		_ = s.db.SetDownloadStatus(ctx, job.ID, database.DownloadExpired, "the staged copy is no longer on disk")
		return database.DownloadJob{}, fmt.Errorf("%w: staged copy is gone", database.ErrNotFound)
	}
	_ = s.db.TouchDownload(ctx, job.ID)
	return job, nil
}

// CancelStaged stops a staged download and removes whatever it wrote.
func (s *Service) CancelStaged(ctx context.Context, jobID string) error {
	job, err := s.db.DownloadByID(ctx, jobID)
	if err != nil {
		return err
	}
	s.stageCancelMu.Lock()
	if cancel, ok := s.stageCancels[jobID]; ok {
		cancel()
	}
	s.stageCancelMu.Unlock()

	s.removeStagedFile(job)
	if job.Status == database.DownloadPending || job.Status == database.DownloadRunning {
		return s.db.SetDownloadStatus(ctx, jobID, database.DownloadCancelled, "cancelled")
	}
	return nil
}

// DeleteStaged removes a download record and its cached bytes.
func (s *Service) DeleteStaged(ctx context.Context, job database.DownloadJob) error {
	s.stageCancelMu.Lock()
	if cancel, ok := s.stageCancels[job.ID]; ok {
		cancel()
	}
	s.stageCancelMu.Unlock()

	s.removeStagedFile(job)
	return s.db.DeleteDownload(ctx, job.ID)
}

func (s *Service) runStaged(ctx context.Context, job database.DownloadJob, file database.File) {
	ctx, cancel := context.WithCancel(ctx)
	s.stageCancelMu.Lock()
	s.stageCancels[job.ID] = cancel
	s.stageCancelMu.Unlock()
	defer func() {
		cancel()
		s.stageCancelMu.Lock()
		delete(s.stageCancels, job.ID)
		s.stageCancelMu.Unlock()
	}()

	fail := func(err error) {
		s.log.Warn("a staged download failed",
			zap.String("job", job.ID), zap.String("name", job.Name), zap.Error(err))
		if setErr := s.db.SetDownloadStatus(ctx, job.ID, database.DownloadFailed, err.Error()); setErr != nil {
			s.log.Warn("could not record a failed staged download", zap.Error(setErr))
		}
		s.notifyDownload(job, 0, file.Size, err)
	}

	// A staged download is one whole-file transfer, so it takes a task slot
	// exactly like a WebDAV read or a browser download does. Nothing about
	// running in the background exempts it from the configured limit.
	release, err := s.AcquireDownloadTask(ctx)
	if err != nil {
		fail(err)
		return
	}
	defer release()

	if err := s.db.SetDownloadStatus(ctx, job.ID, database.DownloadRunning, ""); err != nil {
		fail(err)
		return
	}
	s.notifyDownload(job, 0, file.Size, nil)

	dir := filepath.Join(s.cfg.CacheRoot(), job.ID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		fail(fmt.Errorf("create cache directory: %w", err))
		return
	}
	target := filepath.Join(dir, safeCacheName(file.Name))

	// The copy goes to a temporary name and is renamed on success, so a
	// half-written file can never be mistaken for a complete one by a request
	// that arrives mid-transfer.
	partial := target + ".part"
	out, err := os.Create(partial)
	if err != nil {
		fail(fmt.Errorf("create cache file: %w", err))
		return
	}

	cleanupPartial := func() {
		out.Close()
		if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
			s.log.Warn("could not clean up a failed staged download",
				zap.String("path", dir), zap.Error(err))
		}
	}

	reader, err := s.OpenFile(ctx, file)
	if err != nil {
		cleanupPartial()
		fail(err)
		return
	}
	defer reader.Close()

	written, err := s.copyStaged(ctx, out, reader, job, file.Size)
	if err != nil {
		cleanupPartial()
		if ctx.Err() != nil {
			// A cancelled job already has its status set by CancelStaged.
			return
		}
		fail(err)
		return
	}
	if err := out.Close(); err != nil {
		cleanupPartial()
		fail(fmt.Errorf("finish cache file: %w", err))
		return
	}
	if written != file.Size {
		cleanupPartial()
		fail(fmt.Errorf("staged %d of %d bytes", written, file.Size))
		return
	}
	if err := os.Rename(partial, target); err != nil {
		cleanupPartial()
		fail(fmt.Errorf("publish cache file: %w", err))
		return
	}

	expires := time.Now().Add(s.cfg.RuntimeSettings().CacheTTL).UnixMilli()
	if err := s.db.SetDownloadCache(ctx, job.ID, target, expires); err != nil {
		fail(err)
		return
	}
	if err := s.db.SetDownloadStatus(ctx, job.ID, database.DownloadReady, ""); err != nil {
		fail(err)
		return
	}

	ready := job
	ready.Status = database.DownloadReady
	ready.CachePath = target
	s.notifyDownload(ready, file.Size, file.Size, nil)
	s.log.Info("staged a download",
		zap.String("job", job.ID), zap.String("name", job.Name), zap.Int64("bytes", written))
}

// copyStaged is io.Copy with progress reporting and cancellation, throttled so
// a fast transfer does not write a database row per megabyte.
func (s *Service) copyStaged(
	ctx context.Context,
	dst io.Writer,
	src io.Reader,
	job database.DownloadJob,
	total int64,
) (int64, error) {
	const (
		bufSize          = 1 << 20
		progressInterval = 500 * time.Millisecond
	)

	buf := make([]byte, bufSize)
	var written int64
	lastReport := time.Now()

	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}

		n, readErr := src.Read(buf)
		if n > 0 {
			if _, err := dst.Write(buf[:n]); err != nil {
				return written, fmt.Errorf("write cache file: %w", err)
			}
			written += int64(n)

			if time.Since(lastReport) >= progressInterval {
				lastReport = time.Now()
				if err := s.db.SetDownloadProgress(ctx, job.ID, written); err != nil {
					s.log.Debug("could not record staged download progress", zap.Error(err))
				}
				s.notifyDownload(job, written, total, nil)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				_ = s.db.SetDownloadProgress(ctx, job.ID, written)
				return written, nil
			}
			return written, readErr
		}
	}
}

func (s *Service) notifyDownload(job database.DownloadJob, downloaded, total int64, err error) {
	if s.OnDownloadProgress != nil {
		s.OnDownloadProgress(job, downloaded, total, err)
	}
}

// makeRoomFor evicts least-recently-used staged copies until the incoming file
// fits under the configured limit. Callers hold stageMu, so two downloads
// cannot both decide there is room for the same free space.
func (s *Service) makeRoomFor(ctx context.Context, size int64) error {
	limit := s.cfg.RuntimeSettings().CacheLimit
	if limit <= 0 {
		return ErrStagingDisabled
	}

	used, _, err := s.db.CacheUsage(ctx)
	if err != nil {
		return err
	}
	if used+size <= limit {
		return nil
	}

	candidates, err := s.db.EvictableDownloads(ctx)
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		if used+size <= limit {
			break
		}
		s.removeStagedFile(candidate)
		if err := s.db.DeleteDownload(ctx, candidate.ID); err != nil {
			s.log.Warn("could not drop an evicted download record",
				zap.String("job", candidate.ID), zap.Error(err))
			continue
		}
		used -= candidate.TotalSize
		if used < 0 {
			used = 0
		}
	}

	if used+size > limit {
		return fmt.Errorf("%w: needs %d bytes, %d of %d in use",
			ErrCacheFull, size, used, limit)
	}
	return nil
}

// SweepCache drops expired staged copies and brings the cache back under its
// limit. It runs on the maintenance ticker alongside the other sweeps.
func (s *Service) SweepCache(ctx context.Context) {
	settings := s.cfg.RuntimeSettings()
	now := time.Now().UnixMilli()

	candidates, err := s.db.EvictableDownloads(ctx)
	if err != nil {
		s.log.Warn("could not enumerate the download cache", zap.Error(err))
		return
	}

	var used int64
	var live []database.DownloadJob
	for _, job := range candidates {
		expired := !job.ExpiresAt.IsZero() && job.ExpiresAt.UnixMilli() < now
		terminal := job.Status == database.DownloadFailed ||
			job.Status == database.DownloadCancelled ||
			job.Status == database.DownloadExpired
		if expired || terminal {
			s.removeStagedFile(job)
			if job.CachePath != "" {
				if err := s.db.SetDownloadCache(ctx, job.ID, "", 0); err != nil {
					s.log.Warn("could not clear an expired cache path", zap.Error(err))
				}
			}
			if expired && job.Status != database.DownloadExpired {
				_ = s.db.SetDownloadStatus(ctx, job.ID, database.DownloadExpired, "the staged copy expired")
			}
			continue
		}
		used += job.TotalSize
		live = append(live, job)
	}

	// EvictableDownloads is ordered least-recently-used first, so trimming
	// from the front is the eviction order.
	for _, job := range live {
		if settings.CacheLimit <= 0 || used <= settings.CacheLimit {
			break
		}
		s.removeStagedFile(job)
		if err := s.db.DeleteDownload(ctx, job.ID); err != nil {
			s.log.Warn("could not drop an evicted download record", zap.Error(err))
			continue
		}
		used -= job.TotalSize
	}

	s.removeOrphanCacheDirs(ctx)
}

// removeOrphanCacheDirs deletes cache directories with no matching row, which
// is what a crash between creating a directory and committing its job leaves
// behind.
func (s *Service) removeOrphanCacheDirs(ctx context.Context) {
	root := s.cfg.CacheRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		if !os.IsNotExist(err) {
			s.log.Debug("could not read the download cache directory", zap.Error(err))
		}
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := s.db.DownloadByID(ctx, entry.Name()); err == nil {
			continue
		} else if !errors.Is(err, database.ErrNotFound) {
			continue
		}
		path := filepath.Join(root, entry.Name())
		if err := os.RemoveAll(path); err != nil {
			s.log.Warn("could not remove an orphaned cache directory",
				zap.String("path", path), zap.Error(err))
		}
	}
}

func (s *Service) removeStagedFile(job database.DownloadJob) {
	if job.ID == "" {
		return
	}
	// The whole per-job directory goes, not just the file, so a leftover
	// .part from an interrupted run cannot survive its own job.
	dir := filepath.Join(s.cfg.CacheRoot(), job.ID)
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		s.log.Warn("could not remove a staged download",
			zap.String("path", dir), zap.Error(err))
	}
}

// CacheStatus describes the download cache for the settings page.
type CacheStatus struct {
	Dir   string `json:"dir"`
	Used  int64  `json:"used"`
	Limit int64  `json:"limit"`
	Files int64  `json:"files"`
}

func (s *Service) CacheStatus(ctx context.Context) (CacheStatus, error) {
	used, count, err := s.db.CacheUsage(ctx)
	if err != nil {
		return CacheStatus{}, err
	}
	return CacheStatus{
		Dir:   s.cfg.CacheRoot(),
		Used:  used,
		Limit: s.cfg.RuntimeSettings().CacheLimit,
		Files: count,
	}, nil
}

// PurgeCache removes every staged copy, which is the "clear the cache" button.
func (s *Service) PurgeCache(ctx context.Context) (int64, error) {
	jobs, err := s.db.EvictableDownloads(ctx)
	if err != nil {
		return 0, err
	}
	var freed int64
	for _, job := range jobs {
		s.removeStagedFile(job)
		if err := s.db.DeleteDownload(ctx, job.ID); err != nil {
			s.log.Warn("could not drop a purged download record", zap.Error(err))
			continue
		}
		freed += job.TotalSize
	}
	s.removeOrphanCacheDirs(ctx)
	return freed, nil
}

// safeCacheName keeps a stored name from escaping its cache directory or
// upsetting the local filesystem. The name is cosmetic here — the directory is
// the job id — so replacing awkward characters costs nothing.
func safeCacheName(name string) string {
	cleaned := filepath.Base(filepath.Clean("/" + name))
	if cleaned == "." || cleaned == string(filepath.Separator) || cleaned == "" {
		return "download"
	}
	return cleaned
}
