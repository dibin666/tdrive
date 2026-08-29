# tdrive

Turn a Telegram account into a network drive: a web interface, a WebDAV mount,
and files larger than Telegram allows.

Written in Go, following the upload path of
[caamer20/Telegram-Drive](https://github.com/caamer20/Telegram-Drive) — a
Rust/Tauri desktop app — but as a server, and with the one thing that project
does not do: **files over 2 GB are split across several Telegram objects and
reassembled transparently on the way out.**

A 12 GB video is seven messages in your channel. In the web interface it is one
file. Over WebDAV it is one file. Scrub to the middle of it in the browser and
the seek lands wherever it needs to. Nothing above the storage layer knows the
file was ever split.

---

## Why the 2 GB limit exists, and what tdrive does about it

Telegram's `upload.saveBigFilePart` allows at most 4000 parts of at most
512 KiB. That product — 2000 MiB — is the hard ceiling on a single stored
object, and it is where the familiar "2 GB" figure comes from. Premium raises
what a client may *send*, not those two protocol numbers.

So tdrive splits. The default segment size is **1900 MiB** (exactly 3800 upload
parts, with headroom under the ceiling), configurable. Every segment but the
last is exactly that size, which is what lets a byte range be mapped onto a
segment with arithmetic instead of a lookup — a seek into the middle of a
40 GB file costs nothing.

## The channel is the system of record

The SQLite database is an index. It can be deleted, corrupted, or left behind
on an old machine, and the drive comes back.

Every directory is one message and every file segment is one message, each
carrying a structured caption:

```
影片 4K.mkv

#tdrive #v1 #file #id_01K2QG7YM4… #pid_01K2QF3XR8… #n_5LSN2ZLQOM…
#seg_3_7 #sz_13421772800 #ss_1992294400 #电影 #_2024
```

- `#id_` / `#pid_` rebuild the tree; `#n_` is the exact name, base32-encoded so
  spaces, slashes and emoji survive.
- `#seg_3_7` says which segment this is and how many there are.
- `#电影` and `#_2024` are the folder names as ordinary hashtags, so searching
  a Telegram client for `#电影` shows that folder's files. Searching `#id_…`
  shows every segment of one file.

**Settings → 重建索引** replays the channel and rebuilds everything from those
captions. It is the disaster-recovery path, and it doubles as the test that the
format carries enough: if a rebuild cannot reproduce the drive, the writer is
at fault.

## Performance

The reference implementation streams a download through one connection in
sequential 512 KiB chunks. tdrive:

- opens a **pool of MTProto connections** (`TDRIVE_TG_POOL_SIZE`, default 8);
- fetches **1 MiB chunks several at a time** while releasing them strictly in
  order, using a continuous queue rather than fixed batches so one slow round
  trip does not stall the others;
- **ramps readahead**: a media player's 4 KiB probe costs one chunk, a
  sequential read widens to a 32 MiB window within a few megabytes;
- uploads **512 KiB parts concurrently** (`TDRIVE_UPLOAD_THREADS`, default 8)
  instead of one at a time.

Nothing is spooled to disk on the normal paths. A 40 GB upload uses a few
megabytes of memory: the browser slices the file and PUTs one segment per
request, and the server pipes each request body straight into Telegram.

## Running it

```bash
docker run -d --name tdrive -p 8080:8080 \
  -v tdrive-data:/data \
  -e TDRIVE_ADMIN_USER=admin \
  -e TDRIVE_ADMIN_PASSWORD='choose-something-long' \
  ghcr.io/OWNER/tdrive:latest
```

Or copy `.env.example` to `.env` and `docker compose up -d`.

Open <http://localhost:8080> and the setup wizard walks through four steps: the
admin account, an `api_id`/`api_hash` pair from
[my.telegram.org/apps](https://my.telegram.org/apps), a Telegram login, and the
storage channel — either a new private channel it creates for you, or one you
already have.

`/data` holds the index, the Telegram session and the upload spool. It is the
only thing to back up, and even losing it is recoverable.

## WebDAV

Mounted at `/dav`, guarded by the same accounts as the web interface.

```bash
rclone config create tdrive webdav \
  url=http://localhost:8080/dav vendor=other \
  user=admin pass="$(rclone obscure 'your-password')"

rclone mount tdrive: ~/mnt/tdrive
```

Finder, Windows Explorer and Cyberduck all work with `http://host:8080/dav/`
and the same username and password.

Split files are invisible here: `PROPFIND` reports one resource of the full
size, `GET` streams the reassembled bytes, and range requests across segment
boundaries return exactly what was asked for.

## Configuration

Everything has a working default; `TDRIVE_DATA_DIR` is the only thing a
container really needs.

| Variable | Default | |
|---|---|---|
| `TDRIVE_DATA_DIR` | `/data` | index, session, spool |
| `TDRIVE_LISTEN` | `:8080` | bind address |
| `TDRIVE_BASE_URL` | — | external origin; set behind a reverse proxy so cookies are marked `Secure` |
| `TDRIVE_ADMIN_USER` / `TDRIVE_ADMIN_PASSWORD` | — | seeds the first account; ignored once one exists |
| `TDRIVE_TG_APP_ID` / `TDRIVE_TG_APP_HASH` | — | skips a wizard step |
| `TDRIVE_SEGMENT_SIZE` | `1900MiB` | split size; must be a multiple of 512 KiB and at most 2000 MiB |
| `TDRIVE_TG_POOL_SIZE` | `8` | MTProto connections per datacenter |
| `TDRIVE_UPLOAD_THREADS` | `8` | concurrent part uploads within a segment |
| `TDRIVE_STREAM_CONCURRENCY` | `6` | concurrent chunk reads |
| `TDRIVE_STREAM_BUFFERS` | `8` | completed chunks held in memory per stream |
| `TDRIVE_WEBDAV_ENABLED` | `true` | |
| `TDRIVE_LOG_LEVEL` | `info` | |

Sizes accept `1900MiB`, `2GB` or a plain byte count.

## Layout

```
cmd/tdrive          entry point and wiring
internal/tagcodec   the caption format — encode, decode, and its limits
internal/reader     byte ranges → segments, and the parallel chunk pipeline
internal/drive      the service: paths, uploads, renames, deletes
internal/tgc        MTProto client, connection pool, login flow
internal/indexer    rebuild the index from channel history
internal/dav        WebDAV adapter
internal/api        REST endpoints and SSE
ui/                 React interface, embedded into the binary
```

`internal/drive/backend.go` defines the Telegram operations the drive needs as
an interface. That seam is why the segmentation logic — the part most likely to
harbour a silent off-by-one — is covered by tests that run without a Telegram
account:

```bash
go test ./...              # includes split/stitch, resume, rebuild, WebDAV
go test -race ./...        # the reader and uploader are both concurrent
```

## Development

```bash
# 一键启动：源码有更新时自动重建，否则直接启动；默认使用 ./data
./start.sh

# 如需使用其他数据目录
TDRIVE_DATA_DIR=/path/to/data ./start.sh
```

```bash
# Terminal 1
go run ./cmd/tdrive          # needs ui/dist to exist; `cd ui && pnpm build` once

# Terminal 2
cd ui && pnpm dev            # proxies /api and /dav to :8080
```

## Caveats

- Files are stored unencrypted. Anyone with access to the Telegram account can
  read them.
- Telegram's terms and rate limits still apply. This is not unlimited storage,
  and it should not be the only copy of anything important.
- Editing a caption by hand in a Telegram client will confuse the index; the
  rebuild is forgiving but cannot invent what was deleted.
- Not affiliated with Telegram.
