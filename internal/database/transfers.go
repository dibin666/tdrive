package database

import (
	"context"
	"strings"
)

// A transfer is an upload or a download. They live in separate tables because
// their state machines differ — an upload has a segment bitset, a download has
// a cache path — but the transfer panel shows one merged, filterable list, and
// that filtering has to happen in SQL rather than by fetching everything and
// sorting in the browser.

// TransferFilter narrows a query of transfer history. Zero values mean "no
// constraint", so the empty filter is the whole (limited) list.
type TransferFilter struct {
	// UserID scopes to one account. Server-initiated jobs have no user and are
	// visible to everyone, because they change the shared drive.
	UserID string
	// AllUsers lifts the scoping entirely, which is what an administrator
	// looking at the whole deployment wants.
	AllUsers bool
	// Statuses is an OR-set; empty means any status.
	Statuses []string
	// Sources is an OR-set over the upload source column. It has no effect on
	// downloads, which have modes rather than sources.
	Sources []string
	// From and To bound created_at in Unix milliseconds. This is the date
	// filter the transfer page exposes.
	From, To int64
	// Query matches the transfer name.
	Query string
	Limit int
}

func (f TransferFilter) limit() int {
	if f.Limit <= 0 || f.Limit > 1000 {
		return 200
	}
	return f.Limit
}

// scope builds the ownership predicate shared by both tables.
func (f TransferFilter) scope() (string, []any) {
	if f.AllUsers {
		return "1 = 1", nil
	}
	return "(user_id = ? OR user_id IS NULL)", []any{f.UserID}
}

func inClause(column string, values []string) (string, []any) {
	if len(values) == 0 {
		return "", nil
	}
	placeholders := make([]string, len(values))
	args := make([]any, len(values))
	for i, v := range values {
		placeholders[i] = "?"
		args[i] = v
	}
	return column + " IN (" + strings.Join(placeholders, ", ") + ")", args
}

func (f TransferFilter) common(includeSources bool) ([]string, []any) {
	scope, args := f.scope()
	where := []string{scope}

	if clause, extra := inClause("status", f.Statuses); clause != "" {
		where = append(where, clause)
		args = append(args, extra...)
	}
	if includeSources {
		if clause, extra := inClause("source", f.Sources); clause != "" {
			where = append(where, clause)
			args = append(args, extra...)
		}
	}
	if f.From > 0 {
		where = append(where, "created_at >= ?")
		args = append(args, f.From)
	}
	if f.To > 0 {
		where = append(where, "created_at <= ?")
		args = append(args, f.To)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		where = append(where, "name LIKE ?")
		args = append(args, "%"+q+"%")
	}
	return where, args
}

// FilterUploads returns matching upload jobs, newest first.
func (d *DB) FilterUploads(ctx context.Context, f TransferFilter) ([]UploadJob, error) {
	// A source filter that names no upload source at all means the caller
	// asked only for download sources; answering with every upload would be
	// the opposite of what they wanted.
	if len(f.Sources) > 0 {
		matched := false
		for _, s := range f.Sources {
			switch s {
			case "webui", "local", "remote", "webdav", "aliyunpan":
				matched = true
			}
		}
		if !matched {
			return nil, nil
		}
	}

	where, args := f.common(true)
	args = append(args, f.limit())

	rows, err := d.read.QueryContext(ctx,
		`SELECT `+jobCols+` FROM upload_jobs WHERE `+strings.Join(where, " AND ")+
			` ORDER BY created_at DESC LIMIT ?`, args...)
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

// FilterDownloads returns matching download jobs, newest first.
func (d *DB) FilterDownloads(ctx context.Context, f TransferFilter) ([]DownloadJob, error) {
	where, args := f.common(false)
	args = append(args, f.limit())

	rows, err := d.read.QueryContext(ctx,
		`SELECT `+downloadCols+` FROM download_jobs WHERE `+strings.Join(where, " AND ")+
			` ORDER BY created_at DESC LIMIT ?`, args...)
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

// terminalStatuses are the only ones history deletion will touch. Deleting a
// running job's row would orphan the transfer that is still writing to it.
var terminalStatuses = []string{"complete", "failed", "cancelled", "expired", "ready"}

// DeleteFinishedUploads removes history rows. ids narrows to specific jobs;
// with no ids every finished row matching the filter goes. It returns the
// deleted rows' ids so a caller can report exactly what happened.
func (d *DB) DeleteFinishedUploads(ctx context.Context, f TransferFilter, ids []string) (int64, error) {
	where, args := f.common(true)

	clause, extra := inClause("status", terminalStatuses)
	where = append(where, clause)
	args = append(args, extra...)

	if len(ids) > 0 {
		idClause, idArgs := inClause("id", ids)
		where = append(where, idClause)
		args = append(args, idArgs...)
	}

	res, err := d.write.ExecContext(ctx,
		`DELETE FROM upload_jobs WHERE `+strings.Join(where, " AND "), args...)
	if err != nil {
		return 0, Translate(err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// FinishedDownloadsToDelete resolves which download rows a deletion would
// remove, so the caller can unlink their cached files before the rows that
// point at them are gone.
func (d *DB) FinishedDownloadsToDelete(ctx context.Context, f TransferFilter, ids []string) ([]DownloadJob, error) {
	where, args := f.common(false)

	clause, extra := inClause("status", terminalStatuses)
	where = append(where, clause)
	args = append(args, extra...)

	if len(ids) > 0 {
		idClause, idArgs := inClause("id", ids)
		where = append(where, idClause)
		args = append(args, idArgs...)
	}

	rows, err := d.read.QueryContext(ctx,
		`SELECT `+downloadCols+` FROM download_jobs WHERE `+strings.Join(where, " AND "), args...)
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

// DeleteDownloads removes the named download rows.
func (d *DB) DeleteDownloads(ctx context.Context, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	clause, args := inClause("id", ids)
	res, err := d.write.ExecContext(ctx, `DELETE FROM download_jobs WHERE `+clause, args...)
	if err != nil {
		return 0, Translate(err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
