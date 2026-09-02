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

const (
	// CacheDir is an operator-selected location and may be shared with other
	// applications. Keep every directory that tdrive owns below a dedicated
	// child, so a sweep can never mistake somebody else's directory for an
	// orphaned download.
	stagedCacheNamespace  = ".tdrive-download-cache"
	stagedCacheMarker     = ".tdrive-owned"
	stagedCacheMarkerText = "tdrive download cache v1\n"
)

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
	request := struct {
		FileID string `json:"fileId"`
		UserID string `json:"userId"`
	}{FileID: req.FileID, UserID: req.UserID}
	operation, err := s.beforePluginOperation(ctx, "downloads.stage", request, &request)
	if err != nil {
		return database.DownloadJob{}, err
	}
	req = StageRequest{FileID: request.FileID, UserID: request.UserID}
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
	if _, err := ensureStagedCacheRoot(s.cfg.CacheRoot()); err != nil {
		return database.DownloadJob{}, err
	}

	// An existing ready copy is the whole point of a cache.
	if existing, err := s.db.StagedDownloadFor(ctx, file.ID, time.Now().UnixMilli()); err == nil {
		if _, statErr := os.Stat(existing.CachePath); statErr == nil {
			_ = s.db.TouchDownload(ctx, existing.ID)
			s.afterPluginOperation(ctx, operation)
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
		s.afterPluginOperation(ctx, operation)
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

	s.scheduleStagedWorker(context.WithoutCancel(ctx), job, file)
	s.afterPluginOperation(ctx, operation)
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
		s.scheduleStagedWorker(context.WithoutCancel(ctx), job, file)
	}
}

// scheduleStagedWorker makes ResumeStaged safe to call more than once while a
// connection is becoming ready. The database row remains pending until the
// goroutine gets a task slot, so a database-only check cannot prevent duplicate
// workers during that window.
func (s *Service) scheduleStagedWorker(ctx context.Context, job database.DownloadJob, file database.File) {
	s.stageRunMu.Lock()
	if _, exists := s.stageRuns[job.ID]; exists {
		s.stageRunMu.Unlock()
		return
	}
	s.stageRuns[job.ID] = struct{}{}
	s.stageRunMu.Unlock()

	go func() {
		defer func() {
			s.stageRunMu.Lock()
			delete(s.stageRuns, job.ID)
			s.stageRunMu.Unlock()
		}()
		s.runStaged(ctx, job, file)
	}()
}

// StagedFile returns the on-disk path of a ready staged copy, refreshing its
// LRU position. It reports ErrNotFound when the copy is not usable, so callers
// fall through to streaming from Telegram.
func (s *Service) StagedFile(ctx context.Context, jobID string) (database.DownloadJob, error) {
	request := struct {
		JobID string `json:"jobId"`
	}{JobID: jobID}
	operation, err := s.beforePluginOperation(ctx, "downloads.stagedFile", request, &request)
	if err != nil {
		return database.DownloadJob{}, err
	}
	job, err := s.db.DownloadByID(ctx, request.JobID)
	if err != nil {
		return database.DownloadJob{}, err
	}
	if job.CachePath == "" || (job.Status != database.DownloadReady && job.Status != database.DownloadComplete) {
		return database.DownloadJob{}, fmt.Errorf("%w: staged copy is not ready", database.ErrNotFound)
	}
	if !job.ExpiresAt.IsZero() && !time.Now().Before(job.ExpiresAt) {
		// Do this synchronously on the read path as well as in the hourly
		// sweep. A client must not keep a staged capability alive merely by
		// polling or downloading it before maintenance gets a turn.
		s.removeStagedFile(job)
		_ = s.db.SetDownloadCache(ctx, job.ID, "", 0)
		_ = s.db.SetDownloadStatus(ctx, job.ID, database.DownloadExpired, "the staged copy expired")
		return database.DownloadJob{}, fmt.Errorf("%w: staged copy expired", database.ErrNotFound)
	}
	if _, err := os.Stat(job.CachePath); err != nil {
		s.removeStagedFile(job)
		_ = s.db.SetDownloadCache(ctx, job.ID, "", 0)
		_ = s.db.SetDownloadStatus(ctx, job.ID, database.DownloadExpired, "the staged copy is no longer on disk")
		return database.DownloadJob{}, fmt.Errorf("%w: staged copy is gone", database.ErrNotFound)
	}
	_ = s.db.TouchDownload(ctx, job.ID)
	s.afterPluginOperation(ctx, operation)
	return job, nil
}

// CancelStaged stops a staged download and removes whatever it wrote.
func (s *Service) CancelStaged(ctx context.Context, jobID string) error {
	request := struct {
		JobID string `json:"jobId"`
	}{JobID: jobID}
	operation, err := s.beforePluginOperation(ctx, "downloads.cancel", request, &request)
	if err != nil {
		return err
	}
	jobID = request.JobID
	job, err := s.db.DownloadByID(ctx, jobID)
	if err != nil {
		return err
	}
	if job.Mode != database.DownloadStaged {
		return fmt.Errorf("%w: only staged downloads can be cancelled here", database.ErrNotFound)
	}
	s.stageCancelMu.Lock()
	if cancel, ok := s.stageCancels[jobID]; ok {
		cancel()
	}
	s.stageCancelMu.Unlock()

	s.removeStagedFile(job)
	if _, err := s.db.SetDownloadStatusIf(ctx, jobID, database.DownloadCancelled, "cancelled",
		database.DownloadPending, database.DownloadRunning, database.DownloadReady); err != nil {
		return err
	}
	_ = s.db.SetDownloadCache(ctx, jobID, "", 0)
	s.afterPluginOperation(ctx, operation)
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
		if ctx.Err() != nil {
			return
		}
		cleanupCtx := context.WithoutCancel(ctx)
		// A failed worker has already removed its bytes. Clear the persisted
		// path too, otherwise conservative cache accounting would reserve the
		// failed file until the next maintenance sweep.
		_ = s.db.SetDownloadCache(cleanupCtx, job.ID, "", 0)
		if _, setErr := s.db.SetDownloadStatusIf(cleanupCtx, job.ID, database.DownloadFailed, err.Error(),
			database.DownloadPending, database.DownloadRunning); setErr != nil {
			s.log.Warn("could not record a failed staged download", zap.Error(setErr))
		} else {
			s.notifyDownload(job, 0, file.Size, err)
		}
	}

	// A staged download is one whole-file transfer, so it takes a task slot
	// exactly like a WebDAV read or a browser download does. Nothing about
	// running in the background exempts it from the configured limit.
	lease, err := s.leaseDownload(ctx, "", file.Size)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		fail(err)
		return
	}
	account := lease.account
	var downloaded int64
	defer func() {
		lease.recordQuotaBytes(downloaded)
		lease.release()
	}()

	ok, err := s.db.SetDownloadStatusIf(ctx, job.ID, database.DownloadRunning, "",
		database.DownloadPending, database.DownloadRunning)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		fail(err)
		return
	}
	if !ok {
		// The job was cancelled or deleted while it waited for a download
		// slot. Do not turn that terminal state back into running.
		return
	}
	s.notifyDownload(job, 0, file.Size, nil)

	target, dir, persisted := persistedStagedTarget(job)
	if !persisted {
		cacheRoot, err := ensureStagedCacheRoot(s.cfg.CacheRoot())
		if err != nil {
			fail(err)
			return
		}
		dir = filepath.Join(cacheRoot, job.ID)
		target = filepath.Join(dir, safeCacheName(file.Name))
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		fail(fmt.Errorf("create cache directory: %w", err))
		return
	}
	job.CachePath = target
	// Persist the destination before bytes start moving. Apart from making a
	// restart able to find a partial copy, this lets cancellation and eviction
	// remove the correct old-root file if CacheDir changes while the job runs.
	if err := s.db.SetDownloadCache(ctx, job.ID, target, 0); err != nil {
		s.removeStagedFile(job)
		if ctx.Err() != nil {
			return
		}
		fail(err)
		return
	}

	// The copy goes to a temporary name and is renamed on success, so a
	// half-written file can never be mistaken for a complete one by a request
	// that arrives mid-transfer.
	// A crash can leave the final name published before the status update; a
	// resumed worker owns this job, so discard that stale candidate first.
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		s.removeStagedFile(job)
		fail(fmt.Errorf("replace stale cache file: %w", err))
		return
	}
	partial := target + ".part"
	out, err := os.Create(partial)
	if err != nil {
		s.removeStagedFile(job)
		fail(fmt.Errorf("create cache file: %w", err))
		return
	}

	cleanupPartial := func() {
		out.Close()
		s.removeStagedFile(job)
	}

	reader, err := s.OpenFile(ctx, file, account)
	if err != nil {
		cleanupPartial()
		fail(err)
		return
	}
	defer reader.Close()

	written, err := s.copyStaged(ctx, out, reader, job, file.Size)
	downloaded = written
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
	if ctx.Err() != nil {
		cleanupPartial()
		return
	}
	if err := s.db.SetDownloadCache(ctx, job.ID, target, expires); err != nil {
		cleanupPartial()
		if ctx.Err() != nil {
			return
		}
		fail(err)
		return
	}
	ok, err = s.db.SetDownloadStatusIf(ctx, job.ID, database.DownloadReady, "", database.DownloadRunning)
	if err != nil {
		cleanupPartial()
		if ctx.Err() != nil {
			return
		}
		fail(err)
		return
	}
	if !ok {
		// Cancellation or history deletion won the race after the copy was
		// published. Leave no bytes behind and do not resurrect the job.
		cleanupPartial()
		return
	}
	if err := ctx.Err(); err != nil {
		// A cancellation immediately after the conditional status update is
		// still safe: CancelStaged removes the published path and marks the
		// row cancelled.
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
	s.stageMu.Lock()
	defer s.stageMu.Unlock()

	settings := s.cfg.RuntimeSettings()
	now := time.Now().UnixMilli()

	candidates, err := s.db.EvictableDownloads(ctx)
	if err != nil {
		s.log.Warn("could not enumerate the download cache", zap.Error(err))
		return
	}

	// CacheUsage includes pending and running jobs as reservations. The
	// evictable query below intentionally cannot return those jobs, but their
	// space still has to be present when deciding whether ready copies push the
	// cache over a newly lowered limit.
	used, _, err := s.db.CacheUsage(ctx)
	if err != nil {
		s.log.Warn("could not total the download cache", zap.Error(err))
		return
	}
	var live []database.DownloadJob
	for _, job := range candidates {
		expired := !job.ExpiresAt.IsZero() && job.ExpiresAt.UnixMilli() <= now
		terminal := job.Status == database.DownloadFailed ||
			job.Status == database.DownloadCancelled ||
			job.Status == database.DownloadExpired
		if expired || terminal {
			used -= job.TotalSize
			if used < 0 {
				used = 0
			}
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
	root, ok := existingStagedCacheRoot(s.cfg.CacheRoot())
	if !ok {
		// A configured CacheDir may be shared with unrelated applications. Do
		// not create or sweep anything unless the tdrive-owned namespace and
		// its marker are present.
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if !os.IsNotExist(err) {
			s.log.Debug("could not read the download cache directory", zap.Error(err))
		}
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == stagedCacheMarker {
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
	if job.ID == "" || job.Mode != database.DownloadStaged {
		return
	}
	// Ready jobs persist the exact path they used. That matters after an
	// administrator moves CacheDir: deriving the directory from the current
	// setting would leave the old copy behind. Remove only the known target and
	// its .part sibling, then remove the now-empty job directory; never recurse
	// through an arbitrary persisted path.
	if target, dir, ok := persistedStagedTarget(job); ok {
		for _, path := range []string{target, target + ".part"} {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				s.log.Warn("could not remove a staged download",
					zap.String("path", path), zap.Error(err))
			}
		}
		if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
			// An unexpected file is left in the per-job directory rather
			// than recursively deleting it. The next sweep can inspect it.
			if entries, readErr := os.ReadDir(dir); readErr != nil || len(entries) == 0 {
				s.log.Warn("could not remove a staged download directory",
					zap.String("path", dir), zap.Error(err))
			}
		}
		return
	}

	// Older in-flight rows may not have a persisted path yet. The fallback is
	// safe because it is confined to the marker-guarded namespace and a single
	// job id below it.
	root, ok := existingStagedCacheRoot(s.cfg.CacheRoot())
	if !ok || filepath.Base(job.ID) != job.ID || job.ID == "." || job.ID == ".." {
		return
	}
	dir := filepath.Join(root, job.ID)
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		s.log.Warn("could not remove a staged download",
			zap.String("path", dir), zap.Error(err))
	}
}

// persistedStagedTarget accepts only the path shape tdrive writes: a file
// directly below a directory named after the job. Keeping this check small
// lets resumed jobs continue in an old CacheDir without trusting an arbitrary
// database string as a directory to remove recursively.
func persistedStagedTarget(job database.DownloadJob) (target, dir string, ok bool) {
	if job.CachePath == "" || filepath.Base(job.ID) != job.ID || job.ID == "." || job.ID == ".." {
		return "", "", false
	}
	target = filepath.Clean(job.CachePath)
	dir = filepath.Dir(target)
	if filepath.Base(dir) != job.ID || filepath.Base(target) == "." || filepath.Base(target) == string(filepath.Separator) {
		return "", "", false
	}
	return target, dir, true
}

// ensureStagedCacheRoot creates and verifies the namespace tdrive owns below
// the operator-selected cache directory. The marker makes a maintenance pass
// fail closed if an existing directory with the same name was not created by
// this application.
func ensureStagedCacheRoot(parent string) (string, error) {
	if parent == "" {
		return "", errors.New("download cache directory is empty")
	}
	root := filepath.Join(parent, stagedCacheNamespace)
	if err := os.MkdirAll(root, 0o750); err != nil {
		return "", fmt.Errorf("create download cache namespace: %w", err)
	}
	marker := filepath.Join(root, stagedCacheMarker)
	info, err := os.Lstat(marker)
	if errors.Is(err, os.ErrNotExist) {
		entries, readErr := os.ReadDir(root)
		if readErr != nil {
			return "", fmt.Errorf("inspect unclaimed download cache namespace: %w", readErr)
		}
		if len(entries) != 0 {
			return "", errors.New("download cache namespace is not owned by tdrive")
		}
		file, createErr := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr == nil {
			_, writeErr := io.WriteString(file, stagedCacheMarkerText)
			closeErr := file.Close()
			if writeErr != nil || closeErr != nil {
				_ = os.Remove(marker)
				if writeErr != nil {
					return "", fmt.Errorf("write download cache marker: %w", writeErr)
				}
				return "", fmt.Errorf("close download cache marker: %w", closeErr)
			}
			return root, nil
		}
		if !errors.Is(createErr, os.ErrExist) {
			return "", fmt.Errorf("create download cache marker: %w", createErr)
		}
		info, err = os.Lstat(marker)
	}
	if err != nil {
		return "", fmt.Errorf("inspect download cache marker: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("download cache marker is not a regular file")
	}
	contents, err := os.ReadFile(marker)
	if err != nil {
		return "", fmt.Errorf("read download cache marker: %w", err)
	}
	if string(contents) != stagedCacheMarkerText {
		return "", errors.New("download cache namespace is not owned by tdrive")
	}
	return root, nil
}

func existingStagedCacheRoot(parent string) (string, bool) {
	if parent == "" {
		return "", false
	}
	root := filepath.Join(parent, stagedCacheNamespace)
	marker := filepath.Join(root, stagedCacheMarker)
	info, err := os.Lstat(marker)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	contents, err := os.ReadFile(marker)
	if err != nil || string(contents) != stagedCacheMarkerText {
		return "", false
	}
	return root, true
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
	s.stageMu.Lock()
	defer s.stageMu.Unlock()

	// Active jobs have no evictable row yet, but their workers still own a
	// partial file and a cache reservation. Cancel them before clearing the
	// completed rows so a purge cannot be undone by a worker finishing later.
	active, err := s.db.ResumableDownloads(ctx)
	if err != nil {
		return 0, err
	}
	var freed int64
	for _, job := range active {
		if err := s.CancelStaged(ctx, job.ID); err != nil {
			return freed, err
		}
		if err := s.db.DeleteDownload(ctx, job.ID); err != nil && !errors.Is(err, database.ErrNotFound) {
			return freed, err
		}
		freed += job.TotalSize
	}

	jobs, err := s.db.EvictableDownloads(ctx)
	if err != nil {
		return freed, err
	}
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

// PurgeDownloadHistory removes old terminal download records and any staged
// bytes they still reference. Direct and segmented downloads have no files to
// unlink, while ready staged rows must be cleaned using their persisted paths.
func (s *Service) PurgeDownloadHistory(ctx context.Context, olderThanMS int64) (int64, error) {
	jobs, err := s.db.FinishedDownloadsBefore(ctx, olderThanMS)
	if err != nil {
		return 0, err
	}
	if len(jobs) == 0 {
		return 0, nil
	}

	ids := make([]string, 0, len(jobs))
	for _, job := range jobs {
		s.removeStagedFile(job)
		ids = append(ids, job.ID)
	}
	return s.db.DeleteDownloads(ctx, ids)
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
