# doge Web 控制台 HTTP / WebSocket API 规范

版本：v5（文件上传与对话附件，见 §4.13）。用途：供外部团队据此设计自己的 Web UI 或客户端。所有字段、状态码、事件名均与代码
（`internal/web/`、`internal/agent/`、`internal/ai/`）逐一核对。

## 1. 总览

doge 的 Web 控制台是**与 agent 进程同进程运行**的 HTTP 服务器 + WebSocket 广播器。
外部 UI 通过 REST 完成登录、查询、控制，通过 WebSocket 订阅实时事件流（对话输出、
工具执行、权限询问）。两者共享同一套数据模型与鉴权。

架构要点：

- **同进程**：Web 与 TUI（若有）共享同一个 `AgentSession`，实时互相可见；会话仍写入
  同一个 JSONL 文件。
- **按窗口隔离**：每个浏览器窗口/客户端用一个 `clientID` 标识，REST 通过 `X-Doge-Client`
  头携带，WebSocket 通过 `?client=` 查询参数携带。不同窗口可以各跑各的会话（首次
  切会话时自动分叉出独立 AgentSession）。
- **鉴权（v2）**：除登录页与静态资源外，所有 `/api/*` 与 `/ws` 接受三种凭证：
  `Authorization: Bearer <token>`（**客户端与独立部署 WebUI 的推荐方式**）、
  `?token=<token>`（浏览器 WebSocket/EventSource 无法设置 header 时的回退），
  以及登录签发的 HttpOnly 会话 cookie（内置前端沿用）。详见 §2。
- **CORS（v2）**：默认仍不设跨域头（同源-only）。配置 `--web-cors` /
  `settings.web.corsOrigins` 后允许指定来源的浏览器跨域调用，供剥离部署的 WebUI
  使用。详见 §2.3。

### 启动

```bash
doge --web                # Web + TUI 同时运行
doge --web --no-tui       # 仅 Web（headless，适合容器/远程）
doge --web --no-tui --port 8080 --web-host 0.0.0.0
```

| 参数 | 默认 |
|---|---|
| `--port <n>` | `settings.web.port`，否则 `8080` |
| `--web-host <host>` | `settings.web.host`，否则 `127.0.0.1` |
| `--web-token <tok>` | `settings.web.token`，否则每次启动随机生成 |

启动时控制台会打印访问 URL 与 token，token 同时写入 `~/.doge/agent/web-token`
（0600，重启失效）。也可在 `settings.json` 里用 `web` 节配置（见 README）。

## 2. 授权与安全

### 2.1 凭证通道（v2）

服务端持有一个访问 token（每次启动随机 32 字节 hex，或 `--web-token` 固定值）。
所有受保护接口按以下顺序验收凭证：

| 通道 | 形式 | 适用场景 |
|---|---|---|
| **Bearer** | `Authorization: Bearer <token>` | **客户端（CLI/桌面/脚本）与独立部署 WebUI 的推荐方式**；无过期、无状态，常量时间比较 |
| **Query** | `?token=<token>` | 浏览器 WebSocket / EventSource（无法设置请求头） |
| **Cookie** | `doge_web_session=<随机值>`（`POST /api/auth/login` 签发） | 内置同源前端 |

- Bearer/Query 失败同样触发 300ms 延时（防暴力猜解）；错误一律
  `401 {"error":"unauthorized"}`。
- Cookie 会话有效期 7 天，进程内状态，重启即全部失效。
- token 先 `TrimSpace` 再比较，从终端/文件复制带换行也能过。
- **`?token=` 会进入反代/网关的访问日志**，能设置 header 的客户端应优先 Bearer。

**客户端接入流程**：拿到 token（启动打印 / `~/.doge/agent/web-token` / 用户输入）→
`GET /api/auth/whoami` 验证（`{"ok":true,"method":"bearer"}`）→ 直接调用各 API。
无需 login。

**`?token=` URL 参数自动登录**：内置前端读到后调 `/api/auth/login` 换 cookie；
服务端同时原生接受 `/ws?token=...`（浏览器 WS 场景）。

### 2.2 登录/登出（cookie 通道，主要为内置前端保留）

1. `POST /api/auth/login` 提交 `{"token": "..."}`。
2. 成功 → 200 `{"ok":true}` 并 `Set-Cookie: doge_web_session=<随机值>`；
   失败 → 401 `{"error":"invalid token"}`。
3. `POST /api/auth/logout` 吊销当前 cookie。

### 2.3 CORS（独立部署 WebUI）

默认**不设任何 CORS 头**（同源-only）。需要跨域时配置：

```bash
doge --web --no-tui --web-cors https://ui.example.com   # 可重复传多个
doge --web --no-tui --web-cors '*'                      # 允许所有来源
```

或 settings.json：`"web": {"corsOrigins": ["https://ui.example.com"]}`。

- 指定来源时：回显 `Access-Control-Allow-Origin: <origin>` +
  `Allow-Credentials: true`（cookie 可跨域携带，但 cookie 是 `SameSite=Strict`，
  浏览器实际不会发——**跨域部署请用 Bearer token 存 localStorage**）。
- `'*'` 时：`Access-Control-Allow-Origin: *`，不支持 cookie 凭证，同样请用 Bearer。
- 预检 OPTIONS 一律 204（未配置来源时也如此，只是不带允许头，由浏览器拒绝）。
- WebSocket 不受 CORS 约束（服务端 Accept 任意 Origin），跨域 WS 用
  `/ws?token=...&client=...`。

### 2.4 鉴权边界

| 路径 | 鉴权 |
|---|---|
| `/`（index.html / app.js / style.css 等静态资源） | 公开（登录页本身需能加载） |
| `/api/auth/login` | 公开 |
| 其余 `/api/*` | Bearer / `?token=` / cookie 任一 |
| `/ws` | 同上（浏览器推荐 `?token=`） |

未带有效凭证访问受保护接口 → `401`，响应体为 JSON 字符串
`{"error":"unauthorized"}`。

## 3. 通用约定

- **Base URL**：`http(s)://<host>:<port>`，所有接口相对该地址。
- **Content-Type**：请求体与响应体均为 `application/json; charset=utf-8`。
- **错误体**：统一 `{"error": "<描述>"}`（`/api/auth/login` 的 400 也含在内）。
- **gzip**：客户端声明 `Accept-Encoding: gzip` 时对响应透明压缩；WebSocket 升级
  请求跳过压缩。
- **HTTP 状态码语义**：

| 状态码 | 含义 |
|---|---|
| 200 | 成功（查询、同步操作） |
| 202 | 已接受（异步操作：`/api/prompt`、`/api/turn/edit`，结果经 WS 推送） |
| 204 | CORS 预检应答 |
| 400 | 请求体非法 / 参数为空 / 模型 spec 无效 |
| 401 | 未登录 / 凭证无效 / token 错误 |
| 403 | 会话只读（其他窗口/TUI 持有写控制） |
| 404 | 路由不存在 / 资源不存在（mesh 未启用、日志归档不存在、pending ask 不存在） |
| 409 | agent 忙 / 操作冲突（会话被占、删除失败等） |
| 413 | 上传请求体超过 `uploads.maxRequestBytes`（§4.13） |
| 500 | 服务端错误（重载配置失败、会话列表读取失败等） |
| 503 | 服务不可用（`/api/shutdown` 未接线） |

## 4. REST API

### 4.1 认证

#### POST /api/auth/login

请求：
```json
{ "token": "..." }
```
响应：`200 {"ok":true}` + `Set-Cookie: doge_web_session=...`
错误：`400 {"error":"token required"}`（空）/ `400 {"error":"invalid request body"}` /
`401 {"error":"invalid token"}`

#### POST /api/auth/logout

请求：空 Body。响应：`200 {"ok":true}`，并清除 cookie。

#### GET /api/auth/whoami

验证当前凭证并报告通道。客户端拿它做 token 自检（比打 `/api/status` 轻、无会话语义）。

响应：`200 {"ok":true,"method":"bearer"|"query"|"cookie"}`

### 4.2 状态与统计

#### GET /api/status

当前窗口的实时 agent 状态（结构见 §5.1）。前端据此判断是否 busy、当前模型、错误等。

#### GET /api/stats

当前会话 token / 缓存 / 花费统计（结构见 §5.4）。

### 4.3 对话控制

#### POST /api/prompt

提交一条用户消息并启动一次异步 run。

请求：
```json
{ "text": "你的问题", "attachments": ["<uploadID>", "..."] }
```

`attachments`（v5，可选）引用暂存区上传（§4.13）：图片直接作为多模态内容进入
消息，小文本（≤64 KiB）全文注入，其余以引用行提示 agent 用 `read` 工具读取。
空文本 + 有附件是合法请求。附件 ID 不存在 → `400 unknown attachment`。

响应：`202 {"ok":true}`。结果不在这条响应里，而是通过 WebSocket 事件流推送
（§6）。多次同时提交会因 busy 而 409。

错误：
- `400 {"error":"empty prompt"}` — 空消息（TrimSpace 后为空且无附件）
- `400 {"error":"invalid request body"}` — Body 非法
- `403` — 会话只读
- `409 {"error":"agent is busy"}` — 已有 run 在飞行中

#### POST /api/abort

中断当前 run。请求：空 Body。响应：`200 {"ok":true}`。非运行中调用也无害。

> 提供商停摆自动恢复：SSE 流超过静默预算（settings.json `streamIdleSeconds`，默认 120s）
> 无任何数据时，后端自动中断该流并以 `error` 消息（`errorMessage` 形如
> `provider stalled: no data for …`）结束 run，会话随即释放——不依赖本接口。

#### POST /api/compact

触发上下文压缩（compaction）。响应：`200 {"ok":true}`；
失败 `403`（只读）或 `409`（其他错误，如运行中）。

#### POST /api/turn/edit

回滚到第 `seq` 条**用户消息**（0 起始序号，按会话内 user 消息顺序计数），替换为
`text` 并异步重跑。旧尾部消息被弃用（按分支重写）。

请求：
```json
{ "seq": 2, "text": "修改后的消息" }
```
响应：`202 {"ok":true}`。错误：`400`（空文本/非法 Body）/ `403`（只读）/ `409`。

### 4.4 模型

#### GET /api/models

列出**可用**模型（能解析到 API key 的，或无需 key 的本地端点；当前活动模型始终
包含在内），结构见 §5.5。响应：`200 {"models": [ModelInfo...]}`。

`?all=1` 改为返回**完整目录**（含暂无凭证的模型），供配置类 UI 使用。

#### POST /api/model

切换当前活动模型。

请求：
```json
{ "spec": "provider/model-id" }
```
响应：`200 {"ok":true}`。错误：`400 {"error":"empty model spec"}` / `400`（spec 无效）/
`403`（只读）。

### 4.5 会话管理

> `id` 字段均可使用会话 ID、会话文件路径或文件名（basename）作为标识。

#### GET /api/session

当前窗口活动会话 + 完整已提交转录（结构见 §5.2）。响应：`200 SessionInfo`。

#### GET /api/sessions

列出持久化会话（只读，含首句预览），结构见 §5.3。
响应：`200 {"sessions": [SessionSummary...]}`。

#### POST /api/session/new

开启全新会话（当前窗口视图切换；若当前为共享会话则**分叉**出独立会话）。
响应：`200 {"ok":true}`，并向该窗口的 WS 广播 `{"type":"session_reset"}`。

#### POST /api/session/switch

切换到既有会话（按 id / 路径 / basename）。

请求：
```json
{ "id": "<session id 或路径>" }
```
响应：`200 {"ok":true}`，并向该窗口 WS 广播 `{"type":"session_reset"}`。
错误：`409`（会话不存在、目标目录已删除、运行中切换等）。

#### POST /api/session/delete

删除持久化会话。**当前会话、TUI 正在使用的会话、被其他窗口占用的会话不可删除。**

请求：
```json
{ "id": "<session id>" }
```
响应：`200 {"ok":true}`，并向**所有**窗口广播 `{"type":"session_list_changed"}`。
错误：`400 {"error":"..."}`（不存在 / 不可删原因）。

#### POST /api/session/claim

请求当前会话的写控制权。无争用会话立即获得；被他人持有则进入 FIFO 等待队列、
保持只读直到持有者让出。

请求：空 Body。响应：`200 {"owned": bool, "queued": bool}`。

#### POST /api/session/takeover

**强制夺取**当前会话的写控制权：无论当前持有者是谁（另一个 web 窗口或 TUI），
立即释放它并转给请求窗口，不等持有者让出。用于多窗口下"忘了是哪个窗口持锁、
旧持有者一直不释放"的场景；也供后续非浏览器 client 接入（带 `X-Doge-Client`
头即可声明窗口身份）。

请求：空 Body。响应：`200 {"owned": true, "prevOwner": "<旧持有者id，无则空串>"}`。
夺取成功会向**所有**窗口广播 `{"type":"session_control","key":"<会话路径>","owner":"<新持有者>"}`，
让被夺方的 UI 立刻变为只读（可继续轮询 `GET /api/status` 确认 `writable`）。

### 4.6 配置

#### POST /api/reload

重新读取 `models.json` / `auth.json` / `permissions.json`（等价 TUI 的 `/reload`），
同时热刷新技能目录（技能集变化重建 skill 工具与系统提示）并重读上下文文件
（全局 `~/.doge/agent/AGENTS.md` 与项目 `AGENTS.md`/`CLAUDE.md`，改动即时生效）。
响应：`200 {"ok":true}`；失败 `500`。

### 4.7 配置与诊断（v2 新增，客户端管理面板用）

#### GET /api/config

agent 配置总览：版本、目录布局与生效配置。响应：

```json
{
  "version": "0.7.1 (abc1234)",
  "agentDir": "/Users/me/.doge/agent",
  "cwd": "/Users/me/proj",
  "meshEnabled": true,
  "paths": {
    "settings":        "/Users/me/.doge/agent/settings.json",
    "projectSettings": "/Users/me/proj/.doge/settings.json",
    "models":          "/Users/me/.doge/agent/models.json",
    "auth":            "/Users/me/.doge/agent/auth.json",
    "sessions":        "/Users/me/.doge/agent/sessions",
    "logs":            "/Users/me/.doge/agent/logs/doge.log",
    "meshConfig":      "/Users/me/.doge/agent/mesh.json",
    "uploads":         "/Users/me/.doge/agent/uploads"
  },
  "settings": { ...生效 Settings（全局+项目合并，项目优先）... }
}
```

#### GET /api/config/settings

读取生效配置：`200 {"settings": {...}, "globalPath": "...", "projectPath": "..."}`
（结构同 `settings.json`：defaultProvider/defaultModel/defaultThinkingLevel/
compaction/theme/shellPath/streamIdleSeconds/logLevel/web 等）。

#### PUT /api/config/settings

**整体替换全局** `settings.json`（项目配置不动；预期流程 GET → 改 → PUT）。
请求体为完整 Settings 文档；未知字段会被丢弃（文件本就按强类型解析）。

响应：`200 {"ok":true,"restartRequired":true}` — **所有 settings 字段都在启动时
读取**，改动需重启生效；`400 {"error":"invalid request body"}`。

#### GET /api/config/auth

列出已配置的提供商 API key（**掩码显示，永不回传明文**）：

```json
{ "providers": { "openai": { "configured": true, "maskedKey": "…abcd" } } }
```

#### POST /api/config/auth

写入一个提供商 key（`key` 为空则删除该项），随后自动触发一次 `/api/reload`
等价的运行时刷新：

```json
{ "provider": "openai", "key": "sk-..." }
```

响应：`200 {"ok":true,"reloaded":true}`（刷新失败时 `"reloaded":false` 并附
`"error"`——文件已写入，可稍后重试 `/api/reload`）；`400`（缺 provider / body 非法）。

#### DELETE /api/config/auth/{provider}

删除一个提供商 key 并刷新运行时。响应：`200 {"ok":true,"reloaded":true}`。

#### GET /api/logs

尾部读取诊断日志（logx 事件日志，含 agent/ai/permission/mesh/web 事件时间线）：

- `?lines=N`：返回最后 N 行（默认 200，上限 5000）。
- `?date=YYYY-MM-DD`：读指定日期的滚动归档（`doge.log.<date>`，大小滚动产生
  `<date>.N` 也可指定）而不是当天文件。

响应：`200 {"file":"<路径>","lines":["..."]}`；`404`（归档不存在）/ `400`
（date 形如路径穿越的非法值）。

### 4.8 mesh 网络（v2 扩充）

#### GET /api/mesh

mesh 面板数据。未启用 mesh（未加 `-tags mesh` 或无 token）时 `404`。响应：

```json
{
  "self":     { ...本节点 AgentProfile（peerId/name/roles/version/os/arch/model/skills/cwds/accepting/load/maxLoad）... },
  "addrs":    ["/ip4/192.168.1.10/tcp/4001"],
  "config":   { ...mesh.json 快照（name/roles/allowRemoteTasks/maxConcurrentTasks/forwards/bootstrapPeers，含 token）... },
  "peers":    [ { "id":"...", "addrs":[...], "profile": {...}, "latency": "12ms" } ],
  "tasks":    [ { "taskId":"...", "peer":"...", "status":"running|recovered|completed|error|aborted", "progress":"...", "dispatched":"..." } ],
  "forwards": [ { "name":"web", "mode":"expose|listen", "local":"...", "bind":"...", "port":8080, "peer":"...", "remote":"...", "enabled":true } ]
}
```

`config` 含网络 token（能调 API 即已持有访问 token，mesh token 不构成额外暴露面）；
v5 起 mesh 文件传输配置随快照带出：`allowRemoteFiles`（默认 false）、
`fileMaxBytes`（默认 256 MiB）。`forwards` 是实时状态，`config.forwards` 是
启动时快照，以 `forwards` 为准。

#### GET /api/mesh/forwards

列出转发规则：`200 {"forwards": [ForwardRule...]}`；mesh 未启用 `404`。

#### POST /api/mesh/forwards

新增转发规则（校验 → 写 mesh.json → **热生效**，无需重启）。请求体为一条规则：

```json
{ "name": "db", "mode": "expose", "local": "127.0.0.1:5432", "enabled": true }
{ "name": "web", "mode": "listen", "bind": "127.0.0.1", "port": 8080, "peer": "<peerID>", "remote": "web", "enabled": true }
```

响应：`200 {"ok":true}`；`400`（校验失败，等价 CLI `doge mesh forward add` 的规则
约束）；`404`（mesh 未启用）。

#### POST /api/mesh/forwards/{name}/enable 、 /disable

热启停单条规则（不动其他规则）。响应：`200`；`404`（规则不存在）。

#### DELETE /api/mesh/forwards/{name}

停机并删除规则（持久化到 mesh.json）。响应：`200`；`404`。

### 4.9 进程管理（v2 新增）

#### POST /api/shutdown

优雅停止整个 agent 进程（等价终端 Ctrl+C：先排空 Web 连接；会话按消息粒度已落盘）。
响应 `200 {"ok":true}` 后约 300ms 执行。供管理多节点的客户端远程关停节点。
未接线时 `503 {"error":"shutdown not available"}`。

### 4.10 MCP（v3 新增，MCP 服务器管理）

MCP 接入详见 `docs/mcp-design.md`。以下接口管理 MCP server 状态与工具，
`mcpServers` 配置在 `~/.doge/agent/mcp.json` / `<cwd>/.doge/mcp.json` /
`<cwd>/.mcp.json`（后者优先）。**所有配置类改动都不自动生效**——写文件后需调
`POST /api/mcp/reload`（重新发现），会话级启停需 `POST /api/reload` 落地。

#### GET /api/mcp

MCP 管理器状态：server 列表（**含已禁用的**）、连接状态、工具元数据、缓存/错误。
未初始化（`--no-mcp` 或未接线）时 `404`。

```json
{
  "enabled": true,
  "servers": [
    {
      "name": "chrome",
      "transport": "stdio",
      "enabled": true,
      "connected": true,
      "cache": false,
      "tools": [ { "name": "get_screenshot", "description": "..." } ],
      "error": ""
    }
  ]
}
```

#### POST /api/mcp/reconnect

立即断开并重连一个 server（`"server": ""` = 全部），刷新工具缓存。

请求：`{"server":"chrome"}`。响应：`200 {"ok":true}`；`404`（server 不存在）。

#### POST /api/mcp/enable 、 /api/mcp/disable

启用/禁用 server（写 `disabled` 字段到项目 `<cwd>/.doge/mcp.json`，不改源文件；
禁用的 server 仍出现在 `/api/mcp` 列表中，只是不连接不被调用）。

请求：`{"server":"chrome"}`。响应：`200 {"ok":true}`；`404`。

#### POST /api/mcp/servers 、 DELETE /api/mcp/servers/{name}

配置管理。POST 按**社区 `mcpServers` 粘贴格式**整批导入到项目 `.doge/mcp.json`
（字段级合并 upsert，可多项；`type` 支持 `"stdio"`/`"http"`/`"sse"`，缺省按
command/url 推断）。DELETE 从项目文件删除一个定义。两者都**不触碰运行状态**，
之后调 `POST /api/mcp/reload` 生效；只存在于全局/共享配置里的 server 无法经此
删除（400，请改用 disable 或编辑源文件）。

```json
POST /api/mcp/servers
{
  "my-mcp-server": { "type": "stdio", "command": "npx", "args": ["-y", "some-mcp-server"] },
  "docs": { "type": "http", "url": "https://mcp.example.com/mcp" }
}
```

响应：`200 {"ok":true}`；`400`（名字非法 / type 非法 / 缺 command+url / 目标不在项目配置）。

#### POST /api/mcp/reload

重读全部 mcp 配置文件并重建 server 注册表（断开现有连接）。配置增删、启停的
统一生效入口。**重建后的工具组会立即分发给所有活动会话**（TUI 共享会话、各
web 窗口会话、远端任务 fork）——运行中导入的配置无需重启即可被 agent 使用。
响应：`200 {"ok":true}`；失败 `500`（配置损坏）。

### 4.11 会话级 MCP 启停（v3 新增）

每个**普通会话**可单独停用/启用 MCP 工具组（`mcp` 代理 + direct 提升工具整体
切换）；远端派发任务的会话（`kind == "remote"`）不可单独配置。改动只记录意图，
由该窗口的 `POST /api/reload` 手动落地——用户自己选择生效时机。

#### GET /api/session/mcp

本窗口当前会话的状态：

```json
{
  "available": true,     // 本进程注入了 mcp 工具组（未配置/--no-mcp 时 false）
  "kind": "",            // ""=普通会话，"remote"=远端任务会话
  "enabled": true,       // 期望状态（含待生效的变更）
  "mounted": true,       // 当前实际挂载
  "pending": false       // enabled != mounted，待重载生效
}
```

#### POST /api/session/mcp

记录启停意图。请求 `{"enabled": false}`。响应：
`200 {"ok":true,"pending":true}`；随后 `POST /api/reload` 的响应带
`{"ok":true,"mcpApplied":true}` 表示本次重载应用了会话 MCP 变更。
错误：`403`（远端任务会话）；`404`（本进程无 mcp 工具组）；`400`（缺 enabled）。

fork 继承：从某会话分叉出的新会话继承其挂载状态，之后可独立再启停。

#### POST /api/mcp/servers/{name}/direct 、 /{name}/autostart

切换单个 server 的两个默认行为（内存即时生效 + 持久化到项目
`.doge/mcp.json`，显式写出可覆盖全局/共享配置里的相反设置）：

- **direct**：是否默认把该 server 的全部工具提升为一等工具（`server__tool`
  直接进模型工具列表，注意上下文占用）；关闭则改回 `mcp` 代理按需
  search→call。切换后工具组立即重建并分发到活动会话。
- **autostart**：是否随发现/reload 自动启动并连接。开启立即连接一次；关闭
  断开现有连接（下次调用按需重连）。

请求：`{"enabled": true}`。响应：`200 {"ok":true}`；`404`（server 不存在）；
`400`（缺 enabled）。导入时也可直接在定义里写 `"directTools": true` /
`"autoStart": true` 达到同样效果。

#### GET /api/config 的 mcp 段

`/api/config` 响应含 `mcp` 段（同 `/api/mcp` 的 `{enabled, servers}`），
`paths` 含 `mcpConfig` / `mcpProject` / `mcpShared` 三个配置路径。

### 4.12 Skills（v4 新增，技能查看与会话级状态）

Skills 闭环设计见 `docs/skills-design.md`。接口只读：技能是文件系统上的
`SKILL.md`（agentskills.io 格式），发现目录为 `~/.doge/agent/skills` 与
`<cwd>/.doge/skills`（以及 `--skill` 指定的额外目录）。列表/详情每次请求都
**即时重新扫描**，磁盘上的技能增删无需重启即反映。

#### GET /api/skills

已安装技能摘要（不含正文，正文量用 `contentLength` 提示；正文用详情接口按需取）。

```json
{
  "dirs": ["~/.doge/agent/skills", "<cwd>/.doge/skills"],
  "skills": [
    {
      "name": "html-ppt",
      "description": "HTML PPT Studio — ...",
      "location": "~/.doge/agent/skills/html-ppt/SKILL.md",
      "disableModelInvocation": false,
      "contentLength": 13548
    }
  ]
}
```

#### GET /api/skills/{name}

单个技能的完整详情：元数据 + `SKILL.md` 正文（frontmatter 已剥离）+ 技能包
文件树（references/scripts/templates/assets 递归、去隐藏项、按路径排序），
供面板浏览技能随附资源。

```json
{
  "name": "html-ppt",
  "description": "...",
  "location": ".../SKILL.md",
  "disableModelInvocation": false,
  "contentLength": 13548,
  "content": "# html-ppt — HTML PPT Studio\n...",
  "files": [
    { "path": "SKILL.md", "size": 14265 },
    { "path": "references/layouts.md", "size": 3110 }
  ]
}
```

错误：`404`（技能不存在）。

#### GET /api/session/skills

当前会话**已加载**的技能集合——即其 `skill` 工具与 `<available_skills>` 块背后
的那组（镜像 `/api/session/mcp` 的语义）。新开会话/窗口各自加载，这里反映的是
本窗口所属会话。

```json
{ "skills": [ { "name": "html-ppt", "description": "...", "location": "...", "disableModelInvocation": false, "contentLength": 13548 } ] }
```

#### GET /api/config 的 skills 段

`/api/config` 响应含 `skills` 段：`{dirs, count, names}`——面板一屏概览
已装技能名，正文不在此列。

### 4.13 文件上传与对话附件（v5 新增）

文件先经上传接口进入**暂存区**（`~/.doge/agent/uploads/<id>/`，内容+元数据），
拿到 `uploadID` 后随 `POST /api/prompt` 的 `attachments` 字段进对话。上传与
发送两步分离：客户端可重发消息而不必重传文件。文件内容不内嵌会话 JSONL
（多模态图片除外），会话里只存 uploadID 引用。

限制（`settings.json` 的 `uploads` 节，零值取默认）：单请求/单文件 100 MiB、
暂存区配额 512 MiB、保留 7 天（每小时后台清理，超额按最旧先删）、图片直接
注入上限 5 MiB、文本全文注入上限 64 KiB。

#### POST /api/uploads

`multipart/form-data`，`file` 字段可重复（一次多文件）。文件名服务端清洗
（basename + 去控制字符），MIME 以内容嗅探为准。单文件超限/失败不影响同请求
其余文件，逐项在响应里报 `error`。

```json
{
  "saved": 1,
  "uploads": [
    { "id": "a1b2…(64 hex)", "name": "arch.png", "size": 183421,
      "mimeType": "image/png", "sha256": "9f86…", "source": "web",
      "window": "winA", "createdAt": 1786000000000 }
  ]
}
```

错误：`413 {"error":"request too large"}`（请求体超上限，整体拒绝）/
`400 {"error":"multipart request required"}` / `400 {"error":"no file field"}`。

#### GET /api/uploads

暂存区列表（新→旧）：`200 {"uploads": [UploadInfo...]}`（结构同上逐项）。

#### GET /api/uploads/{id} / GET /api/uploads/{id}/content

单查元数据；`content` 返回文件字节（`Content-Type` 为嗅探值，整体下载、无
Range）。`Content-Disposition` 按类型分派：**仅图片（svg 除外）`inline`**
（供 `<img>` 直显），其余一律 `attachment`——以 `<script>` 开头的文件会被嗅探
为 `text/html`，内联即同源执行（cookie 自动附带），故非图片不内联；响应恒带
`X-Content-Type-Options: nosniff`。`404` 上传不存在。

#### DELETE /api/uploads/{id}

从暂存区删除。响应 `200 {"ok":true}`；`404`。

#### POST /api/uploads/{id}/place（v5）

把暂存区文件复制到白名单目的地（确定性放置，不经 agent/permissions）：

```json
{ "dest": "deploy/server.crt", "overwrite": false }
→ 200 {"ok":true,"path":"/Users/me/proj/deploy/server.crt"}
```

- `dest` 相对路径相对当前会话 cwd。白名单根：会话 cwd 子树、
  `~/.doge/agent/skills/` 子树、`models.json` 与 `mcp.json` 两个精确文件。
- 越界 → `400 destination outside allowed roots`；目标存在且未 overwrite →
  `409`；目标是 symlink → `400`（防写入逃逸）。

#### POST /api/uploads/{id}/apply（v5）

应用暂存文件为配置，**双段确认**：默认 dry-run（校验 + 预览，不落盘），
`confirm:true` 才写盘并触发 reload。`kind`：

| kind | dry-run 预览 | confirm 行为 |
|---|---|---|
| `mcp` | 解析社区 mcpServers 格式，列出将合并的 server | 合并进项目 `.doge/mcp.json` → 重建注册表 → 新工具组分发到活动会话（等价 `/api/mcp/servers` + `/api/mcp/reload`） |
| `models` | 解析 models.json 数组，列出将导入的模型 | 备份全局 `models.json` → `models.json.bak` → 整体替换 → 触发本窗口 `/api/reload` 等价动作 |
| `skills-zip` | zip 安全解包校验（拒绝 zip-slip/symlink/超限），列出将安装的技能 | 解包覆盖安装到 `~/.doge/agent/skills/<name>/` → 触发 reload（技能热刷新，活动会话即时生效） |
| `permissions` | 解析策略，展示默认值/规则计数摘要 | 备份全局 `permissions.json` → `.bak` → 整体替换 → reload |

响应：dry-run `200 {"ok":true,"dryRun":true,"diff":"<预览文本>"}`；confirm
`200 {"ok":true,"reloaded":true,...}`（reload 失败时 `reloaded:false` 并带
`error`，文件已写入可重试）。`auth.json`/`mesh.json`/`settings.json` 不提供
apply。未知 kind → `400`。

#### 附件如何进入对话（服务端行为）

| 附件 | 处理 |
|---|---|
| 图片（嗅探 image/*）且当前模型支持图片输入且 ≤5 MiB | 直接作为 `image` 内容块进入用户消息（多模态） |
| 文本类（嗅探 text/*）且 ≤64 KiB | 全文注入 `text` 块：`[文件 <name>, <size>]\n<内容>` |
| 其余（大二进制/超限图片/模型不支持图片） | 引用行 `text` 块：`[附件 i/n] <绝对路径> size=… mime=… sha256=… 可用 read 工具读取`，agent 按需用 `read`/`bash` 处理 |

转录中该用户消息的 `content` 为数组形态（§5.6/§5.7）：text 块 + image 块。

## 5. 数据模型

### 5.1 Status（GET /api/status）

```json
{
  "sessionId": "e2a3...",
  "cwd": "/Users/me/proj",
  "model": "model/deepseek-v4-flash",
  "thinking": "max",
  "streaming": false,
  "pendingTools": [],
  "errorMessage": "",
  "version": "0.3.2",
  "hasAssistant": true,
  "writable": true,
  "owner": "winA"
}
```

| 字段 | 说明 |
|---|---|
| `sessionId` | 当前会话 ID |
| `cwd` | 会话工作目录 |
| `model` | 当前模型 `provider/id` |
| `thinking` | 当前思维级别（`off`/`minimal`/`low`/`medium`/`high`/`xhigh`/`max`） |
| `streaming` | 是否有 run 在飞行中（busy 判定依据） |
| `pendingTools` | 正在执行的工具调用 ID 列表 |
| `errorMessage` | 最近一次错误消息（无则空串） |
| `version` | 服务端版本号 |
| `hasAssistant` | 会话是否已有 assistant 消息 |
| `writable` | 本窗口是否持有当前会话写控制权（false=只读，被其他窗口/TUI 持有） |
| `owner` | 当前写控制持有者的窗口 id（无争用时为空串） |

### 5.2 SessionInfo（GET /api/session）

```json
{
  "id": "e2a3...",
  "cwd": "/Users/me/proj",
  "filePath": "/Users/me/.doge/agent/sessions/....jsonl",
  "messages": [ Message, ... ]
}
```

### 5.3 SessionSummary（GET /api/sessions）

```json
{
  "id": "e2a3...",
  "filePath": "/Users/me/.doge/agent/sessions/....jsonl",
  "cwd": "/Users/me/proj",
  "timestamp": 1784600000000,
  "name": "proj",
  "messageCount": 12,
  "firstMessage": "介绍一下这个仓库"
}
```

`timestamp` 为 Unix 毫秒。`firstMessage` 为会话首句预览（截断），`name` 为目录名。

### 5.4 SessionStats（GET /api/stats）

```json
{
  "sessionId": "e2a3...",
  "cwd": "/Users/me/proj",
  "model": "model/deepseek-v4-flash",
  "messages": 12,
  "userMessages": 6,
  "assistantMessages": 5,
  "toolMessages": 1,
  "inputTokens": 12000,
  "outputTokens": 3400,
  "reasoningTokens": 1500,
  "cacheRead": 6000,
  "cacheWrite": 800,
  "totalTokens": 16200,
  "cacheHitPct": 50.0,
  "cost": 0.0312,
  "currency": "¥"
}
```

`cacheHitPct` 为缓存命中 token 占发送给模型总 prompt token 的比例（0–100）。
`cost` 为按 `models.json` 单价折算的估算花费（未配置单价时为 0），`currency` 为
货币符号（缺省 `$`）。

### 5.5 ModelInfo（GET /api/models）

```json
{
  "spec": "model/deepseek-v4-flash",
  "name": "Deepseek-v4-flash",
  "provider": "model",
  "id": "deepseek-v4-flash",
  "api": "openai-completions",
  "contextWindow": 1000000,
  "reasoning": true
}
```

### 5.6 Message

对话消息统一结构。**`content` 有两种形态**：

- `role == "user"` 且只有单个纯文本块 → `content` 为**字符串**；
- 其余情况（assistant / toolResult / 多块 user）→ `content` 为 **ContentBlock 数组**。

```json
{
  "role": "assistant",
  "content": [ { "type": "text", "text": "..." } ],
  "api": "openai-completions",
  "provider": "model",
  "model": "deepseek-v4-flash",
  "responseModel": "...",
  "responseId": "...",
  "usage": {
    "input": 1000, "output": 500, "cacheRead": 200, "cacheWrite": 100,
    "totalTokens": 1500, "reasoning": 100,
    "cost": { "input": 0.001, "output": 0.001, "cacheRead": 0.0001, "cacheWrite": 0.0001, "total": 0.002 }
  },
  "stopReason": "stop",
  "errorMessage": "",
  "timestamp": 1784600000000
}
```

| 角色 | 相关字段 |
|---|---|
| `user` | `content`（字符串或数组）、`timestamp` |
| `assistant` | `content`（数组）、`api`/`provider`/`model`/`responseModel`/`responseId`、`usage`、`stopReason`（`pending`/`stop`/`length`/`toolUse`/`error`/`aborted`）、`errorMessage` |
| `toolResult` | `content`（数组）、`toolCallId`、`toolName`、`details`（对象）、`isError`、`addedToolNames`、`usage` |

### 5.7 ContentBlock

判别联合，按 `type` 只带相关字段：

```json
{ "type": "text",     "text": "...", "textSignature": "..." }
{ "type": "thinking", "thinking": "...", "thinkingSignature": "...", "redacted": false }
{ "type": "image",    "data": "<base64>", "mimeType": "image/png" }
{ "type": "toolCall", "id": "call_1", "name": "bash", "arguments": { "command": "ls" } }
```

`redacted` / `textSignature` / `thoughtSignature` 等可选字段缺省省略。

## 6. WebSocket 协议

### 6.1 连接

```
WS /ws?client=<windowID>&token=<accessToken>
```

- 凭证与 REST 同一套：非浏览器客户端可用 cookie 或标准 WS 头部（`Authorization`）；
  浏览器无法设置 WS 请求头，用 `?token=`。
- `client` 查询参数为窗口 ID，缺省 `default`。**与 REST 的 `X-Doge-Client` 头应使用
  同一个值**，使 REST 与 WS 落在同一窗口视图。
- 服务端 Accept 任意 Origin（鉴权已由 token 把关）。
- 入站消息上限 1 MiB；出站广播队列 512 帧，慢客户端会被主动断开。

### 6.2 首帧：session_info 全量快照

连接建立后立即推送一条 `session_info`，客户端可用它重建完整视图（无需先调
`/api/session`）：

```json
{
  "type": "session_info",
  "sessionId": "e2a3...",
  "cwd": "/Users/me/proj",
  "model": "model/deepseek-v4-flash",
  "thinking": "max",
  "streaming": false,
  "pendingTools": [],
  "errorMessage": "",
  "version": "0.3.2",
  "messages": [ Message, ... ],
  "streamingMessage": null
}
```

`streamingMessage`：仅当 `streaming == true` 时非空，为正在流式输出的 assistant
部分消息（用于中途重连时接续显示）。

### 6.3 服务端 → 客户端事件

所有帧都是带 `type` 字段的 JSON。一次 run 的事件顺序：

```
agent_start
  turn_start
    message_start / message_end   (用户输入消息，成对出现)
    message_start                 (assistant 开始，空壳)
    message_update ...            (流式增量，每条携带完整部分消息)
    message_end                   (assistant 完成)
    tool_execution_start
    tool_execution_update ...
    tool_execution_end
    message_start / message_end   (toolResult 消息成对)
    ...（多轮工具循环重复上面的 assistant + tool 段）
  turn_end
agent_end
```

各事件载荷：

| type | 载荷 | 说明 |
|---|---|---|
| `agent_start` | 无 | 一次 run 开始 |
| `turn_start` | 无 | 一轮（一次模型往返）开始 |
| `message_start` | `message`（Message） | 消息开始（user 输入 / assistant 空壳 / toolResult） |
| `message_update` | `message`（Message） | assistant 流式增量，`message` 为**累计的部分消息**，直接整体替换即可 |
| `message_end` | `message`（Message） | 消息完成，可 append 到已提交转录 |
| `tool_execution_start` | `toolCallId`、`toolName`、`args`（对象） | 工具开始执行 |
| `tool_execution_update` | `toolCallId`、`partialResult`（任意） | 工具中间进度（bash 为 `{"Content":[{"type":"text","text":"..."}]...}`，结构不固定，仅用于展示） |
| `tool_execution_end` | `toolCallId`、`toolName`、`result`、`isError` | 工具结束，`result` 见下 |
| `turn_end` | `message`（Message）、`toolResults`（[Message]） | 一轮完成 |
| `agent_end` | `messages`（[Message]） | run 完成，`messages` 为本次 run 新增的全部消息（含用户输入、assistant、toolResult），可整体替换转录 |
| `permission_ask` | `id`、`request` | 权限询问（§6.4） |
| `session_reset` | 无 | 本窗口会话被新建/切换，客户端应重新拉取 |
| `session_list_changed` | 无 | 某处删除会话，客户端应刷新列表 |

`tool_execution_end` 的 `result` 结构（键为小写）：

```json
{
  "isError": false,
  "content": [ { "type": "text", "text": "..." } ],
  "details": { "...": "..." },
  "addedTools": []
}
```

> 注意：`tool_execution_update` 的 `partialResult` 是 Go 结构体默认序列化（键为
> `Content`/`Details` 等大写），与 `tool_execution_end` 的 `result`（小写键）不一致。
> 前端应把 `partialResult` 当不透明进度展示。

### 6.4 权限询问（permission_ask）

当一次工具调用（如 bash 命令）的策略决策为 `ask` 时，服务端广播：

```json
{
  "type": "permission_ask",
  "id": "ask-1",
  "request": {
    "kind": "bash",
    "toolName": "bash",
    "command": "rm -rf /tmp/proj",
    "path": "/tmp/proj",
    "reason": "权限规则: bash 需确认",
    "cwd": "/Users/me/proj"
  }
}
```

客户端通过**同一条 WS 连接**回复（客户端 → 服务端唯一有意义的消息）：

```json
{
  "type": "permission_response",
  "id": "ask-1",
  "response": { "allowOnce": true, "allowAlways": false, "deny": false }
}
```

- 三个布尔字段互斥语义：`allowOnce` 放行一次、`allowAlways` 本次会话内始终放行、
  `deny` 拒绝；全 false 视为无操作。
- 无任何 WS 客户端在线时，`ask` 直接按拒绝处理（无人值守不阻塞）。
- 超时 5 分钟。

**REST 等价通道（v2）**：不维持 WS 的客户端可直接回

```
POST /api/permission/answer
{ "id": "ask-1", "allowOnce": true }
```

响应 `200 {"ok":true}`；ask 已过期/未知 → `404`；缺 id → `400`。注意 REST 通道同样
受"无人在线即拒绝"约束——收到 `permission_ask` 的途径（WS 广播）与应答通道可以
不同，但必须在超时内应答。

### 6.5 断线重连

WS 断线后服务端不保留该窗口的推送状态，客户端应**指数退避重连**（内置前端
`1s → 2s → 4s … ≤ 15s`）。重连后首帧 `session_info` 自带全量快照，无需额外对齐。

## 7. 会话与窗口模型（写控制）

这是外部 UI 最容易踩坑的地方：

- **窗口（window/clientID）**：`X-Doge-Client`（REST）/ `?client=`（WS）标识一个
  浏览器窗口。不同窗口可以打开不同会话。
- **共享会话**：Web 启动时与 TUI 共享同一会话。窗口首次「新建/切换会话」时自动
  **分叉**独立 AgentSession（独立转录、独立工作目录），此后互不影响。
- **写控制（ownership）**：同一会话可被多个窗口/TUI 同时查看，但**只有一个持有者**
  能写（Prompt/Abort/Compact/SwitchModel/RewriteTurn 均要求持有）。规则：
  - 会话无争用时，第一个查看者自动成为持有者（惰性）。
  - 他人持有 → 你的写操作返回 `403`，错误消息为
    `会话正在其他窗口使用（只读），可请求接管`。
  - 调 `POST /api/session/claim` 请求控制权：无人持有立即获得；否则进入 FIFO 队列
    保持只读，直到持有者让出（切走/断开）。
  - **断开自愈**：持有者窗口的 WebSocket 全部断开（刷新换 clientID、关闭标签页）
    后写权视为放弃——下次有窗口查看或 claim 该会话时自动让位，不会出现"无人活跃
    持有却永远只读"的幽灵锁；对当前会话重复 `session/switch` 也不丢写权。
  - **强制夺取**：被拦截（排队/403）时，调 `POST /api/session/takeover` 显式强夺——
    立即释放当前持有者（含 TUI）转给本窗口，不必等它让出；用于多窗口下忘了
    哪个窗口持锁、旧持有者不释放的场景。夺取会广播 `session_control` 事件，
    `GET /api/status` 的 `writable`/`owner` 反映最新归属。
- **对外部 UI 的实操建议**：一个窗口对应一个固定 `clientID`（用 `localStorage`/
  `sessionStorage` 持久化）；进入会话后先 `claim` 一次，若 `owned` 为 false 则界面
  置为只读并提示可请求接管或强制夺取；每次写操作失败遇 `403` 时提示接管；
  收到 WS `session_control` 事件时重拉 `/api/status` 同步 `writable`/`owner`。

## 8. 典型接入流程

### 8.0 客户端 / 独立部署 WebUI（v2 推荐）

1. 拿到访问 token（启动打印 / `~/.doge/agent/web-token` / 用户输入）。
2. `GET /api/auth/whoami`（带 `Authorization: Bearer <token>`）自检。
3. 之后所有 REST 带 Bearer 头（+ 固定 `X-Doge-Client` 标识窗口）；WS 连
   `/ws?token=<token>&client=<id>`。
4. 独立部署的 WebUI 需服务端配置 `--web-cors <origin>`；token 存
   localStorage（跨域下 cookie 不可用）。

### 8.1 登录并打开会话

1. `POST /api/auth/login` `{"token": "..."}` → 拿 cookie（内置前端路径；客户端
   可跳过，直接 Bearer）。
2. `GET /api/session` → 当前会话转录；`GET /api/sessions` → 历史列表。
3. 连接 `WS /ws?client=<id>`（带凭证），收 `session_info` 首帧。
4. 如需指定会话：`POST /api/session/switch {"id": ...}`（服务端广播
   `session_reset`，重拉 `/api/session` 即可）。

### 8.2 发送消息并流式渲染

1. `POST /api/prompt {"text": "..."}` → 202（若 409 表示忙，提示等待）。
2. WS 依次收到 `agent_start → turn_start → message_start/end(user) →
   message_start → message_update* → message_end → ... → turn_end → agent_end`。
3. `message_update` 的 `message.content` 直接替换渲染；`message_end` 的消息 append
   到转录；`agent_end` 的 `messages` 为权威结果。
4. `tool_execution_start/update/end` 驱动工具执行卡片状态。

### 8.3 权限确认

收到 `permission_ask` → 弹窗展示 `request.command` / `request.toolName` /
`request.path` / `request.reason` → 回 `permission_response`。

### 8.4 中断 / 编辑重跑 / 压缩

- 中断：`POST /api/abort`。
- 编辑重跑：对某条 user 消息求序号 → `POST /api/turn/edit {"seq": n, "text": "..."}`
  → 202，之后新一轮流式事件。
- 压缩：`POST /api/compact`。

## 9. 外部 WebUI 最小实现清单

| 能力 | 接口/事件 | 必选 |
|---|---|---|
| 鉴权 | Bearer header / `?token=`（客户端）；login+cookie（内置前端） | ✓ |
| 会话列表/切换/新建/删除 | `GET /api/sessions`、`session/switch|new|delete` | ✓ |
| 当前会话与转录 | `GET /api/session` + WS `session_info` | ✓ |
| 发送消息 + 流式渲染 | `POST /api/prompt` + WS `message_*` | ✓ |
| 文件/图片附件（v5） | `POST /api/uploads` + `POST /api/prompt` 的 `attachments` | 可选（推荐） |
| 工具执行状态 | WS `tool_execution_*` | ✓（建议） |
| 中断 | `POST /api/abort` | ✓ |
| 状态栏（模型/busy/错误） | `GET /api/status` + WS `agent_*` | ✓ |
| 权限确认弹窗 | WS `permission_ask` + `permission_response` 或 `POST /api/permission/answer` | ✓（bash 场景必需） |
| 写控制接管 | `POST /api/session/claim` + 403 处理 | ✓（多窗口场景必需） |
| 模型切换/列表 | `GET /api/models`、`POST /api/model` | 可选 |
| 统计面板 | `GET /api/stats` | 可选 |
| 编辑重跑 | `POST /api/turn/edit` | 可选 |
| 上下文压缩 | `POST /api/compact` | 可选 |
| 配置热重载 | `POST /api/reload` | 可选 |
| 配置查看/编辑 | `GET /api/config`、`GET/PUT /api/config/settings` | 可选（管理面板） |
| API key 管理 | `GET/POST /api/config/auth`、`DELETE /api/config/auth/{provider}` | 可选（管理面板） |
| 诊断日志 | `GET /api/logs` | 可选（排障） |
| mesh 面板 | `GET /api/mesh` | 可选（mesh 场景） |
| mesh 转发规则管理 | `/api/mesh/forwards` 系列 | 可选（mesh 场景） |
| MCP 服务器管理 | `GET /api/mcp`、`/api/mcp/reconnect|enable|disable|reload|servers` | 可选（MCP 场景） |
| 会话级 MCP 启停 | `GET/POST /api/session/mcp` + reload 落地 | 可选（普通会话） |
| 远程关停节点 | `POST /api/shutdown` | 可选（多节点管理） |

## 10. 注意事项

- **CORS 默认关闭**：浏览器跨域接入需配置 `--web-cors`/`web.corsOrigins` 或同源/
  反代部署；非浏览器客户端无此约束。跨域场景鉴权用 Bearer，不要依赖 cookie。
- **`?token=` 的取舍**：浏览器 WS/EventSource 只能用它，但会进访问日志；能设
  header 就用 Bearer。
- **WS 与 REST 的窗口一致性**：`?client=` 与 `X-Doge-Client` 必须传同一值，否则
  事件流与 REST 操作落在不同窗口。
- **异步语义**：`/api/prompt` 与 `/api/turn/edit` 返回 202 只代表已接收，后续全部
  靠 WS 事件；不要轮询 `/api/status` 替代（虽然可用）。
- **busy 判定**：`Status.streaming` 为 true 时 `prompt` 会 409；UI 应在收到
  `agent_start` 后禁用发送、`agent_end`/`agent_end` 的 `streaming=false` 后恢复。
- **只读 403**：多窗口场景下写操作随时可能 403，UI 需优雅处理并提示接管。
- **会话文件惰性创建**：新会话在首条 assistant 消息落盘前没有文件，`GET
  /api/sessions` 可能不包含它；`agent_end` 后刷新列表即可。
- **消息内容两种形态**：`user` 单文本块时 `content` 是字符串，其余是数组，渲染时
  需兼容。
