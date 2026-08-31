# tdrive 插件

tdrive 插件是独立的 Go 子进程。插件通过 `github.com/hashicorp/go-plugin`
使用本地 RPC 与主程序通信。插件崩溃不会直接带崩主程序；插件可以被停用、重启或卸载。

tdrive **不在服务器上编译插件**。插件作者自己交叉编译好各平台二进制并发布到
release，tdrive 只做「下载 → 校验 SHA-256 → 握手 → 启动」。因此主镜像保持
distroless，不含 Go、Git 或 shell，单个容器即可完成插件安装。

## 信任边界

插件是**全信任代码**：

- 安装时只确认一次，不提供能力勾选，也没有第二次授权。
- 插件可以调用公开 Host SDK 的文件树、上传、下载、传输、Telegram 状态、用户、运行设置、事件、命名空间数据和 HTTP 扩展接口。
- 插件可以注册核心操作的 Before/After Hook，并修改 Before Hook 的 JSON 请求；因此它可以改变所有已公开的核心操作行为。
- 插件进程以 tdrive 进程的权限运行，拥有该进程全部操作系统能力。RPC 隔离的是主程序内存，不是安全沙箱。
- 普通用户访问插件 HTTP 路由时仍然经过 tdrive 登录认证；插件管理 API 仍然只允许管理员。

不要安装不信任的插件。检查页显示清单地址、版本、目标平台、二进制 SHA-256、作者、
许可证、API 版本和声明能力。SHA-256 保证「装上去的字节就是检查页上那一份」，但它
不保证那份字节是安全的。

信任链是三级固定的：商店索引的 `manifestDigest` 固定清单 → 清单的
`artifacts[].sha256` 固定二进制 → 启动前 go-plugin 再校验一次二进制哈希。手动粘贴
清单地址时没有第一级，此时固定强度取决于该地址是否不可变（release 资产或带 tag 的
raw 地址是不可变的，指向分支的 raw 地址不是）。

## 目录结构

插件源码根目录应包含 `tdrive.plugin.json`，同一份文件也随 release 一起发布：

```text
my-plugin/
├── tdrive.plugin.json
├── go.mod
├── README.md
└── cmd/my-plugin/main.go
```

最小入口：

```go
package main

import (
    "context"
    tdrive "github.com/dibin/tdrive/pkg/plugin"
)

type plugin struct{}

func (plugin *plugin) Manifest() tdrive.Manifest {
    return tdrive.Manifest{
        ID: "my-plugin", Name: "My plugin", Version: "1.0.0",
        SDKVersion: "0.1", APIVersion: 1,
        Author: "Example", License: "MIT",
        RepositoryURL: "https://github.com/example/my-plugin",
        Entrypoint: "./cmd/my-plugin",
    }
}

func (plugin *plugin) Initialize(_ context.Context, _ tdrive.Host) error {
    return nil
}

func main() {
    tdrive.Serve(&plugin{})
}
```

如果实现可选接口，插件还可以获得 Hook、事件和 HTTP 扩展：

```go
func (plugin *plugin) Before(ctx context.Context, operation tdrive.Operation) (tdrive.OperationResult, error) {
    // 修改 operation.Payload，或返回 Allowed: false 拒绝操作。
    return tdrive.OperationResult{Allowed: true, Payload: operation.Payload}, nil
}

func (plugin *plugin) OnEvent(ctx context.Context, event tdrive.Event) {
    // 处理 manifest.events 中声明的事件。
}

func (plugin *plugin) HandleHTTP(ctx context.Context, request tdrive.HTTPRequest) (tdrive.HTTPResponse, error) {
    return tdrive.HTTPResponse{Status: 200, Body: []byte("ok")}, nil
}
```

## Manifest

`tdrive.plugin.json` 字段如下：

| 字段 | 必填 | 规则 |
|---|---:|---|
| `id` | 是 | 小写字母开头，只允许小写字母、数字和 `-`，最多 63 字符 |
| `name` | 是 | 展示名称 |
| `description` | 否 | 简短纯文本描述 |
| `version` | 是 | SemVer 2.0.0，例如 `1.2.0` |
| `sdkVersion` | 是 | SDK 版本字符串 |
| `apiVersion` | 是 | 当前为 `1` |
| `minTdriveVersion` | 否 | 要求的最低 tdrive 版本 |
| `author` | 是 | 作者或组织 |
| `license` | 是 | SPDX 许可证标识，例如 `MIT` |
| `repositoryUrl` | 是 | 公开 HTTPS 源码仓库 |
| `documentationUrl` | 否 | 公开 HTTPS 文档地址 |
| `artifacts` | 是 | 平台 → 预编译二进制，至少一项 |
| `entrypoint` | 否 | 相对 Go package 路径，例如 `./cmd/my-plugin`；只作说明，tdrive 不使用 |
| `capabilities` | 否 | 只用于安装检查页展示，不参与授权 |
| `events` | 否 | 要订阅的事件名，`*` 表示全部事件 |
| `routes` | 否 | 插件 HTTP 路由声明 |

`artifacts` 的 key 写成 `goos/goarch`，value 是绝对 HTTPS 地址加 64 位小写十六进制
SHA-256：

```json
"artifacts": {
  "linux/amd64": {
    "url": "https://github.com/owner/my-plugin/releases/download/v1.2.0/my-plugin-linux-amd64",
    "sha256": "3b1f…"
  },
  "linux/arm64": {
    "url": "https://github.com/owner/my-plugin/releases/download/v1.2.0/my-plugin-linux-arm64",
    "sha256": "9c04…"
  },
  "windows/amd64": {
    "url": "https://github.com/owner/my-plugin/releases/download/v1.2.0/my-plugin-windows-amd64.exe",
    "sha256": "7ae2…"
  }
}
```

tdrive 只查找与自己 `GOOS/GOARCH` 完全一致的那一项。缺少对应平台时安装会失败，并在
错误里列出该插件实际发布了哪些平台。

tdrive 主程序发布 Linux 和 Windows 版本，插件两边都支持，`windows/amd64` 和
`windows/arm64` 是合法的平台 key。**release 资产叫什么名字都行**：tdrive 下载后会
按自己的规则重命名成 `<插件目录>/<id>`，在 Windows 上是 `<插件目录>/<id>.exe`。
`.exe` 后缀不是习惯问题——Go 的 `os/exec` 解析绝对路径时，对没有扩展名的路径根本
不会去 stat 它本身，只会尝试路径加上各个 `PATHEXT` 后缀，因此少了后缀会在
`CreateProcess` 之前就以 `ErrNotFound` 失败。

**`artifacts` 只写在 JSON 文件里，不要写进 Go 代码的 `Manifest()`。** 二进制不可能
包含自身的 SHA-256——把摘要填进源码会改变被哈希的字节，永远算不出不动点。因此
`Manifest()` 返回的结构体不填 `Artifacts`，tdrive 在比对「插件自报 manifest」和
「已安装 manifest」时也会跳过这个字段。除 `artifacts` 外的所有字段必须与发布的
`tdrive.plugin.json` 完全一致，否则插件装上后启动会被拒绝。

路由项格式：

```json
{
  "path": "/",
  "methods": ["GET"],
  "ui": true
}
```

插件路由最终位于 `/plugins/{id}/...`。`ui: true` 的路由会在设置页显示“打开”。

## Host API

插件通过 `Host.Call` 调用主程序。请求和响应是 JSON，常用方法如下：

| 方法 | 用途 |
|---|---|
| `files.list` | 读取目录列表，参数 `{ "path": "/" }` |
| `files.stat` | 读取文件或目录信息 |
| `files.mkdir` | 创建目录 |
| `files.rename` | 重命名 |
| `files.move` | 移动，参数 `{ "from": "/a", "toDir": "/b" }` |
| `files.delete` | 删除文件或目录 |
| `files.beginUpload` | 创建可续传上传任务 |
| `files.putSegment` | 通过 `Host.OpenStream` 上传一个分片 |
| `files.completeUpload` | 完成上传任务 |
| `files.abortUpload` | 取消或失败上传任务 |
| `files.readChunk` | 读取不超过 16 MiB 的数据块 |
| `downloads.stage` | 将文件暂存到服务器 |
| `downloads.cancel` | 取消暂存下载 |
| `users.list` | 读取非敏感用户信息 |
| `settings.get` | 读取运行设置 |
| `settings.update` | 更新运行设置 |
| `telegram.status` | 读取 Telegram 连接状态 |
| `events.publish` | 发布自定义事件 |
| `data.get` | 读取插件自己的命名空间数据 |
| `data.set` | 写入插件自己的命名空间数据 |
| `data.delete` | 删除插件自己的命名空间数据 |

大数据使用 `Host.OpenStream`。`files.read` 参数包含 `fileId`、`offset` 和 `size`，
单次流读取上限为 64 MiB；`files.putSegment` 参数包含 `jobId`、`index` 和 `size`，
插件把分片字节写入返回的流，关闭流后等待分片提交。上传等长时间操作应使用 tdrive
的上传任务和分片 API，避免一次 RPC 携带整个文件。

## Hook 和事件

核心操作会按以下名字触发 Before/After Hook：

```text
files.list
files.stat
files.open
files.mkdir
files.rename
files.move
files.delete
files.deleteByID
files.beginUpload
files.putSegment
files.completeUpload
downloads.stage
downloads.stagedFile
downloads.cancel
```

Before Hook 可以修改请求 JSON 或拒绝操作；After Hook 是通知性质，操作已经完成后
才执行，Hook 错误不会回滚核心操作。事件类型包括：

```text
upload download index telegram tree
```

事件订阅只对 manifest 中声明的类型生效。没有活跃插件时，核心不创建插件事件订阅，
核心 Hook 指针保持为空。

## 构建和发布

插件必须使用与 tdrive 兼容的 Go 版本构建，并关闭 CGO。逐个平台交叉编译，然后把
`sha256sum` 的结果填回 `artifacts`：

```bash
for platform in linux/amd64 linux/arm64 windows/amd64; do
  os="${platform%/*}"; arch="${platform#*/}"
  suffix=""; [ "$os" = windows ] && suffix=.exe
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath -buildvcs=false \
    -o "my-plugin-$os-$arch$suffix" ./cmd/my-plugin
done
sha256sum my-plugin-*
```

把二进制和填好摘要的 `tdrive.plugin.json` 一起作为 release 资产上传。管理员安装时
粘贴的就是这个 `tdrive.plugin.json` 的地址：

```bash
gh release create v1.2.0 \
  my-plugin-linux-amd64 my-plugin-linux-arm64 my-plugin-windows-amd64.exe \
  tdrive.plugin.json
```

同样的事情在 GitHub Actions 里：

```yaml
- uses: actions/setup-go@v7
  with:
    go-version: '1.26'
- name: Build and publish
  env:
    GH_TOKEN: ${{ github.token }}
  run: |
    set -euo pipefail
    assets=()
    for platform in linux/amd64 linux/arm64 windows/amd64; do
      os="${platform%/*}"; arch="${platform#*/}"
      suffix=""; [ "$os" = windows ] && suffix=.exe
      asset="my-plugin-$os-$arch$suffix"
      CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
        go build -trimpath -buildvcs=false -o "$asset" ./cmd/my-plugin
      # 用实际摘要替换 manifest 中的占位值。
      digest="$(sha256sum "$asset" | cut -d' ' -f1)"
      jq --arg a "$platform" --arg d "$digest" \
        '.artifacts[$a].sha256 = $d' tdrive.plugin.json > tmp.json
      mv tmp.json tdrive.plugin.json
      assets+=("$asset")
    done
    gh release create "$GITHUB_REF_NAME" "${assets[@]}" tdrive.plugin.json
```

本地调试示例插件：

```bash
cd examples/plugin-hello
go build -trimpath -o plugin-hello ./cmd/plugin-hello
```

`examples/plugin-hello/tdrive.plugin.json` 里的 `sha256` 是全 0 占位值，发布前必须替换
成真实摘要，否则 tdrive 会拒绝安装。

插件运行日志通过独立插件日志通道处理。插件可以在 `Initialize` 中保存 Host，
并在 `Shutdown` 中关闭自身的后台工作。

## 安装流程

管理员在 WebUI“设置 → 插件”中选择“商店”或“安装插件”：

1. 粘贴插件清单（`tdrive.plugin.json`）的 HTTPS 地址，或从商店选择条目。
2. tdrive 下载清单（上限 1 MiB），校验字段、SemVer 和 API 版本，按本机
   `GOOS/GOARCH` 选出对应二进制，并计算清单本身的 SHA-256。从商店安装时还会核对
   索引中的 `manifestDigest`。**这一步不下载也不执行任何二进制。**
3. 页面展示检查结果：作者、许可证、目标平台、二进制 SHA-256。点击一次“确认安装”，
   这里不显示权限勾选。
4. tdrive 把二进制流式下载到 staging 目录，边写边算 SHA-256，与清单中声明的摘要
   比对。不一致就中止并删除临时文件，磁盘上不留残留。
5. 校验通过后原子替换旧版本，并通过 go-plugin 的 `SecureConfig` 在 exec 前再次校验
   二进制哈希，然后完成 RPC 握手和 manifest 比对。
6. 写入本地 SQLite 元数据并立即启动插件；成功响应状态为 `active`。

如果下载、校验、握手或启动失败，旧版本保持不变，staging 文件会被清理。所有地址
只允许 HTTPS；不接受本地路径、SSH 地址、URL 凭据或明显的私网地址，每一次重定向都
会重新走同一套校验。建议商店和生产安装都使用不可变的 release 资产地址。

## 配置和部署

| 环境变量 | 默认值 | 用途 |
|---|---|---|
| `TDRIVE_PLUGIN_DIR` | `<data dir>/plugins` | 插件二进制和临时目录；插件私有数据固定保存在 `<data dir>/plugin-data/<插件 ID>` |
| `TDRIVE_PLUGIN_STORE_URL` | 空 | 插件商店 `index.json` 的 HTTPS 地址；空值关闭商店 |
| `TDRIVE_PLUGIN_MAX_BINARY_BYTES` | `256MiB` | 单个插件二进制大小上限 |

Docker Compose 只有一个 `tdrive` 服务和一个数据卷。插件目录默认在 `/data/plugins`，
插件私有数据在 `/data/plugin-data/<插件 ID>`，都和数据库、Telegram 会话同卷，所以插件
及其下载的运行时二进制会随数据卷一起备份。非 Docker 二进制部署不需要
任何额外组件：安装插件只用到 HTTPS 出站访问。

启动插件进程时，tdrive 会通过 `TDRIVE_PLUGIN_DATA_DIR` 传入该插件的绝对持久化目录。
插件应把登录凭据、缓存和运行时下载的二进制放在这个目录中，不要根据自身可执行文件的
位置推导数据目录；插件更新时可执行文件会被原子替换，但这个目录保持不变。

tdrive 只在管理员主动检查或安装插件时访问网络。启动路径不会下载任何东西，也不会
为已停用的插件启动进程。

## 插件商店规范

商店只负责发现插件，不代替用户信任判断，也不绕过一次安装确认。索引可以是静态
HTTPS JSON 文件，格式如下：

```json
{
  "updatedAt": "2026-08-31T00:00:00Z",
  "plugins": [
    {
      "id": "example",
      "name": "Example",
      "description": "Short description",
      "version": "1.0.0",
      "author": "Example",
      "repositoryUrl": "https://github.com/example/tdrive-plugin-example",
      "manifestUrl": "https://github.com/example/tdrive-plugin-example/releases/download/v1.0.0/tdrive.plugin.json",
      "manifestDigest": "64 lowercase hexadecimal SHA-256 characters",
      "documentationUrl": "https://example.com/tdrive-plugin-example",
      "license": "MIT",
      "tags": ["files", "notification"]
    }
  ]
}
```

`manifestDigest` 是 `manifestUrl` 所指文件的 SHA-256。它是整条信任链的起点：索引固定
清单，清单固定二进制。安装时 tdrive 会重新核对这个值，对不上就拒绝。

提交商店索引 Pull Request 前必须满足：

- 源码仓库根目录有 `tdrive.plugin.json`，`id` 唯一，`version` 是合法 SemVer，`apiVersion` 与支持的 tdrive 版本兼容。
- 源码、文档、清单和二进制地址都使用公开 HTTPS；禁止把 token、密码、私钥或 Telegram 凭据提交到仓库。
- `manifestUrl` 指向不可变地址（release 资产，或带 tag 的 raw 地址），不能指向分支。
- `manifestDigest` 是该清单文件的 SHA-256，安装时仍会重新核对。
- 清单的 `artifacts` 至少覆盖 `linux/amd64` 和 `linux/arm64`，二进制以 `CGO_ENABLED=0`、`-trimpath` 构建；建议同时提供 `windows/amd64`。只支持部分平台时必须在条目中明确说明。
- 每个 `artifacts` 条目的 `sha256` 与实际发布的二进制一致，且发布后不再替换同名资产。
- manifest 提供作者、许可证、源码仓库和文档地址；建议同时提供安全联系方式。
- README 说明功能、Hook、外部服务、数据处理和已知风险，并提供可运行测试。
- 构建过程可复现，不依赖未声明的本地文件、私有路径或交互式登录。
- 商店条目中的名称、版本、作者、许可证、仓库地址和摘要必须与清单/release 一致。

推荐的索引仓库布局：

```text
plugin-registry/
├── index.json
├── schema.json
└── README.md
```

默认空索引位于 [`plugins/index.json`](../plugins/index.json)。
