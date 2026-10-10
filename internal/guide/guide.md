# doge 使用指南：启动命令与配置

版本：0.9.0（2026-08-27）。本文档覆盖 doge 的全部启动命令、参数、配置文件与配置项，
是 README 的展开版；字段、默认值均与代码逐一核对（`cmd/doge/`、`internal/coding/`、
`internal/ai/`、`internal/mcp/`、`internal/mesh/`、`internal/web/`）。

> 本指南**内置于二进制**（源文件 `internal/guide/guide.md`，随构建嵌入），agent 侧经
> 内嵌 `guide` 工具按模块查询（不带参数返回模块列表）；独立部署、无仓库文档的机器
> 上同样可用，内容与本二进制版本一致。

## 1. 构建与版本
<!-- module: build | 构建目标与产物（默认/mesh/linux 交叉编译）、版本号注入、-tags mesh 的含义 -->

```bash
make build              # 构建当前平台二进制 → dist/doge（默认构建不含 mesh，体积最小）
make build-mesh         # 构建带 P2P mesh 的二进制 → dist/doge-mesh（-tags mesh）
make build-linux        # 交叉编译 linux/amd64 → dist/doge-linux-amd64
make build-mesh-linux   # 交叉编译 linux/amd64 + mesh → dist/doge-mesh-linux-amd64
make build-darwin       # 交叉编译 darwin/amd64 → dist/doge-darwin-amd64
```

- Makefile 会把当前 git commit 写入二进制：`./dist/doge --version` 显示如
  `0.9.0 (10f8bca)`，工作区有未提交改动时带 `-dirty` 后缀；`go build -o doge ./cmd/doge`
  直接构建则只有版本号。
- 不带 `-tags mesh` 的构建不含 libp2p 依赖；mesh 相关功能（`doge mesh *`、
  `--mesh`、`mesh_forward` 工具）只有 `make build-mesh` 产物才可用。
- 本地开发：`go vet ./...`、`go test ./...`；mesh 侧 `go test -tags mesh ./internal/mesh/...`
  （Makefile `test-mesh` 目标）。

## 2. 启动命令

### 2.1 运行模式
<!-- module: run-modes | TUI/print/json 三种运行模式的进入条件与用法、会话导出 HTML -->

```bash
# 交互模式（TUI）：stdin/stdout 均为 TTY 时默认进入
doge

# 非交互：-p 或 --mode print（stdout 非 TTY 时也会自动进入）
doge -p "介绍一下这个仓库"
doge --mode print "…"

# JSON 事件流（每行一个事件，适合脚本消费）
doge --mode json "…"

# 导出会话为 HTML 文件
doge --export <session-id>
```

注意：`-p` 是 bool 开关，模型等选项要放在消息前面：`doge --model openai/gpt-5.2 -p "…"`。
管道空调用（`echo -n "" | doge -p`）直接拒绝，不发起 LLM 请求。

### 2.2 模型选择
<!-- module: model-selection | --model 语法（provider/model[:thinking]）、--list-models 查看可用模型 -->

```bash
doge --model deepseek/deepseek-chat                      # provider/model
doge --model anthropic/claude-sonnet-4-5:high            # 追加思维级别
doge --list-models                                       # 列出内置 + 自定义模型目录
doge --api-key sk-...                                    # 覆盖 auth.json / 环境变量
```

模型解析优先级：`--model` 参数 > settings `defaultProvider`+`defaultModel` > 内置默认模型。
`--model` 未指定思维级别时取 settings `defaultThinkingLevel`。

### 2.3 会话选择
<!-- module: session-selection | --resume/--continue/--session/--fork/--no-session 会话选择与持久化 -->

```bash
doge                        # 新会话（惰性创建：首条消息落盘前不产生任何磁盘痕迹）
doge --continue             # 继续最近会话（--resume 同义）
doge --session <id 或路径>   # 恢复指定会话（支持绝对路径）
doge --no-session           # 不持久化会话
```

会话分叉（ForkNew/ForkSession）在 Web 侧自动发生——Web 第一次「新建/切换会话」时
分叉出独立 Agent 会话；CLI 的 `--fork` 目前为占位参数（报错提示，未接线）。

### 2.4 Web 控制台
<!-- module: web | Web 控制台：启动参数、访问 token、CORS、无头模式（--no-tui） -->

```bash
doge --web                          # Web + TUI 同进程运行，共享同一会话
doge --no-tui                       # 仅 Web（headless；隐含 --web，适合 Docker/远程）
doge --web --no-tui --port 8080 --web-host 0.0.0.0
doge --no-web                       # 显式禁用 settings 里启用的 Web
doge --web-cors https://ui.example.com   # 允许跨域（可重复，或 '*'）
doge --web-token my-secret          # 固定访问 token
```

Web 参数完整说明与 settings `web` 节的对应关系见 §4.3。

### 2.5 MCP
<!-- module: mcp-flags | --no-mcp/--mcp-config/--mcp-trace 等 MCP 启动参数 -->

```bash
doge --web                          # 无需额外参数，配置好 mcp.json 即用（懒加载）
doge --no-mcp                       # 显式禁用 MCP
doge --mcp-config /path/mcp.json    # 覆盖全局 mcp.json（项目级文件仍合并）
doge --mcp-trace                    # 请求级调试日志（写入 ~/.doge/agent/logs/doge.log）
```

### 2.6 Mesh
<!-- module: mesh-quickstart | mesh 组网三步（new/join/daemon）、mesh 子命令、agent 侧工具（remote_agent/remote_tool/mesh_forward/mesh_peers）与 NAT 穿透 -->

```bash
doge --mesh                         # mesh 与 agent 会话共存（TUI/print/Web 均可）
doge --mesh-token <token>           # 用指定 token 加入（覆盖 mesh.json 的 token）
doge mesh new --name mynode         # 生成新网络 token（写 mesh.json）
doge mesh join <token> [--bootstrap <addr>...] [--name N]   # 用共享 token 加入
doge mesh daemon [--port N]         # 常驻节点（无 agent 会话，Ctrl+C 退出）
doge mesh peers [--wait 5]          # 列出已发现节点及其能力
doge mesh profile [peer]            # 查看本机/指定节点完整 profile
doge mesh forward add <name> --mode expose|listen …   # 新增转发规则
doge mesh forward enable|disable|remove <name>        # 热启停/移除单条规则
doge mesh forward list              # 列出转发规则
```

Mesh 只有 `-tags mesh` 构建可用；详细概念、安全边界与组网方式见
`docs/mesh-design.md` 与 README「Mesh」章节。

mesh 会话（`doge --mesh`）的 agent 侧工具：`mesh_forward`（管理本机转发规则）、
`mesh_peers`（发现节点与能力）、`remote_agent`（派任务给远端 agent 执行，`files`
参数可随任务把暂存区文件递给远端）、`remote_agent_status`（查在途远端任务）、
`remote_tool`（**直接调用远端节点级
工具**——精确操作远端 forward/listen 绑定、查远端网格视图，不经远端 agent、
远端不跑 LLM；直接调用默认开放，远端写 `allowRemoteTools: false` 可关闭）、
`mesh_file`（跨节点文件传输：`stage` 本地文件进暂存区、`send` 推给对端、
`pull` 拉回、`list` 看对端可分享文件；对端须开 `allowRemoteFiles`，收到的文件
只进暂存区，由 agent 用 read 决定用途）。各工具的 `peer`
参数可用节点名（`mesh_peers` 输出的 `name=`）代替 peer ID，重名即时报错并列出
候选。NAT 穿透默认
启用（端口映射 + 自动中继 + 打洞）；两侧都在 NAT 后时经开了 `relay: true`
的种子节点中继互通，详见 README「NAT 穿透」与 `docs/mesh-design.md` §11.16。

### 2.7 其它参数
<!-- module: misc-flags | 日志级别等其余启动参数 -->

| 参数 | 说明 |
|---|---|
| `--system-prompt <text>` | 自定义系统提示词 |
| `--skill <dir>` | 额外技能目录（替换默认发现目录） |
| `--prompts <dir>` | 额外 prompt 模板目录（替换默认发现目录） |
| `--approve` | 自动批准项目信任 |
| `--log-level <level>` | 日志级别 debug\|info\|warn\|error\|off（默认 settings `logLevel`，再默认 info） |
| `--version` / `--help` | 版本 / 帮助 |

## 3. 配置目录与优先级

### 3.1 目录结构
<!-- module: dir-layout | ~/.doge/agent 与项目 .doge 的目录结构、每个文件放什么 -->

配置与数据都在 `~/.doge/agent/`（可用 `DOGE_CODING_AGENT_DIR` 覆盖；见 §3.3）。
首次运行只创建目录结构，配置文件需手工编辑。

```
~/.doge/agent/
├── AGENTS.md          # 全局指令与环境定义（跨项目角色设定，见 §4.8）
├── auth.json          # API 密钥（§4.1）
├── models.json        # 自定义模型目录（§4.2）
├── settings.json      # 全局设置（§4.3）
├── mcp.json           # 全局 MCP 服务器配置（§4.4）
├── permissions.json   # 权限规则（§4.5，可选）
├── mesh.json          # mesh 网络配置（§4.6，mesh new 生成）
├── mesh-key           # 节点身份私钥（0600，独立于 mesh.json，勿随 token 复制）
├── web-token          # 本次启动的 Web 访问 token（0600，重启即失效）
├── sessions/          # 会话 JSONL 文件（append-only 会话树）
├── uploads/           # 上传暂存区（web/mesh 文件统一落地，配额+TTL 自动清理）
├── logs/              # 诊断日志 doge.log（按天滚动）
├── mcp-cache/         # MCP 工具元数据缓存（<server>.json）
├── skills/            # 技能目录（SKILL.md，agentskills.io 格式）
└── prompts/           # prompt 模板目录
```

项目级目录 `<项目>/.doge/` 下同样放置 `settings.json`、`mcp.json`、
`permissions.json`、`skills/`、`prompts/`；`<项目>/.mcp.json` 作为与其它工具互通的
MCP 共享配置。

### 3.2 合并规则
<!-- module: merge-rules | 全局与项目级配置的合并优先级规则 -->

- **settings.json / mcp.json**：全局 + 项目读取后深合并，**项目配置优先**（字段级）。
- **permissions.json**：全局 + 项目叠加，项目配置只能收紧、不能放宽全局 `deny`
  （如全局为 `ask`，项目可提到 `allow`？**不能**——项目规则永远不能放行全局
  `deny` 的项）。
- **skills / prompts**：全局与项目目录全部扫描，同名技能/模板按目录顺序后者覆盖。
- **auth.json / models.json / mesh.json**：仅全局（无项目级）。

### 3.3 环境变量
<!-- module: env-vars | 环境变量表：API key 兜底、目录覆盖、运行时选择等 -->

| 变量 | 说明 |
|---|---|
| `DOGE_CODING_AGENT_DIR` | 覆盖配置根目录 `~/.doge/agent` |
| `DOGE_CODING_AGENT_SESSION_DIR` | 覆盖会话目录（默认 `<agent>/sessions`） |
| `DOGE_WEB_NO_OPEN=1` | 禁止 Web 启动时自动打开浏览器（CI/无头环境） |
| `DOGE_CODE_RUNTIME=node\|python3` | 强制指定 PTC `code` 工具运行时（默认 PATH 探测） |
| `DOGE_DEBUG` | 非空时把诊断日志同时打到 stderr（用于排查） |
| `DOGE_UI_DEBUG=<path>` | TUI 事件调试日志写到指定文件 |
| `DOGE_CODE_DEBUG` | 非空时记录 PTC `code` 工具的 fd3 协议调试信息 |
| `OPENAI_API_KEY` / `ANTHROPIC_API_KEY` / `DEEPSEEK_API_KEY` 等 | provider 密钥兜底 |
| `NO_COLOR` | 禁用颜色输出（TUI 自适应降级） |
| `SHELL` | bash 工具使用的 shell |

密钥解析优先级：`--api-key` 参数 > `auth.json` > 环境变量。

## 4. 配置文件详解

### 4.1 auth.json —— API 密钥
<!-- module: auth-json | API 密钥存储格式、/login /logout 与环境变量兜底 -->

```json
{
  "openai":    { "type": "api_key", "key": "sk-..." },
  "anthropic": { "type": "api_key", "key": "sk-ant-..." },
  "model":     { "type": "api_key", "key": "sk-..." }
}
```

- 键名是 provider，必须与模型配置里的 `provider` 一致。内置 provider：
  `openai` / `anthropic` / `deepseek` / `qwen` / `moonshot` / `zhipu` / `zai` /
  `openrouter` / `local`；自定义模型用自己的 provider 名。
- `local` 端点（如 Ollama）无需密钥。

### 4.2 models.json —— 自定义模型目录
<!-- module: models-json | 自定义模型定义：provider、模型、上下文窗口、thinking、单价 -->

JSON 数组，每个元素一个模型：

```json
[
  {
    "id": "deepseek-v4-flash",
    "name": "Deepseek-v4-flash",
    "api": "openai-completions",
    "provider": "model",
    "baseUrl": "https://api.example.com/v1",
    "contextWindow": 1000000,
    "maxTokens": 384000,
    "input": ["text"],
    "reasoning": true,
    "thinkingLevelMap": {
      "minimal": "low", "low": "low", "medium": "medium",
      "high": "high", "xhigh": "high", "max": "high"
    },
    "headers": {
      "X-Api-Version": "2026-08-01"
    },
    "sessionAffinity": "openrouter",
    "impersonate": "codex",
    "cost": {
      "input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0,
      "currency": "¥",
      "multiplier": 1,
      "peakValley": {
        "periods": [
          { "start": "23:00", "end": "07:00", "multiplier": 0.5 },
          { "start": "09:00", "end": "11:00", "multiplier": 1.5 },
          { "start": "19:00", "end": "21:00", "multiplier": 1.5 }
        ]
      }
    }
  }
]
```

字段说明：

| 字段 | 说明 |
|---|---|
| `id` / `name` | 模型 ID（`--model provider/id` 用）与展示名 |
| `api` | `openai-responses` / `openai-completions` / `anthropic-messages` |
| `provider` | provider 名，决定密钥查找与请求头风格 |
| `baseUrl` | API 端点 |
| `contextWindow` / `maxTokens` | 上下文窗口 / 单次最大输出 token |
| `input` | `["text"]` 或 `["text", "image"]`（多模态） |
| `reasoning` | 启用思维模式；`thinkingLevelMap` 把 doge 思维级别（minimal/low/medium/high/xhigh/max）映射到模型参数——anthropic 系为 token 预算（数字），openai 系为级别字符串（如 `"high"`） |
| `headers` | 随请求附加的 HTTP 头，键名大小写不敏感。值为 `null`（或 `""`）表示**抑制**该头（含请求层的默认头）。同名头请求级设置优先于模型级 |
| `sessionAffinity` | 缓存亲和头开关：`"openai"` 发 `session_id` + `x-client-request-id` + `x-session-affinity`；`"openrouter"` 发 `x-session-id`；其他非空值发 `x-client-request-id` + `x-session-affinity`；缺省关闭 |
| `impersonate` | 客户端身份声明：部分按客户端工具限制/计费的网关只认请求头标识，设置后自动注入对应客户端的 User-Agent 与署名头（见下表）；不设则以 doge 自己的 UA 直连。仅改写请求头、不绕过任何鉴权，是否允许以特定客户端身份接入由网关条款判定。显式 headers 优先级更高，值大小写不敏感，未知值忽略 |

`impersonate` 支持值（版本号为代表性值，会随上游漂移）：

| 值 | User-Agent | 附加头 |
|---|---|---|
| `claude-code` | `claude-cli/2.1.75 (external, cli)` | `x-app: cli` |
| `codex` | `codex_cli_rs/0.0.1` | `originator: codex_cli_rs` |
| `openclaw` | `openclaw/2026.7.1` | `HTTP-Referer`、`X-OpenRouter-Title`、`X-OpenRouter-Categories` 等 |

没有 `cursor` / `opencode`（opencode 的 LLM 传输层不设 UA、无一致可复现的标识；cursor 闭源无公开可核实值）。

`cost` 字段（全部可填 0/省略）：

- `input` / `output` / `cacheRead` / `cacheWrite`：单价（每 M tokens，单位自定）
- `currency`：费用前缀货币符号（`$` / `¥` / `€` / `CNY`），缺省 `$`
- `multiplier`：单价 → 展示值换算倍率（单价存美元想按 ¥ 展示时填汇率如 `7.2`）
- `peakValley.periods`：峰谷价时段。`start`/`end` 为本地时间 `HH:MM`，`end` 早于
  `start` 表示跨午夜（`23:00`–`07:00`），`00:00`–`24:00` 全天；时段左闭右开，
  可配任意多个

内置模型目录：`doge --list-models`。

### 4.3 settings.json —— 设置（全局 + 项目合并）
<!-- module: settings-json | 全局设置字段表：web、日志、流超时、上下文策略等 -->

```json
{
  "defaultProvider": "model",
  "defaultModel": "deepseek-v4-flash",
  "defaultThinkingLevel": "max",
  "streamIdleSeconds": 120,
  "compaction": { "enabled": true, "reserveTokens": 40000, "keepRecentTokens": 120000 },
  "logLevel": "info",
  "web": {
    "enabled": true,
    "host": "127.0.0.1",
    "port": 8080,
    "token": "my-secret",
    "noTui": true,
    "corsOrigins": ["https://ui.example.com"]
  },
  "uploads": {
    "maxRequestBytes": 104857600,
    "maxFileBytes": 104857600,
    "quotaBytes": 536870912,
    "retentionDays": 7,
    "maxImageInlineBytes": 5242880,
    "maxTextInlineBytes": 65536
  }
}
```

| 字段 | 说明 |
|---|---|
| `defaultProvider` + `defaultModel` | 默认模型（未传 `--model` 时生效，两者须同时设置） |
| `defaultThinkingLevel` | 默认思维级别 |
| `streamIdleSeconds` | 提供商静默预算（默认 120）。SSE 连接超过该时长无数据（或请求迟迟等不到响应头）即判定停摆，中断本次流并释放会话 |
| `compaction.enabled` / `reserveTokens` / `keepRecentTokens` | 上下文压缩开关与阈值（结构体字段名大小写不敏感，小驼峰写法同样生效） |
| `logLevel` | 诊断日志级别（`--log-level` 覆盖） |
| `web.enabled` | 配置即启动 Web，无需 `--web` |
| `web.host` / `web.port` | 监听地址 / 端口（`--web-host` / `--port` 覆盖） |
| `web.token` | 固定访问 token（缺省则每次随机生成并写入 `web-token`） |
| `web.noTui` | 仅 Web 模式（`--no-tui` 覆盖） |
| `web.corsOrigins` | 允许跨域的 WebUI 来源（`--web-cors` 覆盖，可重复） |
| `uploads.*` | 文件上传限制（零值取默认）：单请求/单文件 100MiB、暂存区配额 512MiB、保留 7 天（`~/.doge/agent/uploads`，后台周期清理）、图片直接注入对话上限 5MiB、文本全文注入上限 64KiB |

Web 参数优先级：**CLI 参数 > settings `web` 节 > 内置默认**。


### 4.4 mcp.json —— MCP 服务器（全局 + 项目合并）
<!-- module: mcp-json | MCP server 配置：stdio/http、directTools、approveTools、项目级 .mcp.json -->

社区标准 `mcpServers` 格式，三个文件按优先级合并（后者优先，字段级）：

1. `~/.doge/agent/mcp.json`（全局）
2. `<项目>/.doge/mcp.json`（项目）
3. `<项目>/.mcp.json`（项目共享，与其它工具互通）

```json
{
  "mcpServers": {
    "chrome": {
      "type": "stdio",
      "command": "npx",
      "args": ["-y", "chrome-devtools-mcp@latest"],
      "directTools": ["get_screenshot", "navigate"]
    },
    "docs": { "type": "http", "url": "https://mcp.example.com/mcp" }
  },
  "settings": {
    "toolPrefix": "__",
    "idleTimeout": 600,
    "requestTimeoutMs": 30000,
    "approveTools": ["chrome__*"]
  }
}
```

server 级字段：

| 字段 | 说明 |
|---|---|
| `type` | `"stdio"`（command+args，缺省）或 `"http"`（+url；`"sse"` 视同 http）——与 Claude Desktop / Cursor 等工具格式互通 |
| `command` / `args` | stdio 启动命令 |
| `url` | Streamable HTTP 端点 |
| `env` / `headers` | 进程环境 / 请求头 |
| `disabled` | 禁用它（仍显示在面板，可重新启用） |
| `autoStart` | 发现/重载后自动启动并连接（默认懒连接） |
| `directTools` | `true` = 全部工具提升为一等工具，或工具名数组 |
| `includeTools` / `excludeTools` | 工具清单过滤 |
| `approveTools` | glob；配置后未命中的工具调用直接拒绝 |
| `searchKeywords` | 代理搜索关键词 |

顶层 `settings`：

| 字段 | 说明 |
|---|---|
| `toolPrefix` | direct 工具名前缀（默认 `__`，命名 `server__tool`） |
| `idleTimeout` | 空闲回收秒数（默认 600） |
| `requestTimeoutMs` | 请求超时（默认 30000） |
| `directTools` | 全局的一等工具提升（同 server 级写法） |
| `approveTools` | 全局工具允许清单 |
| `trace` | 全局请求追踪 |

- **懒加载**：没有 mcp 配置就不初始化、不建缓存目录、不 spawn 任何进程。
- 工具元数据缓存到 `~/.doge/agent/mcp-cache/<server>.json`，`mcp search/describe`
  无连接也能用；`/mcp reconnect` 或首次调用时拉取刷新。
- 模型侧默认只注入一个 `mcp` 代理工具，按需 search → describe → call；
  `directTools` 提升高频工具为一等工具。设计细节见 `docs/mcp-design.md`。
- Web 控制台「Ⓜ MCP」面板可管理服务器状态、导入配置、切换一等工具与自启、
  会话级启用/停用；API 见 `docs/web-api.md` §4.10–4.11。

### 4.5 permissions.json —— 权限规则
<!-- module: permissions-json | 工具权限规则：allow/deny/ask、路径边界、cwd 绑定、默认策略 -->

文件位于 `~/.doge/agent/permissions.json`（可选；项目级 `.doge/permissions.json`
叠加）。合并规则：后读的文件只能**收紧**决策——项目文件永远不能把全局 `deny`
放宽为 `allow`（`allow` 与更严格的既有决策冲突时被忽略）；`autoDecide` 各风险级
同样只能按 `deny > llm > allow` 的方向收紧。内置高危命令库无需配置即生效且不可被放行
（`rm -rf /`、`mkfs`、`dd` 写设备、`shutdown/reboot/halt/poweroff`、fork bomb 等）；
`curl|sh`、读取 `auth.json`/密钥/`~/.ssh` 默认拒绝但可显式配置放行。

```json
{
  "defaultPolicy": { "tools": "allow", "bash": "allow" },
  "tools": { "bash": "ask" },
  "bash": [
    { "pattern": "git *", "decision": "ask" },
    { "pattern": "git status", "decision": "allow" }
  ],
  "special": { "externalDirectory": "ask" }
}
```

| 字段 | 说明 |
|---|---|
| `defaultPolicy.tools` / `.bash` | 全局默认决策 `allow` / `deny` / `ask` |
| `tools` | 按工具名覆盖（`bash`、`read`、`write`、`edit`、`grep`、`code`、`mesh_forward` 等） |
| `bash` | 命令模式规则数组；匹配按“更具体的模式优先”（长模式优先） |
| `special.externalDirectory` | `read/write/edit` 等访问会话工作目录之外的路径时的决策（默认放行；路径先绝对化+清理，`../` 穿越无法绕过） |

通配符：`*` 匹配任意字符（含 `/`），`?` 匹配单个字符。

`ask` 交互（TUI）：状态条显示 `⚠ 权限请求`，按 `y` 放行一次、`a` 本次会话始终
放行、`d`/`esc` 拒绝。纯 Web 模式下由浏览器确认。

**无人值守自动决策**（`-p` / `--mode json` 等非交互模式）：

```json
{
  "defaultPolicy": { "bash": "ask" },
  "autoDecide": { "readOnly": "allow", "projectLocal": "allow", "system": "llm", "network": "llm", "unknown": "deny" },
  "llmApproval": { "enabled": true },
  "humanOnly": ["git push --force*"]
}
```

| 风险级 | 示例 | 默认 |
|---|---|---|
| `readOnly` 只读 | `ls`、`cat`、`git status`、`docker ps`、`nginx -t` | 自动放行 |
| `projectLocal` 项目内 | `git add/commit`、`go build`、`docker build` | 拒绝（可配置放行） |
| `system` 系统变更 | `systemctl restart`、`sudo apt install`、`docker rm` | 拒绝或 LLM 审批 |
| `network` 网络 | `git push/pull`、`docker push`、`scp` | 拒绝或 LLM 审批 |
| `unknown` 未识别 | `make`、自定义脚本 | 拒绝或 LLM 审批 |

- `autoDecide.<级别>`（五级均生效）：`allow` / `deny` / `llm`（或留空 = LLM 审批）
- `llmApproval.enabled`：启用后未在 autoDecide 里明确 allow/deny 的
  system/network/unknown 命令交给模型审批（返回 `{"allow": bool, "reason": "…"}`，
  审批失败一律拒绝）；`llmApproval.model` 可单独指定审批模型
- `humanOnly`：匹配的命令永远要求人工——无人值守时直接拒绝，不交给 LLM

**防绕过机制**（内置，不可关闭）：命令按 `;` `&&` `||` `|` `$()` 反引号分段、
每段独立决策取最严；`sh -c` / `bash -c` / `eval` / `source` 递归提取内层命令
（深度上限 4）；`rm` 递归+强制指向 `/`、`~`、顶层目录一律硬拒（用户规则无法放行）；
curl/wget 归 network 级；LLM 审批的待审批命令用 `<command>` 围栏包裹防注入。

### 4.6 mesh.json —— mesh 网络配置
<!-- module: mesh-json | mesh 网络配置：token、relay 中继、转发规则、远端任务分级、cwds 白名单 -->

`~/.doge/agent/mesh.json`（`doge mesh new` 生成；没有该文件时 `--mesh` 报错提示先建网）：

```json
{
  "token": "<base64，网络凭据>",
  "name": "laptop-a",
  "roles": ["general", "researcher"],
  "allowRemoteTasks": false,
  "remoteTaskLevel": "readonly",
  "allowRemoteTools": true,
  "allowRemoteFiles": false,
  "fileMaxBytes": 268435456,
  "privateNetwork": false,
  "relay": false,
  "listenPort": 4001,
  "bootstrapPeers": [],
  "cwds": { "projA": "/Users/me/workspace/projA" },
  "forwards": [
    { "name": "docker-daemon", "mode": "expose", "local": "unix:///var/run/docker.sock", "enabled": true },
    { "name": "remote-docker", "mode": "listen", "bind": "127.0.0.1", "port": 2375, "peer": "12D3Koo...", "remote": "docker-daemon", "enabled": true }
  ]
}
```

- `token`：网络凭据（base64 JSON：`rendezvous` + `psk`，可内嵌 `bootstrapPeers`）。
  同一 token 的节点属于同一网络；`mesh new` 生成后线下共享
- `name` / `roles`：节点名 / 能力角色（广播给对端；`name` 可作为远端调用的
  peer 别名，重名会被拒绝并要求用 peer ID）
- `allowRemoteTasks`：是否接受远端 agent 任务（`remote_agent`）
- `remoteTaskLevel`：远端任务权限分级 `readonly`（默认）/ `write` / `full`
- `allowRemoteTools`：是否接受 `remote_tool` 直接调用本机节点级工具
  （mesh_forward/mesh_peers/remote_agent_status，不经远端 agent；**默认开**，
  显式写 `false` 关闭，`trustedPeers` 可收窄）
- `allowRemoteFiles`：是否接受 trusted peer 经 `/doge/file/0.1` 收发文件
  （`mesh_file` 工具与 remote_agent 的 files 参数；**默认关**）。收到的文件
  只进上传暂存区 `~/.doge/agent/uploads`，绝不直达文件系统；`fileMaxBytes`
  限单文件上限（默认 256MiB）
- `privateNetwork`：私有/VPN 模式（libp2p 传输层 PSK；网络内所有节点须同模式）
- `relay`：本节点作为中继（circuit v2 hop，供 NAT 后节点互通；种子节点开启）
- `listenPort`：固定监听端口（`0`=每次随机；种子节点应设固定值，配合 `mesh-key`
  让 token 内嵌地址在重启后依然有效）
- `bootstrapPeers`：非空用自建种子、为空用公共 DHT（token 内嵌种子也生效）
- `cwds`：声明本机可达项目目录（远端任务的 cwd 白名单）
- `forwards`：声明式转发规则，每 3 秒自动热重载，`enable/disable` 可热启停
- NAT 穿透（端口映射/自动中继/打洞）默认启用，无需配置；两侧都在 NAT 后时经
  开了 `relay: true` 的种子中继互通

节点身份保存在**独立文件 `mesh-key`**（0600），与 `mesh.json` 分开，**不要随
mesh.json 复制到别的机器**。`trustedPeers`（同时约束远端任务、直接工具调用与
文件传输）、`remoteTaskLevel`、`maxConcurrentTasks` 按需配置。

### 4.7 skills / prompts —— 技能与模板（文件系统即配置）
<!-- module: skills-prompts | 技能（SKILL.md）与提示词模板的目录约定、frontmatter 格式、调用方式 -->

技能（SKILL.md，agentskills.io 格式）与 prompt 模板都是**纯文件配置**，增删无需
重启，天然适合 git 管理：

- 发现目录：`~/.doge/agent/skills` 与 `<项目>/.doge/skills`（`--skill <dir>` 可整体
  替换）；子目录或直接放置的 `SKILL.md` 均被识别。
- 模板目录：`~/.doge/agent/prompts` 与 `<项目>/.doge/prompts`（`--prompts <dir>`
  替换默认）。
- 模型侧通过三条路径使用技能（渐进式披露）：
  - **发现**：system prompt 只列技能 name + description（`<available_skills>` 块）
  - **加载**：`skill` 工具读取 SKILL.md 正文后照着执行（非交互模型路径）
  - **显式**：用户输入 `/skill:name` 直接注入正文；模型/用户发出的
    `<skill name location/>` 调用块也会被 harness 识别并回填
- 管理 API：`GET /api/skills`、`GET /api/skills/{name}`、`GET /api/session/skills`；
  Web 控制台状态栏有技能面板。设计细节见 `docs/skills-design.md`，API 规范见
  `docs/web-api.md` §4.12。

### 4.8 会话、日志与 Web token
<!-- module: session-log-web | 会话文件位置、日志滚动、web token 文件 -->

- **会话**：`~/.doge/agent/sessions/` 下 append-only JSONL 会话树，支持分支恢复与
  LLM 摘要压缩（compaction）。`DOGE_CODING_AGENT_SESSION_DIR` 可改目录。
- **日志**：诊断日志（agent/ai/permission/mesh/web 事件）写入
  `~/.doge/agent/logs/doge.log`，按天滚动；`--log-level` / settings `logLevel`
  控制级别，`DOGE_DEBUG` 同时输出到 stderr。
- **Web token**：每次启动随机生成 32 字节 token（`crypto/rand`），打印在控制台并
  写入 `~/.doge/agent/web-token`（0600，重启即失效）；`--web-token` /
  settings `web.token` 可固定。凭证三通道：`Authorization: Bearer <token>`
  （客户端/独立部署 WebUI 推荐）、`?token=`（浏览器 WS 自动登录）、登录换发的
  HttpOnly + SameSite=Strict 会话 cookie（7 天）。

### 4.8 AGENTS.md —— 全局指令与项目上下文
<!-- module: agents-md | 两层上下文文件：全局角色设定/环境定义 + 项目工作约定，注入系统提示词 -->

系统提示词里的上下文文件分两层，都随 `/reload` 热重读，不存在则跳过：

| 层 | 文件 | 用途 |
|---|---|---|
| 全局 | `~/.doge/agent/AGENTS.md` | 跨项目的角色设定、环境定义、回复偏好（语言、语气、默认习惯）——对应其它工具的全局 CLAUDE.md / soul.md |
| 项目 | `<项目>/AGENTS.md`（或 `CLAUDE.md`，先到先得） | 本仓库的工作约定（构建命令、代码风格、注意事项） |

注入顺序：内置操作规则 → `<user_instructions>`（全局）→ `<project_context>`（项目）
→ 技能清单 → cwd/平台/日期。全局在前、项目在后，项目的细节约定可以覆盖全局。
`--system-prompt` 是另一条路：整体替换内置提示词（连操作规则与工具清单一起去掉），
只适合实验性场景，日常分层配置用这两层文件。

## 5. TUI 命令速查
<!-- module: tui | TUI 斜杠命令与快捷键速查 -->

| 命令 | 说明 |
|---|---|
| `/` | 命令菜单（边输边过滤，Tab 补全） |
| `/new` | 立即开始全新会话 |
| `/skill:name` | 显式注入技能正文 |
| `/model <provider/model[:thinking]>` | 切换模型 |
| `/sessions` | 历史会话选择器（Enter 打开、d 按两次删除、Esc 关闭） |
| `/open <id>` | 按 id 切换会话 |
| `/stats` | token 统计覆盖层（消息数、输入/输出/推理、缓存命中、花费） |
| `/compact` | 手动触发上下文压缩 |
| `/reload` | 重读 models.json / auth.json / permission.json + 热刷新技能、重读 AGENTS.md 上下文（无需重启） |
| `/mcp`、`/mcp status` | MCP 面板：连接状态、工具数、缓存/错误 |
| `/mcp tools [server]` | 列出工具 |
| `/mcp reconnect [server]` | 立即重连（空=全部），刷新工具缓存 |
| `/mcp enable\|disable <server>` | 启用/禁用（写项目 `.doge/mcp.json`） |
| `/mcp setup` | 生成项目 `.mcp.json` 脚手架并重新发现 |
| `/mesh` | mesh 面板 |
| `/exit`、`/quit` | 退出 |
| `/help` | 帮助 |

快捷键：`↑/↓` 输入历史、`esc` 中断、`ctrl+c` 清空输入（空输入退出）、`ctrl+d`
退出、`ctrl+y` 复制最后回复（OSC 52）、`ctrl+t` 切换思考显示、`ctrl+o` 切换工具
输出。