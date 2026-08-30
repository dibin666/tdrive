# tdrive

将 Telegram 账号转变为无限容量的网络硬盘。

**[English](README.md)**

## 功能特性

- **Web 文件管理器** — 内置现代化 Web UI，右键菜单、Ctrl / Shift 多选、框选、键盘快捷键、批量重命名；移动端支持长按、左滑操作和下拉刷新
- **多格式在线预览** — 视频、图片（缩放平移）、音频、PDF（pdf.js 分页渲染）、代码高亮、Markdown、Excel、Word 和 zip 目录浏览
- **H.265 / MKV 播放** — 三级回退：浏览器原生 → WebCodecs 硬件解码 + 画布渲染 → WebAssembly 软件解码，MKV 容器和 HEVC 编码都能直接在线看
- **可复用下载直链** — 生成带独立令牌的完整 URL，可粘贴到 aria2、IDM、迅雷或另一台设备，支持多线程和断点续传，可随时撤销
- **多种下载方式** — 直接下载 / 服务器暂存后下载 / 分卷分别下载再本地合并；分卷大文件默认推荐先暂存，最稳妥
- **WebDAV** — 作为网络硬盘挂载，兼容 rclone、macOS Finder、Windows Explorer 等客户端；与 WebUI 共用同一套并发限制和权限
- **大文件分片** — 自动将文件按 ~1.9 GB 分片存储到 Telegram，突破单文件 2 GB 上限；跨分片的 HTTP Range 请求对客户端完全透明
- **浏览器分片上传** — 前端按服务器的分片边界切割文件并逐片上传，断点只丢一个分片而非整个文件，支持并发和续传
- **VPS 本地上传** — Docker 可只读挂载 VPS 目录，WebUI 上传弹窗中直接选择服务器上的文件，无需先下载到浏览器
- **离线下载** — 提交一个 URL，服务器端直接拉取并存入 Telegram，大文件无需经过浏览器
- **并行下载** — 多连接并发预取 1 MiB 块，替代单连接顺序读取，下载速度显著提升
- **索引重建** — 数据库只是缓存；丢失或损坏后可从 Telegram 频道完整重建目录树、文件元数据和归属关系
- **多用户** — JWT 认证、角色管理、12 项细粒度权限、按用户限定目录范围、存储配额、账号启停、登录会话管理和操作审计日志
- **传输中心** — 上传和下载合并为一个可筛选的列表：按类型 / 状态 / 来源 / 日期区间筛选，显示平均速度和用时，可删除历史记录
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

如需从 VPS 直接上传已有文件，Compose 默认会把宿主机的 `./vps-files` 以只读方式挂载到容器的
`/vps`。登录管理员账号后，打开 WebUI“设置 → 运行参数”，把“VPS 本地上传目录”设置为
`/vps`，上传弹窗中就可以浏览该目录；不需要设置 `TDRIVE_LOCAL_DIR` 环境变量。若要换宿主机目录，
只需设置 Compose 的 `TDRIVE_LOCAL_PATH`，WebUI 中仍填写容器内的 `/vps`。

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
| `TDRIVE_LOCAL_PATH` | `./vps-files` | Docker Compose 宿主机上用于 VPS 文件上传的目录；以只读方式挂载 |
| `TDRIVE_TG_UPLOAD_PART_SIZE` | `512KiB` | WebUI 首次配置前的 Telegram 上传分片默认值 |
| `TDRIVE_TG_RATE_LIMIT` | `100ms` | WebUI 首次配置前的 Telegram 请求间隔默认值 |
| `TDRIVE_UPLOAD_CONCURRENCY` | `2` | WebUI 首次配置前的同时上传任务数默认值 |
| `TDRIVE_DOWNLOAD_CONCURRENCY` | `2` | WebUI 首次配置前的同时下载任务数默认值 |
| `TDRIVE_CACHE_DIR` | `<数据目录>/cache` | 下载暂存目录 |
| `TDRIVE_CACHE_LIMIT` | `20GiB` | 下载暂存磁盘上限，设为 0 关闭暂存功能 |
| `TDRIVE_MAX_DOWNLOAD_CONNS` | `8` | 单个下载允许的并发连接数 |

使用管理员账号登录后，其余运行参数可在 WebUI 的“设置”中配置：

| WebUI 设置 | 默认值 | 说明 |
|---|---|---|
| Telegram `api_id` / `api_hash` | *（空）* | 来自 [my.telegram.org](https://my.telegram.org/apps) 的凭据 |
| 存储分片大小 | `1900 MiB` | 每个 Telegram 对象的大小（上限取决于上传分片大小）；修改后只影响新上传文件 |
| Telegram 上传分片 | `512 KiB` | 单个 `saveBigFilePart` 请求的大小；修改后只影响新上传文件 |
| Telegram 请求间隔 | `100 ms` | Telegram RPC 请求之间的最小间隔，过小可能触发限流 |
| Telegram 连接池 | `8` | MTProto 连接池大小 |
| 上传线程 | `8` | 单分片内并发上传线程 |
| 下载并发块数 | `6` | 并发下载块数 |
| 同时上传任务数 | `2` | WebUI、VPS、离线下载和 WebDAV 共享的整文件上传上限；超出后等待 |
| 同时下载任务数 | `2` | WebUI、直链和 WebDAV 共享的整文件下载上限；一个下载开的多条连接只算一个任务 |
| 单个下载连接数 | `8` | 多线程下载最多开几条连接，超出返回 429 并自动退避 |
| 下载暂存目录 | *（空）* | 留空则使用数据目录下的 `cache` |
| 暂存磁盘上限 | `20 GiB` | 超出后按最近使用时间淘汰；设为 0 关闭暂存 |
| 暂存保留时长 | `24 小时` | 暂存完成后可重复下载的时长 |
| 分享直链有效期 | `168 小时` | 新建直链的默认有效期，0 表示永不过期 |
| VPS 本地上传目录 | *（空）* | 服务器或容器内的可读目录；Docker Compose 默认挂载路径为 `/vps`，留空则禁用 |
| WebDAV | 启用 | 启用或禁用 WebDAV 挂载 |
| 日志级别 | `info` | 运行时日志级别 |

性能参数页提供「保守 / 均衡 / 极速」三档预设，也可以逐项微调。分片相关的控件是受约束的：
Telegram 上传分片只列出合法取值，存储分片滑杆的上限和步长随之变化，非法组合根本无法选出。

这些设置会保存在 SQLite 数据目录中，并且无需重启服务即可生效。连接池大小或请求间隔变化时，Telegram 连接会自动重建。

`TDRIVE_LOCAL_PATH` 是 Docker Compose 的宿主机路径，不是容器内路径。若使用 `docker run`，请手动添加
`-v /srv/repository:/vps:ro`，然后在 WebUI“设置 → 存储与暂存”中填写 `/vps`；不需要设置环境变量。

## 多用户与权限

管理员可以在“设置 → 用户管理”里为每个账号配置：

- **权限**（12 项）— 浏览、下载、上传、VPS 本地上传、离线下载、新建文件夹、重命名、移动、删除、WebDAV、服务器暂存、生成直链。
  不单独配置时跟随角色默认值；管理员始终拥有全部权限。
- **目录范围** — 把账号限定在某个子目录内，该目录就是它看到的根目录。WebUI 和 WebDAV 同时生效。
- **存储配额** — 按账号上传的文件累计。归属关系写进 Telegram 消息标签（`#own_`），重建索引后依然准确。
- **启停与会话** — 停用会立即终止该账号所有已登录会话；也可以单独注销某台设备。

所有账号和设置变更都会记录到操作日志，可在“设置 → 索引与日志”里按动作筛选并导出 CSV。

## 下载

从 WebUI 下载时会先询问方式：

| 方式 | 适用场景 |
|---|---|
| 直接下载 | 单卷文件。浏览器多线程直连，服务器边从 Telegram 读边发 |
| 先暂存到服务器 | **分卷文件的推荐方式**。服务器先把各分卷拼成完整文件写到磁盘，再由客户端从本地磁盘高速多线程取走 |
| 分卷下载后合并 | 不想占服务器磁盘时。每个分卷单独下载，Chrome / Edge 下自动写入同一个目标文件；其它浏览器逐卷下载并附带合并脚本 |

多线程写盘依赖 File System Access API（Chrome / Edge）。其它浏览器可以改用单线程，或者复制直链交给 aria2、IDM 等工具。

无论开多少条连接，服务器都把同一个文件的下载算作**一个**下载任务，因此并发限制和多线程下载不会互相打架。

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
- **前端** — React 19, Vite, Tailwind CSS 4, TypeScript；预览用 mediabunny / libav.js / pdf.js / shiki / SheetJS 等，全部按需懒加载
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
