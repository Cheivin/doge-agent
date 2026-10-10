# doge 文件上传与传输设计（Web 客户端 / WebUI / mesh）

关联：`web-api.md`（Web API 规范）、`mesh-design.md` §5.5（config-push 预留）、
`skills-design.md`（skills 目录发现）、`mcp-design.md`（配置 reload 分发先例）。

## 1. 目标与场景（需求细化）

一句话：**任何来源（网页、客户端、mesh 对端）都能把文件交到 doge 手里，
doge 既能把它当对话素材解析，也能确定性地放置/应用，还能跨节点搬运。**

拆成五个具体场景：

| # | 场景 | 例子 | 主路径 |
|---|---|---|---|
| S1 | **作为对话输入，让 agent 解析** | 网页拖一张架构图问"这设计有什么问题"；客户端推一份线上日志让 agent 排障 | 附件注入对话（§5） |
| S2 | **放置文件** | 发一个证书放到项目 `deploy/` 下；发一个脚本放到 `~/bin` | agent 自主放置 或 显式 place API（§4.4） |
| S3 | **应用配置** | 粘贴一份新的 `mcpServers` 配置导入；换一份 `models.json`；装一个 skill 包 | 显式 apply API（§4.5）/ skills 安装路径 |
| S4 | **mesh 主→从：给远端任务递文件** | "把这个 CSV 发给节点 B，让它基于文件里的数据出报表" | mesh file 协议 + remote_agent 附件（§7） |
| S5 | **mesh 双向：能力对齐与产物回传** | 把 skill 包下发到从侧；远端任务产出文件拉回主侧 | mesh file 协议（§7），config-push 语义（§8） |

**主次关系**：S1 是主入口——"根据情况去解析/配置/放置"的"根据情况"就是 agent
自己判断，所以**默认路径是把文件作为附件交给 agent + 一条自然语言指令**，agent
用现有工具（read/write/edit/配置 API/mesh 工具）完成任务并照常过权限系统。
显式 place/apply API 是给"不要 LLM 参与、要确定性、要脚本化"的场景，是补充而非主路径。

## 2. 现状盘点（调查结论）

已就绪、可直接复用：

- **多模态消息管道全线贯通**：`ai.ContentBlock` 有 `image` 类型（`ImageBlock(data,
  mimeType)`，`internal/ai/types.go`）；`agent.Agent.Prompt(input any)` 接受
  `*ai.Message`/`[]*ai.Message`；`read` 工具读图片已返回 image 块
  （`internal/coding/tools/read.go` 的 `imageMime`）；Web 前端已能渲染 image 块。
  **缺的只是 Web API 入口**。
- **鉴权零成本复用**：`requireAuth` 的 Bearer/`?token=`/cookie 三通道对任何新
  `/api/*` 路由同样适用（`internal/web/auth.go`）。
- **配置"写盘 → reload → 分发"先例**：MCP 的 `UpsertServers` → `/api/mcp/reload`
  → `redistributeMcp()` 分发到所有活跃会话，是 S3 的完整参照。
- **mesh 侧**：`/doge/forward/<name>/0.1` 已证明"独立 stream 承载无 framing 字节流"
  的模式（`forward.go`）；relay 已 `WithInfiniteLimits()`；PSK 握手 + stream
  RemotePeer 已提供来源认证；`config-push`/`config-ack` 消息类型与
  `AllowRemoteConfig` 开关已预留（mesh-design §5.5，未实现）。
- **`io.ReadAll(io.LimitReader(r.Body, 1<<20))`** 是现有唯一的受限读范式
  （`mcpapi.go`），可推广。

缺口：

- Web 侧**没有任何 multipart/上传代码**，全项目无人用 `http.MaxBytesReader`。
- **WebSocket 入站有隐藏地雷**：coder/websocket 默认读上限 **32768 字节**，
  `hub.go` 里的 `maxInbound = 1<<20` 是从未生效的死代码，`SetReadLimit` 从未调用。
  （web-api.md §6.1 声称"入站上限 1 MiB"与实际不符，需顺手修正。）
- 会话两层签名是纯文本：`web.Backend.Prompt(text string)` 与
  `AgentSession.Prompt(text string)`，携带不了附件。
- mesh 侧**没有任何文件/二进制传输机制**：Envelope 帧上限 16 MiB
  （`maxFrameBytes`），远端任务结果 256 KiB 硬截断；`Mesh.Send` 接口注释明确
  "大 payload 必须走独立 stream"。
- skills 安装没有写入口径（只能手动放目录），且 `s.skills` 在会话创建时固化，
  无热刷新。

## 3. 总体设计：统一上传暂存区（Upload Store）

**所有来源的文件先进同一个暂存区，后续三种用途（进对话/放置/应用）从暂存区取，
与来源解耦。**

```
HTTP POST /api/uploads ──┐
mesh /doge/file 收包   ──┼──→ ~/.doge/agent/uploads/<uploadID>/<name>
(未来 CLI 粘贴等)      ──┘         │
                                  ├─ S1: prompt attachments → 注入对话
                                  ├─ S2: place API / agent write 工具
                                  ├─ S3: apply API（校验→diff→确认→reload）
                                  └─ S4/S5: mesh_file 工具转发到对端暂存区
```

`internal/coding/uploads/`（新包，无 build tag，与 mesh 构建正交）：

```go
type Meta struct {
    ID        string    // 32 字节 hex，即存储目录名
    Name      string    // 清洗后的原始文件名（filepath.Base + 控制字符剥离 + ≤255 字节）
    Size      int64
    MIME      string    // http.DetectContentType 嗅探结果；客户端声明仅作参考
    SHA256    string    // 全量哈希，mesh 校验与去重依据
    Source    string    // "web" | "mesh:<peerID>"
    Window    string    // web 上传时的窗口 ID（可空）
    CreatedAt time.Time
}

type Store struct { ... }          // dir 由 AgentDir()/uploads 解析
func (s *Store) Save(src io.Reader, meta Meta) (*Meta, error)   // 流式落盘+哈希
func (s *Store) Get(id string) (*Meta, os.File)                 // 元数据+内容
func (s *Store) List() []Meta
func (s *Store) Delete(id string) error
func (s *Store) Sweep() (removed int, err error)                // TTL + 配额清理
```

存储规则：内容写 `uploads/<id>/<name>`，元数据写 `uploads/<id>/meta.json`；
`Save` 边写边算 sha256，超过单文件上限立即截断报错；`Sweep` 由 web server 起
定时任务调用（每小时），策略见 §9。

> 会话 JSONL 里**只存 uploadID + 元数据摘要**，不内嵌文件内容——图片除外
> （多模态必需，见 §5），避免会话文件膨胀。

## 4. Web HTTP API

路由注册沿用 `Server.handler()` 的 `mux.Handle("POST /api/...", s.requireAuth(...))`
风格，全部走既有鉴权。

### 4.1 上传

```
POST /api/uploads
Content-Type: multipart/form-data
字段: file（可重复，多文件一次请求）；可选 text（随附说明，≤4KB）

200 {
  "uploads": [
    { "id": "a1b2...", "name": "arch.png", "size": 183421,
      "mimeType": "image/png", "sha256": "9f86...", "createdAt": 1786000000000 }
  ]
}
```

- 服务端显式设 `http.MaxBytesReader(w, r.Body, settings.uploads.maxRequestBytes)`
  （默认 100 MiB，见 §9），超限 `413 {"error":"request too large"}`。
- 非 multipart 或缺 file 字段 → `400`。
- 每文件独立校验单文件上限与清洗文件名；单文件失败不影响同请求其余文件
  （逐项返回 `{"error": "..."}`）。
- **不走 WebSocket**：WS 入站 32 KiB 默认上限且无分帧协议；multipart 是客户端
  生态最通用的口径。顺手修复：`hub.go` 真正调用 `conn.SetReadLimit(maxInbound)`
  并同步 web-api.md §6.1 的描述（独立小改，随 P1 落地）。

### 4.2 查询 / 下载 / 删除

```
GET    /api/uploads                 → {"uploads": [Meta...]}
GET    /api/uploads/{id}            → Meta（含 path 提示）
GET    /api/uploads/{id}/content    → 文件字节（Content-Type=嗅探值，支持 Range 可后补）
DELETE /api/uploads/{id}            → {"ok":true}
```

`content` 端点供 WebUI 缩略图/预览与客户端回取；响应头带
`Content-Disposition: inline; filename="<name>"`。

### 4.3 附件发送（S1 主路径）

扩展现有 prompt 接口而非另开新路由：

```
POST /api/prompt
{ "text": "看下这张架构图", "attachments": ["a1b2..."] }
→ 202 {"ok":true}
```

- `attachments` 可选，元素为 uploadID；ID 不存在 → `400 {"error":"unknown attachment: ..."}`
- 语义：上传与发送**两步分离**——先 `POST /api/uploads` 再 `POST /api/prompt`。
  好处：客户端可重发 prompt 而不必重传文件；mesh/本地共用同一批文件；失败重试简单。
- 空文本 + 有附件是合法请求（"看看这个文件"）。
- 附件随消息进入会话后，`GET /api/session` 的 Message 里 user 消息 content 为
  数组形态（text 块 + image 块 / 引用 text 块），前端已兼容数组渲染。

### 4.4 放置（S2 确定性路径）

```
POST /api/uploads/{id}/place
{ "dest": "deploy/server.crt", "overwrite": false }
→ 200 {"ok":true,"path":"/Users/me/proj/deploy/server.crt"}
```

- `dest` 为相对路径时相对**当前会话 cwd**；支持的前缀根白名单（EvalSymlinks
  之后校验，防 symlink 逃逸）：
  1. 当前会话 cwd 之下；
  2. `~/.doge/agent/skills/` 之下（装 skill 包时用）；
  3. `~/.doge/agent/` 下的白名单文件（`models.json`、`mcp.json`，见 §4.5）。
- 越界 → `400 {"error":"destination outside allowed roots"}`；`overwrite:false`
  且目标存在 → `409`。
- 该 API 是 **harness 直写**，不经 agent、不经 permissions——守卫就是上面的
  白名单 + web token。若希望"agent 决定放哪"，用 §4.3 附件路径让 agent 自己
  `write`（照常过权限询问），两条路并存。

### 4.5 应用配置（S3 确定性路径）

```
POST /api/uploads/{id}/apply
{ "kind": "mcp" }                    // mcp | models | skills-zip | permissions
→ 200 { "ok":true, "dryRun":true, "diff": "..." }   // 第一步：校验+预览
POST /api/uploads/{id}/apply
{ "kind": "mcp", "confirm": true }   // 第二步：确认写入+触发 reload
→ 200 { "ok":true, "reloaded":true }
```

- 两段式（dry-run → confirm）是硬约束：kind 对应的解析器先验证文件合法性并
  生成 diff（文本 diff 或结构化摘要），`confirm:true` 才落盘。
- 各 kind 行为：
  - `mcp`：按社区 `mcpServers` 格式解析 → 字段级合并进项目 `.doge/mcp.json`
    （复用 `UpsertServers` 逻辑）→ 自动触发 `/api/mcp/reload` 等价动作。
  - `models`：解析校验后写入 `~/.doge/agent/models.json`（整体替换，替换前
    备份为 `models.json.bak`）→ 触发 `/api/reload`。
  - `skills-zip`：必须是 zip → 解包逐项检查（见 §10）→ 展开到
    `~/.doge/agent/skills/<name>/` → 提示"新会话生效"（若做了 §5.5 的技能
    热刷新则即时生效）。
  - `permissions`：**本地 web 面板允许**（token 鉴权边界内），但只做整体替换 +
    备份 + reload，且 dry-run 阶段展示策略摘要（新 deny/新 allow 计数）。
- `auth.json`、`mesh.json`、`settings.json` **不提供** apply（前者是凭据、后两者
  已有专门管理接口），需要时走 place 到 cwd + agent 处理。

## 5. 会话层：附件注入策略与签名扩展

### 5.1 注入策略（按类型分派）

`AgentSession` 把附件转成 `ai.Message` 内容块，规则：

| 附件类型 | 处理 | 理由 |
|---|---|---|
| 图片（嗅探 image/*）且模型 `SupportsImages()` 且解码后 ≤ 5 MiB | `ImageBlock(data, mime)` 直接进 user 消息 | 多模态现成管道 |
| 图片但不满足上一条 | 引用行（见下）| 防 base64 撑爆会话 JSONL 与上下文 |
| 文本类（UTF-8 合法）且 ≤ 64 KiB | 全文注入 text 块：`[文件 <name>, <size>B]\n<内容>` | 小文本直接给模型，省一次工具往返 |
| 其余（大二进制/未知/zip 等） | 引用行 | agent 按需 `read`/`bash` 处理 |

引用行格式（作为 text 块附在用户消息尾部）：

```
[附件 1/2] /Users/me/.doge/agent/uploads/a1b2.../data.csv
  size=183421 mime=text/csv sha256=9f86d0… 可用 read 工具读取
```

图片超限降级时引用行额外提示"图片过大，未直接注入；可要求缩放后再看"。

### 5.2 签名扩展（不破坏现有调用方）

```go
// coding.AgentSession（agent_session.go）
type Attachment struct { Path, Name, MIME string; Size int64; SHA256 string }
func (s *AgentSession) PromptWithAttachments(text string, atts []Attachment) error

// web.Backend / sessionBackend（backend.go）
Prompt(text string) error                              // 保持不动（TUI 等调用方零改动）
PromptWithAttachments(text string, ids []string) error // 新增：查 Store → 分派 → 落 AgentSession
```

`PromptWithAttachments` 内部组 `ai.Message{Role: user, Content: [TextBlock, ImageBlock...]}`,
走既有 `Agent.Prompt(any)`；steering（运行中追加）语义与 `Prompt` 一致。

## 6. WebUI 前端（内置 SPA）

- 输入框：📎 按钮（文件选择）+ 拖拽到输入区 + 粘贴截图（`paste` 事件抓
  `clipboardData.files`）。
- 上传中的条目显示为待发附件 chip（进度条可后补，fetch 无流式进度时显示 spinner）；
  发送时随 prompt 带出 `attachments`。
- 已发送附件在转录里按类型渲染：图片直接 `<img>`（`/api/uploads/{id}/content`），
  其他显示文件名 + 大小 chip（点击下载）。
- 前端单文件上限先用固定 100 MiB 校验提示，超限不再发起请求。

## 7. mesh 文件传输（S4/S5）

### 7.1 协议：`/doge/file/0.1`（独立 stream，不复用 Envelope 通道）

沿用 `forward.go` 的先例：**独立协议 stream + JSON 头 + 裸字节流**，帧读写复用
`stream.go` 的 `writeFrame/readFrame`（长度前缀，头帧远小于 16 MiB 限制）：

```
主侧开 stream → 从侧 handler:
→ frame(FileHeader)        # JSON：{id, name, size, sha256, mode, mimeType,
                           #       purpose: "task-file"|"place"|"pull",
                           #       cwd?: 别名, replyTask?: taskID}
→ <size 字节原始数据>       # io.Copy 到 uploads 临时文件，边收边哈希
← frame(FileAck)           # {id, ok, uploadId?, code?, error?}
                           # code: denied | too-large | quota | bad-hash | bad-cwd | busy
```

- 主侧 `Send` 每文件一 stream（与现有 Envelope 的每消息一 stream 一致，不引连接池）。
- 校验链（从侧）：peerTrusted（既有）→ `allowRemoteFiles` 开关（新，默认 false）
  → `size ≤ fileMaxBytes`（mesh.json，默认 256 MiB）→ uploads 磁盘配额 →
  sha256 比对（不匹配删除半成品并回 `bad-hash`）→ 落入本节点 Upload Store
  （`Source: "mesh:<peerID>"`）。
- **落点只有暂存区**，`purpose=place` 也只是把目标路径记入 meta——是否真的写入
  cwd 由接收侧 agent/用户决定（对齐"文件不直达文件系统"的安全基线，与 §4.4
  的显式白名单不同：远端来的一律多一道人/agent 确认）。
- 取消/中断：stream 重置即中止，半成品文件由 `Sweep` 清理；不做断点续传（P4 再议）。

### 7.2 agent 工具：`mesh_file`

master 侧注入（`cmd/doge/mesh_mesh.go`，与 `mesh_peers` 等同级）：

```
mesh_file(action=send, peer=<id>, upload=<uploadID|本地路径>, purpose=task-file|place, cwd=<别名>)
mesh_file(action=list, peer=<id>)        # 对端暂存区里本 peer 发来的文件
mesh_file(action=pull, peer=<id>, upload=<对端 uploadID>)   # S5 产物回传：从对端暂存区拉回
```

- `send` 本地路径源同样先入本节点 Store 再发（统一哈希/配额/审计口径）。
- `pull` 复用同一协议，方向反转（`purpose:"pull"`，从侧校验请求方对该文件的
  归属——`Source` 是它自己——才发）。

### 7.3 与 remote_agent 的结合（S4 闭环）

`remote_agent` 工具增加可选参数 `files: ["<uploadID>"...]`：

1. master 对每个文件先走 `mesh_file send(purpose=task-file)`，拿到对端 uploadId；
2. `TaskPayload` 增加 `Files []RemoteFileRef{UploadID, Name, Size, SHA256}`；
3. 从侧 `runTask` fork 会话后，按 §5.1 的引用行规则把文件注入 fork 会话首条
   上下文（文件本体留在从侧暂存区）；
4. 任务产出若需回主侧，从侧 agent 无法主动 push（默认 deny 下收不了）——
   由 master 侧 agent 在收到结果后用 `mesh_file pull` 拉取，从侧在
   `ResultPayload` 里回报产物文件的 uploadId 列表（新增 `Files` 字段）。

### 7.4 权限与配置（mesh.json 增量）

```json
{
  "allowRemoteFiles": false,
  "fileMaxBytes": 268435456,
  "trustedPeers": ["..."]
}
```

- 默认 deny + TrustedPeers 白名单，与 `allowRemoteTasks` 同构；
- `remote_agent files` 参数额外受 `RemoteTaskLevel ≥ write` 约束（readonly 任务
  不该带文件副作用）；
- headless 从侧同样可收（daemon 已接受直接工具调用），web 面板 `GET /api/mesh`
  的 config 快照自然带出新字段。

## 8. 与 config-push（mesh-design §5.5）的关系

§5.5 预留的 `config-push` 是**声明式配置下发**（skills/models/mcp 四类白名单、
默认 deny、防提权），定位是"让从侧能力对齐"。本设计的 file 协议是**通用文件
搬运**。两者保留为两个机制，但共享底座：

- config-push 实现时，其 `Files` 字段的落盘复用 Upload Store 与 §10 的 zip/路径
  校验代码；
- 白名单规则不放松：`permission.json`/`auth.json`/`mesh.json` 在任何远端来向
  （file 协议的 place 或 config-push）都**不可直达**；
- 时序上 file 协议（P3）先行，config-push 维持预留（P4+ 视需求启动）。

## 9. 配置与设置项

`settings.json` 新增 `uploads` 节（全局，`internal/coding/settings.go`）：

```json
{
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

- 未配置全部取上述默认值；`quotaBytes` 是 Upload Store 总量上限，`Sweep` 超额时
  按最旧先删（mesh 来源的文件同样参与淘汰，meta 里有来源可追溯）。
- `GET /api/config` 的 `paths` 增加 `"uploads": "~/.doge/agent/uploads"`。

## 10. 安全设计

| 风险 | 对策 |
|---|---|
| 路径穿越（文件名/dest） | 文件名 `filepath.Base` + 控制字符剥离；dest 白名单根 + `EvalSymlinks` 后复核；zip 解包逐项拒绝 `..`、绝对路径、symlink 条目与超长路径（zip-slip） |
| 请求体炸弹 | `http.MaxBytesReader`（默认 100 MiB）；multipart 逐文件上限；流式落盘不整体驻留内存 |
| 伪装类型 | MIME 以 `http.DetectContentType` 嗅探为准，客户端声明仅存 meta 供参考 |
| 上传内容触发执行 | 文件只是数据：agent 后续 read/bash 照常过 permissions；place/apply 不执行任何内容 |
| 配置提权 | apply 双段确认 + 备份；`permissions` 仅本地 token 边界内；`auth.json`/`mesh.json`/`settings.json` 无 apply |
| mesh 远端写文件系统 | 远端来文件一律只进暂存区，`allowRemoteFiles` 默认 deny + TrustedPeers；sha256 校验；单文件与配额上限 |
| 会话膨胀 | 引用行注入为大文件默认；图片内联有 `maxImageInlineBytes` 上限 |
| 磁盘耗尽 | `quotaBytes` + 定时 `Sweep`（TTL + 配额） |
| 磁盘剩余空间 | `Save` 前检查 uploads 卷可用空间低于 1 GiB 时拒绝新上传 |

## 11. 分期实施

### P1：Web 上传 + 对话附件（S1 完整闭环）

- 新增 `internal/coding/uploads/`（Store + Sweep + 测试）。
- Web：`POST/GET/DELETE /api/uploads*`、`POST /api/prompt` 扩展 attachments、
  `http.MaxBytesReader`；settings `uploads` 节。
- 会话：`PromptWithAttachments` 两层签名 + §5.1 分派逻辑。
- 前端：📎/拖拽/粘贴上传、附件 chip、转录渲染（图片直显/文件 chip）。
- 顺手修：`hub.go` 真正 `SetReadLimit(maxInbound)`，修正 web-api.md §6.1。
- 验收：拖图→prompt→模型看图作答全链路；20 MB 日志附件走引用行且 agent 能
  `read` 分析；超 100 MiB 上传得到 413；web-api.md 增补 §4.x 与新事件说明。

### P2：放置与配置应用（S2/S3 确定性路径）

- `POST /api/uploads/{id}/place`（白名单根 + symlink 复核）。
- `POST /api/uploads/{id}/apply`（mcp/models/skills-zip/permissions 四 kind，
  双段确认，mcp 接 `UpsertServers` + reload 分发）。
- 技能热刷新（建议随本批）：`ReloadConfig` 增加重扫 skills 并对活跃会话
  `SetTools + SetSystemPrompt` 重建（`addSessionTools` 已有重建入口）。
- 验收：上传 `mcpServers` JSON → dry-run 见 diff → confirm 后新工具出现在活跃
  会话；上传 skill zip → 新会话可见（做了热刷新则即时可见）。

### P3：mesh 文件传输（S4/S5）

- `internal/mesh/libp2p/file.go`：`/doge/file/0.1` handler + 发送端；envelope 无
  新消息类型（ack 走 file stream 自身帧）。
- `mesh_file` 工具（send/list/pull）；`remote_agent` 的 `files` 参数与
  `TaskPayload.Files`、`ResultPayload.Files`；从侧注入 fork 会话。
- mesh.json 三字段；`GET /api/mesh` config 快照带出。
- 验收（`-tags mesh`）：`remote_agent(files=[csv])` 远端能 read 到文件并出结果；
  非 TrustedPeers 发送得到 denied；sha256 错配得到 bad-hash 且无残留文件；
  256 MiB 文件经中继链路可传。

### P4（可选增强，另行立项）

- 断点续传（Range/分块协议）、上传进度事件（WS `upload_progress`）、
  uploads 管理面板、config-push 正式实现（§8）、转发通道压缩。

## 12. 实施记录

### P1（已完成）- `internal/coding/uploads/`：Store（Save/Get/Meta/List/Delete/Sweep，流式落盘
  + sha256、文件名清洗、hex ID 防穿越、Statfs 磁盘余量检查）+ 单测。
- `internal/coding/settings.go`：`UploadSettings` 节（零值默认）；
  `UploadsDir()` 进 `EnsureAgentDir`。
- `internal/coding/attachments.go`：`Attachment`/`AttachmentFromMeta` +
  `AgentSession.PromptWithAttachments`（steering 语义与 Prompt 一致）；
  分派：图片多模态（模型支持且 ≤5MiB）→ `ImageBlock`，文本 ≤64KiB →
  全文注入，其余 → 引用行（含 path/size/mime/sha256/read 提示）。
  `AgentSession` 不持有 Store——附件自带路径，web/mesh 来源解耦。
- `internal/web/`：`POST/GET /api/uploads`、`GET /api/uploads/{id}`、
  `GET .../content`（inline 下载）、`DELETE`；multipart 逐文件独立成败；
  `http.MaxBytesReader`（超限 413）；`POST /api/prompt` 增加 `attachments`
  （未知 ID 同步 400）；`Backend.PromptWithAttachments` 接口方法；
  `Start/Shutdown` 挂每小时 Sweep；`/api/config` paths 增 `uploads`。
- 前端：📎 选择/输入框粘贴/拖拽上传、待发附件 chips（可移除）、发送携带
  attachments；转录渲染 image 块直显 + `[附件]` 引用行渲染为可下载文件卡片。
- 修复：`hub.go` 此前从未调用的 `SetReadLimit(maxInbound)`（coder/websocket
  默认 32 KiB，文档承诺的 1 MiB 此前并不生效）。
- 文档：web-api.md v5（§4.13 + prompt attachments + 413 + 清单）、guide.md
  settings-json 模块补 `uploads` 节。
- 验证：`go test ./...` 与 `-tags mesh` 全绿；headless smoke（上传 33B 文本
  → 全文注入、27MB 二进制 → 引用行，多块 user 消息落转录）。

### P2（已完成）

- 技能热刷新：`AgentSessionOptions.SkillsDirs`（fork 继承）+
  `AgentSession.reloadSkills()` 挂进 `ReloadConfig`——技能集变化时重建
  skill 工具与系统提示，新装技能免新建会话即时生效。
- zip 安全解包：`uploads.ExtractZip`（拒绝 zip-slip/绝对路径/symlink 条目、
  单文件与总量上限），P3 config-push 可复用。
- `POST /api/uploads/{id}/place`：白名单根（cwd 子树 / skills 目录 /
  models.json+mcp.json 精确文件）+ 逐级 EvalSymlinks 复核 + symlink 拒绝。
- `POST /api/uploads/{id}/apply`：mcp/models/skills-zip/permissions 四 kind
  双段确认（dry-run 出 diff → confirm 落盘 + reload）；mcp 接
  `UpsertServers` + reload 分发；models/permissions 整体替换前备份 `.bak`。
- 前端：Skills 面板「从 zip 安装」入口（上传 → dry-run 预览 → 确认安装）。
- 测试：apply 六用例（place 越界/冲突、models/skills/permissions/mcp 双段、
  未知 kind）、热刷新两向（装→工具出现、删→工具摘除）。

### P3（已完成）

- 协议：`/doge/file/0.1` 独立 stream（`libp2p/file.go`），三种 purpose——
  send（头帧含 size/sha256 → 裸字节 → FileAck）、pull（对端回头帧+字节）、
  list（回 fileList 帧）。发送端预读现算 sha256；接收端校验链：
  peerTrusted（`allowRemoteFiles` + trustedPeers，与 allowRemoteTasks 同构）
  → `fileMaxBytes`（默认 256MiB）→ sha256 核对（错配删半成品回 bad-hash）
  → 落本节点 Upload Store（`Source: "mesh:<peerID>"`）。
- `mesh_file` 工具（`libp2p/file_tool.go`）：stage（本地文件进暂存区，
  Source=agent）/ send / pull / list；master 本地会话与 full 级从侧 fork
  均注入。
- `remote_agent` 新增 `files` 参数：派发前逐个 SendFile 到对端暂存区，
  `TaskPayload.Files` 携带引用；从侧 `remoteTaskLevel=readonly` 时拒绝带
  文件任务；fork 后任务文本前注入与 web 一致的附件引用行。
- pull 归属校验：mesh 来的文件只回发给原发送方、agent 暂存的产物任何
  受信 peer 可拉、web 上传的文件不对 mesh 暴露。
- 配置：mesh.json 增 `allowRemoteFiles`（默认 false）/`fileMaxBytes`；
  `mesh.Options.Uploads` 注入共享暂存区（main.go 统一创建，web/mesh 共用）。
- 任务产物回传链路：从侧 agent `mesh_file(stage)` 得到 uploadID 写进回复 →
  master agent `mesh_file(pull)` 拉回。
- 测试（`-tags mesh`）：send/pull 全链路、denied、bad-hash 无残留、
  too-large、list 归属过滤、readonly 任务带文件拒绝。

### 复审修复（随 P0/P1 一批）

- **暂存区清理挂 main/daemon**（不再挂 web server）：TUI-only / mesh-only
  模式 TTL 与配额同样生效；`Store.StartSweep(ctx)` 随进程。
- **`Store.Save` 配额写前校验**：现存总量达配额即拒（`ErrQuota`），mesh 路径
  回 ack code `quota`；并发写的窗口内可能超一个文件，Sweep 兜底压回。
- **zip 解包限流按实际字节**：单文件/总量上限不再信条目头声明的
  `UncompressedSize64`（可伪造），`io.LimitReader(max+1)` 探测超限，失败不
  留半成品。
- **单文件上限口径统一**：mesh 接收端按 `min(fileMaxBytes, uploads
  MaxFileBytes)` 前置校验，区间文件回 `too-large`；pull 同口径。
- **文件流 deadline**：收发两侧限期，对端挂流不再永久阻塞调用方。
- **headless daemon 接 uploads store**：设计声称可收文件，此前未接线。
- web `/api/uploads/{id}/content` 存储型 XSS 加固见 web-api.md §4.13
  （非图片强制 attachment + nosniff）。

## 13. 已定决策与开放问题

已定（本设计的默认取值，实施时不再复议；三个开放问题已拍板按
默认执行：上限 100MiB/256MiB 且续传留 P4、技能热刷新进 P2、permissions kind
保留）：

1. 上传与发送两步分离，`/api/prompt` 以 uploadID 引用附件。
2. 文件上传走 HTTP multipart，不走 WebSocket。
3. 所有来源统一先入 `~/.doge/agent/uploads` 暂存区，用途与来源解耦。
4. mesh 文件走独立 `/doge/file/0.1` stream，不经 Envelope；远端文件只进暂存区。
5. `allowRemoteFiles` 默认 deny，与 `allowRemoteTasks` 同构。
6. apply 双段确认（dry-run → confirm），敏感配置不提供 apply。
7. 会话只存引用；图片按上限内联，其余引用行注入。

开放问题（实施前需拍板）：

- **OQ1** 单文件/请求上限取值：本设计取 100 MiB（请求）/256 MiB（mesh 单文件）。
  若实际场景有视频/数据集类大文件需求，P1 就应该把分块/续传提到 P4 之前。
- **OQ2** 技能热刷新放 P2 还是推迟：不做热刷新则 skills-zip apply 后提示
  "新会话生效"，体验打折；做则动 `ReloadConfig`，改动面中等。
- **OQ3** `permissions` kind 是否保留：本地 token 边界内风险可控，但也可选择
  只开放 mcp/models/skills 三类，permissions 一律让用户手动改文件。
