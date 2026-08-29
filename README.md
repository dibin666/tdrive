# tdrive

Turn your Telegram account into a network drive with unlimited storage.

**[中文文档](README.zh-CN.md)**

## Features

- **Web file manager** — built-in modern Web UI with upload, download, preview (video / image / audio / PDF / text), mkdir, rename, and move
- **WebDAV** — mount as a network drive; compatible with rclone, macOS Finder, Windows Explorer, and other clients
- **Segmented storage** — files are automatically split into ~1.9 GB segments to overcome Telegram's 2 GB per-object limit; cross-segment HTTP Range requests are fully transparent to clients
- **Browser segmented upload** — the frontend slices files on the server's segment boundaries and uploads each slice; a dropped connection costs one segment instead of everything, with concurrency and resume support
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

4. Open `http://localhost:8080` in your browser and follow the wizard to log in to Telegram and select a storage channel.

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
| `TDRIVE_TG_APP_ID` | *(empty)* | Telegram API ID from [my.telegram.org](https://my.telegram.org/apps); can also be entered in the wizard |
| `TDRIVE_TG_APP_HASH` | *(empty)* | Telegram API Hash |
| `TDRIVE_SEGMENT_SIZE` | `1900MiB` | Segment size (ceiling 2000MiB) |
| `TDRIVE_TG_POOL_SIZE` | `8` | MTProto connection pool size |
| `TDRIVE_UPLOAD_THREADS` | `8` | Concurrent upload threads per segment |
| `TDRIVE_STREAM_CONCURRENCY` | `6` | Concurrent download chunks |
| `TDRIVE_WEBDAV_ENABLED` | `true` | Enable WebDAV |
| `TDRIVE_LOG_LEVEL` | `info` | Log level |

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
