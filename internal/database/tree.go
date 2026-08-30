package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"unicode/utf8"
)

// nullable turns the empty string into SQL NULL. The root is modelled as a NULL
// parent rather than a sentinel row, so this conversion happens on every write
// that touches parent_id or dir_id.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func text(ns sql.NullString) string { return ns.String }

const dirCols = `id, parent_id, name, path, channel_id, tg_msg_id, created_at, updated_at, owner_id`

func scanDir(row interface{ Scan(...any) error }) (Dir, error) {
	var (
		d                      Dir
		parent, channel, owner sql.NullString
		created, updated       int64
	)
	err := row.Scan(&d.ID, &parent, &d.Name, &d.Path, &channel, &d.TGMsgID, &created, &updated, &owner)
	if err != nil {
		return Dir{}, Translate(err)
	}
	d.ParentID, d.ChannelID, d.OwnerID = text(parent), text(channel), text(owner)
	d.CreatedAt, d.UpdatedAt = msToTime(created), msToTime(updated)
	return d, nil
}

// InsertDir writes a directory row. The caller supplies the ID because the
// Telegram caption embeds it, and the caption has to be built before the
// message is sent.
func (d *DB) InsertDir(ctx context.Context, dir Dir) error {
	now := nowMS()
	_, err := d.write.ExecContext(ctx,
		`INSERT INTO dirs (`+dirCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		dir.ID, nullable(dir.ParentID), dir.Name, dir.Path, nullable(dir.ChannelID),
		dir.TGMsgID, now, now, nullable(dir.OwnerID))
	if err != nil {
		return fmt.Errorf("insert dir %q: %w", dir.Path, Translate(err))
	}
	return nil
}

func (d *DB) DirByID(ctx context.Context, id string) (Dir, error) {
	return scanDir(d.read.QueryRowContext(ctx, `SELECT `+dirCols+` FROM dirs WHERE id = ?`, id))
}

func (d *DB) DirByPath(ctx context.Context, path string) (Dir, error) {
	return scanDir(d.read.QueryRowContext(ctx, `SELECT `+dirCols+` FROM dirs WHERE path = ?`, path))
}

// DirChild resolves one path element inside a parent, which is what path
// walking needs.
func (d *DB) DirChild(ctx context.Context, parentID, name string) (Dir, error) {
	return scanDir(d.read.QueryRowContext(ctx,
		`SELECT `+dirCols+` FROM dirs WHERE ifnull(parent_id, '') = ? AND name = ?`,
		parentID, name))
}

func (d *DB) ListDirs(ctx context.Context, parentID string) ([]Dir, error) {
	rows, err := d.read.QueryContext(ctx,
		`SELECT `+dirCols+` FROM dirs WHERE ifnull(parent_id, '') = ? ORDER BY name`, parentID)
	if err != nil {
		return nil, Translate(err)
	}
	defer rows.Close()

	var out []Dir
	for rows.Next() {
		dir, err := scanDir(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dir)
	}
	return out, Translate(rows.Err())
}

// SubtreeSizes totals the stored bytes under each child directory of parentID,
// keyed by directory id.
//
// A folder's size is everything below it, not just the files sitting directly
// inside, which is what a file manager shows and what the listing previously
// had no way to report at all. One recursive walk answers for every child at
// once: a query per row would turn a forty-folder listing into forty round
// trips. Pending files are excluded for the same reason ListFiles excludes
// them — their bytes are not in the drive yet.
func (d *DB) SubtreeSizes(ctx context.Context, parentID string) (map[string]int64, error) {
	const q = `
		WITH RECURSIVE subtree(root, id) AS (
		    SELECT id, id FROM dirs WHERE ifnull(parent_id, '') = ?
		    UNION ALL
		    SELECT subtree.root, d.id FROM dirs d JOIN subtree ON d.parent_id = subtree.id
		)
		SELECT subtree.root, ifnull(sum(f.size), 0)
		FROM subtree
		LEFT JOIN files f ON f.dir_id = subtree.id AND f.status <> 'pending'
		GROUP BY subtree.root`

	rows, err := d.read.QueryContext(ctx, q, parentID)
	if err != nil {
		return nil, Translate(err)
	}
	defer rows.Close()

	out := make(map[string]int64)
	for rows.Next() {
		var (
			id   string
			size int64
		)
		if err := rows.Scan(&id, &size); err != nil {
			return nil, Translate(err)
		}
		out[id] = size
	}
	return out, Translate(rows.Err())
}

// AllDirs returns the whole tree, used by the sidebar and by the indexer when
// it rebuilds paths.
func (d *DB) AllDirs(ctx context.Context) ([]Dir, error) {
	rows, err := d.read.QueryContext(ctx, `SELECT `+dirCols+` FROM dirs ORDER BY path`)
	if err != nil {
		return nil, Translate(err)
	}
	defer rows.Close()

	var out []Dir
	for rows.Next() {
		dir, err := scanDir(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dir)
	}
	return out, Translate(rows.Err())
}

// RenameDir updates a directory and rewrites the stored path of every
// descendant. Paths are denormalised so that WebDAV PROPFIND and the UI can
// resolve a path in one indexed lookup; the cost is this rewrite, which is a
// single UPDATE rather than a tree walk.
func (d *DB) RenameDir(ctx context.Context, id, newName, newPath string) error {
	return d.Tx(ctx, func(tx txExec) error {
		var oldPath string
		err := tx.QueryRowContext(ctx, `SELECT path FROM dirs WHERE id = ?`, id).Scan(&oldPath)
		if err != nil {
			return Translate(err)
		}

		res, err := tx.ExecContext(ctx,
			`UPDATE dirs SET name = ?, path = ?, updated_at = ? WHERE id = ?`,
			newName, newPath, nowMS(), id)
		if err := affectedOne(res, err, "rename dir"); err != nil {
			return err
		}

		// Rewrite descendants by swapping the path prefix. The LIKE pattern
		// includes the separator so a sibling like "/Movies2" is untouched
		// while renaming "/Movies".
		_, err = tx.ExecContext(ctx,
			`UPDATE dirs SET path = ? || substr(path, ?), updated_at = ?
			 WHERE path LIKE ? ESCAPE '\'`,
			newPath, prefixCut(oldPath), nowMS(), escapeLike(oldPath)+`/%`)
		return Translate(err)
	})
}

// MoveDir reparents a directory. The caller is responsible for rejecting a move
// into the directory's own subtree; VFS does that check because it knows the
// paths.
func (d *DB) MoveDir(ctx context.Context, id, newParentID, newPath string) error {
	return d.Tx(ctx, func(tx txExec) error {
		var oldPath string
		if err := tx.QueryRowContext(ctx, `SELECT path FROM dirs WHERE id = ?`, id).Scan(&oldPath); err != nil {
			return Translate(err)
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE dirs SET parent_id = ?, path = ?, updated_at = ? WHERE id = ?`,
			nullable(newParentID), newPath, nowMS(), id)
		if err := affectedOne(res, err, "move dir"); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE dirs SET path = ? || substr(path, ?), updated_at = ?
			 WHERE path LIKE ? ESCAPE '\'`,
			newPath, prefixCut(oldPath), nowMS(), escapeLike(oldPath)+`/%`)
		return Translate(err)
	})
}

// DeleteDir removes a directory; ON DELETE CASCADE takes the descendants and
// their files and segments with it. The Telegram messages are deleted
// separately by the caller, which collects them first via SubtreeSegments.
func (d *DB) DeleteDir(ctx context.Context, id string) error {
	res, err := d.write.ExecContext(ctx, `DELETE FROM dirs WHERE id = ?`, id)
	return affectedOne(res, err, "delete dir")
}

const fileCols = `id, dir_id, name, size, mime, segment_size, segment_count, status, channel_id, created_at, updated_at, owner_id`

func scanFile(row interface{ Scan(...any) error }) (File, error) {
	var (
		f                   File
		dir, channel, owner sql.NullString
		created, updated    int64
	)
	err := row.Scan(&f.ID, &dir, &f.Name, &f.Size, &f.MIME, &f.SegmentSize,
		&f.SegmentCount, &f.Status, &channel, &created, &updated, &owner)
	if err != nil {
		return File{}, Translate(err)
	}
	f.DirID, f.ChannelID, f.OwnerID = text(dir), text(channel), text(owner)
	f.CreatedAt, f.UpdatedAt = msToTime(created), msToTime(updated)
	return f, nil
}

// InsertFile creates the file row up front, in pending status, so that a
// crashed upload leaves a resumable record instead of orphaned Telegram
// messages nobody can find.
func (d *DB) InsertFile(ctx context.Context, f File) error {
	now := nowMS()
	_, err := d.write.ExecContext(ctx,
		`INSERT INTO files (`+fileCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.ID, nullable(f.DirID), f.Name, f.Size, f.MIME, f.SegmentSize,
		f.SegmentCount, string(f.Status), nullable(f.ChannelID), now, now, nullable(f.OwnerID))
	if err != nil {
		return fmt.Errorf("insert file %q: %w", f.Name, Translate(err))
	}
	return nil
}

func (d *DB) FileByID(ctx context.Context, id string) (File, error) {
	return scanFile(d.read.QueryRowContext(ctx, `SELECT `+fileCols+` FROM files WHERE id = ?`, id))
}

// FileInDir looks up a file by name, restricted to complete files because a
// half-uploaded file must not be readable through WebDAV.
func (d *DB) FileInDir(ctx context.Context, dirID, name string) (File, error) {
	return scanFile(d.read.QueryRowContext(ctx,
		`SELECT `+fileCols+` FROM files WHERE ifnull(dir_id, '') = ? AND name = ?`, dirID, name))
}

// ListFiles returns the visible files of a directory. Pending uploads are
// excluded: they have no readable bytes yet.
func (d *DB) ListFiles(ctx context.Context, dirID string) ([]File, error) {
	rows, err := d.read.QueryContext(ctx,
		`SELECT `+fileCols+` FROM files
		 WHERE ifnull(dir_id, '') = ? AND status <> 'pending' ORDER BY name`, dirID)
	if err != nil {
		return nil, Translate(err)
	}
	defer rows.Close()

	var out []File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, Translate(rows.Err())
}

func (d *DB) UpdateFileStatus(ctx context.Context, id string, status FileStatus) error {
	res, err := d.write.ExecContext(ctx,
		`UPDATE files SET status = ?, updated_at = ? WHERE id = ?`, string(status), nowMS(), id)
	return affectedOne(res, err, "update file status")
}

// SetFileSize corrects the recorded size once an upload of unknown length
// finishes, which happens for chunked WebDAV PUTs and some remote URLs.
func (d *DB) SetFileSize(ctx context.Context, id string, size int64, segmentCount int) error {
	res, err := d.write.ExecContext(ctx,
		`UPDATE files SET size = ?, segment_count = ?, updated_at = ? WHERE id = ?`,
		size, segmentCount, nowMS(), id)
	return affectedOne(res, err, "set file size")
}

func (d *DB) RenameFile(ctx context.Context, id, newName string) error {
	res, err := d.write.ExecContext(ctx,
		`UPDATE files SET name = ?, updated_at = ? WHERE id = ?`, newName, nowMS(), id)
	return affectedOne(res, err, "rename file")
}

func (d *DB) MoveFile(ctx context.Context, id, newDirID string) error {
	res, err := d.write.ExecContext(ctx,
		`UPDATE files SET dir_id = ?, updated_at = ? WHERE id = ?`, nullable(newDirID), nowMS(), id)
	return affectedOne(res, err, "move file")
}

func (d *DB) DeleteFile(ctx context.Context, id string) error {
	res, err := d.write.ExecContext(ctx, `DELETE FROM files WHERE id = ?`, id)
	return affectedOne(res, err, "delete file")
}

const segCols = `file_id, idx, size, tg_msg_id, tg_doc_id, access_hash, dc_id, file_reference`

func scanSegment(row interface{ Scan(...any) error }) (Segment, error) {
	var s Segment
	err := row.Scan(&s.FileID, &s.Index, &s.Size, &s.TGMsgID, &s.TGDocID,
		&s.AccessHash, &s.DCID, &s.FileReference)
	if err != nil {
		return Segment{}, Translate(err)
	}
	return s, nil
}

// UpsertSegment records a landed segment. Upsert rather than insert because a
// resumed upload may re-send a segment whose row was already committed before
// the crash.
func (d *DB) UpsertSegment(ctx context.Context, s Segment) error {
	_, err := d.write.ExecContext(ctx,
		`INSERT INTO segments (`+segCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (file_id, idx) DO UPDATE SET
		   size = excluded.size, tg_msg_id = excluded.tg_msg_id,
		   tg_doc_id = excluded.tg_doc_id, access_hash = excluded.access_hash,
		   dc_id = excluded.dc_id, file_reference = excluded.file_reference`,
		s.FileID, s.Index, s.Size, s.TGMsgID, s.TGDocID, s.AccessHash, s.DCID, s.FileReference)
	if err != nil {
		return fmt.Errorf("upsert segment %d of %s: %w", s.Index, s.FileID, Translate(err))
	}
	return nil
}

// Segments returns a file's segments in index order, which is the order the
// reader stitches them in.
func (d *DB) Segments(ctx context.Context, fileID string) ([]Segment, error) {
	rows, err := d.read.QueryContext(ctx,
		`SELECT `+segCols+` FROM segments WHERE file_id = ? ORDER BY idx`, fileID)
	if err != nil {
		return nil, Translate(err)
	}
	defer rows.Close()

	var out []Segment
	for rows.Next() {
		s, err := scanSegment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, Translate(rows.Err())
}

// RefreshFileReference writes back a file reference that had to be re-resolved
// after Telegram expired the cached one.
func (d *DB) RefreshFileReference(ctx context.Context, fileID string, idx int, ref []byte) error {
	_, err := d.write.ExecContext(ctx,
		`UPDATE segments SET file_reference = ? WHERE file_id = ? AND idx = ?`, ref, fileID, idx)
	return Translate(err)
}

// SetSegmentDC records where a document really lives after Telegram answered a
// read with FILE_MIGRATE, so later reads go straight to the right datacenter.
func (d *DB) SetSegmentDC(ctx context.Context, fileID string, idx, dc int) error {
	_, err := d.write.ExecContext(ctx,
		`UPDATE segments SET dc_id = ? WHERE file_id = ? AND idx = ?`, dc, fileID, idx)
	return Translate(err)
}

// TGMessage locates one Telegram message, used when deleting or when editing a
// caption during a rename.
type TGMessage struct {
	ChannelID string
	MsgID     int
}

// SubtreeMessages collects every Telegram message under a directory: the
// directory records themselves and every segment of every file. Callers gather
// these before deleting rows, because the cascade erases the coordinates.
func (d *DB) SubtreeMessages(ctx context.Context, dirID string) ([]TGMessage, error) {
	var root string
	if err := d.read.QueryRowContext(ctx, `SELECT path FROM dirs WHERE id = ?`, dirID).Scan(&root); err != nil {
		return nil, Translate(err)
	}
	like := escapeLike(root) + `/%`

	const q = `
		WITH subtree AS (
		    SELECT id, channel_id, tg_msg_id FROM dirs
		    WHERE id = ?1 OR path LIKE ?2 ESCAPE '\'
		)
		SELECT channel_id, tg_msg_id FROM subtree WHERE tg_msg_id > 0
		UNION ALL
		SELECT f.channel_id, s.tg_msg_id
		FROM files f
		JOIN segments s ON s.file_id = f.id
		WHERE f.dir_id IN (SELECT id FROM subtree)`

	rows, err := d.read.QueryContext(ctx, q, dirID, like)
	if err != nil {
		return nil, Translate(err)
	}
	defer rows.Close()

	var out []TGMessage
	for rows.Next() {
		var (
			ch  sql.NullString
			mid int
		)
		if err := rows.Scan(&ch, &mid); err != nil {
			return nil, Translate(err)
		}
		out = append(out, TGMessage{ChannelID: ch.String, MsgID: mid})
	}
	return out, Translate(rows.Err())
}

// FileMessages lists the Telegram messages backing one file, in segment order.
func (d *DB) FileMessages(ctx context.Context, fileID string) ([]TGMessage, error) {
	rows, err := d.read.QueryContext(ctx,
		`SELECT f.channel_id, s.tg_msg_id FROM files f
		 JOIN segments s ON s.file_id = f.id
		 WHERE f.id = ? ORDER BY s.idx`, fileID)
	if err != nil {
		return nil, Translate(err)
	}
	defer rows.Close()

	var out []TGMessage
	for rows.Next() {
		var (
			ch  sql.NullString
			mid int
		)
		if err := rows.Scan(&ch, &mid); err != nil {
			return nil, Translate(err)
		}
		out = append(out, TGMessage{ChannelID: ch.String, MsgID: mid})
	}
	return out, Translate(rows.Err())
}

// ReplaceIndex swaps in a freshly rebuilt index in one transaction.
//
// The old rows are deleted and the new ones inserted together, so a rebuild
// that fails partway leaves the existing index intact rather than half-erased.
// dirs must be ordered parents-first for the self-referencing foreign key on
// dirs.parent_id to hold at every step.
func (d *DB) ReplaceIndex(ctx context.Context, dirs []Dir, files []File, segments []Segment) error {
	// Owner ids come out of Telegram captions, so they can name an account
	// that has since been deleted. The foreign key would reject the whole
	// rebuild over it, which would be a spectacularly bad trade: the file is
	// real, the owner is only bookkeeping. Unknown owners become NULL.
	known, err := d.knownUserIDs(ctx)
	if err != nil {
		return err
	}
	ownerOf := func(id string) any {
		if id == "" || !known[id] {
			return nil
		}
		return id
	}

	return d.Tx(ctx, func(tx txExec) error {
		// Deleting dirs cascades to files and segments, but the root-level
		// files have no directory to cascade from, so they go explicitly.
		for _, stmt := range []string{
			`DELETE FROM segments`,
			`DELETE FROM files`,
			`DELETE FROM dirs`,
		} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("clear index: %w", Translate(err))
			}
		}

		now := nowMS()
		for _, dir := range dirs {
			_, err := tx.ExecContext(ctx,
				`INSERT INTO dirs (`+dirCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				dir.ID, nullable(dir.ParentID), dir.Name, dir.Path,
				nullable(dir.ChannelID), dir.TGMsgID, now, now, ownerOf(dir.OwnerID))
			if err != nil {
				return fmt.Errorf("rebuild dir %q: %w", dir.Path, Translate(err))
			}
		}
		for _, f := range files {
			_, err := tx.ExecContext(ctx,
				`INSERT INTO files (`+fileCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				f.ID, nullable(f.DirID), f.Name, f.Size, f.MIME, f.SegmentSize,
				f.SegmentCount, string(f.Status), nullable(f.ChannelID), now, now,
				ownerOf(f.OwnerID))
			if err != nil {
				return fmt.Errorf("rebuild file %q: %w", f.Name, Translate(err))
			}
		}
		for _, s := range segments {
			_, err := tx.ExecContext(ctx,
				`INSERT INTO segments (`+segCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				s.FileID, s.Index, s.Size, s.TGMsgID, s.TGDocID,
				s.AccessHash, s.DCID, s.FileReference)
			if err != nil {
				return fmt.Errorf("rebuild segment %d of %s: %w", s.Index, s.FileID, Translate(err))
			}
		}
		return nil
	})
}

// Stats backs the storage panel in the UI.
type Stats struct {
	Dirs         int   `json:"dirs"`
	Files        int   `json:"files"`
	Segments     int   `json:"segments"`
	TotalBytes   int64 `json:"totalBytes"`
	BrokenFiles  int   `json:"brokenFiles"`
	PendingFiles int   `json:"pendingFiles"`
}

func (d *DB) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	row := d.read.QueryRowContext(ctx, `
		SELECT (SELECT count(*) FROM dirs),
		       (SELECT count(*) FROM files WHERE status = 'complete'),
		       (SELECT count(*) FROM segments),
		       (SELECT ifnull(sum(size), 0) FROM files WHERE status = 'complete'),
		       (SELECT count(*) FROM files WHERE status = 'broken'),
		       (SELECT count(*) FROM files WHERE status = 'pending')`)
	err := row.Scan(&s.Dirs, &s.Files, &s.Segments, &s.TotalBytes, &s.BrokenFiles, &s.PendingFiles)
	return s, Translate(err)
}

// knownUserIDs is the set of accounts that currently exist, used to keep a
// rebuild from tripping the owner foreign key on an account that is gone.
func (d *DB) knownUserIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := d.read.QueryContext(ctx, `SELECT id FROM users`)
	if err != nil {
		return nil, Translate(err)
	}
	defer rows.Close()

	out := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, Translate(err)
		}
		out[id] = true
	}
	return out, Translate(rows.Err())
}

// escapeLike neutralises the wildcards in a stored path so that a directory
// literally named "100%" cannot match its siblings during a subtree rewrite.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// prefixCut is the 1-based SQLite substr() offset just past prefix. SQLite
// counts characters in substr() while Go's len counts bytes, so a path with
// non-ASCII names — the common case here, since folders get named in Chinese —
// would be sliced in the middle of a rune if the byte length were used.
func prefixCut(prefix string) int {
	return utf8.RuneCountInString(prefix) + 1
}
