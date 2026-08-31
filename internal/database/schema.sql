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

-- One row per Telegram login. A deployment may hold several, because Telegram
-- meters FLOOD_WAIT and transfer quota per account: a second account is the
-- only way to get a second budget, and multiple api_id values on one phone
-- number buy nothing at all.
--
-- Every account is fully isolated — its own credentials, its own session file,
-- its own connection pool and its own share of the task limits — so one being
-- throttled never stalls the others.
CREATE TABLE tg_accounts (
    id           TEXT PRIMARY KEY,
    label        TEXT NOT NULL DEFAULT '',
    app_id       INTEGER NOT NULL,
    app_hash     TEXT NOT NULL,
    -- Relative to the data directory. The primary account keeps the historical
    -- session.json so an upgrade does not have to re-authenticate.
    session_file TEXT NOT NULL,
    enabled      INTEGER NOT NULL DEFAULT 1,
    -- The primary account owns the setup wizard, the index rebuild and the
    -- channel invites that add the others.
    is_primary   INTEGER NOT NULL DEFAULT 0,
    tg_user_id   INTEGER NOT NULL DEFAULT 0,
    username     TEXT NOT NULL DEFAULT '',
    phone        TEXT NOT NULL DEFAULT '',
    position     INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL
);
CREATE INDEX idx_tg_accounts_enabled ON tg_accounts (enabled);

CREATE TABLE channels (
    id          TEXT PRIMARY KEY,
    tg_id       INTEGER NOT NULL UNIQUE,
    -- The primary account's access hash. Every account resolves its own; this
    -- column stays as the primary's copy and as the fallback for a database
    -- written before accounts existed.
    access_hash INTEGER NOT NULL,
    title       TEXT NOT NULL,
    is_default  INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL
);

-- Telegram access hashes are minted per account: the value one account holds
-- for a channel is meaningless to another. can_post records whether the
-- account was actually admitted with posting rights, which is what decides
-- whether it may be scheduled for uploads.
CREATE TABLE channel_accounts (
    channel_id  TEXT NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    account_id  TEXT NOT NULL REFERENCES tg_accounts (id) ON DELETE CASCADE,
    access_hash INTEGER NOT NULL,
    can_post    INTEGER NOT NULL DEFAULT 0,
    checked_at  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (channel_id, account_id)
) WITHOUT ROWID;

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
    -- Which account uploaded this segment, and therefore whose access_hash and
    -- file_reference the two columns above hold. Another account reading this
    -- segment must re-resolve its own handle from tg_msg_id first. Empty means
    -- unknown, which is how rows written before accounts existed and rows
    -- recovered by an index rebuild are marked.
    account_id     TEXT NOT NULL DEFAULT '',
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
    -- webdav is a download the server did not start: a mounted client asked
    -- for the bytes, and the row exists so the transfer panel can show it.
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

-- Installed plugins are local configuration, not Telegram index data. They are
-- kept separate so an index rebuild can never enable, disable, or remove a
-- plugin. manifest_url and manifest_digest identify the published manifest the
-- administrator confirmed; the manifest itself is stored as received and
-- validated again before a binary is started.
CREATE TABLE plugins (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    version         TEXT NOT NULL,
    author          TEXT NOT NULL,
    enabled         INTEGER NOT NULL DEFAULT 1,
    status          TEXT NOT NULL DEFAULT 'disabled',
    source          TEXT NOT NULL,
    manifest_url    TEXT NOT NULL,
    manifest_digest TEXT NOT NULL,
    binary_digest   TEXT NOT NULL,
    binary_path     TEXT NOT NULL,
    manifest_json   TEXT NOT NULL,
    error           TEXT NOT NULL DEFAULT '',
    installed_at    INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);
CREATE INDEX idx_plugins_enabled ON plugins (enabled);

-- Namespaced plugin state is deliberately opaque to the core. A plugin can
-- persist small settings without opening the host database itself.
CREATE TABLE plugin_data (
    plugin_id  TEXT NOT NULL REFERENCES plugins (id) ON DELETE CASCADE,
    key        TEXT NOT NULL,
    value      BLOB NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (plugin_id, key)
) WITHOUT ROWID;
