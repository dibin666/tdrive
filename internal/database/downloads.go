package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const downloadCols = `id, user_id, file_id, name, total_size, downloaded_bytes, mode, status,
	error, cache_path, created_at, updated_at, started_at, finished_at, expires_at, last_used_at`

func scanDownload(row interface{ Scan(...any) error }) (DownloadJob, error) {
	var (
		j                         DownloadJob
		userID, fileID            sql.NullString
		created, updated          int64
		started, finished         int64
		expires, lastUsed         int64
		mode, status, cachePath   string
		errText                   string
		totalSize, downloadedByte int64
	)
	err := row.Scan(&j.ID, &userID, &fileID, &j.Name, &totalSize, &downloadedByte,
		&mode, &status, &errText, &cachePath,
		&created, &updated, &started, &finished, &expires, &lastUsed)
	if err != nil {
		return DownloadJob{}, Translate(err)
	}

	j.UserID, j.FileID = text(userID), text(fileID)
	j.TotalSize, j.DownloadedBytes = totalSize, downloadedByte
	j.Mode, j.Status = DownloadMode(mode), DownloadStatus(status)
	j.Error, j.CachePath = errText, cachePath
	j.CreatedAt, j.UpdatedAt = msToTime(created), msToTime(updated)
	// A zero column means "never happened", which has to stay the zero
	// time.Time rather than becoming 1970.
	if started > 0 {
		j.StartedAt = msToTime(started)
	}
	if finished > 0 {
		j.FinishedAt = msToTime(finished)
	}
	if expires > 0 {
		j.ExpiresAt = msToTime(expires)
	}
	if lastUsed > 0 {
		j.LastUsedAt = msToTime(lastUsed)
	}
	return j, nil
}

func (d *DB) InsertDownload(ctx context.Context, j DownloadJob) error {
	now := nowMS()
	if j.Mode == "" {
		j.Mode = DownloadDirect
	}
	if j.Status == "" {
		j.Status = DownloadPending
	}
	_, err := d.write.ExecContext(ctx,
		`INSERT INTO download_jobs (`+downloadCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, 0, ?)`,
		j.ID, nullable(j.UserID), nullable(j.FileID), j.Name, j.TotalSize, j.DownloadedBytes,
		string(j.Mode), string(j.Status), j.Error, j.CachePath, now, now, now)
	if err != nil {
		return fmt.Errorf("insert download job %q: %w", j.Name, Translate(err))
	}
	return nil
}

func (d *DB) DownloadByID(ctx context.Context, id string) (DownloadJob, error) {
	return scanDownload(d.read.QueryRowContext(ctx,
		`SELECT `+downloadCols+` FROM download_jobs WHERE id = ?`, id))
}

// SetDownloadProgress records how far a staged download has got.
func (d *DB) SetDownloadProgress(ctx context.Context, id string, downloaded int64) error {
	_, err := d.write.ExecContext(ctx,
		`UPDATE download_jobs SET downloaded_bytes = ?, updated_at = ? WHERE id = ?`,
		downloaded, nowMS(), id)
	return Translate(err)
}

// SetDownloadStatus moves a job's lifecycle forward, maintaining the timing
// brackets on the way through so that nothing else has to remember to.
func (d *DB) SetDownloadStatus(ctx context.Context, id string, status DownloadStatus, errMsg string) error {
	ok, err := d.setDownloadStatus(ctx, id, status, errMsg, "", nil)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// SetDownloadStatusIf changes a status only while the row is in one of the
// supplied states. Background staged workers use this conditional transition
// so a cancellation that wins a race cannot be overwritten by a late "ready".
func (d *DB) SetDownloadStatusIf(
	ctx context.Context,
	id string,
	status DownloadStatus,
	errMsg string,
	expected ...DownloadStatus,
) (bool, error) {
	if len(expected) == 0 {
		return false, nil
	}
	placeholders := make([]string, len(expected))
	args := make([]any, len(expected))
	for i, state := range expected {
		placeholders[i] = "?"
		args[i] = string(state)
	}
	return d.setDownloadStatus(ctx, id, status, errMsg,
		"status IN ("+strings.Join(placeholders, ", ")+")", args)
}

func (d *DB) setDownloadStatus(
	ctx context.Context,
	id string,
	status DownloadStatus,
	errMsg string,
	condition string,
	conditionArgs []any,
) (bool, error) {
	now := nowMS()
	sets := []string{"status = ?", "error = ?", "updated_at = ?"}
	args := []any{string(status), errMsg, now}

	switch status {
	case DownloadRunning:
		// Only the first transition to running starts the clock; a resumed job
		// must not reset it.
		sets = append(sets, "started_at = CASE WHEN started_at = 0 THEN ? ELSE started_at END")
		args = append(args, now)
	case DownloadReady, DownloadComplete, DownloadFailed, DownloadCancelled, DownloadExpired:
		sets = append(sets, "finished_at = ?")
		args = append(args, now)
	}

	args = append(args, conditionArgs...)
	args = append(args, id)
	where := "id = ?"
	if condition != "" {
		where = condition + " AND " + where
	}
	res, err := d.write.ExecContext(ctx,
		`UPDATE download_jobs SET `+strings.Join(sets, ", ")+` WHERE `+where, args...)
	if err != nil {
		return false, fmt.Errorf("set download status: %w", Translate(err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("set download status: %w", err)
	}
	return n > 0, nil
}

// SetDownloadCache records where a staged copy landed and when it may be
// evicted.
func (d *DB) SetDownloadCache(ctx context.Context, id, path string, expiresAtMS int64) error {
	res, err := d.write.ExecContext(ctx,
		`UPDATE download_jobs SET cache_path = ?, expires_at = ?, last_used_at = ?, updated_at = ? WHERE id = ?`,
		path, expiresAtMS, nowMS(), nowMS(), id)
	return affectedOne(res, err, "set download cache")
}

// TouchDownload marks a staged file as recently used, which is what keeps it
// from being the first thing LRU eviction throws away.
func (d *DB) TouchDownload(ctx context.Context, id string) error {
	_, err := d.write.ExecContext(ctx,
		`UPDATE download_jobs SET last_used_at = ? WHERE id = ?`, nowMS(), id)
	return Translate(err)
}

func (d *DB) DeleteDownload(ctx context.Context, id string) error {
	res, err := d.write.ExecContext(ctx, `DELETE FROM download_jobs WHERE id = ?`, id)
	return affectedOne(res, err, "delete download job")
}

// StagedDownloadFor finds a ready staged copy of a file that has not expired,
// so a second request for the same file reuses the cached bytes instead of
// pulling it out of Telegram again.
func (d *DB) StagedDownloadFor(ctx context.Context, fileID string, nowMillis int64) (DownloadJob, error) {
	return scanDownload(d.read.QueryRowContext(ctx,
		`SELECT `+downloadCols+` FROM download_jobs
		 WHERE file_id = ? AND mode = 'staged' AND status IN ('ready', 'complete')
		   AND cache_path <> '' AND (expires_at = 0 OR expires_at > ?)
		 ORDER BY created_at DESC LIMIT 1`, fileID, nowMillis))
}

// ActiveStagedFor finds a staged download of the same file that is already
// running, so two people asking for the same file wait on one transfer rather
// than starting two.
func (d *DB) ActiveStagedFor(ctx context.Context, fileID string) (DownloadJob, error) {
	return scanDownload(d.read.QueryRowContext(ctx,
		`SELECT `+downloadCols+` FROM download_jobs
		 WHERE file_id = ? AND mode = 'staged' AND status IN ('pending', 'running')
		 ORDER BY created_at DESC LIMIT 1`, fileID))
}

// CacheUsage totals staged bytes and reservations. Pending and running jobs
// reserve their complete target size even before cache_path is populated;
// terminal rows with a path are included until their bytes are cleaned up.
func (d *DB) CacheUsage(ctx context.Context) (bytes int64, count int64, err error) {
	var sum sql.NullInt64
	err = d.read.QueryRowContext(ctx,
		`SELECT sum(total_size), count(*) FROM download_jobs
		 WHERE mode = 'staged' AND
		       (status IN ('pending', 'running') OR
		        (cache_path <> '' AND status IN ('ready', 'complete', 'expired', 'failed', 'cancelled')))`).Scan(&sum, &count)
	if err != nil {
		return 0, 0, Translate(err)
	}
	return sum.Int64, count, nil
}

// FinishedDownloadsBefore returns terminal download rows old enough to be
// removed from transfer history. The caller unlinks staged files before
// deleting these rows because the database is not allowed to become the only
// place that knows where bytes live.
func (d *DB) FinishedDownloadsBefore(ctx context.Context, olderThanMS int64) ([]DownloadJob, error) {
	now := time.Now().UnixMilli()
	rows, err := d.read.QueryContext(ctx,
		`SELECT `+downloadCols+` FROM download_jobs
		 WHERE status IN ('complete', 'failed', 'cancelled', 'expired', 'ready')
		   AND updated_at < ?
		   AND NOT (mode = 'staged' AND status IN ('ready', 'complete')
		            AND cache_path <> '' AND (expires_at = 0 OR expires_at > ?))
		 ORDER BY updated_at`, olderThanMS, now)
	if err != nil {
		return nil, Translate(err)
	}
	defer rows.Close()

	var out []DownloadJob
	for rows.Next() {
		job, err := scanDownload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, job)
	}
	return out, Translate(rows.Err())
}

// EvictableDownloads lists staged copies in least-recently-used order, which
// is the order the cache sweeper removes them in.
func (d *DB) EvictableDownloads(ctx context.Context) ([]DownloadJob, error) {
	rows, err := d.read.QueryContext(ctx,
		`SELECT `+downloadCols+` FROM download_jobs
		 WHERE mode = 'staged' AND cache_path <> '' AND status IN ('ready', 'complete', 'expired', 'failed', 'cancelled')
		 ORDER BY last_used_at ASC`)
	if err != nil {
		return nil, Translate(err)
	}
	defer rows.Close()

	var out []DownloadJob
	for rows.Next() {
		j, err := scanDownload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, Translate(rows.Err())
}

// ResumableDownloads are staged jobs interrupted by a restart. Unlike an
// upload, nothing needs a client to reconnect: the server has everything it
// needs to carry on.
func (d *DB) ResumableDownloads(ctx context.Context) ([]DownloadJob, error) {
	rows, err := d.read.QueryContext(ctx,
		`SELECT `+downloadCols+` FROM download_jobs
		 WHERE mode = 'staged' AND status IN ('pending', 'running') ORDER BY created_at`)
	if err != nil {
		return nil, Translate(err)
	}
	defer rows.Close()

	var out []DownloadJob
	for rows.Next() {
		j, err := scanDownload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, Translate(rows.Err())
}
