package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrJobFinished is returned when work arrives for a job that has already been
// cancelled or failed. The caller's job is to stop, not to retry: the file row
// the segment belonged to has been deleted along with the transfer.
var ErrJobFinished = errors.New("database: this upload has already been stopped")

const jobCols = `id, user_id, file_id, dir_id, name, total_size, segment_size, segment_count,
	done_mask, uploaded_bytes, status, error, source, source_url, created_at, updated_at,
	started_at, finished_at`

func scanJob(row interface{ Scan(...any) error }) (UploadJob, error) {
	var (
		j                     UploadJob
		userID, fileID, dirID sql.NullString
		created, updated      int64
		started, finished     int64
	)
	err := row.Scan(&j.ID, &userID, &fileID, &dirID, &j.Name, &j.TotalSize,
		&j.SegmentSize, &j.SegmentCount, &j.DoneMask, &j.UploadedBytes,
		&j.Status, &j.Error, &j.Source, &j.SourceURL, &created, &updated,
		&started, &finished)
	if err != nil {
		return UploadJob{}, Translate(err)
	}
	j.UserID, j.FileID, j.DirID = text(userID), text(fileID), text(dirID)
	j.CreatedAt, j.UpdatedAt = msToTime(created), msToTime(updated)
	if started > 0 {
		j.StartedAt = msToTime(started)
	}
	if finished > 0 {
		j.FinishedAt = msToTime(finished)
	}
	return j, nil
}

func (d *DB) InsertJob(ctx context.Context, j UploadJob) error {
	now := nowMS()
	if j.Source == "" {
		if j.SourceURL != "" {
			j.Source = "remote"
		} else {
			j.Source = "webui"
		}
	}
	_, err := d.write.ExecContext(ctx,
		`INSERT INTO upload_jobs (`+jobCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0)`,
		j.ID, nullable(j.UserID), nullable(j.FileID), nullable(j.DirID), j.Name, j.TotalSize,
		j.SegmentSize, j.SegmentCount, j.DoneMask, j.UploadedBytes,
		string(j.Status), j.Error, j.Source, j.SourceURL, now, now)
	if err != nil {
		return fmt.Errorf("insert upload job %q: %w", j.Name, Translate(err))
	}
	return nil
}

func (d *DB) JobByID(ctx context.Context, id string) (UploadJob, error) {
	return scanJob(d.read.QueryRowContext(ctx, `SELECT `+jobCols+` FROM upload_jobs WHERE id = ?`, id))
}

// ListJobs returns a user's transfers, newest first, for the transfer panel.
// Server-initiated jobs have no user and are shown to everyone, since they
// affect the shared drive and would otherwise be invisible.
func (d *DB) ListJobs(ctx context.Context, userID string, limit int) ([]UploadJob, error) {
	rows, err := d.read.QueryContext(ctx,
		`SELECT `+jobCols+` FROM upload_jobs WHERE user_id = ? OR user_id IS NULL
		 ORDER BY created_at DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, Translate(err)
	}
	defer rows.Close()

	var out []UploadJob
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, Translate(rows.Err())
}

// ResumableJobs are jobs left mid-flight by a restart. Only server-driven
// transfers can be picked back up automatically: a browser upload needs the
// client to re-send bytes, so it stays pending until the browser reconnects.
func (d *DB) ResumableJobs(ctx context.Context) ([]UploadJob, error) {
	rows, err := d.read.QueryContext(ctx,
		`SELECT `+jobCols+` FROM upload_jobs
		 WHERE status IN ('pending', 'running') AND source_url <> '' ORDER BY created_at`)
	if err != nil {
		return nil, Translate(err)
	}
	defer rows.Close()

	var out []UploadJob
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, Translate(rows.Err())
}

// MarkSegmentDone records one landed segment atomically. Read-modify-write of
// the bitset happens inside the transaction because several segments of the
// same file finish concurrently, and a lost update would make the job look
// incomplete forever.
func (d *DB) MarkSegmentDone(ctx context.Context, jobID string, idx int, bytes int64) (UploadJob, error) {
	var job UploadJob
	err := d.Tx(ctx, func(tx txExec) error {
		var (
			mask     []byte
			uploaded int64
			count    int
			current  JobStatus
		)
		err := tx.QueryRowContext(ctx,
			`SELECT done_mask, uploaded_bytes, segment_count, status FROM upload_jobs WHERE id = ?`, jobID).
			Scan(&mask, &uploaded, &count, &current)
		if err != nil {
			return Translate(err)
		}
		// A segment can still be in flight when the transfer is cancelled, and
		// it finishes some seconds later. Recording it would set the row back to
		// running against a file that Abort has already deleted, so the caller is
		// told to stop instead.
		if current.Aborted() {
			return ErrJobFinished
		}

		// Re-sending a segment after a retry must not double-count bytes.
		if !MaskHas(mask, idx) {
			mask = MaskSet(mask, idx)
			uploaded += bytes
		}

		status := JobRunning
		done := true
		for i := 1; i <= count; i++ {
			if !MaskHas(mask, i) {
				done = false
				break
			}
		}
		if done {
			status = JobComplete
		}

		now := nowMS()
		finished := int64(0)
		if done {
			finished = now
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE upload_jobs SET done_mask = ?, uploaded_bytes = ?, status = ?, updated_at = ?,
			   started_at = CASE WHEN started_at = 0 THEN ? ELSE started_at END,
			   finished_at = CASE WHEN ? > 0 THEN ? ELSE finished_at END
			 WHERE id = ?`, mask, uploaded, string(status), now, now, finished, finished, jobID)
		if err != nil {
			return Translate(err)
		}
		job, err = scanJob(tx.QueryRowContext(ctx, `SELECT `+jobCols+` FROM upload_jobs WHERE id = ?`, jobID))
		return err
	})
	return job, err
}

// SetJobStatus moves a job's lifecycle forward and maintains the timing
// brackets on the way.
//
// started_at is only written on the first transition to running, because this
// is called on every segment (internal/api/upload.go) and a resumed upload
// must not keep resetting its own clock. finished_at is written on any
// terminal status, so an average speed can be computed without counting the
// time the job spent queued behind the concurrency limit.
func (d *DB) SetJobStatus(ctx context.Context, id string, status JobStatus, errMsg string) error {
	sets, args := jobStatusUpdate(status, errMsg)
	args = append(args, id)
	res, err := d.write.ExecContext(ctx,
		`UPDATE upload_jobs SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	return affectedOne(res, err, "set job status")
}

// SetJobStatusIf moves a job forward only from one of the given statuses,
// reporting whether it applied.
//
// Every transition made by a transfer that is under way has to go through this
// rather than SetJobStatus. A cancelled upload has workers still unwinding —
// the next segment request, the fetch loop's error path — and an unconditional
// write from one of them puts the row back into a state the transfer panel then
// shows as live. Refusing the transition is what makes "cancelled" stick.
func (d *DB) SetJobStatusIf(ctx context.Context, id string, status JobStatus, errMsg string, from ...JobStatus) (bool, error) {
	if len(from) == 0 {
		return false, errors.New("set job status: no source status given")
	}
	sets, args := jobStatusUpdate(status, errMsg)
	placeholders := make([]string, 0, len(from))
	for _, allowed := range from {
		placeholders = append(placeholders, "?")
		args = append(args, string(allowed))
	}
	args = append(args, id)
	res, err := d.write.ExecContext(ctx,
		`UPDATE upload_jobs SET `+strings.Join(sets, ", ")+
			` WHERE status IN (`+strings.Join(placeholders, ", ")+`) AND id = ?`, args...)
	if err != nil {
		return false, Translate(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, Translate(err)
	}
	return n > 0, nil
}

// jobStatusUpdate builds the assignment list shared by both status writers,
// including the timing brackets described above.
func jobStatusUpdate(status JobStatus, errMsg string) ([]string, []any) {
	now := nowMS()
	sets := []string{"status = ?", "error = ?", "updated_at = ?"}
	args := []any{string(status), errMsg, now}

	switch status {
	case JobRunning:
		sets = append(sets, "started_at = CASE WHEN started_at = 0 THEN ? ELSE started_at END")
		args = append(args, now)
	case JobComplete, JobFailed, JobCancelled:
		sets = append(sets, "finished_at = ?")
		args = append(args, now)
	}
	return sets, args
}

// SetJobFile links a job to the file row it is filling in, which only becomes
// known after the destination directory has been resolved.
func (d *DB) SetJobFile(ctx context.Context, id, fileID string) error {
	res, err := d.write.ExecContext(ctx,
		`UPDATE upload_jobs SET file_id = ?, updated_at = ? WHERE id = ?`, nullable(fileID), nowMS(), id)
	return affectedOne(res, err, "set job file")
}

// SetJobSize records the real size of a transfer whose length was unknown when
// it started, together with the segment count that implies.
func (d *DB) SetJobSize(ctx context.Context, id string, total int64, segments int) error {
	res, err := d.write.ExecContext(ctx,
		`UPDATE upload_jobs SET total_size = ?, segment_count = ?, updated_at = ? WHERE id = ?`,
		total, segments, nowMS(), id)
	return affectedOne(res, err, "set job size")
}

func (d *DB) DeleteJob(ctx context.Context, id string) error {
	res, err := d.write.ExecContext(ctx, `DELETE FROM upload_jobs WHERE id = ?`, id)
	return affectedOne(res, err, "delete job")
}

// PurgeFinishedJobs trims transfer history so the panel stays useful and the
// table stays small.
func (d *DB) PurgeFinishedJobs(ctx context.Context, olderThanMS int64) (int64, error) {
	res, err := d.write.ExecContext(ctx,
		`DELETE FROM upload_jobs
		 WHERE status IN ('complete', 'failed', 'cancelled') AND updated_at < ?`, olderThanMS)
	if err != nil {
		return 0, Translate(err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
