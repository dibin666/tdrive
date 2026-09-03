# tdrive

Turn your Telegram account into a high-speed, unlimited-storage personal cloud drive.

**[中文文档](README.zh-CN.md)**

---

## Quick Installation

Once started, open `http://<your-server-ip>:8080` in your browser.

### 1. Minimal Installation (Single command, fast trial)

Run directly with Docker:

```bash
docker run -d \
  --name tdrive \
  -p 8080:8080 \
  -v tdrive-data:/data \
  -e TDRIVE_ADMIN_USER=admin \
  -e TDRIVE_ADMIN_PASSWORD=your-secure-password \
  ghcr.io/dibin666/tdrive:latest
```

Open `http://localhost:8080` and log in with username `admin` and the password set above.

---

### 2. Detailed Installation (Docker Compose, recommended for production)

Docker Compose provides easier volume management, reverse proxy setup, and host directory mounting.

#### Step 1: Prepare files

Create a project directory:

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
      # Persistent data directory (SQLite database, Telegram sessions, etc.)
      - tdrive-data:/data
      # Optional: mount a host directory for direct VPS-local file imports
      - ./vps-files:/vps:ro
    environment:
      # Bootstrap admin credentials (first run only)
      TDRIVE_ADMIN_USER: ${TDRIVE_ADMIN_USER:-admin}
      TDRIVE_ADMIN_PASSWORD: ${TDRIVE_ADMIN_PASSWORD}
      # External origin URL (set when using a reverse proxy, e.g. https://drive.example.com)
      TDRIVE_BASE_URL: ${TDRIVE_BASE_URL:-}

volumes:
  tdrive-data:
```

Create `.env`:

```bash
# Admin credentials (required, at least 8 characters)
TDRIVE_ADMIN_USER=admin
TDRIVE_ADMIN_PASSWORD=change-this-please

# External domain name (uncomment if behind a reverse proxy or using HTTPS)
# TDRIVE_BASE_URL=https://drive.example.com
```

#### Step 2: Start the service

```bash
docker compose up -d
```

View logs:

```bash
docker compose logs -f
```

---

### 3. Run from Binary (without Docker)

Download pre-built release binaries for your platform:

```bash
# Specify data directory and launch
TDRIVE_DATA_DIR=./data ./tdrive
```

Visit `http://localhost:8080` to follow the initial setup wizard.

---

## First-time Setup

After signing in as admin, complete these 3 steps to start using your drive:

1. **Get Telegram credentials**: Go to [my.telegram.org/apps](https://my.telegram.org/apps), log in with your Telegram account, and get your `api_id` and `api_hash`.
2. **Link Telegram account**: In the drive WebUI, go to **Settings → Telegram**, enter `api_id` and `api_hash`, then sign in with your phone number and SMS code.
3. **Choose a storage channel**: Select an existing private channel or create a new one to store your files.

Now you can upload and download files freely.

---

## Core Features

- **Modern Web File Manager**: Drag-and-drop, multi-select, rubber-band selection, right-click actions, batch rename, and touch gesture support.
- **Rich Online Preview**: Preview images, audio, video, PDF, Markdown, syntax-highlighted code, Office documents, and zip contents directly.
- **Automatic File Chunking**: Files exceeding 1.9 GB are automatically split to bypass Telegram's 2 GB limit, and transparently joined upon download.
- **Fast Download & Direct Links**: Multi-threaded parallel downloading and resume support; generate revocable download links for IDM or Aria2.
- **WebDAV Support**: Mount as a network drive on Windows, macOS, Linux, Rclone, Infuse, or Alist.
- **Multi-Account Failover**: Configure multiple Telegram accounts as fallbacks. When the primary account hits Telegram rate limits, tasks automatically failover without interruption.
- **Multi-User & Granular Permissions**: Multi-user isolation, directory-scoped access, storage quotas, and 12 granular permissions.
- **Remote Fetch & VPS Local Import**: Download remote URLs directly to Telegram without passing through the browser, or import local files on the VPS instantly.
- **Zero Data Loss**: The local database is only an index cache; files, folder structures, and metadata can be fully reconstructed from the Telegram channel at any time.
- **Plugin System**: Extend functionality with isolated Go subprocess plugins.
- **Ultra-lightweight**: Pure Go, single binary + SQLite, zero heavy external dependencies, minimal CPU/memory footprint.

---

## WebDAV Usage

WebDAV is mounted at `/dav` by default. Use your drive username and password:

```bash
# rclone example
rclone config create tdrive webdav \
  url=http://localhost:8080/dav \
  vendor=other \
  user=admin \
  pass="$(rclone obscure your-password)"

# List files
rclone ls tdrive:
```

---

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `TDRIVE_ADMIN_USER` | `admin` | Bootstrap admin username (first run only) |
| `TDRIVE_ADMIN_PASSWORD` | *(empty)* | Bootstrap admin password (required, ≥8 chars) |
| `TDRIVE_BASE_URL` | *(empty)* | External origin URL (required when behind a reverse proxy) |
| `TDRIVE_DATA_DIR` | `/data` | Data directory for SQLite database and sessions |
| `TDRIVE_LISTEN` | `:8080` | HTTP listen address and port |
| `TDRIVE_LOCAL_PATH` | `./vps-files` | Host directory mounted for VPS-local upload |

> **Note**: Runtime performance parameters (connection pool size, upload/download threads, chunk size, cache limits) can be configured directly in **WebUI Settings** without restarting the server.

---

## Build from Source

```bash
# 1. Build frontend
cd ui && pnpm install && pnpm build && cd ..

# 2. Compile binary
go build -trimpath -o tdrive ./cmd/tdrive
```

---

## License

MIT
