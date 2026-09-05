# tdrive

以 Telegram 为存储后端的个人网盘。Go 后端 + React 前端，支持 WebDAV、大文件自动分片、离线下载、多 Telegram 账号故障转移与子进程插件。

**[English](README.md)**

---

## 快速开始

服务启动后，在浏览器访问 `http://<服务器IP>:8080`。

### 1. Docker Run（单命令运行）

直接运行容器：

```bash
docker run -d \
  --name tdrive \
  -p 8080:8080 \
  -v tdrive-data:/data \
  -e TDRIVE_ADMIN_USER=admin \
  -e TDRIVE_ADMIN_PASSWORD=your-secure-password \
  ghcr.io/dibin666/tdrive:latest
```

在浏览器打开 `http://localhost:8080`，使用账号 `admin` 和上面设置的密码登录。

---

### 2. Docker Compose（推荐生产部署）

创建项目目录并进入：

```bash
mkdir -p tdrive && cd tdrive
```

创建 `docker-compose.yml`：

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

创建 `.env`：

```bash
TDRIVE_ADMIN_USER=admin
TDRIVE_ADMIN_PASSWORD=change-this-please

# 使用反向代理或域名访问时配置
# TDRIVE_BASE_URL=https://drive.example.com
```

启动服务：

```bash
docker compose up -d
```

查看日志：

```bash
docker compose logs -f
```

---

### 3. 二进制运行（无 Docker）

从 Release 下载对应平台的预编译二进制：

```bash
TDRIVE_DATA_DIR=./data ./tdrive
```

打开 `http://localhost:8080` 完成初始化配置。

---

## 首次配置

使用管理员账号登录后，按顺序完成以下三步：

1. **获取 Telegram API 凭据**：访问 [my.telegram.org/apps](https://my.telegram.org/apps) 登录账号，获取 `api_id` 与 `api_hash`。
2. **绑定 Telegram 账号**：在网盘界面打开 **设置 → Telegram**，填入 `api_id` 与 `api_hash`，输入手机号和收到的登录验证码。
3. **选择存储频道**：选择现有的私有频道或新建私有频道。tdrive 将该频道作为底层存储仓库。

---

## 功能特性

- **Web 文件管理**：拖拽上传、框选、多选、右键菜单、批量重命名与触屏手势。
- **在线预览**：支持图片、音视频、PDF、Markdown、代码高亮、Office 文档与 ZIP 压缩包预览。
- **大文件自动分片**：单文件超过 1.9 GB 时自动分卷切片，绕过 Telegram 2 GB 单文件限制；下载时自动组装合并。
- **断点续传与下载直链**：支持多线程并发下载、断点续传，可生成带时效的独立直链导入外部下载器。
- **WebDAV 挂载**：挂载路径为 `/dav`，支持挂载至 Windows、macOS、Linux、Rclone、Infuse 与 Alist。
- **多账号故障转移**：支持添加多个 Telegram 账号。当前账号触发 Telegram FloodWait 限流时，任务自动切换至备用账号继续传输。
- **多用户与 13 项权限控制**：支持多账号隔离、目录作用域限定、存储配额，提供 13 项独立权限位（`read`、`download`、`upload`、`upload_local`、`remote_fetch`、`mkdir`、`rename`、`move`、`delete`、`webdav`、`stage`、`share`、`plugins`）。
- **离线下载与服务器本地导入**：支持在服务器端直接拉取远程 HTTP 链接写入 Telegram；支持挂载宿主机目录直接导入文件，不经过浏览器中转。
- **频道数据重建**：本地 SQLite 仅作为加速索引缓存。本地数据损坏或丢失时，可从 Telegram 频道全量恢复目录结构与文件元数据。
- **独立子进程插件**：插件作为独立 Go 子进程运行，通过本地 RPC 通信。仅下载校验预编译二进制的 SHA-256，宿主机无需 Go 编译环境。

---

## 插件系统

tdrive 插件采用 `go-plugin` 机制，作为独立子进程通过本地 RPC 与主服务交互。

- **按账号独立安装**：插件按用户账号分别安装与管理。每个账号拥有独立的插件列表、二进制文件（`<数据目录>/plugins/<用户ID>/<插件ID>`）、私有数据目录（`<数据目录>/plugin-data/<用户ID>/<插件ID>`）与子进程。不同账号可运行同一插件的不同版本。
- **权限控制**：新增 `plugins` 权限位，默认仅管理员持有。插件以主进程权限执行操作系统命令，授予该权限等同于授予代码执行权限。
- **侧边栏独立入口**：声明了 UI 路由的插件会直接显示在左侧边栏的「插件」独立分组中，点击即可打开对应插件页面（`/plugin/{id}`）。插件商店、安装管理与启用开关保留在「设置 → 插件」。
- **进程数量限制**：通过环境变量 `TDRIVE_PLUGIN_MAX_PER_USER`（默认 `4`）与 `TDRIVE_PLUGIN_MAX_PROCESSES`（默认 `32`）限制单账号与全实例运行的插件子进程总数。设为 `0` 或负数表示不设上限。

---

## WebDAV 挂载

WebDAV 挂载路径默认为 `/dav`，使用网盘账号密码登录：

```bash
# rclone 配置示例
rclone config create tdrive webdav \
  url=http://localhost:8080/dav \
  vendor=other \
  user=admin \
  pass="$(rclone obscure your-password)"

# 查看文件列表
rclone ls tdrive:
```

---

## 多账号使用限制

- **独立手机号**：每个绑定的 Telegram 账号必须使用不同手机号。同一手机号创建的多个 API 凭据仍共用相同的限流配额。
- **频道操作权限**：备用账号加入存储频道后，必须拥有发送消息、编辑消息和删除消息的权限。
- **独立代理支持**：可在后台为每个 Telegram 账号单独配置专属的 SOCKS5 或 HTTP 代理。

---

## 配置说明

### 环境变量

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `TDRIVE_ADMIN_USER` | `admin` | 初始管理员用户名（仅首次启动生效） |
| `TDRIVE_ADMIN_PASSWORD` | 无 | 初始管理员密码（必填，≥8 位） |
| `TDRIVE_BASE_URL` | 空 | 外部公开访问地址（例如 `https://drive.example.com`） |
| `TDRIVE_DATA_DIR` | `/data` 或 `./data` | 数据存放目录（保存数据库、登录会话和插件等） |
| `TDRIVE_LISTEN` | `:8080` | HTTP 监听地址 |
| `TDRIVE_LOCAL_PATH` | `./vps-files` | 宿主机目录挂载路径（供服务器本地秒传导入） |
| `TDRIVE_CACHE_DIR` | `/data/cache` | 分卷暂存与分片缓存目录 |
| `TDRIVE_CACHE_LIMIT` | `20GiB` | 磁盘暂存缓存上限 |
| `TDRIVE_MAX_DOWNLOAD_CONNS` | `8` | 暂存下载的最大并发连接数 |
| `TDRIVE_PLUGIN_DIR` | `<数据目录>/plugins` | 插件可执行文件保存目录 |
| `TDRIVE_PLUGIN_STORE_URL` | GitHub raw 索引地址 | 插件商店远程索引地址（设为空则关闭插件商店） |
| `TDRIVE_PLUGIN_MAX_BINARY_BYTES` | `256MiB` | 插件二进制文件体积上限 |
| `TDRIVE_PLUGIN_MAX_PER_USER` | `4` | 单账号允许安装的插件最大数量（≤0 表示不限制） |
| `TDRIVE_PLUGIN_MAX_PROCESSES` | `32` | 整个实例允许并发运行的插件子进程总数上限（≤0 表示不限制） |

> 连接并发、上传分片大小、限流延迟等性能参数均可在 WebUI **设置** 中直接调节并持久化，即时生效，无需重启服务。

---

## 源码构建

环境要求：Go 1.24+、Node.js 20+、pnpm。

```bash
# 1. 构建前端
cd ui && pnpm install && pnpm build && cd ..

# 2. 编译后端二进制
go build -trimpath -o tdrive ./cmd/tdrive
```

---

## 开源协议

[MIT](LICENSE)
