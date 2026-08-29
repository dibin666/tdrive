# tdrive

将 Telegram 账号转变为无限容量的网络硬盘。

**[English](README.md)**

## 功能特性

- **Web 文件管理器** — 内置现代化 Web UI，支持上传、下载、预览（视频 / 图片 / 音频 / PDF / 文本）、创建文件夹、重命名和移动
- **WebDAV** — 作为网络硬盘挂载，兼容 rclone、macOS Finder、Windows Explorer 等客户端
- **大文件分片** — 自动将文件按 ~1.9 GB 分片存储到 Telegram，突破单文件 2 GB 上限；跨分片的 HTTP Range 请求对客户端完全透明
- **浏览器分片上传** — 前端按服务器的分片边界切割文件并逐片上传，断点只丢一个分片而非整个文件，支持并发和续传
- **离线下载** — 提交一个 URL，服务器端直接拉取并存入 Telegram，大文件无需经过浏览器
- **并行下载** — 多连接并发预取 1 MiB 块，替代单连接顺序读取，下载速度显著提升
- **索引重建** — 数据库只是缓存；丢失或损坏后可从 Telegram 频道完整重建目录树和文件元数据
- **多用户** — JWT 认证、角色管理（管理员 / 普通用户），首次运行时有引导向导
- **轻量部署** — 单二进制 + 一个 SQLite 文件，纯 Go 编译无需 CGO；提供 amd64 / arm64 多架构 Docker 镜像

## 快速开始

### Docker Compose（推荐）

1. 复制示例环境文件：

```bash
cp .env.example .env
```

2. 编辑 `.env`，至少设置管理员密码：

```bash
TDRIVE_ADMIN_USER=admin
TDRIVE_ADMIN_PASSWORD=your-secure-password
```

3. 启动：

```bash
docker compose up -d
```

4. 打开浏览器访问 `http://localhost:8080`，按向导完成 Telegram 登录和频道选择。

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

### 从二进制运行

```bash
TDRIVE_DATA_DIR=./data ./tdrive
```

启动后访问 `http://localhost:8080`，首次会显示设置向导。

## 环境变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `TDRIVE_DATA_DIR` | `./data`（二进制）或 `/data`（容器） | 数据目录（SQLite、Telegram 会话、上传缓存） |
| `TDRIVE_LISTEN` | `:8080` | HTTP 监听地址 |
| `TDRIVE_BASE_URL` | *（空）* | 外部可达地址（反向代理时设置） |
| `TDRIVE_ADMIN_USER` | *（空）* | 初始管理员用户名（仅首次生效） |
| `TDRIVE_ADMIN_PASSWORD` | *（空）* | 初始管理员密码（≥8 位） |
| `TDRIVE_TG_APP_ID` | *（空）* | Telegram API ID，来自 [my.telegram.org](https://my.telegram.org/apps)，也可在向导中填写 |
| `TDRIVE_TG_APP_HASH` | *（空）* | Telegram API Hash |
| `TDRIVE_SEGMENT_SIZE` | `1900MiB` | 分片大小（上限 2000MiB） |
| `TDRIVE_TG_POOL_SIZE` | `8` | MTProto 连接池大小 |
| `TDRIVE_UPLOAD_THREADS` | `8` | 单分片内并发上传线程 |
| `TDRIVE_STREAM_CONCURRENCY` | `6` | 并发下载块数 |
| `TDRIVE_WEBDAV_ENABLED` | `true` | 启用 WebDAV |
| `TDRIVE_LOG_LEVEL` | `info` | 日志级别 |

## WebDAV

WebDAV 默认挂载在 `/dav`，使用与 Web UI 相同的账号密码（HTTP Basic 认证）。

```bash
# rclone 示例
rclone config create tdrive webdav \
  url=http://localhost:8080/dav \
  vendor=other \
  user=admin \
  pass="$(rclone obscure your-password)"

rclone ls tdrive:
```

## 技术栈

- **后端** — Go, [gotd/td](https://github.com/gotd/td) (MTProto), [chi](https://github.com/go-chi/chi), [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite)（纯 Go SQLite）
- **前端** — React 19, Vite, Tailwind CSS 4, TypeScript
- **容器** — 多阶段 Dockerfile, distroless 基础镜像, GitHub Actions CI/CD

## 构建

```bash
# 安装依赖并构建前端
cd ui && pnpm install && pnpm build && cd ..

# 编译
go build -trimpath -o tdrive ./cmd/tdrive
```

或使用内置脚本，它会在源码有变动时自动重新编译：

```bash
./start.sh
```

## 许可证

MIT
