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

// migrate applies schema.sql once. The schema is versioned through SQLite's
// user_version so that a later release can add steps without a separate
// migrations table.
func (d *DB) migrate(ctx context.Context) error {
	var version int
	if err := d.write.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version >= 2 {
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
	} else if version == 1 {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE upload_jobs ADD COLUMN source TEXT NOT NULL DEFAULT 'webui'`); err != nil {
			return fmt.Errorf("add source column: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE upload_jobs SET source = 'remote' WHERE source_url <> ''`); err != nil {
			return fmt.Errorf("migrate existing sources: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 2"); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}
	return tx.Commit()
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
