-- tdrive schema.
--
-- This database is an index, not the system of record. Every row here can be
-- reconstructed by replaying the Telegram channel through internal/indexer, so
-- migrations are free to be destructive about derived data but must never lose
-- the Telegram coordinates (channel, message id, document id) that make
-- recovery possible.

CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE COLLATE NOCASE,
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL CHECK (role IN ('admin', 'user')),
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);

-- Access tokens are stateless JWTs; only the long-lived refresh token is
-- stored, hashed, so that logging out actually revokes something.
CREATE TABLE refresh_tokens (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash BLOB NOT NULL UNIQUE,
    expires_at INTEGER NOT NULL,
    revoked    INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
);
CREATE INDEX idx_refresh_user ON refresh_tokens (user_id);
CREATE INDEX idx_refresh_expires ON refresh_tokens (expires_at);

-- Small singleton values: the JWT signing secret, the Telegram app
-- credentials entered through the setup wizard, the active channel.
CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE channels (
    id          TEXT PRIMARY KEY,
    tg_id       INTEGER NOT NULL UNIQUE,
    access_hash INTEGER NOT NULL,
    title       TEXT NOT NULL,
    is_default  INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL
);

-- The drive root is not a row: parent_id IS NULL means "at the root", which
-- lines up with tagcodec's #pid_root.
CREATE TABLE dirs (
    id         TEXT PRIMARY KEY,
    parent_id  TEXT REFERENCES dirs (id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    path       TEXT NOT NULL UNIQUE,
    channel_id TEXT REFERENCES channels (id),
    tg_msg_id  INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_dirs_parent_name ON dirs (ifnull(parent_id, ''), name);
CREATE INDEX idx_dirs_parent ON dirs (parent_id);

-- One row per logical file, whatever its size. size is the logical total; the
-- fact that it may span several Telegram objects lives in segments.
CREATE TABLE files (
    id            TEXT PRIMARY KEY,
    dir_id        TEXT REFERENCES dirs (id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    size          INTEGER NOT NULL,
    mime          TEXT NOT NULL DEFAULT '',
    segment_size  INTEGER NOT NULL,
    segment_count INTEGER NOT NULL,
    -- pending: still uploading. complete: every segment present.
    -- broken: the indexer found gaps, surfaced in the UI rather than hidden.
    status        TEXT NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending', 'complete', 'broken')),
    channel_id    TEXT REFERENCES channels (id),
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_files_dir_name ON files (ifnull(dir_id, ''), name);
CREATE INDEX idx_files_dir ON files (dir_id);
CREATE INDEX idx_files_status ON files (status);

-- idx is 1-based to match tagcodec's #seg_i_n, so a caption and a row can be
-- compared without an off-by-one conversion in between.
--
-- file_reference is Telegram's anti-hotlinking token and expires after about
-- an hour. It is cached here only as a hint; readers refresh it on
-- FILE_REFERENCE_EXPIRED and write the new one back.
--
-- dc_id records which datacenter holds the document. It is usually the
-- account's home DC but not guaranteed, and reading from the wrong one fails
-- with FILE_MIGRATE, so the reader needs it to pick the right connection.
CREATE TABLE segments (
    file_id        TEXT NOT NULL REFERENCES files (id) ON DELETE CASCADE,
    idx            INTEGER NOT NULL,
    size           INTEGER NOT NULL,
    tg_msg_id      INTEGER NOT NULL,
    tg_doc_id      INTEGER NOT NULL,
    access_hash    INTEGER NOT NULL,
    dc_id          INTEGER NOT NULL DEFAULT 0,
    file_reference BLOB,
    PRIMARY KEY (file_id, idx)
) WITHOUT ROWID;

-- An upload survives a restart at segment granularity: done_mask is a bitset
-- over segment indices, so resuming re-sends only the segments that never
-- landed rather than the whole file.
-- user_id is nullable: a transfer can be started by the server rather than by
-- a person, and such a job must still be recorded and resumable.
CREATE TABLE upload_jobs (
    id             TEXT PRIMARY KEY,
    user_id        TEXT REFERENCES users (id) ON DELETE CASCADE,
    file_id        TEXT REFERENCES files (id) ON DELETE SET NULL,
    dir_id         TEXT,
    name           TEXT NOT NULL,
    total_size     INTEGER NOT NULL,
    segment_size   INTEGER NOT NULL,
    segment_count  INTEGER NOT NULL,
    done_mask      BLOB NOT NULL,
    uploaded_bytes INTEGER NOT NULL DEFAULT 0,
    status         TEXT NOT NULL
                   CHECK (status IN ('pending', 'running', 'complete', 'failed', 'cancelled')),
    error          TEXT NOT NULL DEFAULT '',
    source         TEXT NOT NULL DEFAULT 'webui',
    source_url     TEXT NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);
CREATE INDEX idx_jobs_user ON upload_jobs (user_id);
CREATE INDEX idx_jobs_status ON upload_jobs (status);
