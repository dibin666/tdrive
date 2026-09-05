# tdrive

Personal cloud storage backed by Telegram. Pure Go backend with React WebUI, WebDAV, chunked uploads, offline downloads, multi-account failover, and subprocess plugins.

**[中文文档](README.zh-CN.md)**

---

## Quick Start

Open `http://<your-server-ip>:8080` after startup.

### 1. Docker Run (Fastest)

Run a container:

```bash
docker run -d \
  --name tdrive \
  -p 8080:8080 \
  -v tdrive-data:/data \
  -e TDRIVE_ADMIN_USER=admin \
  -e TDRIVE_ADMIN_PASSWORD=your-secure-password \
  ghcr.io/dibin666/tdrive:latest
```

Open `http://localhost:8080` and log in with username `admin` and your password.

---

### 2. Docker Compose (Production)

Create a working directory:

```bash
mkdir -p tdrive && cd tdrive
```

Create `docker-compose.yml`:

```yaml
services:
  tdrive:
    image: ghcr.io/dibin666/tdrive:latest
    container_name: tdrive
    restart: unless-stopped
    ports:
      - "8080:8080"
    volumes:
      - tdrive-data:/data
      - ${TDRIVE_LOCAL_PATH:-./vps-files}:/vps:ro
    environment:
      TDRIVE_ADMIN_USER: ${TDRIVE_ADMIN_USER:-admin}
      TDRIVE_ADMIN_PASSWORD: ${TDRIVE_ADMIN_PASSWORD:?set this in .env}
      TDRIVE_BASE_URL: ${TDRIVE_BASE_URL:-}

volumes:
  tdrive-data:
```

Create `.env`:

```bash
TDRIVE_ADMIN_USER=admin
TDRIVE_ADMIN_PASSWORD=change-this-please

# Set when using a reverse proxy or domain name
# TDRIVE_BASE_URL=https://drive.example.com
```

Start the container:

```bash
docker compose up -d
```

View logs:

```bash
docker compose logs -f
```

---

### 3. Binary (No Docker)

Download pre-built binaries from releases:

```bash
TDRIVE_DATA_DIR=./data ./tdrive
```

Open `http://localhost:8080` to complete initial setup.

---

## Initial Setup

Log in as administrator and complete three steps:

1. **Get Telegram API credentials**: Visit [my.telegram.org/apps](https://my.telegram.org/apps), log in, and obtain `api_id` and `api_hash`.
2. **Link Telegram account**: In the WebUI, go to **Settings → Telegram**, enter `api_id` and `api_hash`, then enter your phone number and login code.
3. **Set storage channel**: Select an existing private channel or create a new one. tdrive uses this channel to store all files.

---

## Features

- **Web File Manager**: Drag-and-drop upload, box selection, multi-select, right-click context menu, batch rename, and touch gestures.
- **Online Preview**: Preview images, audio, video, PDF, Markdown, syntax-highlighted code, Office documents, and zip archives.
- **Chunked Uploads**: Automatically splits files exceeding 1.9 GB into segments to stay within Telegram's 2 GB limit. Reassembles segments transparently during download.
- **Resumable Downloads & Direct Links**: Multi-threaded downloads with range requests and revocable direct links for external download managers.
- **WebDAV Support**: Mount via `/dav` on Windows, macOS, Linux, Rclone, Infuse, and Alist.
- **Multi-Account Failover**: Add multiple Telegram accounts. When the active account hits FloodWait rate limits, tasks switch to standby accounts.
- **Multi-User & 13 Permissions**: User directory scoping, storage quotas, and 13 granular permissions (`read`, `download`, `upload`, `upload_local`, `remote_fetch`, `mkdir`, `rename`, `move`, `delete`, `webdav`, `stage`, `share`, `plugins`).
- **Offline Download & Local VPS Import**: Fetch remote URLs directly into Telegram on the server. Import host files from a mounted directory without browser bandwidth.
- **Channel Index Recovery**: The SQLite database functions as an index cache. If local data is lost, tdrive rebuilds directory trees and file metadata from the Telegram channel.
- **Subprocess Plugins**: Run standalone Go plugins via RPC over local sockets. Verified with SHA-256 digests; requires no host build toolchain.

---

## Plugin System

tdrive plugins run as independent Go subprocesses via RPC (`go-plugin`).

- **Per-Account Isolation**: Plugins install per user account. Each account owns its plugin list, binaries (`<data>/plugins/<user_id>/<plugin_id>`), private data directory (`<data>/plugin-data/<user_id>/<plugin_id>`), and child processes. Different accounts can run different versions of the same plugin.
- **Permission Bit**: Installation is gated by the `plugins` permission bit. This permission defaults to administrators only. Granting `plugins` allows code execution under the host process privileges.
- **Direct UI Entry**: Plugins that declare UI routes appear under the **Plugins** section in the left sidebar. Click any item to open `/plugin/{id}` directly. Plugin store, installation, and status toggles remain under **Settings → Plugins**.
- **Process Limits**: Bound total subprocesses with `TDRIVE_PLUGIN_MAX_PER_USER` (default `4`) and `TDRIVE_PLUGIN_MAX_PROCESSES` (default `32`). Set to `0` or negative for unlimited.

---

## WebDAV Usage

WebDAV endpoint defaults to `/dav`. Use your tdrive account credentials:

```bash
# Example rclone configuration
rclone config create tdrive webdav \
  url=http://localhost:8080/dav \
  vendor=other \
  user=admin \
  pass="$(rclone obscure your-password)"

# List files
rclone ls tdrive:
```

---

## Multi-Account Requirements

- **Separate Phone Numbers**: Each linked account must use a distinct phone number. Multiple API credentials on one number share Telegram rate limits.
- **Channel Permissions**: Standby accounts joining the storage channel must have permissions to send, edit, and delete messages.
- **Per-Account Proxy**: Configure independent SOCKS5 or HTTP proxies for each Telegram account in the WebUI.

---

## Configuration

### Environment Variables

| Variable | Default | Description |
|---|---|---|
| `TDRIVE_ADMIN_USER` | `admin` | Initial admin username (first run only) |
| `TDRIVE_ADMIN_PASSWORD` | *(empty)* | Initial admin password (required, ≥8 chars) |
| `TDRIVE_BASE_URL` | *(empty)* | Public origin URL (e.g. `https://drive.example.com`) |
| `TDRIVE_DATA_DIR` | `/data` or `./data` | Data directory for SQLite database, sessions, and plugins |
| `TDRIVE_LISTEN` | `:8080` | HTTP listen address |
| `TDRIVE_LOCAL_PATH` | `./vps-files` | Host directory to bind-mount for server-local imports |
| `TDRIVE_CACHE_DIR` | `/data/cache` | Directory for staged downloads and spools |
| `TDRIVE_CACHE_LIMIT` | `20GiB` | Disk cache ceiling for staging split files |
| `TDRIVE_MAX_DOWNLOAD_CONNS` | `8` | Maximum connections for staged file transfers |
| `TDRIVE_PLUGIN_DIR` | `<data>/plugins` | Directory for installed plugin executables |
| `TDRIVE_PLUGIN_STORE_URL` | GitHub raw index | Remote plugin store index JSON URL (empty to disable store) |
| `TDRIVE_PLUGIN_MAX_BINARY_BYTES` | `256MiB` | Maximum allowed plugin executable size |
| `TDRIVE_PLUGIN_MAX_PER_USER` | `4` | Maximum installed plugins per user account (≤0 for unlimited) |
| `TDRIVE_PLUGIN_MAX_PROCESSES` | `32` | Maximum concurrent plugin subprocesses across all accounts (≤0 for unlimited) |

> Runtime parameters (concurrency, part size, rate limits, connection pools) can be adjusted in **WebUI Settings** without restarting the service.

---

## Build from Source

Requirements: Go 1.24+, Node.js 20+, pnpm.

```bash
# 1. Build frontend
cd ui && pnpm install && pnpm build && cd ..

# 2. Build binary
go build -trimpath -o tdrive ./cmd/tdrive
```

---

## License

[MIT](LICENSE)
