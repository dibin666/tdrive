# tdrive

将 Telegram 账号转变为无限容量的网络硬盘。

**[English](README.md)**

## 功能特性

- **Web 文件管理器** — 内置现代化 Web UI，支持上传、下载、预览（视频 / 图片 / 音频 / PDF / 文本）、创建文件夹、重命名和移动
- **WebDAV** — 作为网络硬盘挂载，兼容 rclone、macOS Finder、Windows Explorer 等客户端
- **大文件分片** — 自动将文件按 ~1.9 GB 分片存储到 Telegram，突破单文件 2 GB 上限；跨分片的 HTTP Range 请求对客户端完全透明
- **浏览器分片上传** — 前端按服务器的分片边界切割文件并逐片上传，断点只丢一个分片而非整个文件，支持并发和续传
- **VPS 本地上传** — Docker 可只读挂载 VPS 目录，WebUI 上传弹窗中直接选择服务器上的文件，无需先下载到浏览器
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

4. 打开浏览器访问 `http://localhost:8080`，先创建管理员账号即可进入网盘；Telegram 登录和频道选择可以稍后在“设置”中完成。

如需从 VPS 直接上传已有文件，把 `.env` 中的 `TDRIVE_LOCAL_PATH` 设置为宿主机目录，例如
`TDRIVE_LOCAL_PATH=/srv/repository`。Compose 会以只读方式挂载它，点击 WebUI 的“上传”后，弹窗下方会显示该目录。

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
| `TDRIVE_LOCAL_PATH` | `./vps-files` | Docker 宿主机上用于 VPS 文件上传的目录；以只读方式挂载 |
| `TDRIVE_LOCAL_DIR` | *（空）* | 应用容器内对应的本地目录；Compose 默认使用 `/vps` |

使用管理员账号登录后，其余运行参数可在 WebUI 的“设置”中配置：

| WebUI 设置 | 默认值 | 说明 |
|---|---|---|
| Telegram `api_id` / `api_hash` | *（空）* | 来自 [my.telegram.org](https://my.telegram.org/apps) 的凭据 |
| 分片大小 | `1900 MiB` | 每个 Telegram 对象的大小（上限 `2000 MiB`）；修改后只影响新上传文件 |
| Telegram 连接池 | `8` | MTProto 连接池大小 |
| 上传线程 | `8` | 单分片内并发上传线程 |
| 下载并发块数 | `6` | 并发下载块数 |
| WebDAV | 启用 | 启用或禁用 WebDAV 挂载 |
| 日志级别 | `info` | 运行时日志级别 |

这些设置会保存在 SQLite 数据目录中，并且无需重启服务即可生效。连接池大小变化时，Telegram 连接池会自动重建。

`TDRIVE_LOCAL_PATH` 是 Docker Compose 的宿主机路径，不是容器内路径。若使用 `docker run`，请手动添加
`-v /srv/repository:/vps:ro -e TDRIVE_LOCAL_DIR=/vps`。

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
