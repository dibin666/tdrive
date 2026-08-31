package drive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/tagcodec"
)

// The upload path follows the reference implementation step for step: a
// progress-tracking reader feeds a streaming uploader that issues configured
// saveBigFilePart calls, the resulting InputFile is wrapped in a document
// message carrying the caption, and the send is retried with backoff on
// FLOOD_WAIT (handled here by the floodwait middleware rather than by hand).
//
// Two things are added. Parts within a segment go out concurrently instead of
// one at a time, and a file too large for one Telegram object is split across
// several — which the reference refuses to do at all.

// UploadRequest describes a file about to be stored.
type UploadRequest struct {
	// DirPath is the destination directory; missing ancestors are created.
	DirPath string
	Name    string
	// Size is the exact byte count. It must be known: Telegram's upload
	// protocol commits to a part count in the first request, so a stream of
	// unknown length has to be measured first. UploadUnsized does that.
	Size   int64
	MIME   string
	UserID string
	Source string
	// SourceURL records a server-side fetch so the job can resume after a
	// restart. Browser uploads leave it empty, because only the browser has
	// the bytes.
	SourceURL string
	// is what a WebDAV PUT expects.
	Overwrite bool
}

// Progress reports the running byte count of a file's upload.
type Progress func(uploaded, total int64)

// Begin reserves the name, creates the pending file row and the resumable job.
//
// The file row exists before any bytes move, so a crash leaves something the
// transfer panel can show and resume instead of orphaned Telegram messages that
// nothing points at.
func (s *Service) Begin(ctx context.Context, req UploadRequest) (database.UploadJob, database.File, error) {
	operation, err := s.beforePluginOperation(ctx, "files.beginUpload", req, &req)
	if err != nil {
		return database.UploadJob{}, database.File{}, err
	}
	if err := ValidateName(req.Name); err != nil {
		return database.UploadJob{}, database.File{}, err
	}
	if req.Size < 0 {
		return database.UploadJob{}, database.File{}, errors.New("upload size must be known before starting")
	}

	channel, err := s.storageChannel(ctx)
	if err != nil {
		return database.UploadJob{}, database.File{}, err
	}

	// If an overwrite replaces a file owned by this account, only the size
	// delta consumes quota. Inspect an already-existing directory without
	// creating it first, so a quota rejection still has no filesystem or
	// Telegram side effects.
	quotaBytes := req.Size
	if req.Overwrite {
		if existingDir, dirErr := s.ResolveDir(ctx, req.DirPath); dirErr == nil {
			if existing, fileErr := s.db.FileInDir(ctx, existingDir.ID, req.Name); fileErr == nil &&
				existing.OwnerID == req.UserID {
				quotaBytes -= existing.Size
			}
		} else if !errors.Is(dirErr, ErrNotFound) {
			return database.UploadJob{}, database.File{}, dirErr
		}
	}

	// The quota is checked before anything is created, so a rejected upload
	// leaves no pending file row and no Telegram message behind. Replacing an
	// owned file uses the net size change calculated above.
	if err := s.CheckQuota(ctx, req.UserID, quotaBytes); err != nil {
		return database.UploadJob{}, database.File{}, err
	}

	dir, err := s.Mkdir(ctx, req.DirPath)
	if err != nil {
		return database.UploadJob{}, database.File{}, err
	}
	if err := s.clearName(ctx, dir.ID, req.Name, req.Overwrite); err != nil {
		return database.UploadJob{}, database.File{}, err
	}

	mimeType := req.MIME
	if mimeType == "" {
		mimeType = GuessMIME(req.Name)
	}
	settings := s.cfg.RuntimeSettings()
	segCount := s.cfg.SegmentCount(req.Size)

	file := database.File{
		ID:           database.NewID(),
		DirID:        dir.ID,
		Name:         req.Name,
		Size:         req.Size,
		MIME:         mimeType,
		SegmentSize:  settings.SegmentSize,
		SegmentCount: segCount,
		Status:       database.StatusPending,
		ChannelID:    channel.ID,
		OwnerID:      req.UserID,
	}
	if err := s.db.InsertFile(ctx, file); err != nil {
		if errors.Is(err, database.ErrConflict) {
			return database.UploadJob{}, database.File{}, fmt.Errorf("%w: %s", ErrExists, req.Name)
		}
		return database.UploadJob{}, database.File{}, err
	}

	job := database.UploadJob{
		ID:           database.NewID(),
		UserID:       req.UserID,
		FileID:       file.ID,
		DirID:        dir.ID,
		Name:         req.Name,
		TotalSize:    req.Size,
		SegmentSize:  file.SegmentSize,
		SegmentCount: segCount,
		DoneMask:     database.NewMask(segCount),
		Status:       database.JobPending,
		Source:       req.Source,
		SourceURL:    req.SourceURL,
	}
	if err := s.db.InsertJob(ctx, job); err != nil {
		_ = s.db.DeleteFile(ctx, file.ID)
		return database.UploadJob{}, database.File{}, err
	}
	s.afterPluginOperation(ctx, operation)
	return job, file, nil
}

// clearName makes room for a new file, either refusing or removing what is
// already there.
func (s *Service) clearName(ctx context.Context, dirID, name string, overwrite bool) error {
	existing, err := s.db.FileInDir(ctx, dirID, name)
	if errors.Is(err, database.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !overwrite {
		return fmt.Errorf("%w: %s", ErrExists, name)
	}
	return s.deleteFileRow(ctx, existing)
}

// SegmentSize is the exact byte count of a given segment of a file. Every
// segment but the last is a full SegmentSize; the last holds the remainder.
func SegmentSize(fileSize, segSize int64, index int) int64 {
	start := int64(index-1) * segSize
	if start >= fileSize {
		return 0
	}
	return min(segSize, fileSize-start)
}

// PutSegment stores exactly one segment.
//
// This is the unit the browser uploads in and the unit a resumed transfer
// retries, because it is the largest piece that maps onto a single Telegram
// object: re-sending it is atomic as far as the drive is concerned.
func (s *Service) PutSegment(
	ctx context.Context,
	job database.UploadJob,
	index int,
	r io.Reader,
	size int64,
	progress Progress,
) error {
	request := struct {
		JobID string `json:"jobId"`
		Index int    `json:"index"`
		Size  int64  `json:"size"`
	}{JobID: job.ID, Index: index, Size: size}
	operation, err := s.beforePluginOperation(ctx, "files.putSegment", request, &request)
	if err != nil {
		return err
	}
	job, err = s.db.JobByID(ctx, request.JobID)
	if err != nil {
		return err
	}
	// Nothing is uploaded for a transfer that has already been stopped. The
	// caller may be a browser whose cancellation raced its own in-flight
	// request, or a plugin that has not noticed yet; either way the file row is
	// gone and storing the segment would leave a Telegram document nothing
	// points at.
	if job.Status.Aborted() {
		return fmt.Errorf("%w: %q", database.ErrJobFinished, job.Name)
	}
	index, size = request.Index, request.Size
	if index < 1 || index > job.SegmentCount {
		return fmt.Errorf("segment %d is outside the file's %d segments", index, job.SegmentCount)
	}
	file, err := s.db.FileByID(ctx, job.FileID)
	if err != nil {
		return err
	}
	if want := SegmentSize(file.Size, file.SegmentSize, index); size != want {
		// A wrong length would make Telegram commit to the wrong part count
		// and silently store a truncated segment.
		return fmt.Errorf("segment %d must be %d bytes, got %d", index, want, size)
	}
	channel, err := s.channelFor(ctx, file.ChannelID)
	if err != nil {
		return err
	}

	// The account comes from the job's lease, so every segment of one upload
	// goes through the same login. A server-side transfer that holds no browser
	// lease falls back to a scheduled account.
	account, ok := s.uploadJobAccount(job.ID)
	if !ok {
		account, err = s.metaAccount(ctx)
		if err != nil {
			return err
		}
	}

	doc, account, err := s.uploadSegment(ctx, account, file, channel, index, r, size, progress)
	if err != nil {
		return err
	}

	if err := s.db.UpsertSegment(ctx, database.Segment{
		FileID:        file.ID,
		Index:         index,
		Size:          size,
		TGMsgID:       doc.MsgID,
		TGDocID:       doc.DocID,
		AccessHash:    doc.AccessHash,
		DCID:          doc.DCID,
		FileReference: doc.FileReference,
		// Recording who stored it is what lets a later read know whether the
		// handle above is one it can use.
		AccountID: account.ID(),
	}); err != nil {
		return err
	}
	_, err = s.db.MarkSegmentDone(ctx, job.ID, index, size)
	if err == nil {
		s.afterPluginOperation(ctx, operation)
	}
	return err
}

// uploadSegment performs the Telegram half: stream the bytes, then post the
// document with its tagged caption. It returns the account that actually stored
// the segment, which is not always the one it was handed — see below.
func (s *Service) uploadSegment(
	ctx context.Context,
	account Account,
	file database.File,
	channel database.Channel,
	index int,
	r io.Reader,
	size int64,
	progress Progress,
) (StoredDoc, Account, error) {
	caption, err := s.fileCaption(ctx, file, index)
	if err != nil {
		return StoredDoc{}, account, err
	}

	chRef, err := channelRef(ctx, account, channel)
	if err != nil {
		return StoredDoc{}, account, err
	}

	// An empty file has no bytes to store, and Telegram will not accept a
	// zero-part document. It is recorded as a caption-only message instead:
	// the tags alone are enough to rebuild it, and the reader short-circuits
	// on size 0 without ever asking for a segment.
	if size == 0 && file.Size == 0 {
		msgID, err := account.SendRecord(ctx, chRef, caption)
		if err != nil {
			return StoredDoc{}, account, err
		}
		return StoredDoc{MsgID: msgID}, account, nil
	}

	segName := file.Name
	if file.SegmentCount > 1 {
		// A visible ordering in the channel makes a split file legible to
		// anyone browsing it in a Telegram client.
		segName = fmt.Sprintf("%s.part%03d", file.Name, index)
	}

	spec := UploadSpec{
		Name:     segName,
		MIME:     file.MIME,
		Caption:  caption,
		Body:     r,
		Size:     size,
		Progress: progress,
	}
	doc, err := account.Upload(ctx, chRef, spec)
	if err == nil {
		return doc, account, nil
	}

	// The account was throttled partway through. A segment is the unit a
	// resumed upload retries anyway, so it is also the natural unit to move to
	// another account — but only once, because a body that has already been
	// partly consumed cannot be re-read and a retry storm across every account
	// would be worse than waiting out the FLOOD_WAIT.
	if next, ok := s.failoverAccount(ctx, account, spec.Body); ok {
		s.log.Warn("moving a segment to another telegram account after a rate limit",
			zap.String("file", file.ID), zap.Int("segment", index),
			zap.String("from", account.ID()), zap.String("to", next.ID()), zap.Error(err))

		nextRef, refErr := channelRef(ctx, next, channel)
		if refErr == nil {
			if doc, retryErr := next.Upload(ctx, nextRef, spec); retryErr == nil {
				return doc, next, nil
			}
		}
	}
	return StoredDoc{}, account, fmt.Errorf("upload segment %d of %q: %w", index, file.Name, err)
}

// failoverAccount picks a different account to retry a throttled segment on.
//
// It refuses unless the body can be rewound, because Telegram's uploader has
// already consumed some of it: re-sending from a half-read stream would store a
// truncated segment, which is the one failure this program must never produce.
func (s *Service) failoverAccount(ctx context.Context, failed Account, body io.Reader) (Account, bool) {
	seeker, ok := body.(io.Seeker)
	if !ok {
		return nil, false
	}
	if _, err := seeker.Seek(0, io.SeekStart); err != nil {
		return nil, false
	}
	for _, candidate := range s.cluster.Accounts() {
		if candidate.ID() != failed.ID() && candidate.Available() {
			return candidate, true
		}
	}
	return nil, false
}

// fileCaption builds the tagged caption for one segment, including the
// human-readable ancestor tags that make a folder searchable inside Telegram.
func (s *Service) fileCaption(ctx context.Context, file database.File, index int) (string, error) {
	dirPath := Root
	if file.DirID != "" {
		dir, err := s.db.DirByID(ctx, file.DirID)
		if err != nil {
			return "", err
		}
		dirPath = dir.Path
	}

	return tagcodec.EncodeFile(tagcodec.Record{
		Kind:        tagcodec.KindFile,
		ID:          file.ID,
		ParentID:    file.DirID,
		Name:        file.Name,
		SegIndex:    index,
		SegCount:    file.SegmentCount,
		TotalSize:   file.Size,
		SegmentSize: file.SegmentSize,
		OwnerID:     file.OwnerID,
		HumanTags:   AncestorNames(dirPath),
	})
}

// Complete finalises a job once every segment has landed, making the file
// visible to listings and WebDAV.
func (s *Service) Complete(ctx context.Context, jobID string) (database.File, error) {
	request := struct {
		JobID string `json:"jobId"`
	}{JobID: jobID}
	operation, err := s.beforePluginOperation(ctx, "files.completeUpload", request, &request)
	if err != nil {
		return database.File{}, err
	}
	jobID = request.JobID
	job, err := s.db.JobByID(ctx, request.JobID)
	if err != nil {
		return database.File{}, err
	}
	if !job.Done() {
		return database.File{}, fmt.Errorf("upload is still missing segments %v", job.PendingSegments())
	}
	if err := s.db.UpdateFileStatus(ctx, job.FileID, database.StatusComplete); err != nil {
		return database.File{}, err
	}
	if err := s.db.SetJobStatus(ctx, jobID, database.JobComplete, ""); err != nil {
		return database.File{}, err
	}
	// Only a successful completion closes a browser lease. A missing segment
	// or a transient database error must leave the job retryable.
	s.ReleaseUploadJob(jobID)
	file, err := s.db.FileByID(ctx, job.FileID)
	if err == nil {
		s.afterPluginOperation(ctx, operation)
	}
	return file, err
}

// WatchUploadJob derives a context that CancelUpload can interrupt.
//
// Every piece of work that moves bytes for a job registers here: the HTTP
// handler receiving a browser segment, the goroutine fetching a remote URL, the
// stream a plugin writes its segments into. None of them is running inside the
// request that eventually cancels the transfer, so without this there is
// nothing for a cancellation to interrupt and the bytes keep going to Telegram
// after the record says the transfer stopped.
//
// The returned release must be called when the work finishes; it cancels the
// derived context and drops the registration.
func (s *Service) WatchUploadJob(ctx context.Context, jobID string) (context.Context, context.CancelFunc) {
	if jobID == "" {
		return ctx, func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	token := s.jobCancelID.Add(1)

	s.jobCancelMu.Lock()
	watchers, ok := s.jobCancels[jobID]
	if !ok {
		watchers = make(map[uint64]context.CancelFunc)
		s.jobCancels[jobID] = watchers
	}
	watchers[token] = cancel
	s.jobCancelMu.Unlock()

	return ctx, func() {
		cancel()
		s.jobCancelMu.Lock()
		if watchers, ok := s.jobCancels[jobID]; ok {
			delete(watchers, token)
			if len(watchers) == 0 {
				delete(s.jobCancels, jobID)
			}
		}
		s.jobCancelMu.Unlock()
	}
}

// stopUploadWork interrupts everything registered against a job.
func (s *Service) stopUploadWork(jobID string) {
	s.jobCancelMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.jobCancels[jobID]))
	for _, cancel := range s.jobCancels[jobID] {
		cancels = append(cancels, cancel)
	}
	s.jobCancelMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// CancelUpload stops a transfer and records it as cancelled.
//
// The order matters. The workers are interrupted first, so that by the time
// Abort deletes the file row there is nothing still trying to store segments
// into it; a worker that notices its context is gone leaves the status alone
// rather than reporting a failure over the cancellation.
//
// Cancelling twice is not an error — the transfer panel and the browser can
// both ask — and a transfer that has already stored its file is refused rather
// than deleted, since Abort's cleanup would take the finished file with it.
func (s *Service) CancelUpload(ctx context.Context, jobID string) error {
	job, err := s.db.JobByID(ctx, jobID)
	if err != nil {
		return err
	}
	s.stopUploadWork(jobID)
	switch {
	case job.Status == database.JobComplete:
		return fmt.Errorf("%w: %q has already finished uploading", database.ErrJobFinished, job.Name)
	case job.Status.Aborted():
		return nil
	}
	return s.Abort(ctx, jobID, "cancelled", database.JobCancelled)
}

// Abort tears down a failed or cancelled upload, removing whatever segments did
// land so the channel is not left holding documents nothing points at.
func (s *Service) Abort(ctx context.Context, jobID, reason string, status database.JobStatus) error {
	request := struct {
		JobID  string `json:"jobId"`
		Reason string `json:"reason"`
		Status string `json:"status"`
	}{JobID: jobID, Reason: reason, Status: string(status)}
	operation, err := s.beforePluginOperation(ctx, "files.abortUpload", request, &request)
	if err != nil {
		return err
	}
	jobID, reason = request.JobID, request.Reason
	status = database.JobStatus(request.Status)
	if status != database.JobFailed && status != database.JobCancelled {
		return fmt.Errorf("invalid upload abort status %q", status)
	}
	defer s.ReleaseUploadJob(jobID)

	job, err := s.db.JobByID(ctx, jobID)
	if err != nil {
		return err
	}
	if job.FileID != "" {
		if file, err := s.db.FileByID(ctx, job.FileID); err == nil {
			if err := s.deleteFileRow(ctx, file); err != nil {
				s.log.Warn("could not clean up an aborted upload",
					zap.String("file", file.ID), zap.Error(err))
			}
		}
	}
	err = s.db.SetJobStatus(ctx, jobID, status, reason)
	if err == nil {
		s.afterPluginOperation(ctx, operation)
	}
	return err
}

// abortAfterFailure records why a transfer stopped, unless it stopped because
// somebody cancelled it.
//
// A cancellation interrupts the worker, so the worker's own error is the
// context error the cancellation caused. Reporting that as a failure would
// overwrite the reason CancelUpload already wrote, which is how a transfer the
// user cancelled ended up in the history as "failed: context canceled". The
// recorded status is read rather than inferred from the context, so a caller
// whose request was simply abandoned — a WebDAV client that hung up mid-PUT —
// is still recorded as failed. The cleanup itself runs on an uncancellable
// context for the same reason: the context it was handed is usually the one
// that just died.
func (s *Service) abortAfterFailure(ctx context.Context, jobID string, cause error) {
	cleanupCtx := context.WithoutCancel(ctx)
	// Aborted rather than Terminal: a job whose last segment landed but whose
	// completion then failed reads as complete and still has a pending file row
	// to clean up, so it must not be skipped here.
	if job, err := s.db.JobByID(cleanupCtx, jobID); err == nil && job.Status.Aborted() {
		return
	}
	if err := s.Abort(cleanupCtx, jobID, cause.Error(), database.JobFailed); err != nil {
		s.log.Warn("could not clean up a failed transfer",
			zap.String("job", jobID), zap.Error(err))
	}
}

// UploadStream stores a whole file from one reader of known length, splitting
// it into segments as it goes.
//
// Nothing is buffered to disk: each segment is an io.LimitReader over the same
// source, so a 40 GB upload costs a few megabytes of memory. The flip side is
// that segments go out in order, because a sequential source cannot be read out
// of order. Parallel segments are available to callers that can seek, which in
// practice means the browser slicing a local file.
func (s *Service) UploadStream(ctx context.Context, req UploadRequest, r io.Reader, progress Progress) (database.File, error) {
	lease, err := s.acquireUploadTask(ctx)
	if err != nil {
		return database.File{}, err
	}
	defer lease.release()

	return s.uploadStream(ctx, lease.account, req, r, progress)
}

// uploadStream is the implementation shared by known-size and unsized
// uploads. Callers must already hold the task slot before entering it, and pass
// the account it was taken on so every segment lands on the same login.
func (s *Service) uploadStream(ctx context.Context, account Account, req UploadRequest, r io.Reader, progress Progress) (database.File, error) {
	job, file, err := s.Begin(ctx, req)
	if err != nil {
		return database.File{}, err
	}
	defer s.bindUploadAccount(job.ID, account)()

	ctx, release := s.WatchUploadJob(ctx, job.ID)
	defer release()

	if _, err := s.db.SetJobStatusIf(ctx, job.ID, database.JobRunning, "",
		database.JobPending, database.JobRunning); err != nil {
		return database.File{}, err
	}
	if s.OnRemoteProgress != nil {
		s.OnRemoteProgress(job, 0, file.Size, nil)
	}

	var written int64
	for index := 1; index <= file.SegmentCount; index++ {
		size := SegmentSize(file.Size, file.SegmentSize, index)
		base := written

		err := s.PutSegment(ctx, job, index, io.LimitReader(r, size), size,
			func(uploaded, _ int64) {
				current := base + uploaded
				if progress != nil {
					progress(current, file.Size)
				}
				if s.OnRemoteProgress != nil {
					s.OnRemoteProgress(job, current, file.Size, nil)
				}
			})
		if err != nil {
			s.abortAfterFailure(ctx, job.ID, err)
			if s.OnRemoteProgress != nil {
				s.OnRemoteProgress(job, base, file.Size, err)
			}
			return database.File{}, err
		}
		written += size
	}

	result, err := s.Complete(ctx, job.ID)
	if err == nil && s.OnRemoteProgress != nil {
		completed := job
		completed.Status = database.JobComplete
		s.OnRemoteProgress(completed, file.Size, file.Size, nil)
	}
	return result, err
}

// UploadUnsized stores a stream whose length is not known up front, which in
// practice means a WebDAV PUT using chunked transfer encoding.
//
// The bytes are spooled to a temporary file purely to measure them, because
// Telegram's upload protocol commits to a part count in its first request and
// cannot be told the size afterwards. Callers that do know the length should
// use UploadStream and skip the disk entirely.
func (s *Service) UploadUnsized(ctx context.Context, req UploadRequest, r io.Reader, progress Progress) (database.File, error) {
	lease, err := s.acquireUploadTask(ctx)
	if err != nil {
		return database.File{}, err
	}
	defer lease.release()

	if err := os.MkdirAll(s.cfg.Storage.SpoolDir, 0o750); err != nil {
		return database.File{}, fmt.Errorf("create spool directory: %w", err)
	}
	spool, err := os.CreateTemp(s.cfg.Storage.SpoolDir, "upload-*.part")
	if err != nil {
		return database.File{}, fmt.Errorf("create spool file: %w", err)
	}
	defer func() {
		spool.Close()
		if err := os.Remove(spool.Name()); err != nil && !os.IsNotExist(err) {
			s.log.Warn("could not remove an upload spool file",
				zap.String("path", spool.Name()), zap.Error(err))
		}
	}()

	size, err := io.Copy(spool, r)
	if err != nil {
		return database.File{}, fmt.Errorf("spool upload: %w", err)
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return database.File{}, fmt.Errorf("rewind spool file: %w", err)
	}

	req.Size = size
	return s.uploadStream(ctx, lease.account, req, spool, progress)
}

// PutSegmentsParallel uploads several independently-sourced segments at once.
// The browser uses this shape: it slices a local file and sends segments
// concurrently, which a single server-side stream cannot do.
func (s *Service) PutSegmentsParallel(
	ctx context.Context,
	job database.UploadJob,
	segments map[int]io.Reader,
	progress Progress,
) error {
	lease, err := s.acquireUploadTask(ctx)
	if err != nil {
		return err
	}
	defer lease.release()
	defer s.bindUploadAccount(job.ID, lease.account)()

	file, err := s.db.FileByID(ctx, job.FileID)
	if err != nil {
		return err
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(s.cfg.Storage.SegmentConcurrency)

	var done atomic.Int64
	for idx, r := range segments {
		g.Go(func() error {
			size := SegmentSize(file.Size, file.SegmentSize, idx)
			if err := s.PutSegment(gctx, job, idx, r, size, nil); err != nil {
				return err
			}
			if progress != nil {
				progress(done.Add(size), file.Size)
			}
			return nil
		})
	}
	return g.Wait()
}
