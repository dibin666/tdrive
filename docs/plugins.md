# tdrive 插件开发指南

本文档是 tdrive 插件系统的权威开发规范与参考手册，服务于两类读者：
1. **人类开发者**：了解架构设计动机、安全信任模型、生命周期管理与开发避坑指南。
2. **LLM 编程助手**：作为独立完整的上下文，包含完整的 Host API 请求/响应 JSON 格式、Hook 负载结构、事件定义与开箱即用的完整 Go 代码。

---

## 目录

1. [核心设计与架构模型](#1-核心设计与架构模型)
2. [权限与安全信任边界](#2-权限与安全信任边界)
3. [开发避坑指南（必读）](#3-开发避坑指南必读)
4. [插件清单规范 (tdrive.plugin.json)](#4-插件清单规范-tdrivepluginjson)
5. [完整可运行代码示例](#5-完整可运行代码示例)
6. [Host API 接口规范](#6-host-api-接口规范)
7. [Hook 机制与操作拦截](#7-hook-机制与操作拦截)
8. [事件驱动机制 (Events)](#8-事件驱动机制-events)
9. [HTTP 路由扩展](#9-http-路由扩展)
10. [构建、调试与发布流程](#10-构建调试与发布流程)
11. [环境变量与运维配置](#11-环境变量与运维配置)
12. [插件商店索引规范](#12-插件商店索引规范)

---

## 1. 核心设计与架构模型

### 1.1 术语定义
- **宿主 (Host / tdrive)**：tdrive 主程序，负责文件存储管理、Telegram 通信调度、用户权限和插件生命周期管理。
- **插件 (Plugin)**：由开发者预先编译好的独立 Go 二进制可执行文件。
- **子进程 RPC**：宿主通过 `github.com/hashicorp/go-plugin` 拉起插件子进程，双方基于 `net/rpc` 进行跨进程双向 RPC 通信。插件崩溃不会直接导致主程序崩溃。
- **所有者账号 (Owner / Caller)**：在 tdrive 中执行安装该插件的用户账号。插件严格归属于单个账号。
- **Manifest（插件清单）**：描述插件元数据、版本、路由、订阅事件及各平台预编译二进制 SHA-256 的 `tdrive.plugin.json` 文件。

### 1.2 每账号独立安装架构 (Per-Account Ownership)
tdrive 的插件体系采用**「每账号各自安装」**架构，而非传统的部署级全局单例：

```text
┌─────────────────────────────────────────────────────────────┐
│                       tdrive 宿主进程                       │
│                                                             │
│  ┌───────────────────────┐       ┌───────────────────────┐  │
│  │   账号 A (管理员)     │       │     账号 B (普通用户)  │  │
│  │                       │       │                       │  │
│  │  ┌─────────────────┐  │       │  ┌─────────────────┐  │  │
│  │  │ 插件 aliyunpan  │  │       │  │ 插件 aliyunpan  │  │  │
│  │  │ (v1.2.0 子进程) │  │       │  │ (v1.1.0 子进程) │  │  │
│  │  └────────┬────────┘  │       │  └────────┬────────┘  │  │
│  └───────────┼───────────┘       └───────────┼───────────┘  │
└──────────────┼───────────────────────────────┼──────────────┘
               ▼                               ▼
  数据目录: /data/plugin-data/userA/...     /data/plugin-data/userB/...
  程序路径: /data/plugins/userA/aliyunpan   /data/plugins/userB/aliyunpan
```

每个账号拥有独立的安装列表、独立的二进制副本、独立的子进程实例和独立的数据目录：
- 两个账号可以安装同一个插件的不同版本，运行状态互不干扰。
- 管理员在插件系统中本质上也是一个普通账号，只能查看和管理自己安装的插件。
- 系统的进程预算由「插件数」扩展为「账号数 × 插件数」，由部署级和单账号级双重上限控制。

### 1.3 核心隔离矩阵

| 隔离维度 | 行为说明 | 设计原因 |
|---|---|---|
| **HTTP 路由** | `/plugins/{id}/...` 强制按当前登录调用者解析。访问别人安装的插件返回 **404 Not Found**。 | 404 与「不存在该插件」无法区分，彻底杜绝该路由沦为嗅探他人安装列表的隐式接口。 |
| **Before/After Hook** | 核心操作 Hook 只分发给**发起该请求的那个账号**所拥有的插件。未登录操作（后台维护、索引重建）不分发给任何插件。 | 防止恶意普通用户利用插件 Hook 窃取或篡改其他用户的核心文件请求。 |
| **事件广播** | 按 `userId` 精准定向推送。无 `userId` 的系统级事件仅限 `telegram` 和 `index` 广播；带路径的 `tree` 事件绝不跨账号广播。 | 避免用户 A 的文件名和路径通过目录树变更事件泄漏给用户 B 的插件。 |
| **`users.list` API** | Host API 调用时，**仅返回插件所有者自身**的单个用户信息。 | 插件只归属于其所有者，不需要也禁止窥探整个服务器的用户花名册。 |
| **`settings.*` API** | `settings.get` 和 `settings.update` 严格要求插件所有者必须具备管理员角色 (`admin`)。 | 运行时参数（如 Telegram 凭据、并发线程）属于部署级敏感配置，对齐 `/api/settings` 的鉴权标准。 |
| **KV 数据存储** | `data.get` / `data.set` / `data.delete` 的数据命名空间是 `(userID, pluginID)`。 | 账号 A 和账号 B 安装相同插件时，私有配置与状态完全隔离。 |
| **文件系统存储** | 二进制路径为 `<data>/plugins/<userID>/<pluginID>`；数据持久化目录为 `<data>/plugin-data/<userID>/<pluginID>/`。 | 避免可执行文件覆盖冲突，保证各自的数据与登录凭据相互不可见。 |

---

## 2. 权限与安全信任边界

### 2.1 必须明确的硬约束与信任原则

> **【必须遵守的信任前提】**
> - **插件是全信任代码（Full-Trust Code）**：tdrive 插件**不是**运行在安全沙箱（如 WASM 或 gVisor）中。插件子进程继承宿主 tdrive 进程的全部操作系统权限、网络权限和宿主文件读写能力。RPC 隔离的只是内存崩溃，不是越权防护。
> - **安装即完全授权**：安装检查页仅确认一次，不提供细粒度权限勾选，安装后无二次运行时授权。
> - **`plugins` 权限是代码执行总闸门**：tdrive 权限系统中引入 `PermPlugins`（名称 `"plugins"`）。**默认仅管理员角色持有**。非管理员用户被授予 `plugins` 权限，即等同于被授予在宿主服务器执行任意二进制代码的能力。

### 2.2 Host API 层的权限穿透限制（当前实现状态）
目前 Host API 的 `files.*` 和 `downloads.*` 等底层文件操作**尚未穿透账号作用域检查**（`drive.ScopeOf` 仅在 HTTP API 接入层生效）。这意味着：
- 即使普通用户的 WebUI 账号受限于只读或被限定在特定子目录，其安装的插件通过 `Host.Call("files.*", ...)` 仍然可以读写整棵 drive 树。
- **安全结论**：在 Host API 细粒度权限校验落地前，切勿将 `plugins` 权限下放给不完全信任的普通账号。

### 2.3 严密的防篡改信任链
tdrive 通过三级信任链防止二进制被中间人篡改或恶意替换：
1. **商店层校验**：从商店安装时，核对商店索引中的 `manifestDigest` 与实际下载的 `tdrive.plugin.json` SHA-256 一致。
2. **下载层校验**：流式下载二进制至 staging 目录时计算 SHA-256，与清单内申明的 `artifacts[goos/goarch].sha256` 逐字节比对，不匹配立即中断并删除文件。
3. **执行前校验**：启动进程时，由 `go-plugin` 的 `SecureConfig` 在 `exec` 启动子进程前对落盘二进制重新计算并比对哈希值，确保磁盘文件未被外部篡改。
4. **单次使用确认令牌**：检查接口生成的 `inspectionId` 生命周期为 10 分钟，强绑定发起检查的 `userId`，且确认安装后立即失效销毁。

---

## 3. 开发避坑指南（必读）

写给所有插件作者及适配升级老插件的开发者，以下是最高频的失误点：

### 坑点 1：禁止用 `users.list` 寻找管理员
- **现象**：`users.list` 现在**只返回 1 条记录**（即插件所有者自身）。
- **后果**：如果代码写了 `for _, u := range users { if u.Role == "admin" { ... } }`，当所有者是普通用户时，插件将永远找不到管理员而直接崩溃或挂起！
- **正确做法**：插件需要知道文件上传归属谁时，直接取 `users[0].ID`。插件无需判断调用者身份，因为能请求到插件 HTTP 路由的调用者已被 tdrive 校验确认就是所有者。

### 坑点 2：不要假设 `settings.get` 一定成功
- **现象**：如果插件所有者是非管理员，调用 `settings.get` 会收到明确错误：`"这个插件的所有者不是管理员，无法读取或修改运行参数"`。
- **正确做法**：把 `settings.get` 视为**可选调用**。若失败，应优雅降级回退到插件自带的默认配置；在 UI 上给予明确提示，严禁向用户展示一排全 0 数据的错误界面。

### 坑点 3：`RuntimeSettings` 的 JSON 字段是大驼峰 (PascalCase)
- **现象**：宿主内部的 `config.RuntimeSettings` 结构体**没有加 json 标签**。
- **后果**：Go 的 `json.Marshal` 序列化出来的 key 是 `SegmentSize`、`AppID`、`UploadConcurrency`，而**不是**小驼峰 `segmentSize`。如果插件反序列化时使用了小驼峰结构体，将得到全 0 的空值！

### 坑点 4：`Manifest()` 结构体绝对不要填充 `Artifacts`
- **现象**：可执行二进制在编译时，不可能提前知晓自己编译产物的 SHA-256 哈希。
- **后果**：如果把 `Artifacts` 写入 Go 代码的 `Manifest()`，会导致运行时自报清单与安装时清单无法匹配。
- **规范**：Go 代码里的 `Manifest()` 方法中留空 `Artifacts` 即可（tdrive 运行时比对时会主动忽略此字段）。`artifacts` **仅填写在发布的 `tdrive.plugin.json` 文件中**。

### 坑点 5：`data.get` 查不到 Key 时返回错误，不是返回 null
- **现象**：当某个 `key` 从未写入过时，宿主返回数据库未找到错误（`database: not found`）。
- **正确做法**：判断错误字符串是否包含 `"not found"`。如果是，说明是首次运行或未初始化的键，应初始化默认值，切勿将其当作严重故障抛出。

### 坑点 6：Windows 二进制必须以 `.exe` 结尾
- **现象**：tdrive 下载二进制并落盘时，如果当前是 Windows 平台，会自动补全 `.exe` 后缀。
- **原理**：Go 的 `os/exec` 在 Windows 平台上解析绝对路径时，若无扩展名则不会直接探测原路径，而是遍历 `PATHEXT` 后缀，否则在 `CreateProcess` 之前就会抛出 `ErrNotFound`。编译 Windows 产物时务必输出为 `.exe`。

---

## 4. 插件清单规范 (tdrive.plugin.json)

`tdrive.plugin.json` 放置在源码根目录，并随 GitHub Release 资产一同发布。

### 4.1 完整清单 JSON 范例
```json
{
  "id": "sample-plugin",
  "name": "示例插件",
  "description": "一个演示完整生命周期与接口调用的 tdrive 示例插件",
  "version": "1.0.0",
  "sdkVersion": "0.1",
  "apiVersion": 1,
  "minTdriveVersion": "0.8.0",
  "author": "tdrive-team",
  "license": "MIT",
  "repositoryUrl": "https://github.com/example/tdrive-plugin-sample",
  "documentationUrl": "https://github.com/example/tdrive-plugin-sample/blob/main/README.md",
  "entrypoint": "./cmd/sample-plugin",
  "capabilities": ["events", "http", "hooks"],
  "events": ["tree", "upload", "download", "telegram"],
  "routes": [
    {
      "path": "/",
      "methods": ["GET"],
      "ui": true
    },
    {
      "path": "/api/*",
      "methods": ["GET", "POST", "PUT", "DELETE"],
      "ui": false
    }
  ],
  "artifacts": {
    "linux/amd64": {
      "url": "https://github.com/example/tdrive-plugin-sample/releases/download/v1.0.0/sample-plugin-linux-amd64",
      "sha256": "4b7b3b8c6e289f0a8c2f1f0a8c2f1f0a8c2f1f0a8c2f1f0a8c2f1f0a8c2f1f0a"
    },
    "linux/arm64": {
      "url": "https://github.com/example/tdrive-plugin-sample/releases/download/v1.0.0/sample-plugin-linux-arm64",
      "sha256": "8e3c1b8c6e289f0a8c2f1f0a8c2f1f0a8c2f1f0a8c2f1f0a8c2f1f0a8c2f1f0a"
    },
    "windows/amd64": {
      "url": "https://github.com/example/tdrive-plugin-sample/releases/download/v1.0.0/sample-plugin-windows-amd64.exe",
      "sha256": "9a2f1b8c6e289f0a8c2f1f0a8c2f1f0a8c2f1f0a8c2f1f0a8c2f1f0a8c2f1f0a"
    }
  }
}
```

### 4.2 字段约束规范与验证规则

| 字段 | 类型 | 必填 | 格式与语义约束 |
|---|---|:---:|---|
| `id` | `string` | **是** | 小写字母开头，仅允许小写字母、数字和中划线 `-`，长度不超过 63 字符（正则：`^[a-z][a-z0-9-]{0,62}$`）。 |
| `name` | `string` | **是** | 插件的直观展示名称，不可为空白字符串。 |
| `description` | `string` | 否 | 插件功能简短介绍。 |
| `version` | `string` | **是** | 合法的 SemVer 2.0.0 版本号（如 `1.0.0`, `0.2.1-beta`）。 |
| `sdkVersion` | `string` | **是** | SDK 版本标识，当前为 `"0.1"`。 |
| `apiVersion` | `int` | **是** | 宿主 API 协议版本，当前恒为正整数 `1`。 |
| `minTdriveVersion`| `string`| 否 | 说明性质的最低兼容版本（注：宿主目前仅作展示，不强制拦截）。 |
| `author` | `string` | **是** | 作者名或组织名称。 |
| `license` | `string` | **是** | 规范的 SPDX 许可证标识（如 `MIT`, `Apache-2.0`, `GPL-3.0`）。 |
| `repositoryUrl` | `string` | **是** | 公开的源码仓库地址，**必须为 HTTPS 协议**。 |
| `documentationUrl`| `string`| 否 | 公开的技术文档或使用说明地址，**必须为 HTTPS 协议**。 |
| `entrypoint` | `string` | 否 | 相对 Go package 路径（如 `./cmd/sample-plugin`），仅供阅读，宿主不以此构建。 |
| `capabilities` | `[]string`| 否 | 声明插件能力（如 `events`, `http`），仅供检查页提示，非鉴权边界。 |
| `events` | `[]string`| 否 | 需要监听的事件列表（如 `["tree", "upload"]`），支持 `"*"` 订阅全部。 |
| `routes` | `[]Route` | 否 | HTTP 路由映射列表。`path` 以 `/*` 结尾可通配子路径；`ui: true` 将在左侧导航栏和设置页生成快捷入口。 |
| `artifacts` | `map` | **是** | 平台标识 `goos/goarch` 到二进制地址与 SHA-256 的映射。发布清单中至少包含当前运行架构。 |

---

## 5. 完整可运行代码示例

以下为一个完整、独立、可直接复制构建的 Go 插件源文件（`cmd/sample-plugin/main.go`）：

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	tdriveplugin "github.com/dibin/tdrive/pkg/plugin"
)

// SamplePlugin 实现 tdrive 插件的全部可选与必选接口：
// - tdriveplugin.Plugin (必选: Manifest, Initialize)
// - tdriveplugin.BeforeHook (可选: Before)
// - tdriveplugin.AfterHook (可选: After)
// - tdriveplugin.EventHook (可选: OnEvent)
// - tdriveplugin.HTTPHook (可选: HandleHTTP)
// - tdriveplugin.ShutdownHook (可选: Shutdown)
type SamplePlugin struct {
	host    tdriveplugin.Host
	dataDir string
	ownerID string

	mu        sync.Mutex
	lastEvent string
}

// 1. Manifest: 运行时向宿主报告自身元信息。
// 注意：严禁在此处填充 Artifacts 字段！其余字段必须与发布的 tdrive.plugin.json 一致。
func (p *SamplePlugin) Manifest() tdriveplugin.Manifest {
	return tdriveplugin.Manifest{
		ID:               "sample-plugin",
		Name:             "示例插件",
		Description:      "完整可运行的 tdrive 示例插件",
		Version:          "1.0.0",
		SDKVersion:       "0.1",
		APIVersion:       1,
		Author:           "tdrive-team",
		License:          "MIT",
		RepositoryURL:    "https://github.com/example/tdrive-plugin-sample",
		DocumentationURL: "https://github.com/example/tdrive-plugin-sample/blob/main/README.md",
		Entrypoint:       "./cmd/sample-plugin",
		Capabilities:     []string{"events", "http", "hooks"},
		Events:           []string{"tree", "upload", "telegram"},
		Routes: []tdriveplugin.RouteSpec{
			{Path: "/", Methods: []string{"GET"}, UI: true},
			{Path: "/api/status", Methods: []string{"GET"}},
		},
	}
}

// 2. Initialize: 插件子进程启动后握手的第一步。宿主在此传入反向 RPC Host 接口。
func (p *SamplePlugin) Initialize(ctx context.Context, host tdriveplugin.Host) error {
	p.host = host
	p.dataDir = os.Getenv("TDRIVE_PLUGIN_DATA_DIR")

	// 读取插件所有者账号信息
	var users []tdriveplugin.User
	if err := host.Call(ctx, "users.list", nil, &users); err != nil {
		return fmt.Errorf("users.list 失败: %w", err)
	}
	if len(users) > 0 {
		p.ownerID = users[0].ID
	}

	// 尝试读取数据目录下的配置或调用 data.get
	var savedConfig map[string]any
	err := host.Call(ctx, "data.get", map[string]string{"key": "config"}, &savedConfig)
	if err != nil && !strings.Contains(err.Error(), "not found") {
		// 真实的存储读取故障（非首次运行未找到）
		fmt.Fprintf(os.Stderr, "读取初始数据失败: %v\n", err)
	}

	return nil
}

// 3. Before: 拦截核心操作。可修改操作负载或直接拦截阻断。
func (p *SamplePlugin) Before(ctx context.Context, op tdriveplugin.Operation) (tdriveplugin.OperationResult, error) {
	switch op.Name {
	case "files.delete":
		// 示例拦截：禁止删除含有 "protected" 的文件或目录
		var payload struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(op.Payload, &payload); err == nil {
			if strings.Contains(payload.Path, "protected") {
				return tdriveplugin.OperationResult{
					Allowed: false,
					Error:   "示例插件：受保护的路径不允许删除",
				}, nil
			}
		}
	}

	// 放行其他所有操作
	return tdriveplugin.OperationResult{
		Allowed: true,
		Payload: op.Payload,
	}, nil
}

// 4. After: 核心操作成功完成后的后置通知。
func (p *SamplePlugin) After(ctx context.Context, op tdriveplugin.Operation) {
	// After 是纯后置通知，此处错误不会回滚宿主核心操作
	if op.Name == "files.mkdir" {
		fmt.Fprintf(os.Stderr, "[示例插件] 目录已创建，所有者: %s\n", op.UserID)
	}
}

// 5. OnEvent: 异步接收 manifest.events 中声明的事件。
func (p *SamplePlugin) OnEvent(ctx context.Context, ev tdriveplugin.Event) {
	p.mu.Lock()
	p.lastEvent = fmt.Sprintf("[%s] %s (at: %s)", ev.Type, string(ev.Data), ev.At.Format(time.RFC3339))
	p.mu.Unlock()

	// 演示：当收到 telegram 状态变更事件且所有者为管理员时，尝试查询 runtime settings
	if ev.Type == "telegram" {
		var settings map[string]any
		err := p.host.Call(ctx, "settings.get", nil, &settings)
		if err != nil {
			// 如果所有者不是管理员，会报错；此处属于正常降级分支，不崩溃
			fmt.Fprintf(os.Stderr, "[示例插件] 当前所有者非管理员或无权读取 settings: %v\n", err)
		}
	}
}

// 6. HandleHTTP: 处理所有属于此插件的 HTTP 路由请求。
// 宿主在路由至此之前已完成身份认证与调用者归属匹配。
func (p *SamplePlugin) HandleHTTP(ctx context.Context, req tdriveplugin.HTTPRequest) (tdriveplugin.HTTPResponse, error) {
	switch req.Path {
	case "/":
		html := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head><meta charset="utf-8"><title>示例插件面板</title></head>
<body>
  <h1>欢迎使用示例插件</h1>
  <p>所有者 ID: <code>%s</code></p>
  <p>数据持久化目录: <code>%s</code></p>
  <p><a href="/plugins/sample-plugin/api/status">查看 API 状态</a></p>
</body>
</html>`, p.ownerID, p.dataDir)

		return tdriveplugin.HTTPResponse{
			Status: http.StatusOK,
			Headers: map[string][]string{
				"Content-Type": {"text/html; charset=utf-8"},
			},
			Body: []byte(html),
		}, nil

	case "/api/status":
		p.mu.Lock()
		eventInfo := p.lastEvent
		p.mu.Unlock()

		respData := map[string]any{
			"status":    "active",
			"ownerId":   p.ownerID,
			"lastEvent": eventInfo,
			"timestamp": time.Now().Unix(),
		}
		body, _ := json.Marshal(respData)

		return tdriveplugin.HTTPResponse{
			Status: http.StatusOK,
			Headers: map[string][]string{
				"Content-Type": {"application/json; charset=utf-8"},
			},
			Body: body,
		}, nil

	default:
		return tdriveplugin.HTTPResponse{
			Status: http.StatusNotFound,
			Body:   []byte("Not Found"),
		}, nil
	}
}

// 7. Shutdown: 宿主优雅停机或卸载插件时的收尾回调。
func (p *SamplePlugin) Shutdown(ctx context.Context) error {
	fmt.Fprintln(os.Stderr, "[示例插件] 正在释放资源并退出...")
	return nil
}

func main() {
	// 启动 go-plugin RPC 通信循环（阻塞直到宿主断开）
	tdriveplugin.Serve(&SamplePlugin{})
}
```

---

## 6. Host API 接口规范

插件在 `Initialize` 或各 Hook 中通过 `Host.Call(ctx, method, req, &resp)` 调用宿主服务。

> **统一错误协议**：若调用失败，`Host.Call` 返回 Go 的 `error`。跨进程传递时错误会被展平为字符串。

### 6.1 文件系统类 (files.*)

#### `files.list`
- **说明**：列出指定目录下的文件和子目录。
- **请求负载 (JSON)**：
  ```json
  { "path": "/" }
  ```
  *(注：`path` 为空时宿主默认处理为根目录 `/`)*
- **响应负载 (JSON)**：`[]tdriveplugin.Entry`
  ```json
  [
    {
      "name": "documents",
      "path": "/documents",
      "isDir": true,
      "size": 0,
      "mime": "",
      "id": "dir_123456",
      "segmentCount": 0,
      "status": "",
      "modifiedAt": "2026-09-01T12:00:00Z",
      "createdAt": "2026-09-01T12:00:00Z"
    },
    {
      "name": "photo.jpg",
      "path": "/photo.jpg",
      "isDir": false,
      "size": 2048576,
      "mime": "image/jpeg",
      "id": "file_abcdef",
      "segmentCount": 1,
      "status": "ready",
      "modifiedAt": "2026-09-02T08:30:00Z",
      "createdAt": "2026-09-02T08:30:00Z"
    }
  ]
  ```
- **错误条件**：目录不存在或路径非法时报错（如 `"drive: no such file or directory"`）。

---

#### `files.stat`
- **说明**：获取单个文件或目录的元数据信息。
- **请求负载 (JSON)**：
  ```json
  { "path": "/photo.jpg" }
  ```
- **响应负载 (JSON)**：`tdriveplugin.Entry`（单对象，结构同上）。
- **错误条件**：目标路径不存在时返回不存在错误。

---

#### `files.mkdir`
- **说明**：递归创建目录。宿主底层实现具备幂等性，已存在则直接返回成功。
- **请求负载 (JSON)**：
  ```json
  { "path": "/photos/2026" }
  ```
- **响应负载 (JSON)**：`database.Dir`
  ```json
  {
    "id": "dir_789012",
    "parentId": "dir_123456",
    "name": "2026",
    "path": "/photos/2026",
    "createdAt": "2026-09-05T00:00:00Z",
    "updatedAt": "2026-09-05T00:00:00Z",
    "ownerId": "usr_owner123"
  }
  ```

---

#### `files.rename`
- **说明**：重命名同目录下的文件或子目录。
- **请求负载 (JSON)**：
  ```json
  {
    "path": "/old_name.txt",
    "name": "new_name.txt"
  }
  ```
- **响应负载 (JSON)**：`tdriveplugin.Entry`（更新后的条目）。
- **错误条件**：同名目标已存在，或源文件不存在。

---

#### `files.move`
- **说明**：跨目录移动文件或目录。
- **请求负载 (JSON)**：
  ```json
  {
    "from": "/downloads/report.pdf",
    "toDir": "/archive"
  }
  ```
- **响应负载 (JSON)**：`tdriveplugin.Entry`（目标条目）。
- **错误条件**：源路径不存在，或目标目录不存在。

---

#### `files.delete`
- **说明**：删除指定路径的文件或目录（递归）。
- **请求负载 (JSON)**：
  ```json
  { "path": "/temp/data.log" }
  ```
- **响应负载 (JSON)**：`null` (Go 端传 `nil`)。
- **错误条件**：目标路径不存在。

---

#### `files.beginUpload`
- **说明**：在宿主创建分片可续传上传任务。
- **请求负载 (JSON)**：`tdriveplugin.UploadRequest`
  ```json
  {
    "dirPath": "/videos",
    "name": "demo.mp4",
    "size": 104857600,
    "mime": "video/mp4",
    "userId": "usr_owner123",
    "source": "plugin-sync",
    "sourceUrl": "",
    "overwrite": true
  }
  ```
- **响应负载 (JSON)**：
  ```json
  {
    "job": {
      "id": "job_upl_001",
      "fileId": "file_vid_001",
      "name": "demo.mp4",
      "totalSize": 104857600,
      "segmentSize": 20971520,
      "segmentCount": 5,
      "uploadedBytes": 0,
      "status": "pending",
      "error": ""
    },
    "file": {
      "id": "file_vid_001",
      "dirId": "dir_vid_root",
      "name": "demo.mp4",
      "size": 104857600,
      "mime": "video/mp4",
      "segmentCount": 5,
      "status": "pending",
      "createdAt": "2026-09-05T01:00:00Z",
      "updatedAt": "2026-09-05T01:00:00Z",
      "ownerId": "usr_owner123"
    }
  }
  ```

---

#### `files.completeUpload`
- **说明**：提交完成上传任务。**只有当全部对应分片均已通过流上传入库后方可调用**。
- **请求负载 (JSON)**：
  ```json
  { "jobId": "job_upl_001" }
  ```
- **响应负载 (JSON)**：`tdriveplugin.File`（最终就绪的完整文件元数据）。
- **错误条件**：若分片缺失，返回类似于 `"upload is still missing segments [3 4]"` 的错误。

---

#### `files.abortUpload`
- **说明**：终止并清理上传任务及其已落盘的分片。
- **请求负载 (JSON)**：
  ```json
  {
    "jobId": "job_upl_001",
    "reason": "用户主动取消",
    "state": "cancelled"
  }
  ```
  *(注：`state` 仅接受 `"cancelled"` 或 `"failed"`，其他值默认作为 `"cancelled"`)*
- **响应负载 (JSON)**：`null`。

---

#### `files.readChunk`
- **说明**：读取文件的一小段数据（最大上限 **16 MiB**）。适合小文件或文件头探测。大文件读取必须使用流式接口。
- **请求负载 (JSON)**：
  ```json
  {
    "fileId": "file_vid_001",
    "offset": 0,
    "size": 1048576
  }
  ```
- **响应负载 (JSON)**：
  ```json
  {
    "data": "aGVsbG8gd29ybGQ..."
  }
  ```
  *(注：Go `[]byte` 在 JSON 序列化时表现为标准 Base64 编码字符串)*
- **错误条件**：`offset < 0`、`size < 0` 或 `size > 16777216` (16 MiB) 返回 `"readChunk offset or size is invalid"`。

---

### 6.2 流式数据读写 (Host.OpenStream)

大数据传输严禁通过单次 RPC 传输。必须调用 `Host.OpenStream(ctx, method, req)`，返回一个 `io.ReadWriteCloser`。

#### `files.putSegment` (流式上传分片)
- **参数说明**：
  - `method`: `"files.putSegment"`
  - `request`:
    ```json
    {
      "jobId": "job_upl_001",
      "index": 1,
      "size": 20971520
    }
    ```
- **约束规则**：
  - `index` 从 `1` 开始计数；
  - `size` 必须与该分片实际几何大小精准匹配，且不超过 2 GiB (`2<<30`)；
- **传输流程**：
  1. 调用 `stream, err := host.OpenStream(ctx, "files.putSegment", req)`；
  2. 插件将当前分片的字节流写入 `stream`；
  3. **必须调用 `stream.Close()`**：宿主会在关闭时向 Telegram 发送 EOF 并等待数据库提交，所有的网络超时、校验错误或 TG 配额错误均会在 `Close()` 时返回。

#### `files.read` (流式读取文件)
- **参数说明**：
  - `method`: `"files.read"`
  - `request`:
    ```json
    {
      "fileId": "file_vid_001",
      "offset": 0,
      "size": 67108864
    }
    ```
- **约束规则**：单次流读取 `size` 最大上限为 **64 MiB** (`64<<20`)。超出将返回 `"read stream offset or size is invalid"`。

---

### 6.3 离线下载与暂存类 (downloads.*)

#### `downloads.stage`
- **说明**：将 Telegram 远端文件拉取并暂存在 VPS 本地服务器磁盘。
- **请求负载 (JSON)**：
  ```json
  {
    "fileId": "file_vid_001",
    "userId": "usr_owner123"
  }
  ```
- **响应负载 (JSON)**：`tdriveplugin.DownloadJob`
  ```json
  {
    "id": "job_stage_001",
    "fileId": "file_vid_001",
    "name": "demo.mp4",
    "totalSize": 104857600,
    "downloadedBytes": 0,
    "mode": "stage",
    "status": "pending",
    "error": "",
    "createdAt": "2026-09-05T01:10:00Z",
    "updatedAt": "2026-09-05T01:10:00Z"
  }
  ```

#### `downloads.cancel`
- **说明**：取消一个正在进行的暂存下载任务。
- **请求负载 (JSON)**：
  ```json
  { "jobId": "job_stage_001" }
  ```
- **响应负载 (JSON)**：`null`。

---

### 6.4 用户与运行参数 (users.* & settings.*)

#### `users.list`
- **说明**：获取当前插件所有者信息。**宿主永远只返回 1 条数据**。
- **请求负载 (JSON)**：`null` 或 `{}`。
- **响应负载 (JSON)**：`[]tdriveplugin.User`
  ```json
  [
    {
      "id": "usr_owner123",
      "username": "alice",
      "role": "admin",
      "enabled": true
    }
  ]
  ```

#### `settings.get`
- **说明**：读取 tdrive 运行时配置参数。**要求插件所有者角色必须为 `admin`**；非管理员所有者的插件无法调用此接口，因而不会获取到这些系统级参数。注意：响应中的 `AppID` 和 `AppHash` 为部署级 Telegram 应用凭据，插件获取后严禁写入日志、外传第三方或在自身前端页面回显。
- **请求负载 (JSON)**：`null` 或 `{}`。
- **响应负载 (JSON)**：大驼峰字段对象（源自 `config.RuntimeSettings`）
  ```json
  {
    "AppID": 1234567,
    "AppHash": "0123456789abcdef0123456789abcdef",
    "LocalRoot": "/data/local",
    "SegmentSize": 20971520,
    "PoolSize": 4,
    "UploadThreads": 2,
    "UploadPartSize": 524288,
    "RateLimit": 0,
    "StreamConcurrency": 4,
    "UploadConcurrency": 2,
    "DownloadConcurrency": 2,
    "WebDAVEnabled": true,
    "LogLevel": "info",
    "CacheDir": "",
    "CacheLimit": 10737418240,
    "CacheTTL": 86400000000000,
    "MaxDownloadConns": 4,
    "DownloadGrace": 30000000000,
    "ShareTTL": 0
  }
  ```
- **错误条件**：所有者不是管理员时返回：`"这个插件的所有者不是管理员，无法读取或修改运行参数"`。

#### `settings.update`
- **说明**：更新运行时配置。**要求插件所有者角色必须为 `admin`**。
- **请求负载 (JSON)**：包含需要修改的大驼峰字段对象
  ```json
  {
    "UploadConcurrency": 3,
    "DownloadConcurrency": 3
  }
  ```
- **响应负载 (JSON)**：更新后的完整大驼峰配置对象。

---

### 6.5 Telegram 状态与自定义事件 (telegram.* & events.*)

#### `telegram.status`
- **说明**：读取当前 Telegram 客户端连接及健康状态。
- **请求负载 (JSON)**：`null` 或 `{}`。
- **响应负载 (JSON)**：
  ```json
  {
    "state": "ready",
    "error": "",
    "userId": 987654321,
    "username": "my_tg_bot",
    "firstName": "DriveBot",
    "phone": "+86 138****0000",
    "premium": true,
    "dc": 5,
    "awaitingCode": false,
    "awaitingPassword": false,
    "cooldownMs": 0,
    "channelReady": true,
    "channelChecked": true
  }
  ```
  *(注：Telegram 未配置或运行在模拟空客户端时返回 `null`)*

#### `events.publish`
- **说明**：向宿主事件总线广播自定义事件。
- **请求负载 (JSON)**：
  ```json
  {
    "type": "custom_sync_finished",
    "data": { "itemCount": 42, "status": "ok" },
    "userId": "usr_owner123"
  }
  ```
  *(注：`type` 为必填字符串；`userId` 若为空将视事件类型广播)*
- **响应负载 (JSON)**：`null`。

---

### 6.6 命名空间持久化存储 (data.*)

用于存储插件自身的配置、Token、同步状态断点等。存储命名空间在底层严格隔离为 `(所有者账号 ID, 插件 ID)`。

- **Key 命名规则**：非空字符串，长度 1 ~ 128 字符，**严禁包含字符 `\`、`/` 或空字符 `\0`**。

#### `data.get`
- **请求负载**：`{ "key": "sync_cursor" }`
- **响应负载**：之前存入的原始 JSON 内容（如 `{"cursor": "abc"}`）。
- **错误条件**：键不存在时返回 `database: not found` 错误。

#### `data.set`
- **请求负载**：
  ```json
  {
    "key": "sync_cursor",
    "value": { "cursor": "abc", "offset": 1200 }
  }
  ```
- **响应负载**：`null`。

#### `data.delete`
- **请求负载**：`{ "key": "sync_cursor" }`
- **响应负载**：`null`。

---

## 7. Hook 机制与操作拦截

### 7.1 Hook 触发时序与行为矩阵

- **Before Hook**：核心动作发生**之前**同步触发。插件可检查、修改请求内容，或返回 `Allowed: false` 拒绝操作。任何一个插件拒绝，核心动作立即中止并向调用方返回错误。
- **After Hook**：核心动作已经提交成功后触发。属于纯异步通知，Hook 执行过程中的错误仅记录宿主日志，**不会**回滚已提交的核心操作。
- **自动防环机制**：当插件在 Before Hook 内通过 `Host.Call` 再次调用文件操作时，宿主会自动给上下文注入 `WithHostCall` 标记，**跳过后续 Hook 触发**，杜绝自调用导致无限递归死锁。

### 7.2 完整 Hook 操作与 Payload 结构体对应表

| 操作名称 (`op.Name`) | 触发场景 | `op.Payload` 结构 | 是否支持 Before 篡改参数 |
|---|---|---|:---:|
| `files.list` | 列出目录条目 | `{"path": "/"}` | 支持改写 `Path` |
| `files.stat` | 查询文件信息 | `{"path": "/a.txt"}` | 支持改写 `Path` |
| `files.open` | 准备打开读取文件 | `{"fileId": "file_123"}` | 支持改写 `FileID` |
| `files.mkdir` | 创建目录 | `{"path": "/folder"}` | 支持改写 `Path` |
| `files.rename` | 重命名文件/目录 | `{"path": "/old", "name": "new"}` | 支持改写字段 |
| `files.move` | 移动文件/目录 | `{"from": "/a", "toDir": "/b"}` | 支持改写字段 |
| `files.delete` | 按路径删除 | `{"path": "/a.txt"}` | 支持改写 `Path` |
| `files.deleteByID` | 按文件 ID 删除 | `{"id": "file_123"}` | 支持改写 `ID` |
| `files.beginUpload`| 初始化上传任务 | `tdriveplugin.UploadRequest` | 支持改写上传参数 |
| `files.putSegment` | 上传分片提交 | `{"jobId": "...", "index": 1, "size": 1024}` | 支持改写 |
| `files.completeUpload`| 上传全部完成提交 | `{"jobId": "job_123"}` | 支持改写 |
| `files.abortUpload`| 中止取消上传任务 | `{"jobId": "...", "reason": "...", "status": "..."}` | 支持改写 |
| `downloads.stage` | 提交本地暂存任务 | `{"fileId": "...", "userId": "..."}` | 支持改写 |
| `downloads.stagedFile`| 访问已暂存的本地文件 | `{"jobId": "job_123"}` | 支持改写 |
| `downloads.cancel`| 取消暂存任务 | `{"jobId": "job_123"}` | 支持改写 |
| `http.request` | HTTP 接入层拦截 | `tdriveplugin.HTTPRequest` (含 Headers, Body) | 支持改写 Method, Path, Body 等 |

---

## 8. 事件驱动机制 (Events)

### 8.1 订阅与生命周期
插件必须在 `tdrive.plugin.json` 中的 `events` 数组显式声明需要的事件名称。未声明的事件不会通过 RPC 发送给插件。当部署中没有任何插件声明某事件时，宿主将保持零监听开销。

### 8.2 事件分类与负载 Payload

| 事件类型 (`Type`) | 广播范围 | 负载 `event.Data` (JSON) | 对应 Go 结构体 | 说明 |
|---|---|---|---|---|
| `tree` | **仅**定向给匹配的 `userId` | `{"path": "/photos"}` | `events.TreeChanged` | 目录内容变动通知 |
| `upload` | **仅**定向给匹配的 `userId` | `UploadProgress`（详见下文） | `events.UploadProgress` | 上传进度与状态快照 |
| `download` | **仅**定向给匹配的 `userId` | `DownloadProgress`（详见下文）| `events.DownloadProgress`| 暂存下载进度快照 |
| `telegram` | **全员广播** | `{"state": "ready", ...}` | `tgc.Status` | Telegram 连接/登出状态 |
| `index` | **全员广播** | `IndexProgress`（详见下文） | `events.IndexProgress` | 索引扫描与重构进度 |

#### 详细负载结构

- **`UploadProgress` (`upload`)**：
  ```json
  {
    "jobId": "job_123",
    "fileId": "file_456",
    "name": "archive.zip",
    "uploaded": 52428800,
    "total": 104857600,
    "segment": 2,
    "segmentCount": 4,
    "status": "uploading",
    "error": "",
    "source": "web",
    "sourceUrl": "",
    "speed": 5242880.0
  }
  ```

- **`DownloadProgress` (`download`)**：
  ```json
  {
    "jobId": "job_dl_123",
    "fileId": "file_456",
    "name": "archive.zip",
    "downloaded": 10485760,
    "total": 104857600,
    "mode": "stage",
    "status": "downloading",
    "error": "",
    "speed": 2097152.0
  }
  ```

- **`IndexProgress` (`index`)**：
  ```json
  {
    "scanned": 150,
    "dirs": 12,
    "files": 138,
    "segments": 240,
    "done": false,
    "error": ""
  }
  ```

---

## 9. HTTP 路由扩展

### 9.1 路由挂载与隔离模型
- 插件在清单 `routes` 中声明路由后，宿主将其统一挂载在 `/plugins/{id}/...`。
- **调用者强校验**：请求进入 `/plugins/{id}` 时，宿主提取调用者 Session Cookie 或 Bearer Token。
  - 若调用者未登录：返回 401 或重定向；
  - 若调用者账号**未安装**该插件：**直接返回 404 Not Found**（防止泄漏他人的插件安装信息）；
  - 仅当调用者账号已安装且插件正在运行时，请求才会被转换为 `tdriveplugin.HTTPRequest` 经 RPC 投递到插件的 `HandleHTTP`。

### 9.2 HTTP 约束限制
- **请求体大小限制**：单次 HTTP Body 最大为 **8 MiB** (`8 << 20`)。超过将直接由宿主返回 413 Entity Too Large。大数据上传必须使用 `Host.OpenStream`。
- **RPC 调用超时**：宿主转发 HTTP 给插件子进程的超时时限恒定为 **30 秒** (`pluginCallTimeout`)。
  - 若超时且子进程仍然存活，宿主返回 504 Gateway Timeout，**不会杀死插件子进程**。
- **UI 集成**：清单中设置 `"ui": true` 的 GET 路由，会在 tdrive WebUI 左侧导航栏展示图标入口，并在「系统设置 → 插件」卡片中提供“打开”按钮，跳转至 `/plugins/{id}`。

---

## 10. 构建、调试与发布流程

### 10.1 编译硬性要求
1. **必须关闭 CGO (`CGO_ENABLED=0`)**：tdrive 运行环境基于极简 distroless 容器镜像，宿主无 glibc/musl 动态链接环境。
2. **必须使用 `-trimpath` 标志**：去除本地绝对路径，确保生成的 SHA-256 可稳定复现。
3. **平台与命名规范**：支持 `linux/amd64`、`linux/arm64` 与 `windows/amd64`。Windows 平台二进制**必须**以 `.exe` 作为后缀。

### 10.2 自动化构建与发布脚本

```bash
#!/usr/bin/env bash
set -euo pipefail

PLUGIN_ID="sample-plugin"
VERSION="v1.0.0"
ENTRYPOINT="./cmd/sample-plugin"
PLATFORMS=("linux/amd64" "linux/arm64" "windows/amd64")

assets=()

for platform in "${PLATFORMS[@]}"; do
  os="${platform%/*}"
  arch="${platform#*/}"
  suffix=""
  [ "$os" = "windows" ] && suffix=".exe"
  asset_name="${PLUGIN_ID}-${os}-${arch}${suffix}"

  echo "正在交叉编译 ${platform} -> ${asset_name}..."
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath -buildvcs=false \
    -o "$asset_name" "$ENTRYPOINT"

  # 计算 SHA-256 并更新 tdrive.plugin.json
  digest="$(sha256sum "$asset_name" | cut -d' ' -f1)"
  echo "SHA-256: ${digest}"

  jq --arg plat "$platform" --arg d "$digest" \
    '.artifacts[$plat].sha256 = $d' tdrive.plugin.json > tmp.json && mv tmp.json tdrive.plugin.json

  assets+=("$asset_name")
done

echo "构建完成。准备发布 GitHub Release ${VERSION}..."
gh release create "$VERSION" "${assets[@]}" tdrive.plugin.json \
  --title "${PLUGIN_ID} ${VERSION}" \
  --notes "Release ${VERSION}"
```

### 10.3 GitHub Actions CI/CD 范例

```yaml
name: Release Plugin

on:
  push:
    tags:
      - 'v*'

jobs:
  build-and-release:
    runs-on: ubuntu-latest
    permissions:
      contents: write
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.24'
          check-latest: true

      - name: Build & Update Checksums
        env:
          GH_TOKEN: ${{ github.token }}
        run: |
          set -euo pipefail
          assets=()
          for platform in linux/amd64 linux/arm64 windows/amd64; do
            os="${platform%/*}"
            arch="${platform#*/}"
            suffix=""
            [ "$os" = "windows" ] && suffix=".exe"
            asset="sample-plugin-$os-$arch$suffix"
            
            CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
              go build -trimpath -buildvcs=false -o "$asset" ./cmd/sample-plugin
            
            digest="$(sha256sum "$asset" | cut -d' ' -f1)"
            jq --arg a "$platform" --arg d "$digest" \
              '.artifacts[$a].sha256 = $d' tdrive.plugin.json > tmp.json
            mv tmp.json tdrive.plugin.json
            assets+=("$asset")
          done
          
          gh release create "$GITHUB_REF_NAME" "${assets[@]}" tdrive.plugin.json \
            --title "Release $GITHUB_REF_NAME" \
            --generate-notes
```

---

## 11. 环境变量与运维配置

### 11.1 宿主运维环境变量

| 环境变量 | 默认值 | 作用说明 |
|---|---|---|
| `TDRIVE_PLUGIN_DIR` | `<data dir>/plugins` | 插件二进制文件存储根路径。落盘形式为 `<dir>/<userId>/<pluginId>`。 |
| `TDRIVE_PLUGIN_STORE_URL` | 空 (关闭) | 官方或私有插件商店 `index.json` 的 HTTPS 地址。 |
| `TDRIVE_PLUGIN_MAX_BINARY_BYTES`| `256MiB` (`268435456`) | 单个插件二进制允许下载落盘的最大字节数。 |
| `TDRIVE_PLUGIN_MAX_PER_USER` | `4` | 单个用户账号最多允许启用的插件实例数。`0` 或负数表示无限制。 |
| `TDRIVE_PLUGIN_MAX_PROCESSES` | `32` | 整个 tdrive 实例所有账号加起来允许运行的最大插件子进程总数。 |

### 11.2 插件子进程环境变量
当宿主拉起插件子进程时，会向子进程注入以下专属环境变量：
- `TDRIVE_PLUGIN_DATA_DIR`：**插件专属持久化数据目录的绝对路径**（例如 `/data/plugin-data/usr_123/sample-plugin/`）。插件应将 SQLite 数据库、Token 文件、运行缓存等存储在该目录下。该目录由宿主保证全局唯一，升级更新时被保留。

---

## 12. 插件商店索引规范

插件商店索引文件为一个公开托管的静态 JSON 文件（`index.json`），提供给 tdrive 宿主作为发现列表：

```json
{
  "updatedAt": "2026-09-05T00:00:00Z",
  "plugins": [
    {
      "id": "sample-plugin",
      "name": "示例插件",
      "description": "一个演示完整生命周期与接口调用的 tdrive 示例插件",
      "version": "1.0.0",
      "author": "tdrive-team",
      "repositoryUrl": "https://github.com/example/tdrive-plugin-sample",
      "manifestUrl": "https://github.com/example/tdrive-plugin-sample/releases/download/v1.0.0/tdrive.plugin.json",
      "manifestDigest": "64位小写十六进制SHA-256值",
      "documentationUrl": "https://github.com/example/tdrive-plugin-sample/blob/main/README.md",
      "license": "MIT",
      "tags": ["sync", "tools"]
    }
  ]
}
```

### 商店上架准则
1. `manifestDigest` 必须是 `manifestUrl` 文件的实际 SHA-256 摘要（小写 64 位十六进制），宿主在下载清单时会进行双向校对。
2. `manifestUrl` 和下载地址**严禁使用动态分支或未锁版本的 mutable 链接**（如 raw github `main` 分支），必须使用 Release Tag 不可变链接。
3. 清单中的 `artifacts` 必须至少覆盖 `linux/amd64` 和 `linux/arm64`。
4. 插件内不得含有后门、远程拉取未审查代码执行或泄露凭证的行为。
