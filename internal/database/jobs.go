package database

import (
	"context"
	"database/sql"
	"fmt"
)

const jobCols = `id, user_id, file_id, dir_id, name, total_size, segment_size, segment_count,
	done_mask, uploaded_bytes, status, error, source, source_url, created_at, updated_at`

func scanJob(row interface{ Scan(...any) error }) (UploadJob, error) {
	var (
		j                     UploadJob
		userID, fileID, dirID sql.NullString
		created, updated      int64
	)
	err := row.Scan(&j.ID, &userID, &fileID, &dirID, &j.Name, &j.TotalSize,
		&j.SegmentSize, &j.SegmentCount, &j.DoneMask, &j.UploadedBytes,
		&j.Status, &j.Error, &j.Source, &j.SourceURL, &created, &updated)
	if err != nil {
		return UploadJob{}, Translate(err)
	}
	j.UserID, j.FileID, j.DirID = text(userID), text(fileID), text(dirID)
	j.CreatedAt, j.UpdatedAt = msToTime(created), msToTime(updated)
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
		`INSERT INTO upload_jobs (`+jobCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
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
		)
		err := tx.QueryRowContext(ctx,
			`SELECT done_mask, uploaded_bytes, segment_count FROM upload_jobs WHERE id = ?`, jobID).
			Scan(&mask, &uploaded, &count)
		if err != nil {
			return Translate(err)
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

		_, err = tx.ExecContext(ctx,
			`UPDATE upload_jobs SET done_mask = ?, uploaded_bytes = ?, status = ?, updated_at = ?
			 WHERE id = ?`, mask, uploaded, string(status), nowMS(), jobID)
		if err != nil {
			return Translate(err)
		}
		job, err = scanJob(tx.QueryRowContext(ctx, `SELECT `+jobCols+` FROM upload_jobs WHERE id = ?`, jobID))
		return err
	})
	return job, err
}

func (d *DB) SetJobStatus(ctx context.Context, id string, status JobStatus, errMsg string) error {
	res, err := d.write.ExecContext(ctx,
		`UPDATE upload_jobs SET status = ?, error = ?, updated_at = ? WHERE id = ?`,
		string(status), errMsg, nowMS(), id)
	return affectedOne(res, err, "set job status")
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
