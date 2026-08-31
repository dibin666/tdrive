package drive

import (
	"context"
	"fmt"
	"io"

	"go.uber.org/zap"

	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/localfs"
)

// LocalRequest asks the server to read one file from the configured VPS
// directory and store it in the drive. SourcePath is relative to the local
// directory root and is never an operating-system absolute path.
type LocalRequest struct {
	SourcePath string
	DirPath    string
	Name       string
	UserID     string
	Overwrite  bool
}

// StartLocal reserves a destination and starts a detached upload from the
// server's mounted directory. The source path is stored relative to the mount
// so a pending job can be resumed after a process restart without exposing the
// container's absolute path to the database or WebUI.
func (s *Service) StartLocal(ctx context.Context, req LocalRequest) (database.UploadJob, error) {
	localRoot := s.cfg.RuntimeSettings().LocalRoot
	source := localfs.New(localRoot)
	entry, err := source.Stat(req.SourcePath)
	if err != nil {
		return database.UploadJob{}, err
	}
	if entry.IsDir {
		return database.UploadJob{}, localfs.ErrNotFile
	}

	name := req.Name
	if name == "" {
		name = entry.Name
	}
	if err := ValidateName(name); err != nil {
		return database.UploadJob{}, err
	}

	job, _, err := s.Begin(ctx, UploadRequest{
		DirPath:   req.DirPath,
		Name:      name,
		Size:      entry.Size,
		MIME:      GuessMIME(name),
		UserID:    req.UserID,
		Source:    "local",
		SourceURL: entry.Path,
		Overwrite: req.Overwrite,
	})
	if err != nil {
		return database.UploadJob{}, err
	}

	s.scheduleJobWorker(job.ID, func() {
		s.runLocal(context.WithoutCancel(ctx), job, localRoot, entry.Path)
	})
	return job, nil
}

// runLocal reads the source sequentially, uploading the same segment
// granularity used by server-side URL fetches. It deliberately reopens the
// source in the goroutine so the HTTP request does not own a file descriptor.
func (s *Service) runLocal(ctx context.Context, job database.UploadJob, localRoot, sourcePath string) {
	lease, err := s.acquireUploadTask(ctx)
	if err != nil {
		s.failLocal(ctx, job, err)
		return
	}
	defer lease.release()
	defer s.bindUploadAccount(job.ID, lease.account)()

	source := localfs.New(localRoot)
	fileHandle, info, err := source.Open(sourcePath)
	if err != nil {
		s.failLocal(ctx, job, err)
		return
	}
	defer fileHandle.Close()

	if info.Size != job.TotalSize {
		s.failLocal(ctx, job, fmt.Errorf("local file changed size while it was queued"))
		return
	}
	if err := s.db.SetJobStatus(ctx, job.ID, database.JobRunning, ""); err != nil {
		s.failLocal(ctx, job, err)
		return
	}

	file, err := s.db.FileByID(ctx, job.FileID)
	if err != nil {
		s.failLocal(ctx, job, err)
		return
	}
	if s.OnRemoteProgress != nil {
		s.OnRemoteProgress(job, 0, file.Size, nil)
	}

	for _, index := range job.PendingSegments() {
		start := int64(index-1) * file.SegmentSize
		size := SegmentSize(file.Size, file.SegmentSize, index)
		if _, err := fileHandle.Seek(start, io.SeekStart); err != nil {
			s.failLocal(ctx, job, fmt.Errorf("seek local file to segment %d: %w", index, err))
			return
		}

		err = s.PutSegment(ctx, job, index, io.LimitReader(fileHandle, size), size,
			func(uploaded, _ int64) {
				if s.OnRemoteProgress != nil {
					s.OnRemoteProgress(job, start+uploaded, file.Size, nil)
				}
			})
		if err != nil {
			s.failLocal(ctx, job, err)
			return
		}
	}

	if _, err := s.Complete(ctx, job.ID); err != nil {
		s.failLocal(ctx, job, err)
		return
	}
	if s.OnRemoteProgress != nil {
		completed := job
		completed.Status = database.JobComplete
		s.OnRemoteProgress(completed, file.Size, file.Size, nil)
	}
}

func (s *Service) failLocal(ctx context.Context, job database.UploadJob, err error) {
	s.log.Warn("a local transfer failed",
		zap.String("job", job.ID), zap.String("name", job.Name), zap.Error(err))
	if abortErr := s.Abort(ctx, job.ID, err.Error(), database.JobFailed); abortErr != nil {
		s.log.Warn("could not clean up a failed local transfer",
			zap.String("job", job.ID), zap.Error(abortErr))
	}
	if s.OnRemoteProgress != nil {
		s.OnRemoteProgress(job, 0, job.TotalSize, err)
	}
}
