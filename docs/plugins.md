# tdrive 插件

tdrive 插件是独立的 Go 子进程。插件通过 `github.com/hashicorp/go-plugin`
使用本地 RPC 与主程序通信，源码获取使用 `github.com/hashicorp/go-getter/v2`。
插件崩溃不会直接带崩主程序；插件可以被停用、重启或卸载。

## 信任边界

插件是**全信任代码**：

- 安装时只确认一次，不提供能力勾选，也没有第二次授权。
- 插件可以调用公开 Host SDK 的文件树、上传、下载、传输、Telegram 状态、用户、运行设置、事件、命名空间数据和 HTTP 扩展接口。
- 插件可以注册核心操作的 Before/After Hook，并修改 Before Hook 的 JSON 请求；因此它可以改变所有已公开的核心操作行为。
- 插件进程本身可以使用它构建出的 Go 程序拥有的操作系统能力。RPC 隔离的是主程序内存，不是安全沙箱。
- 普通用户访问插件 HTTP 路由时仍然经过 tdrive 登录认证；插件管理 API 仍然只允许管理员。

不要安装不信任的插件。检查页显示源码地址、固定版本、源码 SHA-256、作者、许可证、API 版本和声明能力，但这些信息不是安全保证。

## 目录结构

插件源码根目录必须包含 `tdrive.plugin.json`：

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

`tdrive.plugin.json` 必须位于源码根目录，字段如下：

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
| `entrypoint` | 是 | 相对 Go package 路径，例如 `./cmd/my-plugin` |
| `capabilities` | 否 | 只用于安装检查页展示，不参与授权 |
| `events` | 否 | 要订阅的事件名，`*` 表示全部事件 |
| `routes` | 否 | 插件 HTTP 路由声明 |

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

## 构建和调试

插件必须使用与 tdrive 兼容的 Go 版本构建，并关闭 CGO：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -buildvcs=false -o my-plugin ./cmd/my-plugin
```

使用示例插件本地编译：

```bash
cd examples/plugin-hello
go build -trimpath -o plugin-hello ./cmd/plugin-hello
```

插件运行日志通过独立插件日志通道处理。插件可以在 `Initialize` 中保存 Host，
并在 `Shutdown` 中关闭自身的后台工作。

## 安装流程

管理员在 WebUI“设置 → 插件”中选择“商店”或“源码安装”：

1. 输入源码 HTTPS 地址和可选固定 Git ref，或从商店选择条目。
2. tdrive-plugin-builder 获取源码、检查根目录 manifest、检查 SemVer/API 版本并计算源码摘要。
3. 页面展示检查结果；点击一次“确认安装”。这里不显示权限勾选。
4. builder 重新获取并核对同一源码摘要，然后用 `CGO_ENABLED=0` 和 `-trimpath` 编译。
5. 二进制先写入 staging 目录，验证 manifest、SHA-256 和 RPC 握手后原子替换旧版本。
6. 写入本地 SQLite 元数据并立即启动插件；成功响应状态为 `active`。

如果构建、握手或启动失败，旧版本保持不变，staging 文件会被清理。源码地址只允许
HTTPS；当前不接受本地路径、SSH 地址、URL 凭据或明显的私网地址。Git 分支会在检查
和安装之间重新校验摘要，建议商店和生产安装都使用不可变 commit 或 release tag。

## 配置和部署

| 环境变量 | 默认值 | 用途 |
|---|---|---|
| `TDRIVE_PLUGIN_DIR` | `<data dir>/plugins` | 插件二进制和临时目录 |
| `TDRIVE_PLUGIN_STORE_URL` | 空 | 插件商店 `index.json` 的 HTTPS 地址；空值关闭商店 |
| `TDRIVE_PLUGIN_BUILDER_ADDRESS` | `<data dir>/plugin-builder.sock` | builder Unix socket 或 loopback 地址 |
| `TDRIVE_PLUGIN_BUILDER_COMMAND` | `tdrive-plugin-builder` | 无 sidecar 时按需启动的 builder 命令 |
| `TDRIVE_PLUGIN_SOURCE_MAX_BYTES` | `512MiB` | 源码树大小上限 |
| `TDRIVE_PLUGIN_BUILD_TIMEOUT` | `10m` | 单次构建超时 |

Docker Compose 使用独立的 `tdrive-plugin-builder` 服务。主 tdrive 容器仍然是
distroless，不包含 Go、Git 或 Shell；两个服务共享插件卷和仅用于 builder 的 Unix
socket。非 Docker 二进制部署需要单独安装 builder，或把
`TDRIVE_PLUGIN_BUILDER_COMMAND` 指向本机可执行文件。

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
      "ref": "v1.0.0",
      "sourceDigest": "64 lowercase hexadecimal SHA-256 characters",
      "documentationUrl": "https://example.com/tdrive-plugin-example",
      "license": "MIT",
      "tags": ["files", "notification"]
    }
  ]
}
```

提交商店索引 Pull Request 前必须满足：

- 源码仓库根目录有 `tdrive.plugin.json`，`id` 唯一，`version` 是合法 SemVer，`apiVersion` 与支持的 tdrive 版本兼容。
- 源码、文档和 release 地址使用公开 HTTPS；禁止把 token、密码、私钥或 Telegram 凭据提交到仓库。
- 每个版本使用固定 release tag 或 commit；`sourceDigest` 是检查过的完整源码树 SHA-256，安装时仍会重新核对。
- manifest 提供作者、许可证、源码仓库和文档地址；建议同时提供安全联系方式。
- README 说明功能、Hook、外部服务、数据处理和已知风险，并提供可运行测试。
- Linux `amd64` 和 `arm64` 都能以 `CGO_ENABLED=0`、`-trimpath` 构建；如果项目只支持其中一个架构，必须在条目中明确说明。
- 构建过程可复现，不依赖未声明的本地文件、私有路径、交互式登录或提交目录外的符号链接。
- 商店条目中的名称、版本、作者、许可证、仓库地址、ref 和摘要必须与源码 manifest/release 一致。

推荐的索引仓库布局：

```text
plugin-registry/
├── index.json
├── schema.json
└── README.md
```

默认空索引位于 [`plugins/index.json`](../plugins/index.json)。
