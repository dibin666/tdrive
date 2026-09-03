# tdrive

把 Telegram 变成你的无限容量私人网盘。

**[English](README.md)**

---

## 快速安装

安装完成后，打开浏览器访问 `http://你的服务器IP:8080` 即可使用。

### 1. 最小安装（一行命令极速体验）

适合想快速尝试的用户，使用 Docker 一行命令直接启动：

```bash
docker run -d \
  --name tdrive \
  -p 8080:8080 \
  -v tdrive-data:/data \
  -e TDRIVE_ADMIN_USER=admin \
  -e TDRIVE_ADMIN_PASSWORD=your-password \
  ghcr.io/dibin666/tdrive:latest
```

启动后在浏览器打开 `http://localhost:8080`，使用账号 `admin` 和上面设置的密码登录。

---

### 2. 详细安装（Docker Compose，推荐日常使用）

推荐使用 Docker Compose 部署，数据持久化更好管理，也更方便配置反向代理和本地目录挂载。

#### 第一步：准备配置文件

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
      # 网盘数据目录（存放数据库、登录会话等）
      - tdrive-data:/data
      # 可选：挂载宿主机目录，方便直接导入服务器已有的大文件
      - ./vps-files:/vps:ro
    environment:
      # 初始管理员账号密码（仅首次启动生效）
      TDRIVE_ADMIN_USER: ${TDRIVE_ADMIN_USER:-admin}
      TDRIVE_ADMIN_PASSWORD: ${TDRIVE_ADMIN_PASSWORD}
      # 外部访问地址（配置域名反向代理时填写，例如 https://pan.example.com）
      TDRIVE_BASE_URL: ${TDRIVE_BASE_URL:-}

volumes:
  tdrive-data:
```

创建 `.env` 环境文件：

```bash
# 初始管理员账号与密码（必填，密码请设置为 8 位以上）
TDRIVE_ADMIN_USER=admin
TDRIVE_ADMIN_PASSWORD=change-this-please

# 绑定域名（如果使用反代或开启 HTTPS 请填写；本地 IP 访问可留空）
# TDRIVE_BASE_URL=https://pan.example.com
```

#### 第二步：启动服务

```bash
docker compose up -d
```

查看运行日志：

```bash
docker compose logs -f
```

---

### 3. 二进制直接运行（无 Docker）

如果不使用 Docker，可以直接下载对应平台的预编译可执行文件：

```bash
# 设置数据目录并启动
TDRIVE_DATA_DIR=./data ./tdrive
```

打开 `http://localhost:8080` 即可看到初始化页面。

---

## 首次使用配置

登录管理员账号后，只需要三步就能把网盘真正用起来：

1. **获取 Telegram 凭据**：浏览器访问 [my.telegram.org](https://my.telegram.org/apps)，登录你的 Telegram 账号，点击 **API development tools** 创建应用，获取 `api_id` 和 `api_hash`。
2. **绑定账号**：在网盘界面打开【设置】→【Telegram】，填入 `api_id` 和 `api_hash`，输入手机号和收到的验证码登录。
3. **设置存储频道**：在设置页选择现有的频道或新建一个私有频道，网盘就会把该频道当作存储仓库。

配置完成后就可以开始上传和管理文件了。

---

## 核心功能

tdrive 把 Telegram 频道当作存储底座，使用体验和常见网盘一样简单：

- **Web 文件管理**：支持文件多选、框选、拖拽、右键操作、批量重命名；移动端支持手势滑动与下拉刷新。
- **多格式在线预览**：支持图片、音视频、PDF、Markdown、代码高亮、Office 文档直接预览以及压缩包查看。
- **大文件自动分卷**：单文件超过 1.9GB 时自动切片上传，突破 Telegram 单文件 2GB 限制；下载时自动合并。
- **高速下载与直链**：支持多线程并发下载与断点续传；可生成带权限的独立直链，直接复制到 IDM 或 Aria2 批量下载。
- **WebDAV 挂载**：支持 WebDAV 协议，可挂载到 Windows、Mac、Linux、Rclone、Infuse 或 Alist，像本地磁盘一样使用。
- **多账号故障转移**：支持添加多个 Telegram 备用账号。主账号触发 Telegram 请求限制时，系统自动切换到备用账号继续传输，保证服务不中断。
- **多用户与权限管理**：支持多用户登录、限定用户的目录访问范围、设置空间配额以及细粒度操作权限。
- **离线下载与本地导入**：支持输入 HTTP 链接直接离线下载到网盘；支持直接挂载服务器本地文件秒级入库，不用经过浏览器中转。
- **数据防丢**：本地数据库仅作为加速缓存。即使数据库损坏或丢失，也能从 Telegram 频道一键完整重建所有目录和文件元数据。
- **插件扩展**：支持独立 Go 插件系统，方便扩展自定义功能。
- **超轻量部署**：单二进制 + SQLite，纯 Go 编写无繁重依赖，内存与 CPU 占用极低。

---

## 常用设置与说明

### WebDAV 挂载

WebDAV 挂载路径默认为 `/dav`，用户名和密码与网页端管理员（或子用户）账号一致：

```bash
# rclone 配置示例
rclone config create tdrive webdav \
  url=http://localhost:8080/dav \
  vendor=other \
  user=admin \
  pass="$(rclone obscure your-password)"

# 列出文件
rclone ls tdrive:
```

### 多 Telegram 账号配置要点

- **手机号要求**：每个账号必须是**不同的手机号**（同一手机号申请多个 API 凭据仍共用相同的限流额度）。
- **频道权限**：备用账号加入存储频道后，必须拥有**发消息、编辑消息、删除消息**权限。
- **独立代理**：每个账号均可在后台单独配置专属的 SOCKS5 或 HTTP 代理。

### 常用环境变量速查

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `TDRIVE_ADMIN_USER` | `admin` | 初始管理员用户名（仅首次启动生效） |
| `TDRIVE_ADMIN_PASSWORD` | 无 | 初始管理员密码（必填，≥8 位） |
| `TDRIVE_BASE_URL` | 空 | 外部访问地址（配置反代域名时填写，例如 `https://pan.example.com`） |
| `TDRIVE_DATA_DIR` | `/data` | 数据存放目录（存放数据库、会话等） |
| `TDRIVE_LISTEN` | `:8080` | HTTP 监听地址与端口 |
| `TDRIVE_LOCAL_PATH` | `./vps-files` | 宿主机本地文件目录（挂载后供 VPS 本地秒传） |

> **提示**：连接池、下载并发数、分片大小等更多性能参数，登录后可以在 WebUI 的【设置】里直观调节，即时生效，无需重启服务。

---

## 源码构建

```bash
# 1. 构建前端
cd ui && pnpm install && pnpm build && cd ..

# 2. 编译二进制
go build -trimpath -o tdrive ./cmd/tdrive
```

---

## 开源协议

本项目采用 [MIT](LICENSE) 许可证开源。
