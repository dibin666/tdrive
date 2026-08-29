# tdrive

Turn your Telegram account into a network drive with unlimited storage.

**[中文文档](README.zh-CN.md)**

## Features

- **Web file manager** — built-in modern Web UI with upload, download, preview (video / image / audio / PDF / text), mkdir, rename, and move
- **WebDAV** — mount as a network drive; compatible with rclone, macOS Finder, Windows Explorer, and other clients
- **Segmented storage** — files are automatically split into ~1.9 GB segments to overcome Telegram's 2 GB per-object limit; cross-segment HTTP Range requests are fully transparent to clients
- **Browser segmented upload** — the frontend slices files on the server's segment boundaries and uploads each slice; a dropped connection costs one segment instead of everything, with concurrency and resume support
- **VPS local upload** — Docker can mount a VPS directory read-only, letting the WebUI upload dialog choose server-side files without sending them through the browser first
- **Remote URL fetch** — submit a URL and the server downloads it directly into Telegram; large files never travel through the browser
- **Parallel download** — multiple concurrent 1 MiB chunk prefetches replace single-connection sequential reads, significantly improving download speed
- **Index rebuild** — the database is just a cache; the full directory tree and file metadata can be reconstructed from the Telegram channel
- **Multi-user** — JWT authentication, role management (admin / user), with a setup wizard on first run
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

To upload files already present on the VPS without sending them through your browser, set `TDRIVE_LOCAL_PATH` in `.env`, for example `TDRIVE_LOCAL_PATH=/srv/repository`. Compose mounts it read-only, and the WebUI upload dialog shows it below the browser upload option.

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
| `TDRIVE_LOCAL_DIR` | *(empty)* | Container-side local source directory; Compose sets this to `/vps` |

After logging in as an administrator, configure the remaining runtime settings in **Settings**:

| WebUI setting | Default | Description |
|---|---|---|
| Telegram `api_id` / `api_hash` | *(empty)* | Credentials from [my.telegram.org](https://my.telegram.org/apps) |
| Segment size | `1900 MiB` | Size of each Telegram object (maximum `2000 MiB`); only new uploads use a changed value |
| Telegram connection pool | `8` | MTProto connection pool size |
| Upload threads | `8` | Concurrent upload threads per segment |
| Download concurrency | `6` | Concurrent download chunks |
| WebDAV | Enabled | Enable or disable the WebDAV mount |
| Log level | `info` | Runtime log level |

These settings are stored in the SQLite data directory and take effect without restarting the server. The Telegram connection pool is rebuilt automatically when its size changes.

`TDRIVE_LOCAL_PATH` is the host-side Compose path. With `docker run`, add `-v /srv/repository:/vps:ro -e TDRIVE_LOCAL_DIR=/vps` manually.

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
- **Frontend** — React 19, Vite, Tailwind CSS 4, TypeScript
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
