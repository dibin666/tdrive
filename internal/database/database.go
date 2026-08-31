// Package database opens and migrates tdrive's SQLite index.
//
// SQLite allows one writer at a time. Under upload load — several segments
// finishing at once, each committing a row — a single shared pool produces
// SQLITE_BUSY even with a generous busy_timeout, because Go hands concurrent
// writers different connections that then deadlock against each other's
// transactions. So this package keeps two pools: many connections for reads
// (WAL lets them proceed while a write is in flight) and exactly one for
// writes, which turns lock contention into ordinary queueing.
package database

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

//go:embed schema.sql
var schemaSQL string

// ErrNotFound is returned by repository lookups instead of sql.ErrNoRows so
// callers do not have to import database/sql.
var ErrNotFound = errors.New("database: not found")

// ErrConflict signals a uniqueness violation, which at this layer always means
// "a directory or file with that name already exists here".
var ErrConflict = errors.New("database: already exists")

// DB is the handle passed around the rest of the program.
type DB struct {
	read  *sql.DB
	write *sql.DB
	path  string
}

// Open creates the database file if needed, applies migrations and returns a
// ready handle.
func Open(ctx context.Context, path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}

	write, err := openPool(path, true)
	if err != nil {
		return nil, err
	}
	read, err := openPool(path, false)
	if err != nil {
		write.Close()
		return nil, err
	}

	db := &DB{read: read, write: write, path: path}
	if err := db.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func openPool(path string, writer bool) (*sql.DB, error) {
	// synchronous=NORMAL is the standard WAL pairing: a crash can lose the
	// last commits but cannot corrupt the file, and this database is
	// rebuildable from Telegram anyway.
	pragmas := []string{
		"_pragma=journal_mode(WAL)",
		"_pragma=synchronous(NORMAL)",
		"_pragma=foreign_keys(1)",
		"_pragma=busy_timeout(10000)",
	}
	dsn := "file:" + path + "?" + strings.Join(pragmas, "&")

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}

	if writer {
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(8)
	}
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite %q: %w", path, err)
	}
	return db, nil
}

// schemaVersion is what schema.sql describes. Anything older is brought up to
// it by the steps in migrate.
const schemaVersion = 6

// upgradeSteps are the statements that take an existing database from the
// version keyed here to the next one. A fresh database skips all of them,
// because schema.sql already describes the final shape — which means every
// step added here must also be reflected in schema.sql.
var upgradeSteps = map[int][]string{
	1: {
		`ALTER TABLE upload_jobs ADD COLUMN source TEXT NOT NULL DEFAULT 'webui'`,
		`UPDATE upload_jobs SET source = 'remote' WHERE source_url <> ''`,
	},
	2: {
		// Fine-grained accounts. perms = 0 keeps existing accounts on their
		// role's defaults, so an upgrade changes nobody's access.
		`ALTER TABLE users ADD COLUMN enabled INTEGER NOT NULL DEFAULT 1`,
		`ALTER TABLE users ADD COLUMN perms INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE users ADD COLUMN scope_path TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN quota_bytes INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE users ADD COLUMN note TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN last_login_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE users ADD COLUMN last_login_ip TEXT NOT NULL DEFAULT ''`,

		`ALTER TABLE refresh_tokens ADD COLUMN user_agent TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE refresh_tokens ADD COLUMN ip TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE refresh_tokens ADD COLUMN last_used_at INTEGER NOT NULL DEFAULT 0`,

		// Ownership. Existing rows stay NULL: nothing knows who uploaded them,
		// and inventing an owner would produce wrong quota numbers. A rebuild
		// fills these in from the captions written after this release.
		`ALTER TABLE files ADD COLUMN owner_id TEXT REFERENCES users (id) ON DELETE SET NULL`,
		`ALTER TABLE dirs ADD COLUMN owner_id TEXT REFERENCES users (id) ON DELETE SET NULL`,
		`CREATE INDEX IF NOT EXISTS idx_files_owner ON files (owner_id)`,

		`ALTER TABLE upload_jobs ADD COLUMN started_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE upload_jobs ADD COLUMN finished_at INTEGER NOT NULL DEFAULT 0`,
		// Old rows have no timing, so the best available bracket is the row's
		// own lifetime. It overstates duration for jobs that queued, which is
		// the conservative direction for a speed number.
		`UPDATE upload_jobs SET started_at = created_at, finished_at = updated_at
		 WHERE status IN ('complete', 'failed', 'cancelled')`,
		`CREATE INDEX IF NOT EXISTS idx_jobs_created ON upload_jobs (created_at)`,

		`CREATE TABLE download_jobs (
			id               TEXT PRIMARY KEY,
			user_id          TEXT REFERENCES users (id) ON DELETE CASCADE,
			file_id          TEXT REFERENCES files (id) ON DELETE SET NULL,
			name             TEXT NOT NULL,
			total_size       INTEGER NOT NULL,
			downloaded_bytes INTEGER NOT NULL DEFAULT 0,
			mode             TEXT NOT NULL DEFAULT 'direct'
			                 CHECK (mode IN ('direct', 'staged', 'segments')),
			status           TEXT NOT NULL
			                 CHECK (status IN ('pending', 'running', 'ready', 'complete', 'failed', 'cancelled', 'expired')),
			error            TEXT NOT NULL DEFAULT '',
			cache_path       TEXT NOT NULL DEFAULT '',
			created_at       INTEGER NOT NULL,
			updated_at       INTEGER NOT NULL,
			started_at       INTEGER NOT NULL DEFAULT 0,
			finished_at      INTEGER NOT NULL DEFAULT 0,
			expires_at       INTEGER NOT NULL DEFAULT 0,
			last_used_at     INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX idx_downloads_user ON download_jobs (user_id)`,
		`CREATE INDEX idx_downloads_status ON download_jobs (status)`,
		`CREATE INDEX idx_downloads_created ON download_jobs (created_at)`,
		`CREATE INDEX idx_downloads_used ON download_jobs (last_used_at)`,

		`CREATE TABLE share_links (
			id           TEXT PRIMARY KEY,
			user_id      TEXT REFERENCES users (id) ON DELETE CASCADE,
			file_id      TEXT NOT NULL REFERENCES files (id) ON DELETE CASCADE,
			token_hash   BLOB NOT NULL UNIQUE,
			kind         TEXT NOT NULL DEFAULT 'file' CHECK (kind IN ('file', 'segment')),
			label        TEXT NOT NULL DEFAULT '',
			expires_at   INTEGER NOT NULL DEFAULT 0,
			revoked      INTEGER NOT NULL DEFAULT 0,
			hits         INTEGER NOT NULL DEFAULT 0,
			created_at   INTEGER NOT NULL,
			last_used_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX idx_shares_file ON share_links (file_id)`,
		`CREATE INDEX idx_shares_user ON share_links (user_id)`,

		`CREATE TABLE audit_log (
			id         TEXT PRIMARY KEY,
			at         INTEGER NOT NULL,
			actor_id   TEXT,
			actor_name TEXT NOT NULL DEFAULT '',
			action     TEXT NOT NULL,
			target     TEXT NOT NULL DEFAULT '',
			detail     TEXT NOT NULL DEFAULT '',
			ip         TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX idx_audit_at ON audit_log (at)`,
		`CREATE INDEX idx_audit_actor ON audit_log (actor_id)`,
	},
	3: {
		// A WebDAV read is now recorded as a download so it shows up in the
		// transfer panel, and 'webdav' is a mode the old CHECK constraint would
		// reject. SQLite cannot alter a constraint in place, so the table is
		// rebuilt — which also means its indexes have to be recreated, since
		// they go with the dropped table.
		`CREATE TABLE download_jobs_v4 (
			id               TEXT PRIMARY KEY,
			user_id          TEXT REFERENCES users (id) ON DELETE CASCADE,
			file_id          TEXT REFERENCES files (id) ON DELETE SET NULL,
			name             TEXT NOT NULL,
			total_size       INTEGER NOT NULL,
			downloaded_bytes INTEGER NOT NULL DEFAULT 0,
			mode             TEXT NOT NULL DEFAULT 'direct'
			                 CHECK (mode IN ('direct', 'staged', 'segments', 'webdav')),
			status           TEXT NOT NULL
			                 CHECK (status IN ('pending', 'running', 'ready', 'complete', 'failed', 'cancelled', 'expired')),
			error            TEXT NOT NULL DEFAULT '',
			cache_path       TEXT NOT NULL DEFAULT '',
			created_at       INTEGER NOT NULL,
			updated_at       INTEGER NOT NULL,
			started_at       INTEGER NOT NULL DEFAULT 0,
			finished_at      INTEGER NOT NULL DEFAULT 0,
			expires_at       INTEGER NOT NULL DEFAULT 0,
			last_used_at     INTEGER NOT NULL DEFAULT 0
		)`,
		`INSERT INTO download_jobs_v4
		 SELECT id, user_id, file_id, name, total_size, downloaded_bytes, mode, status,
		        error, cache_path, created_at, updated_at, started_at, finished_at,
		        expires_at, last_used_at
		 FROM download_jobs`,
		`DROP TABLE download_jobs`,
		`ALTER TABLE download_jobs_v4 RENAME TO download_jobs`,
		`CREATE INDEX idx_downloads_user ON download_jobs (user_id)`,
		`CREATE INDEX idx_downloads_status ON download_jobs (status)`,
		`CREATE INDEX idx_downloads_created ON download_jobs (created_at)`,
		`CREATE INDEX idx_downloads_used ON download_jobs (last_used_at)`,
	},
	4: {
		`CREATE TABLE IF NOT EXISTS plugins (
			id            TEXT PRIMARY KEY,
			name          TEXT NOT NULL,
			version       TEXT NOT NULL,
			author        TEXT NOT NULL,
			enabled       INTEGER NOT NULL DEFAULT 1,
			status        TEXT NOT NULL DEFAULT 'disabled',
			source        TEXT NOT NULL,
			source_url    TEXT NOT NULL,
			ref           TEXT NOT NULL DEFAULT '',
			source_digest TEXT NOT NULL,
			binary_digest TEXT NOT NULL,
			binary_path   TEXT NOT NULL,
			manifest_json TEXT NOT NULL,
			error         TEXT NOT NULL DEFAULT '',
			installed_at  INTEGER NOT NULL,
			updated_at    INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_plugins_enabled ON plugins (enabled)`,
		`CREATE TABLE IF NOT EXISTS plugin_data (
			plugin_id  TEXT NOT NULL REFERENCES plugins (id) ON DELETE CASCADE,
			key        TEXT NOT NULL,
			value      BLOB NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (plugin_id, key)
		) WITHOUT ROWID`,
	},
	5: {
		// Several Telegram accounts instead of one. Only the structure is
		// created here; the existing credentials and channel access hash are
		// carried into the new tables by SeedPrimaryAccount, which also has to
		// run for a database created fresh at this version.
		`CREATE TABLE IF NOT EXISTS tg_accounts (
			id           TEXT PRIMARY KEY,
			label        TEXT NOT NULL DEFAULT '',
			app_id       INTEGER NOT NULL,
			app_hash     TEXT NOT NULL,
			session_file TEXT NOT NULL,
			enabled      INTEGER NOT NULL DEFAULT 1,
			is_primary   INTEGER NOT NULL DEFAULT 0,
			tg_user_id   INTEGER NOT NULL DEFAULT 0,
			username     TEXT NOT NULL DEFAULT '',
			phone        TEXT NOT NULL DEFAULT '',
			position     INTEGER NOT NULL DEFAULT 0,
			created_at   INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_tg_accounts_enabled ON tg_accounts (enabled)`,
		`CREATE TABLE IF NOT EXISTS channel_accounts (
			channel_id  TEXT NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
			account_id  TEXT NOT NULL REFERENCES tg_accounts (id) ON DELETE CASCADE,
			access_hash INTEGER NOT NULL,
			can_post    INTEGER NOT NULL DEFAULT 0,
			checked_at  INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (channel_id, account_id)
		) WITHOUT ROWID`,
		`ALTER TABLE segments ADD COLUMN account_id TEXT NOT NULL DEFAULT ''`,
	},
}

// migrate applies schema.sql once. The schema is versioned through SQLite's
// user_version so that a later release can add steps without a separate
// migrations table.
func (d *DB) migrate(ctx context.Context) error {
	var version int
	if err := d.write.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version >= schemaVersion {
		return nil
	}

	tx, err := d.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer tx.Rollback()

	if version == 0 {
		if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
	} else {
		for from := version; from < schemaVersion; from++ {
			for _, stmt := range upgradeSteps[from] {
				if _, err := tx.ExecContext(ctx, stmt); err != nil {
					if isAlreadyApplied(stmt, err) {
						continue
					}
					return fmt.Errorf("migrate schema v%d to v%d: %w", from, from+1, err)
				}
			}
		}
	}

	// PRAGMA does not accept a bound parameter, and the value is a constant.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}
	return tx.Commit()
}

// isAlreadyApplied recognises the one way a re-run of an upgrade step fails
// that is not a real error: adding a column the table already has.
//
// The other steps lean on CREATE TABLE IF NOT EXISTS for this, but SQLite has
// no ALTER TABLE ... ADD COLUMN IF NOT EXISTS, and a database can genuinely
// arrive at a step with the column present — one created from schema.sql, which
// always describes the final shape, and then rolled back to an older
// user_version. A failed statement does not poison the surrounding transaction,
// so skipping it and carrying on is safe.
func isAlreadyApplied(stmt string, err error) bool {
	if !strings.Contains(strings.ToUpper(stmt), "ADD COLUMN") {
		return false
	}
	return strings.Contains(err.Error(), "duplicate column name")
}

// Read returns the multi-connection pool. Use it for anything that does not
// write, including read-only transactions.
func (d *DB) Read() *sql.DB { return d.read }

// Write returns the single-connection pool. Every INSERT, UPDATE and DELETE
// must go through it.
func (d *DB) Write() *sql.DB { return d.write }

// Path is the on-disk location, used in log messages and the backup endpoint.
func (d *DB) Path() string { return d.path }

// txExec is the subset of *sql.Tx and *sql.DB that repository helpers need, so
// the same statement can run inside or outside a transaction.
type txExec interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Tx runs fn inside a write transaction, rolling back on error. Because the
// write pool has a single connection, transactions serialize here rather than
// failing with SQLITE_BUSY halfway through.
func (d *DB) Tx(ctx context.Context, fn func(tx txExec) error) error {
	tx, err := d.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// Close releases both pools.
func (d *DB) Close() error {
	var errs []error
	if d.read != nil {
		if err := d.read.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if d.write != nil {
		if err := d.write.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Translate maps driver errors onto this package's sentinels so that handlers
// can return 404 and 409 without inspecting SQLite error codes.
func Translate(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}

	var serr *sqlite.Error
	if errors.As(err, &serr) {
		switch serr.Code() {
		case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
			return fmt.Errorf("%w: %v", ErrConflict, err)
		}
	}
	return err
}

// nowMS is the single source of stored timestamps: Unix milliseconds, which
// sort correctly as INTEGER and survive a JSON round trip to the browser.
func nowMS() int64 { return time.Now().UnixMilli() }

// boolInt is SQLite's idea of a boolean.
func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// truncate bounds a display-only string before it is stored. User agents in
// particular are attacker-controlled and unbounded.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
