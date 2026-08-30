# tdrive

Turn your Telegram account into a network drive with unlimited storage.

**[中文文档](README.zh-CN.md)**

## Features

- **Web file manager** — right-click menus, Ctrl/Shift multi-select, rubber-band selection, keyboard shortcuts and batch rename; long-press, swipe actions and pull-to-refresh on touch devices
- **Rich preview** — images (zoom and pan), audio, PDF (paged pdf.js rendering), syntax-highlighted code, Markdown, spreadsheets, Word documents and zip contents. Video is not previewed in the browser: download it, or point a player at the reusable download link
- **Reusable download links** — a full URL with its own token that can be pasted into aria2, IDM or another device, supports parallel connections and resume, and can be revoked at any time
- **Download modes** — direct, server-staged, or per-segment with a local join. Direct and staged both hand a tokenised URL to the browser's own downloader, so it streams to disk and resumes like any other download; a multi-segment file defaults to staging, which is by far the most reliable
- **WebDAV** — mount as a network drive; compatible with rclone, macOS Finder, Windows Explorer, and other clients, sharing the same concurrency limits and permissions as the Web UI
- **Segmented storage** — files are automatically split into ~1.9 GB segments to overcome Telegram's 2 GB per-object limit; cross-segment HTTP Range requests are fully transparent to clients
- **Browser segmented upload** — the frontend slices files on the server's segment boundaries and uploads each slice; a dropped connection costs one segment instead of everything, with concurrency and resume support
- **VPS local upload** — Docker can mount a VPS directory read-only, letting the WebUI upload dialog choose server-side files without sending them through the browser first
- **Remote URL fetch** — submit a URL and the server downloads it directly into Telegram; large files never travel through the browser
- **Parallel download** — multiple concurrent 1 MiB chunk prefetches replace single-connection sequential reads, significantly improving download speed
- **Index rebuild** — the database is just a cache; the directory tree, file metadata and ownership can all be reconstructed from the Telegram channel
- **Multi-user** — JWT authentication, roles, twelve fine-grained permissions, per-account directory scoping, storage quotas, account enable/disable, session management and an audit log
- **Transfer centre** — uploads and downloads in one filterable list: by kind, status, source and date range, with live speed, average speed and elapsed time, and deletable history. Transfers the server drives itself — WebDAV reads and writes, VPS-local uploads, remote fetches, staged downloads — are timed server-side and stream their progress over SSE, so the list moves without being refreshed
- **Lightweight deployment** — single binary + one SQLite file, pure Go with no CGO; multi-arch Docker images for amd64 / arm64

## Quick Start

### Docker Compose (recommended)

1. Copy the example env file:

```bash
cp .env.example .env
```

2. Edit `.env`, set at least the admin password:

```bash
TDRIVE_ADMIN_USER=admin
TDRIVE_ADMIN_PASSWORD=your-secure-password
```

3. Start:

```bash
docker compose up -d
```

4. Open `http://localhost:8080` in your browser. Creating the administrator account is enough to enter the drive; Telegram login and channel selection can be completed later from **Settings**.

To upload files already present on the VPS without sending them through your browser, Compose mounts the default host directory `./vps-files` read-only at `/vps`. After signing in as an administrator, open **Settings → Runtime** and set **VPS local upload directory** to `/vps`; the upload dialog will then show it. No `TDRIVE_LOCAL_DIR` environment variable is required. To use another host directory, set Compose's `TDRIVE_LOCAL_PATH`; the WebUI value remains the container-side path `/vps`.

### Docker

```bash
docker run -d \
  --name tdrive \
  -p 8080:8080 \
  -v tdrive-data:/data \
  -e TDRIVE_ADMIN_USER=admin \
  -e TDRIVE_ADMIN_PASSWORD=change-this-please \
  ghcr.io/dibin666/tdrive:latest
```

### Run from Binary

```bash
TDRIVE_DATA_DIR=./data ./tdrive
```

Visit `http://localhost:8080` after launch; a setup wizard will appear on first run.

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `TDRIVE_DATA_DIR` | `./data` (binary) or `/data` (container) | Data directory (SQLite, Telegram session, upload spool) |
| `TDRIVE_LISTEN` | `:8080` | HTTP listen address |
| `TDRIVE_BASE_URL` | *(empty)* | Externally reachable origin (set when behind a reverse proxy) |
| `TDRIVE_ADMIN_USER` | *(empty)* | Bootstrap admin username (first run only) |
| `TDRIVE_ADMIN_PASSWORD` | *(empty)* | Bootstrap admin password (≥8 characters) |
| `TDRIVE_LOCAL_PATH` | `./vps-files` | Host directory exposed as a read-only VPS upload source in Docker Compose |
| `TDRIVE_TG_UPLOAD_PART_SIZE` | `512KiB` | Initial Telegram upload part-size fallback before WebUI settings are saved |
| `TDRIVE_TG_RATE_LIMIT` | `100ms` | Initial Telegram request-interval fallback before WebUI settings are saved |
| `TDRIVE_UPLOAD_CONCURRENCY` | `2` | Initial whole-file upload task limit before WebUI settings are saved |
| `TDRIVE_DOWNLOAD_CONCURRENCY` | `2` | Initial whole-file download task limit before WebUI settings are saved |
| `TDRIVE_CACHE_DIR` | `<data dir>/cache` | Where staged downloads are assembled |
| `TDRIVE_CACHE_LIMIT` | `20GiB` | Disk the staging cache may hold; 0 disables staging |
| `TDRIVE_MAX_DOWNLOAD_CONNS` | `8` | Parallel connections one download may hold |

After logging in as an administrator, configure the remaining runtime settings in **Settings**:

| WebUI setting | Default | Description |
|---|---|---|
| Telegram `api_id` / `api_hash` | *(empty)* | Credentials from [my.telegram.org](https://my.telegram.org/apps) |
| Storage segment size | `1900 MiB` | Size of each Telegram object (maximum depends on upload part size); only new uploads use a changed value |
| Telegram upload part size | `512 KiB` | Size of one `saveBigFilePart` request; only new uploads use a changed value |
| Telegram request interval | `100 ms` | Minimum delay between Telegram RPC requests; smaller values may trigger rate limits |
| Telegram connection pool | `8` | MTProto connection pool size |
| Upload threads | `8` | Concurrent upload threads per segment |
| Download concurrency | `6` | Concurrent download chunks |
| Simultaneous upload tasks | `2` | Whole-file upload limit shared by WebUI, VPS, remote URL and WebDAV; excess tasks wait |
| Simultaneous download tasks | `2` | Whole-file download limit shared by WebUI, share links and WebDAV. Several connections belonging to one download count as one task |
| Connections per download | `8` | Parallel range requests one download may hold; beyond this the server answers 429 and the client backs off |
| Download cache directory | *(empty)* | Empty uses `cache` under the data directory |
| Download cache limit | `20 GiB` | Least-recently-used staged copies are evicted past this; 0 disables staging |
| Staged copy lifetime | `24 h` | How long a staged file stays available for re-download |
| Share link lifetime | `168 h` | Default expiry of a new download link; 0 means never |
| VPS local upload directory | *(empty)* | Readable directory inside the server/container; Docker Compose mounts it at `/vps` by default; empty disables it |
| WebDAV | Enabled | Enable or disable the WebDAV mount |
| Log level | `info` | Runtime log level |

The performance page offers three presets — conservative, balanced and fast — and per-setting
sliders underneath. The segmenting controls are constraint-driven rather than validated: the
Telegram part size is a list of the sizes Telegram actually accepts, and the segment slider derives
its ceiling and step from it, so an invalid combination cannot be selected in the first place.

These settings are stored in the SQLite data directory and take effect without restarting the server. The Telegram connection is rebuilt automatically when its pool size or request interval changes.

`TDRIVE_LOCAL_PATH` is the host-side Compose path. With `docker run`, add `-v /srv/repository:/vps:ro` manually, then set `/vps` in **Settings → Storage**; no environment variable is required.

## Accounts and permissions

An administrator can configure each account under **Settings → Users**:

- **Permissions** (twelve of them) — browse, download, upload, VPS-local upload, remote fetch,
  mkdir, rename, move, delete, WebDAV, server-side staging and link sharing. An account with no
  explicit mask follows its role's defaults; administrators always hold everything.
- **Directory scope** — confine an account to one subtree, which then becomes its root. It applies
  to the Web UI and WebDAV alike.
- **Storage quota** — counted from the files the account uploaded. Ownership is written into the
  Telegram caption as `#own_`, so rebuilding the index restores it rather than zeroing it.
- **Enable/disable and sessions** — disabling an account ends its live sessions immediately;
  individual devices can also be signed out one at a time.

Account and settings changes are recorded in an audit log, filterable and exportable as CSV under
**Settings → Index and logs**.

## Downloading

The Web UI asks how to fetch a file before it starts:

| Mode | When it is right |
|---|---|
| Direct | Single-segment files. The browser opens several range requests and the server streams from Telegram as it reads |
| Staged on the server | **The recommended mode for split files.** The server assembles the segments into one file on disk first, then the client pulls that at local-disk speed |
| Per-segment | When server disk is not available. Each segment downloads separately; Chrome and Edge write them into one target file automatically, other browsers get the parts plus a join script |

Writing to disk in parallel needs the File System Access API (Chrome/Edge). Other browsers can use a
single connection, or copy the share link into aria2 or IDM instead.

However many connections a download opens, the server counts it as **one** download task, so the
concurrency limit and parallel downloading do not fight each other.

## WebDAV

WebDAV is mounted at `/dav` by default and uses the same credentials as the Web UI (HTTP Basic auth).

```bash
# rclone example
rclone config create tdrive webdav \
  url=http://localhost:8080/dav \
  vendor=other \
  user=admin \
  pass="$(rclone obscure your-password)"

rclone ls tdrive:
```

## Tech Stack

- **Backend** — Go, [gotd/td](https://github.com/gotd/td) (MTProto), [chi](https://github.com/go-chi/chi), [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) (pure Go SQLite)
- **Frontend** — React 19, Vite, Tailwind CSS 4, TypeScript; previews use mediabunny, pdf.js, shiki and SheetJS, all lazily loaded
- **Container** — multi-stage Dockerfile, distroless base image, GitHub Actions CI/CD

## Build

```bash
# Install deps and build frontend
cd ui && pnpm install && pnpm build && cd ..

# Compile
go build -trimpath -o tdrive ./cmd/tdrive
```

Or use the included script, which automatically recompiles when source files change:

```bash
./start.sh
```

## License

MIT
