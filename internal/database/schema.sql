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
    updated_at    INTEGER NOT NULL,
    -- A disabled account keeps its files and its rows but cannot log in, and
    -- cannot mount WebDAV either. Deleting is the destructive option; this is
    -- the reversible one.
    enabled       INTEGER NOT NULL DEFAULT 1,
    -- perms is a bitmask over database.Perm. Zero is not "no permissions": it
    -- means "inherit the role's defaults", so accounts created before
    -- fine-grained permissions existed keep behaving exactly as they did.
    perms         INTEGER NOT NULL DEFAULT 0,
    -- scope_path confines an account to one subtree. Empty means the whole
    -- drive, which is what every account gets by default.
    scope_path    TEXT NOT NULL DEFAULT '',
    -- quota_bytes caps the total size of files this account owns. 0 disables
    -- the check.
    quota_bytes   INTEGER NOT NULL DEFAULT 0,
    note          TEXT NOT NULL DEFAULT '',
    last_login_at INTEGER NOT NULL DEFAULT 0,
    last_login_ip TEXT NOT NULL DEFAULT ''
);

-- Access tokens are stateless JWTs; only the long-lived refresh token is
-- stored, hashed, so that logging out actually revokes something.
--
-- user_agent, ip and last_used_at exist purely so the session list in the UI
-- can say something more useful than "a session".
CREATE TABLE refresh_tokens (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash   BLOB NOT NULL UNIQUE,
    expires_at   INTEGER NOT NULL,
    revoked      INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL,
    user_agent   TEXT NOT NULL DEFAULT '',
    ip           TEXT NOT NULL DEFAULT '',
    last_used_at INTEGER NOT NULL DEFAULT 0
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
    updated_at INTEGER NOT NULL,
    owner_id   TEXT REFERENCES users (id) ON DELETE SET NULL
);
CREATE UNIQUE INDEX idx_dirs_parent_name ON dirs (ifnull(parent_id, ''), name);
CREATE INDEX idx_dirs_parent ON dirs (parent_id);

-- One row per logical file, whatever its size. size is the logical total; the
-- fact that it may span several Telegram objects lives in segments.
--
-- owner_id is who uploaded it, and it is also written into the Telegram
-- caption as #own_, so a rebuilt index restores ownership — and therefore
-- quota accounting — rather than quietly zeroing it.
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
    updated_at    INTEGER NOT NULL,
    owner_id      TEXT REFERENCES users (id) ON DELETE SET NULL
);
CREATE UNIQUE INDEX idx_files_dir_name ON files (ifnull(dir_id, ''), name);
CREATE INDEX idx_files_dir ON files (dir_id);
CREATE INDEX idx_files_status ON files (status);
CREATE INDEX idx_files_owner ON files (owner_id);

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
--
-- started_at and finished_at bracket the time bytes were actually moving.
-- created_at cannot stand in for started_at, because a job that waited in the
-- concurrency queue for ten minutes would otherwise report an average speed
-- that is mostly queueing.
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
    updated_at     INTEGER NOT NULL,
    started_at     INTEGER NOT NULL DEFAULT 0,
    finished_at    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_jobs_user ON upload_jobs (user_id);
CREATE INDEX idx_jobs_status ON upload_jobs (status);
CREATE INDEX idx_jobs_created ON upload_jobs (created_at);

-- A download is a first-class task for the same reason an upload is: it can
-- take an hour, it competes for the same Telegram budget, and a user wants to
-- see it afterwards.
--
-- mode says how the bytes reach the browser:
--   direct   the browser reads straight through the server from Telegram
--   staged   the server first assembles the whole file under cache_path, then
--            serves it from local disk at VPS speed
--   segments the browser fetches each stored segment separately and joins them
-- Only staged occupies disk, and only staged has a cache_path.
CREATE TABLE download_jobs (
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
    -- expires_at is when the staged copy may be evicted. last_used_at drives
    -- LRU eviction when the cache is over its size limit.
    expires_at       INTEGER NOT NULL DEFAULT 0,
    last_used_at     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_downloads_user ON download_jobs (user_id);
CREATE INDEX idx_downloads_status ON download_jobs (status);
CREATE INDEX idx_downloads_created ON download_jobs (created_at);
CREATE INDEX idx_downloads_used ON download_jobs (last_used_at);

-- A share link is a durable, revocable capability to read one file's bytes.
--
-- The short-lived media token signed by internal/auth stays as it is: it is
-- for a <video> element inside an authenticated session. This table is for the
-- other case — a URL a person pastes into aria2, IDM or a phone, which has to
-- keep working tomorrow and has to be revocable.
--
-- Only the hash is stored, exactly like refresh tokens: leaking this table
-- must not hand out working links.
CREATE TABLE share_links (
    id           TEXT PRIMARY KEY,
    user_id      TEXT REFERENCES users (id) ON DELETE CASCADE,
    file_id      TEXT NOT NULL REFERENCES files (id) ON DELETE CASCADE,
    token_hash   BLOB NOT NULL UNIQUE,
    -- kind 'file' serves the whole logical file; 'segment' serves one stored
    -- segment, which is what the split-download mode hands out.
    kind         TEXT NOT NULL DEFAULT 'file' CHECK (kind IN ('file', 'segment')),
    label        TEXT NOT NULL DEFAULT '',
    -- 0 means the link does not expire.
    expires_at   INTEGER NOT NULL DEFAULT 0,
    revoked      INTEGER NOT NULL DEFAULT 0,
    hits         INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_shares_file ON share_links (file_id);
CREATE INDEX idx_shares_user ON share_links (user_id);

-- Who changed what. This is the one table that is not reconstructible from
-- Telegram, which is precisely why it exists: when an administrator asks why
-- an account vanished, nothing else can answer.
CREATE TABLE audit_log (
    id         TEXT PRIMARY KEY,
    at         INTEGER NOT NULL,
    actor_id   TEXT,
    actor_name TEXT NOT NULL DEFAULT '',
    action     TEXT NOT NULL,
    target     TEXT NOT NULL DEFAULT '',
    detail     TEXT NOT NULL DEFAULT '',
    ip         TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_audit_at ON audit_log (at);
CREATE INDEX idx_audit_actor ON audit_log (actor_id);
