# doge MCP 支持设计：标准 mcpServers 接入 + 代理工具控制上下文

状态：已实现（M1 全部 + M2 核心 + M3 的 /mcp 命令与 API）
前置调研：本文件「调研：pi-mcp-adapter」一节
参考实现：[nicobailon/pi-mcp-adapter](https://github.com/nicobailon/pi-mcp-adapter)（Pi 的 MCP 桥接扩展）、
[modelcontextprotocol/go-sdk](https://github.com/modelcontextprotocol/go-sdk)（官方 Go SDK）、
[doge mesh 设计](./mesh-design.md)（进程级共享能力、AgentProfile 广播）

## 1. 目标

1. 用**社区标准 `mcpServers` 配置格式**接入任意 MCP server（stdio / Streamable HTTP），
   已有的 `.mcp.json` / `mcp.json` 不用改就能用。
2. **代理模式控制上下文**：默认只注入一个 `mcp` 代理工具（约 200 tokens），模型按需
   search → describe → call，不把成百上千个 MCP 工具 schema 一股脑塞进 system prompt。
   可选 `directTools` 模式把少量高频工具提升为一等工具。
3. **完全复用 doge 现有体系**：`*agent.AgentTool` 工具模型、permissions 权限门、
   system prompt 重建、TUI/Web 事件流、`code` 工具编排——不另起炉灶。
4. **保持 lazy 哲学**：没有 mcp 配置就不初始化、不建缓存目录、不连任何 server；
   配置了但没人调用该 server 的工具就不 spawn 子进程。
5. **默认构建内置**（不需要 `-tags mesh`）：MCP 客户端无网络边、无 CGO，接入官方
   Go SDK 对二进制体积影响可忽略（当前二进制 ~37MB）。
6. 为 mesh 的「配置协作」目标（mesh-design 目标 6：动态加载/下发 mcp 配置、能力广播）
   留好接口，但不在本期实现。

## 2. 调研：pi-mcp-adapter

### 2.1 核心问题与解法

一个 MCP server 的工具定义可以吃掉 10k+ token 的上下文，**无论用不用都付**。
pi-mcp-adapter 把「全部工具直连」换成「单一代理工具 + 按需发现」：

- `mcp({ search: "screenshot" })` —— 关键词搜索工具（只回名称 + 一句描述，不塞 schema）
- `mcp({ tool: "server_tool", args: {...} })` —— 调用具体工具

代理工具还支持 `status / connect / describe / instructions / auth-start / auth-complete /
ui-messages` 等 action。典型用法是两次调用代替几十个工具常驻上下文。

### 2.2 上下文管理

- 代理模式：单个 `mcp` 工具 ≈ 200 tokens，与配置了多少 server 无关。
- `directTools` 模式：个别工具提升为一等工具，每个约 150–300 tokens；官方建议只对
  5–20 个工具用，75+ 工具的 server 用代理或 `string[]` allowlist。
- **Output Guard**：内联文本 > 50 KiB / 2000 行截断，满量溢出到临时文件；
  二进制 blob ≤ 10 MiB 解码到私有临时文件；代理模式的 `details.mcpResult` 仅 ≤ 16 KiB
  时保留原始，否则摘要 + 溢出。

### 2.3 发现与搜索

空格分词，按 name / server / description / `searchKeywords` 加权匹配，`limit`（默认 12）
+ `offset` 分页；支持 `regex: true`；hyphen/underscore 模糊等价
（`context7_resolve_library_id` 能命中 `context7_resolve-library-id`）；
名字解析失败时返回 top 建议。

### 2.4 配置

标准 `mcpServers` 形状，多级文件按优先级合并（高 → 低）：

1. `~/.config/mcp/mcp.json`（用户全局共享）
2. `~/.agents/mcp.json`、`~/.agents/mcp/mcp.json`（用户全局 tool-agnostic）
3. `<agent dir>/mcp.json`（如 `~/.pi/agent/mcp.json`，工具专属全局）
4. `.mcp.json`（项目本地共享）
5. `<tool project dir>/mcp.json`（如 `.pi/mcp.json`，工具专属项目覆盖）

```json
{
  "mcpServers": {
    "chrome-devtools": {
      "command": "npx",
      "args": ["-y", "chrome-devtools-mcp@1.6.0"]
    }
  }
}
```

`settings` 与 server 定义同文件共存：`toolPrefix`、`idleTimeout`（默认 10 分钟）、
`requestTimeoutMs`、`directTools`、`includeTools` / `excludeTools`、`searchKeywords`、
`approveTools`、`disabled`、`trace` 等。host 专属配置（Cursor / Claude Code / Codex 等）
默认 `off`，可被 `init` / `/mcp setup` 检测并导入，或显式 `imports` 数组采纳。

### 2.5 服务器选项与传输

- **stdio**：`command` + `args`；npx 服务器解析为真实二进制路径，跳过 npm 父进程。
- **HTTP**：`url`（Streamable HTTP + SSE 回退）；`${VAR}` / `$env:VAR` 插值，缺失变量在
  发请求前报错。
- **Unix socket**：`socket`（rmcp-mux）。
- 生命周期：`lazy`（默认，首次调用才连接、空闲断开）、`eager`、`keep-alive`、
  `lazy-keep-alive`。
- 元数据**缓存到磁盘**：search/describe/list 在无活连接时也能工作。

### 2.6 安全

OAuth（PKCE + CSRF state，RFC 8414 issuer 校验）、bearer token（字面量 / 环境变量 /
`!command` 执行 / OS 凭据库，fail-closed 不回退明文）、`!command` 秘密命令（10s 超时、
stdout 限 1 MiB）、`approveTools` 工具审批（glob，headless fail-closed）、
`requestHeadersCommand`（每请求签名头）。

### 2.7 CLI 与编排

`pi-mcp-adapter init / token set`；斜杠命令 `/mcp`（面板）、`/mcp setup / tools / prompts /
reconnect / disable / enable / logout`。`mcpScript` 默认开启：JS 脚本里 `tools.search /
tools.describe / tools.call` 多调用编排，worker thread 30s 截止。

## 3. doge 现状与本设计的接缝

doge 的工具链路（实现已核实）：

- 工具是 `*agent.AgentTool{Name, Label, Description, Parameters(JSON Schema draft
  2020-12), PrepareArguments, Execute(...)}`（`internal/agent/types.go`）。
- `NewAgentSession` 组装 toolset：默认 `tools.AllTools(ops)` + `opts.ExtraTools`
  （`internal/coding/agent_session.go:160`）；`AppendExtraTools` / `SetExtraToolsBuilder`
  可在运行时加工具并重建 system prompt（mesh 工具即由此注入）。
- `AgentTool` → provider 侧 `ai.Tool{Name, Description, Parameters}` 的转换在
  `internal/agent/loop.go:859 toolsToLlm`；OpenAI Responses / Chat Completions /
  Anthropic 三家 provider 都直接吃同一份 JSON Schema parameters
  （`openai_responses.go:118`、`openai_completions.go:96`、`anthropic_messages.go:100`）。
- 参数校验 `ai.ValidateToolArguments`（`loop.go:618`），执行前过权限门
  `checkToolPermission` → `permissions.Decider.Check`（`agent_session.go:213`），
  支持 AskHandler（TUI 人批）与 Approver（LLM 批）。
- `code` 工具（PTC，`tools/code.go`）让模型写 JS/Python 脚本编排工具调用，每个脚本内
  调用路由回 session 的权限与工具实现——**天然可以编排 `mcp` 代理工具，不需要单独的
  mcpScript**。
- 输出截断已有 `tools.MaxOutputBytes = 256 KiB`、`MaxOutputLines = 10000`
  （`tools.go:23`）。
- 配置都在 `~/.doge/agent/`（`DOGE_CODING_AGENT_DIR` 覆盖），项目级 `.doge/` 合并
  （`internal/coding/config.go`）。
- 无现成 MCP 代码；社区里有「隐藏工具 + tool_search 检索才进上下文」的 schema
  延迟加载先例，与本设计的代理模式目标一致。

## 4. 总体架构

新增 `internal/mcp/` 包，默认构建内置：

```
internal/mcp/
  config.go    mcp.json/.mcp.json/.doge/mcp.json 发现与合并（mcpServers + settings）
  manager.go   MCPManager：server 注册表、懒连接、空闲回收、进程级共享
  transport.go 基于 go-sdk 的 stdio / StreamableHTTP 客户端封装（Connect/ListTools/CallTool）
  cache.go     工具元数据磁盘缓存（~/.doge/agent/mcp-cache/），search/describe 无连接可用
  proxy.go     生成 `mcp` 代理工具（search/describe/call/status/connect/reconnect）
  direct.go    directTools 提升：MCP 工具 → *agent.AgentTool（命名 server__tool）
  approval.go  与 permissions 集成的内层工具审批（approveTools glob 复用 wildcard 匹配）
```

```
┌────────────────────────────────────────────────────────────┐
│ cmd/doge (main.go)                                        │
│   └── NewAgentSession ── ExtraToolsBuilder / AppendExtra   │
│         └── internal/mcp.MCPManager（进程级单例，引用计数） │
│               ├─ config: 全局+项目 mcpServers 合并          │
│               ├─ server registry: lazy spawn / idle 回收    │
│               ├─ cache: 工具元数据磁盘缓存                   │
│               └─ 产出: mcp 代理工具 + direct 工具             │
│                     │                                     │
│   agent loop: toolsToLlm → provider（JSON Schema 原样）    │
│   checkToolPermission → permissions.Decider                │
│   code 工具（PTC）可编排 mcp 代理工具                        │
└────────────────────────────────────────────────────────────┘
```

进程级共享 + 引用计数：mesh fork 出的子会话共享同一个 MCPManager，不重复 spawn server
（同 mesh 的 port forward 进程级共享思路）；每个 AgentSession 的 toolset 各自引用
manager 产出的工具实例。

### 4.1 配置发现与合并

沿用社区标准 `mcpServers` 形状，doge 自己的文件位置：

1. `~/.doge/agent/mcp.json`（全局，`DOGE_CODING_AGENT_DIR` 已覆盖路径）
2. `<cwd>/.doge/mcp.json`（项目全局，与现有 `.doge/settings.json` 对齐）
3. `<cwd>/.mcp.json`（项目本地共享，与其他工具互通）

优先级：项目 > 用户。另可选项读 `~/.config/mcp/mcp.json`（`settings.hostConfigDiscovery`
默认 `off`，同 pi-mcp-adapter）。server 名冲突时项目覆盖用户。

`settings` 同文件支持：`toolPrefix`（默认 `""`，即 `server_tool` 全名）、
`idleTimeout`（秒，默认 600）、`requestTimeoutMs`（默认 30000）、`directTools`（bool 或
数组）、`includeTools` / `excludeTools`、`searchKeywords`、`approveTools`（全局或
per-server）、`disabled`、`notifyOnStartupConnect`。

**懒加载**：`MCPManager.New()` 不读配置；首次构建 toolset 时才
`DiscoverAndBuild()`——没有配置文件就直接返回空 toolset，不建 `mcp-cache/` 目录。

### 4.2 代理工具 `mcp`

一个 `*agent.AgentTool`，Name = `mcp`（内置工具无此名，无冲突），Parameters 为
JSON Schema（draft 2020-12），action 多态：

| action | 参数 | 行为 |
|---|---|---|
| `search` | `query`、`server`(可选)、`limit`(默认 12)、`offset` | 加权搜索：name > server > description > searchKeywords，hyphen/underscore 等价，命中返回名称 + 一句描述 |
| `describe` | `tool`（`server__tool`） | 返回完整 inputSchema + 描述（从缓存或活连接） |
| `call` | `tool`、`args`(object 或 JSON string) | 调真实 MCP 工具，过审批 + 输出保护 |
| `status` | - | 各 server 连接状态、工具数、cache 命中 |
| `connect` / `reconnect` | `server`(可选) | 立即连接/重连，返回工具清单摘要 |

调用流程：search/describe 命中缓存即可返回（无连接）；`call` 触发懒连接 →
`CallTool` → 结果过输出保护（截断 / 二进制 spill）→ 组装 `AgentToolResult`。
执行前在 `Execute` 内查 `approveTools`：未批准返回文本结果「需要批准：server__tool
（原因）」而不是执行（与权限门「denied 是决策不是错误」语义一致）。

### 4.3 directTools 提升模式

server 级 `directTools: true`（全量）或 `["tool_a", "tool_b"]`，或全局 settings。
提升的工具转成 `*agent.AgentTool`：

- Name：`server__tool`（`toolPrefix` 可换前缀，默认双下划线，避撞内置与 mesh 命名风格）
- Parameters：MCP 工具 `inputSchema` 原样
- Execute：转发 `CallTool`，结果过输出保护
- 权限：天然走 `checkToolPermission`（tool name = `server__tool`）
- **上下文估算**：每个 direct 工具按 schema 大小估算 token，超过阈值（如 20 个或合计
  > ~6k tokens）打警告，建议换代理模式（对齐 `warnOnLargeDirectTools`）

### 4.4 生命周期与性能

- 懒连接：`call` / `direct` 首次执行该 server 的工具才 `Connect`；stdio 用
  `CommandTransport`（go-sdk），HTTP 用 Streamable HTTP。
- 空闲回收：`idleTimeout` 后无调用则 disconnect（释放子进程 / HTTP 会话），下次用重连。
- 元数据缓存：`ListTools` 结果按 server 名 + 协议版本写入
  `~/.doge/agent/mcp-cache/<server>.json`，带 mtime；search/describe/status 只用缓存。
- npx 解析（可选优化）：stdio command 为 `npx`/`npx.cmd` 时解析真实二进制路径缓存，
  跳过 npm 父进程（对齐 pi-mcp-adapter）。
- 并发：MCPManager 为 server 连接加锁，单 server 串行调用、多 server 并行；
  `code` 脚本内并发编排多个 `mcp call` 安全。

### 4.5 输出保护

复用 `tools` 现有常量（`MaxOutputBytes` 256 KiB / `MaxOutputLines` 10000）作为内联上限；
文本超限截断并在尾部注明「已截断，完整输出在 <临时文件路径>」；二进制内容
（image/audio 等 `isError=false` 的 blob）写临时文件并给路径。错误统一 `ToolError`
文本编码，不崩溃。

### 4.6 权限与审批

- 代理模式：外层 `mcp` 工具走正常 `checkToolPermission`；内层具体 `server__tool` 由
  `approval.go` 查 `approveTools` glob（复用 `permissions/wildcard.go` 的匹配逻辑），
  未配置 ask handler 的 headless 场景 fail-closed 拒绝。
- direct 模式：每个工具天然过 `checkToolPermission`，无需额外逻辑。
- mesh 远程任务（`remote_agent` 派发到从节点）复用从节点自己的权限与审批，无需透传。

### 4.7 CLI / 斜杠命令

`BuiltinSlashCommands` 增加 `/mcp`（对齐 pi-mcp-adapter，不占用 `/mcp` 别名冲突——
doge 无保留 `/mcp`）：

- `/mcp` —— 面板：server 列表、连接状态、工具数、cache 情况
- `/mcp setup` —— 引导生成 `.mcp.json`（scaffold + 常见 server 提示）
- `/mcp tools [server]` —— 列出工具
- `/mcp reconnect [server]`、`/mcp disable <server>`、`/mcp enable <server>`（只写
  `disabled` 字段到项目 `.doge/mcp.json`，不改源文件）
- `/mcp status`

### 4.8 code 工具（PTC）编排

不新增 mcpScript。`code` 脚本通过 `doge_tools` 模块直接调 `mcp` 工具
（`tools.call({ tool: "server_tool", args: {...} })` 的脚本侧等价），一个脚本做
「搜索 → 多 server 并行调用 → 过滤聚合 → 回传结论」，与现有 PTC 完全一致，零新机制。

## 5. 依赖与构建

- `github.com/modelcontextprotocol/go-sdk`（官方 Go SDK，Google 协作维护），用
  `mcp`（Client/CommandTransport/Streamable HTTP）+ `jsonrpc` 包；`mark3labs/mcp-go`
  是其前身、仍在维护，两者 API 同源，选官方。
- 默认构建包含（无需 build tag）：无 CGO、无网络绑定，二进制增量可接受。
- mesh 协作（AgentProfile 广播 MCP 能力、远端下发 mcp 配置）留到 mesh 二阶段，本期
  只保证 MCPManager 可被远程配置驱动（配置对象可从远端覆盖）。

## 6. 里程碑

- **M1**：配置发现合并（全局 + 项目 + `.mcp.json`）、stdio 客户端、`mcp` 代理工具
  （search/call/status）、权限门 + `approveTools`、输出截断。交付：手工配置即可接入
  任意 stdio MCP server。
- **M2**：Streamable HTTP（url + headers + env 插值）、元数据磁盘缓存、idle 回收、
  directTools 提升模式 + token 估算告警、二进制 spill、npx 解析优化。
- **M3**：`/mcp*` 斜杠命令与 setup 引导、并发编排打磨、mesh 能力广播接口
  （AgentProfile 增加 mcpServers 摘要，便于任务按能力路由——衔接 mesh-design 目标 5/6）。

## 7. 未决问题

1. 代理工具名 `mcp` 是否够清晰？备选 `mcp_tools`（更长但语义更明确）；倾向 `mcp`。
2. 输出内联上限沿用 doge 现有 256 KiB 还是对齐 pi 的 50 KiB/2000 行（省 token 但
   破坏既有 read 输出一致性）？倾向沿用现有常量。
3. `directTools` 默认值：默认全关（纯代理）还是按 schema 大小自动提升？倾向默认关。
4. OAuth 认证（`auth-start`/`auth-complete` + OS 凭据库）是否进 M1，还是 M2/M3？
   倾向 M3（stdio 本地 server 大多不需要）。

## 8. 实现记录

已实现（`internal/mcp/`，默认构建内置，官方 go-sdk v1.7.0）：

- **M1**：配置发现合并（agentDir/mcp.json → .doge/mcp.json → .mcp.json，字段级合并）、
  stdio 客户端（CommandTransport）、`mcp` 代理工具（search/describe/call/status/
  connect/reconnect）、approveTools 审批门（复用 `permissions.MatchPattern`）、
  输出保护（256 KiB 截断 + 二进制 spill 临时文件）。
- **M2**：Streamable HTTP（url + env 插值）、工具元数据磁盘缓存
  （agentDir/mcp-cache/<server>.json，search/describe/status 无连接可用）、
  空闲回收（idleTimeout，默认 600s，30s 一轮）、directTools 提升
  （`server__tool` 命名，唯一 eager 例外：direct 模式无缓存时 BuildTools 阶段
  连接一次拉清单）。
- **M3 子集**：`/mcp` TUI 命令（status/tools/reconnect/enable/disable/setup）、
  `--mcp-config/--no-mcp/--mcp-trace` CLI 参数、web API（GET /api/mcp +
  POST reconnect/enable/disable + /api/config 的 mcp 段）。
- **自测**：`internal/mcp/mcp_test.go` — 配置合并、检索加权（hyphen/underscore
  等价）、审批 glob、输出保护、fake stdio MCP server 端到端（真实子进程 +
  SDK 客户端打通 search/status/describe/call/direct/落盘）。

未实现（留待后续）：OAuth（M3 尾）、npx 二进制解析优化、mesh 能力广播
（AgentProfile 的 mcp 段）、code 工具脚本内多调用编排示例。

### 8.1 配置示例

`~/.doge/agent/mcp.json`（或 `<cwd>/.doge/mcp.json`、`<cwd>/.mcp.json`，后者优先）：

```json
{
  "mcpServers": {
    "chrome": {
      "command": "npx",
      "args": ["-y", "chrome-devtools-mcp@latest"],
      "directTools": ["get_screenshot", "navigate"]
    },
    "docs": { "url": "https://mcp.example.com/mcp" }
  },
  "settings": {
    "toolPrefix": "__",
    "idleTimeout": 600,
    "requestTimeoutMs": 30000,
    "approveTools": ["chrome__*"]
  }
}
```

### 8.2 使用

- `doge --web` / TUI：`/mcp` 面板、`/mcp tools`、`/mcp reconnect`、
  `/mcp enable|disable <server>`（写项目 .doge/mcp.json）、`/mcp setup` 脚手架。
- CLI：`--no-mcp` 禁用、`--mcp-config <path>` 覆盖全局文件、
  `--mcp-trace` 调试日志。
- API：`GET /api/mcp`、`POST /api/mcp/reconnect|enable|disable`、`GET /api/config`
  的 mcp 段（详见 docs/web-api.md §4.10）。
