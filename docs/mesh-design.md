# doge mesh 设计：自研轻量 P2P + 对等多 agent

状态：P0–W0–P3+P4 已实现（0.3.0–0.4.1，见 §11.12/§11.13）；协作层后续加固与
      稳定性修复至 0.7.1（见 §11.14）；连接自愈加固 + remote_tool/peer 别名
      见 §11.15；跨节点文件传输（`/doge/file/0.1` + `mesh_file`）见
      [file-transfer-design.md](./file-transfer-design.md) §7/§12（P3 已实施）；
      P5/P6/P7 经评估**关闭**（不再需要完整实现，现有能力已足够，见 §10 说明）
前置调研：edgevpn P2P 集成调研（结论：不引入 edgevpn 全量依赖，自研轻量
P2P 层；调研已归档，正文中"借鉴 edgevpn"均指其开源代码而非集成其库）
参考实现：[nicobailon/pi-subagents](https://github.com/nicobailon/pi-subagents)（单机 subagent 框架）

## 1. 目标

1. 自研轻量 P2P 层（只依赖 go-libp2p + kad-dht + mdns），不引入 edgevpn 全量依赖。
   DHT 第一阶段就上（可借鉴 edgevpn discovery 代码），不局限于局域网。
2. 双包：默认构建无网络（二进制不变），`-tags mesh` 启用网络能力。
3. 三个业务场景：节点间端口转发、agent 间消息协作、会话共享/同步。
   快照和订阅两种会话共享都做；订阅模式复用 web hub 逻辑后做。
4. **对等多 agent**：任意 doge 实例既可作主（派任务）又可作从（接受任务），
   非固定主从拓扑。参考 pi-subagents 的 subagent/intercom 模型，但把"子进程 fanout"
   换成"P2P stream"，把"固定主从"换成"对等 mesh"。
5. **agent 发现与能力感知**：节点间动态互相发现，并广播自己的环境、能力、可用工具/
   skills/MCP/model 等信息——类似增强版的 subagent 描述定义，方便任务派发时按能力路由。
6. **配置协作**：doge 可动态加载配置（auth/models/permission/skills/mcp），主 agent
   可协助配置协作侧（下发/同步 MCP、skills 等）。CLI 需增加配置命令和参数。
7. **工作目录隔离**：agent 可切换工作目录，且主侧可指定远端 agent 会话的工作目录，
   实现不同项目的隔离。

## 2. pi-subagents 模型摘要与对等化差异

pi-subagents（单机）的核心原语：

- **subagent 工具**：主 agent 调用 `subagent` 工具派任务，参数 action=run/list/get/status/interrupt/resume。
- **角色定义**：`agents/*.md`（delegate/oracle/planner/researcher/reviewer/scout/worker 等），每个有 system prompt、tools、skills。
- **fanout-child**：主进程派生子进程跑 subagent，隔离执行。
- **intercom**：subagent ↔ supervisor 双向通信：
  - `contact_supervisor({reason, message})`：subagent 主动问主（need_decision / progress_update）。
  - `intercom({action, to, message})`：底层通用消息。
  - 目标解析：`orchestratorTarget`（session name）。
- **chain**：多 step subagent 序列，每步指定 agent + task。
- **fork 上下文**：subagent 继承主的项目上下文（fresh / fork）。
- **结果交付**：通过文件 + 事件（result-intercom）。

对等化的关键改动：

| 维度 | pi-subagents | doge mesh（本设计） |
|---|---|---|
| 拓扑 | 固定主从（同进程内） | 对等 mesh，任意节点可主可从 |
| 传输 | 子进程 + 文件 + intercom 扩展 | P2P libp2p stream（双向字节流） |
| 角色发现 | 本地 `agents/*.md` | 网络内 peer 广播能力声明 |
| 上下文 | fork 主会话 | fork 远端会话快照，或 fresh |
| 结果 | 文件 + 事件 | stream 内 JSON envelope 回传 |
| 生命周期 | 主进程管理子进程 | 每个任务在从侧 fork 独立 AgentSession |

## 3. 总体架构

分层 + build tags 双包：

```
┌───────────────────────────────────────────────────────┐
│  cmd/doge  (main.go, flag 解析, 运行模式)              │
│    + mesh_stub.go  (!mesh)  : 空实现                   │
│    + mesh_mesh.go  (mesh)   : 构造 mesh + remote 工具  │
├───────────────────────────────────────────────────────┤
│  internal/mesh                                          │
│    mesh.go       (无 tag)  : Mesh 接口 + 类型           │
│    envelope.go   (无 tag)  : 消息信封 + 协议类型        │
│    stub.go       (!mesh)   : Mesh stub                 │
│    libp2p/       (mesh)     : libp2p 实现              │
│      node.go                : host、生命周期            │
│      discovery.go           : mDNS + DHT                │
│      stream.go              : /doge/* stream 协议       │
│      forward.go            : 端口转发                  │
│    agent/        (mesh)     : agent 协作层              │
│      bridge.go              : 主侧（派任务/收结果）     │
│      remote_tool.go         : remote_agent 工具        │
│      handler.go             : 从侧（接受任务/执行）     │
│      session.go             : 会话共享/订阅            │
├───────────────────────────────────────────────────────┤
│  internal/coding  (core，无 P2P 依赖)                   │
│    AgentSession.ForkNew/ForkSession ← 已有，复用        │
│    AgentSessionOptions.Tools    ← 注入 remote_agent     │
│  internal/agent  (AgentEvent ← 复用做事件流)            │
│  internal/web    (Backend 模式 ← 会话共享参考)          │
└───────────────────────────────────────────────────────┘
```

**核心原则**：`internal/coding` 和 `internal/agent` 永不 import libp2p。
mesh 层通过接口注入 core；core 只认接口和协议类型（envelope.go 无 tag）。

## 4. Mesh 层设计

### 4.1 接口（无 build tag，core 可见）

```go
// internal/mesh/mesh.go
type Mesh interface {
    Start(ctx context.Context) error
    Close() error

    ID() string                 // 本节点 PeerID
    Peers() []PeerInfo          // 已连接节点 + 能力声明
    Advertise(cap Capability)   // 广播本节点能力（如 "agent","forward:8080"）

    // 通信
    Send(peerID string, env Envelope) error
    Broadcast(env Envelope) error
    Subscribe(h EnvelopeHandler) (unsubscribe func())

    // 端口转发（独立子系，可单独重载，agent 可控）
    ForwardReload() error                                 // 重读 forwards 配置，重建监听/handler，不重启 host
    ForwardAdd(rule ForwardRule) error
    ForwardRemove(name string) error
    ForwardSetEnabled(name string, enabled bool) error    // 热启停单条
    ForwardList() []ForwardRule
}

type PeerInfo struct {
    ID      string
    Addrs   []string
    Caps    []Capability   // 该节点声明的能力
    Latency time.Duration
}

// ForwardRule 是一条端口转发规则（mesh.json forwards 的内存表示）。
type ForwardRule struct {
    Name    string `json:"name"`
    Mode    string `json:"mode"`    // "expose" | "listen"
    Local   string `json:"local"`  // expose: 要 dial 的本地服务（host:port 或 unix:///path）
    Bind    string `json:"bind"`   // listen: 绑定网卡（0.0.0.0/127.0.0.1/IP）
    Port    int    `json:"port"`   // listen: 监听端口
    Peer    string `json:"peer"`   // listen: 目标 peer ID
    Remote  string `json:"remote"` // listen: 对端 expose 的 service name
    Enabled bool   `json:"enabled"` // false=已停（热启停）
}
```

### 4.2 节点发现

- **局域网**：mDNS，service tag 从共享 token 派生，秒级发现。
- **公网**：DHT + rendezvous（rendezvous string 从 token 派生），第一阶段就上。
- **bootstrap 策略（三选一，`bootstrapPeers`）**：
  1. **默认公共 DHT**：`bootstrapPeers` 为空时用 libp2p 默认公网 bootstrap
     （可借鉴 edgevpn `pkg/discovery/dht.go`）——适合能直连公共基础设施的网络。
  2. **自建种子节点**（推荐，公共 DHT 不可达时的可靠替代）：任意**有公网 IP /
     固定 VPN IP** 的节点自身作为 bootstrap——它监听固定地址 + DHT `ModeServer`，
     其他节点把它加进 `bootstrapPeers` 后经它加入，形成**私有 DHT**，不依赖
     任何公共基础设施。bootstrap 节点无需特殊应用逻辑（它正常跑 mesh，别人连它）。
  3. **私有 VPN 内**：overlay 内一台固定 IP 节点作 bootstrap（配合
     `privateNetwork: true` 时兼容，bootstrap 同样持 PSK）。
- **bootstrap 地址嵌入 token（关键）**：`doge mesh new --bootstrap <multiaddr>...`
  把种子地址写进 token（网络级），新节点仅凭 token 就自动获得 bootstrap 列表——
  避免"先有地址才能加入"的鸡生蛋。token 结构见 §4.4。
- **动态感知**：节点上线/下线由 libp2p 连接通知驱动；定期重广播 profile 防止状态过期。

### 4.3 Agent 能力感知（AgentProfile）

每个节点广播一个 **AgentProfile**（类似 pi-subagents 的 `agents/*.md` 描述，但是对等、
动态、网络范围的）。主侧派任务前查 profile 选择目标；从侧据 profile 判断能否承接。

```go
// internal/mesh/envelope.go（无 tag，协议类型）
type AgentProfile struct {
    PeerID      string            `json:"peerId"`
    Name        string            `json:"name"`       // 人类可读名（可选）
    Roles       []string          `json:"roles"`      // 能力标签，如 "researcher","reviewer","general"
    Version     string            `json:"version"`     // doge 版本
    OS          string            `json:"os"`          // runtime.GOOS
    Arch        string            `json:"arch"`
    Model       string            `json:"model"`       // 当前默认 model spec
    Models      []string          `json:"models"`      // 可用 model 列表
    Tools       []string          `json:"tools"`       // 内置工具
    Skills      []string          `json:"skills"`      // 已加载 skill 名
    MCP         []MCPEndpoint     `json:"mcp"`         // 已配置 MCP server（名+工具数）
    Cwds        []string          `json:"cwds"`        // 可达项目工作目录（绝对路径或别名）
    Accepting   bool              `json:"accepting"`   // 是否接受远端任务
    Load        int               `json:"load"`        // 当前并发任务数 / 上限
    Tags        map[string]string `json:"tags"`        // 自由扩展标签
}
```

- **广播机制与握手时序**：节点间连接建立后**立即互推一次 profile**（握手），
  之后定期重推 + 变更时（skills/models/cwds 变化）即时重推 `MsgAdvertise`
  （payload=AgentProfile，小消息，逐已连 peer fan-out）。不依赖 pubsub。
  其它节点缓存到 `Peers()` 的 profile 字段，带时间戳防过期。
- **角色寻址**（决策2）：peer ID 是唯一身份，role 是能力标识。派任务可按 peer ID 直连，
  或按 role 路由到任一声明该 role 且 `Accepting && Load < 上限` 的节点
  （就近/负载均衡，mesh 内自选）。两者都支持。
- **配置变更触发重广播**：skills/models/cwds 变更后立即重发 profile，保证远端看到最新能力。
- **依赖注意**：`MCP` 字段为**预留**——doge 目前尚无 MCP 支持（内置工具仅
  read/write/edit/bash/grep/find/ls/code）。profile 构建时 MCP 恒为空，待 doge
  MCP 支持立项后启用；配置协作（§5.5）首发也只下发 skills/models。

### 4.4 token 与准入分层

仿 edgevpn 但简化：一个 base64 编码的 JSON，含 `rendezvous`、`psk`、可选的
`bootstrapPeers`（种子节点地址列表，`doge mesh new --bootstrap ...` 生成时嵌入）。
`doge mesh new` 生成；`doge --mesh-token <tok>` 加入。存 `~/.doge/agent/mesh.json`。

**准入分层（两种部署模式，网络级二选一）**：

- **公网模式（默认）**：
  - **传输加密**：libp2p TLS/noise 自动完成，无需配置。**不用** libp2p 的
    `PrivateNetwork`（PSK conn protection）——公网 DHT bootstrap 不持你的 PSK，
    开了就连不上公网发现。
  - **发现隐匿**：`rendezvous` 从 token 派生（随机串），DHT 上不可猜测。
  - **mesh 准入（应用层）**：`psk` 用于节点互验与内容保护——连接建立后双方首条
    消息带 PSK 派生的 HMAC 互验；advertise/service 注册记录用 PSK 加密。
    未持 token 的节点即使在 DHT 上撞到 rendezvous，也过不了互验、读不了记录。
    这正是 edgevpn OTP 的做法（DHT record 加密），照抄。
- **私有/VPN 模式（可选配置，`privateNetwork: true`）**：节点都在受信 overlay
  组网下（Tailscale / ZeroTier / WireGuard / 企业内网，通过 VPN IP 互达），
  不需要公网 DHT bootstrap：
  - **传输层 PSK 准入**：启用 libp2p `PrivateNetwork`（PSK conn protection），
    传输层直接加密整条连接 + 拒绝未持 PSK 的节点（比应用层 HMAC 更底层、更全面）。
    PSK 从 token 的 `psk` 派生 32 字节（`HMAC-SHA256(psk, "libp2p-private-network")`）。
  - **发现**：mDNS（overlay 支持组播时）或 `bootstrapPeers`（VPN IP 列表）
    或 VPN 内 DHT（自建 bootstrap 也持 PSK，兼容）。
  - **约束**：同一 token 网络内**所有节点必须同模式**（PrivateNetwork 传输层
    协商是二元的，混用连不上）；这是网络级配置，写进 mesh.json，网络内一致。
  - 应用层 HMAC 互验仍保留（纵深防御；PrivateNetwork 下可视为冗余）。

三层各司其职：传输层（TLS 或 PrivateNetwork-PSK）管加密与传输准入，
rendezvous/mDNS 管可发现性，应用层 HMAC 管"谁是自己人"（公网模式下是唯一准入）。

### 4.5 端口转发（声明式配置，支持 unix socket、动态启停、指定 bind）

借鉴 edgevpn `services` 思路自研，但改为**声明式配置**。转发规则写在 `mesh.json`
的 `forwards` 里，启动时按配置建立；运行中可经 `doge mesh forward` 增删、
`enable/disable` **单条规则热启停**（关闭监听 / 注销 stream handler，不影响其它规则）。

**方向语义**：一条端到端转发由两端各配一条规则组成——提供方 `expose` + 访问方 `listen`。

- `expose`（本节点是服务提供方）：`local` = 本地要 dial 的服务地址
  （如 `127.0.0.1:8080`、`unix:///var/run/docker.sock`）；注册 service `name` +
  `/doge/forward/0.1` stream handler，远端连来时 dial `local` 双向 `io.Copy`。
  这是 outbound dial，不涉及 bind host。
- `listen`（本节点是访问入口）：`local` = 本地监听地址，`bind` 决定绑哪个网卡
  （`0.0.0.0`=对外所有网卡、`127.0.0.1`=仅本机、或具体 IP）；accept 后开
  `/doge/forward/0.1` stream 到 `peer` 的 `remote` service（expose 端注册的 name），双向 `io.Copy`。

举例（对照用户需求）：
- "A 的 8080 → 访问 B 的 8090 == A 的 8080"：A 配 `expose local=127.0.0.1:8080 name=svcA`；
  B 配 `listen local bind=0.0.0.0 port=8090 peer=A remote=svcA`。
- "A 把 B 的 8091 转发到本地 8081"：B 配 `expose local=127.0.0.1:8091 name=svcB`；
  A 配 `listen local bind=127.0.0.1 port=8081 peer=B remote=svcB`。
- 远端 docker：A 配 `expose local=unix:///var/run/docker.sock name=docker`；
  本地 B 配 `listen local bind=127.0.0.1 port=2375 peer=A remote=docker` →
  `docker -H 127.0.0.1:2375` 即操作远端 docker。

**unix socket**：`local` 支持 `unix:///path`（解析成 network=unix）。expose 可 dial
一个 unix socket 发布；listen 也可在本地建 unix socket 监听。unix↔TCP 桥接时
`io.Copy` 不关心网络层，天然互通。

**安全警告**：`expose unix:///var/run/docker.sock` 等价于把本机 root 级控制权交给
mesh 内所有持 token 节点（docker 可挂载宿主任意路径）。仅在与完全可信的节点组网时
使用；生产建议在 expose 端加 peer 白名单（只允许指定 peer 连该 service）。

**动态启停**：每条规则带 `enabled`（默认 true）。`forward disable <name>` 停掉
该规则的监听 / handler；`forward enable <name>` 重新建立。无需重启 mesh。

**单独重载**：forward 是独立子系，`ForwardReload` 只重读 `mesh.json` 的 `forwards`
重建监听/handler——不重启 mesh host、不断开已建立的 peer 连接、不影响 agent 协作层。
改完配置（CLI 改或 agent 改）调一次 reload 即生效。

**agent 可控**：除 CLI 外，提供 `mesh_forward` 工具（action=list/add/remove/
enable/disable/reload，参数同 ForwardRule），让 agent 在执行任务时自行管理转发。
例如"帮我把远端 docker 拉到本地"，agent 调 `mesh_forward add`（mode=listen,
peer=X, remote=docker, bind=127.0.0.1, port=2375）→ `reload` → 验证 →
`docker -H 127.0.0.1:2375 ...`，全程不需用户切终端。CLI 与工具走同一套 Forward API。

## 5. Agent 协作层设计

### 5.1 消息信封（无 tag，协议定义）

```go
// internal/mesh/envelope.go
type Envelope struct {
    ID       string          `json:"id"`        // UUID
    Type     MsgType         `json:"type"`      // 见下
    From     string          `json:"from"`      // 发送方 PeerID
    To       string          `json:"to"`        // 接收方 PeerID（空=广播）
    ReplyTo  string          `json:"replyTo"`   // 关联的请求 ID
    Payload  json.RawMessage `json:"payload"`
}

type MsgType string
const (
    MsgTask         MsgType = "task"          // 主→从：执行任务
    MsgResult       MsgType = "result"        // 从→主：任务结果
    MsgQuestion     MsgType = "question"      // 从→主：需要决策（≈contact_supervisor）
    MsgAnswer       MsgType = "answer"        // 主→从：回答
    MsgProgress     MsgType = "progress"      // 从→主：进展更新
    MsgSubscribe    MsgType = "subscribe"     // 主→从：订阅会话事件流
    MsgEvent        MsgType = "event"         // 从→主：会话事件（agent 事件流）
    MsgSessionSnap  MsgType = "session-snap"  // 会话快照（请求或推送）
    MsgAdvertise    MsgType = "advertise"     // 广播 AgentProfile
    MsgInterrupt    MsgType = "interrupt"     // 主→从：中断任务
    MsgConfigPush   MsgType = "config-push"   // 主→从：下发配置（skills/models 等）
    MsgConfigAck    MsgType = "config-ack"     // 从→主：配置应用结果
    MsgCwdList      MsgType = "cwd-list"      // 请求/响应：列远端可达工作目录
    MsgError        MsgType = "error"         // 通用拒绝/错误回执（ReplyTo 关联被拒消息）
)

// ErrorPayload 是 MsgError 的 payload：从侧拒任务（peer 不在白名单、cwd 非法、
// 过载）、config-push 被拒、service 不存在等，一律走此通道即时回执，主侧不等超时。
type ErrorPayload struct {
    Code    string `json:"code"`    // "denied" | "busy" | "bad-cwd" | "not-found" | ...
    Message string `json:"message"`
}

// TaskPayload 是 MsgTask 的 wire payload。
//
// 注意分层：只有上半部分小字段暴露给 LLM 的 remote_agent 工具参数；
// Snapshot 是 wire 字段，由工具实现自动从当前会话取并压缩，LLM 看不到也填不了
// （LLM 生成大段 base64 既不现实又会撑爆 context）。
type TaskPayload struct {
    Task    string   `json:"task"`          // 任务描述
    Context string   `json:"context"`        // "fresh"（默认）| "summary" | "fork"
    Model   string   `json:"model"`          // 可选 model hint
    Cwd     string   `json:"cwd"`            // 指定远端会话工作目录（别名或绝对路径）
    Roles   []string `json:"roles"`          // 角色提示（可选，影响从侧 system prompt）
    Timeout int      `json:"timeout"`        // 秒，0=从侧默认

    // —— wire 字段，工具实现自动填充，不进 LLM 工具参数 ——
    Snapshot []byte `json:"snapshot,omitempty"` // 见下"上下文模式"
}
```

**上下文模式（Context）**——LLM 只选模式，快照由工具实现带外准备：

- `fresh`：不带上下文，远端全新会话执行。Snapshot 为空。适合无状态任务。
- `summary`（默认推荐）：工具实现复用 doge 的 compaction 摘要能力，把当前会话
  压成一段摘要放进 Snapshot（几 KB，gzip）。平衡上下文与体积，适合多数派发。
- `fork`：传当前会话近 N 条消息列表（gzip + 截断到 token 上限，复用
  `KeepRecentTokens` 逻辑）。远端 `ForkSession` 加载。体积大，仅当远端确实需要
  完整对话历史时用。

**快照传输**：Snapshot 内联在 MsgTask frame（长度前缀），gzip 压缩（会话文本压缩率高）。
frame 分块读写，避免一次性驻留大 buffer。若未来快照超大（MB 级），可演进为
"task 先 negotiate，再开独立 `/doge/snapshot/0.1` stream 流式拉取"，但当前内联足矣。

所有消息走同一条 `/doge/agent/0.1` stream，JSON frame（长度前缀 + JSON）。
一条长连接 stream 承载一次会话的多轮交互（task → question/answer → progress → result）。
配置下发与能力广播可复用同 stream 或独立 `/doge/config/0.1` stream。

**Broadcast 语义**：libp2p stream 是点对点的，`To` 为空的"广播"**仅限小消息**
（advertise 等几 KB 内），实现为逐已连 peer fan-out；不引入 pubsub 依赖。
大消息（含 Snapshot 的 task）禁止广播，只能点对点。

**frame 编码备注**：`[]byte` 走 JSON 会 base64（gzip 后体积 ×1.33）。当前可接受；
若实测 snapshot 传输成为瓶颈，frame 层改为"长度前缀 + 类型 + JSON 头 + 原始字节体"
的二进制格式，Snapshot 不再 base64。

### 5.2 主侧（派任务）

主 agent 通过 `remote_agent` 工具派任务给远端节点：

```go
// internal/mesh/collab/remote_tool.go  (+build mesh)
// 工具名：remote_agent
// 参数（全是小数据，LLM 直接填；不暴露 Snapshot）：
//   peer     string   // 目标 PeerID；空则按 roles 路由
//   roles    []string // 角色标签，peer 为空时据此选目标
//   task     string   // 任务描述
//   context  string   // "fresh" | "summary"（默认）| "fork"
//   async    bool     // true=异步（立即返回 taskID，完成时回调），默认 false（同步阻塞）
//   model    string   // 可选 model hint
//   cwd      string   // 指定远端会话工作目录（别名或绝对路径，须在远端 profile.Cwds 内）
//   timeout  int      // 秒，0=从侧默认
// 行为：工具实现按 context 自动取/压缩会话快照 → 发 MsgTask（Envelope.ID=taskID）
//       → async=false：阻塞等 MsgResult 返回；async=true：立即返回"已派发 task=<id>"
//       （结果异步回调，见 §5.6）。LLM 不接触快照字节。
```

- 工具构造时注入 `Mesh` 句柄；由 `cmd/doge/mesh_mesh.go` 在 mesh 构建时追加到
  `AgentSessionOptions.Tools`（core 已支持 Tools override）。
- 派发前先查目标 `AgentProfile`：按 peer ID 直连，或按 role 选 `Accepting && Load<上限`
  的节点（决策2：peer ID 唯一寻址 + role 能力路由，两者都支持）。
- 工具 Execute 内部：开 stream → 发 task → 读循环
  （question→见下处理→answer；progress→更新工具进度；result→返回工具结果）。

**question 处理策略（决策1，修订版——原 steering 注入方案有时序死锁）**：

> 原方案"注入 steering/followUp 流交用户回答"不可行：同步模式下工具 Execute 正
> 阻塞等 `MsgAnswer`，agent loop 正阻塞等工具返回，steering 只在 turn 边界被消费——
> 永远轮不到，死锁。answer 必须绕开 agent loop 的 turn 结构。

修正后的机制——工具 Execute 内部持一个 answer channel，回答经**旁路**写入：

- **有人在线**（`InteractiveCount() > 0`，见 §8.5.2——注意不是 `ConsumerCount()`，
  Web hub 是常驻订阅者但没有浏览器打开时不应算"人在线"）：question 经 session
  pub-sub 推给 TUI/Web 渲染成一条待答提示（体验同本地 ask），**用户回复直接写入
  answer channel**（不进对话流、不经 agent loop），工具拿到答案即回 `MsgAnswer`。
- **无人在线**（`InteractiveCount() == 0`）：工具内部用 `ModelRuntime` **独立调一次
  LLM** 自答（与 compaction `summarize` 同模式，不进主对话 transcript），答案回从侧。
- **超时**：question 带超时（默认 5 分钟），超时未答则回 `MsgError(timeout)`，
  从侧自行决策继续或中止任务。

异步模式（§5.6）下同理：bridge 的 question handler 走同一套旁路，不依赖 followUp。

### 5.3 从侧（接受任务）

```go
// internal/mesh/collab/handler.go  (+build mesh)
// 注册 /doge/agent/0.1 stream handler：
//   1. 解析 Envelope，Type=task
//   2. 校验发送方在白名单内
//   3. 解析 TaskPayload.Cwd：
//      - 空 → 用本地默认 cwd
//      - 别名 → 解析到 profile.Cwds 对应绝对路径
//      - 绝对路径 → 必须在 profile.Cwds 白名单内，否则拒绝
//   4. 以该 cwd ForkNew() 一个独立 AgentSession（core 已支持 per-session cwd）
//   5. 异步执行 task，订阅 agent 事件
//   6. 运行中按需发 question/progress 给主
//   7. 完成后发 result（含 output、stats、artifacts 列表）
```

- 从侧需要"无人值守执行"能力：复用 doge 现有的 print mode agent loop（非交互）。
- **工作目录隔离（决策3）**：`Cwd` 在 `AgentProfile.Cwds` 声明的可达目录内才被接受，
  实现不同项目隔离——从侧不暴露任意本地路径。Cwds 在 mesh.json 里显式声明（别名→路径），
  可由配置协作（§5.5）动态增减。**注意**：cwd 白名单只是入口校验，不是沙箱——
  真正的边界靠权限策略的路径约束（见 §8 远端任务分级）；bash 仍可 cd 到别处。
- 权限：任务在本地跑工具走**本地** permission.json；远端不可绕过。
  远端任务**默认只读工具集**（read/grep/find/ls），写/执行需独立开关（见 §8）。
  无人值守时 ask 默认 deny；接 `LLMApprover` 自动批准仅限分级允许的范围内。
- 并发：每个 task 一个 fork session，互不干扰；限制并发数（profile.Load 上限）。
- **超时与中断**：task 超时（Timeout 或从侧默认）→ 主侧发 `MsgInterrupt`；
  从侧收到后对 fork session 调 `Abort()` 并回 `MsgResult(aborted)`。主侧主动放弃
  （用户取消/agent Abort）同样发 interrupt。从侧过载/白名单外/cwd 非法则回
  `MsgError` 即时拒绝，主侧不等超时。
- **result 截断**：从侧 LLM 输出复用 doge 已有的 `MaxOutputBytes`（256KB）截断；
  超长时 result 附提示"输出已截断，可用 session-snap 拉取完整会话"，避免撑爆主侧 context。

### 5.4 会话共享/同步

两种模式都做（决策3）：

1. **快照模式**（请求-响应，先做）：
   - B 发 `session-snap` 请求 A 的会话；A 回快照（消息列表 JSON）。
   - B 用 `ForkSession` 加载，或只读查看。适合异步、低频、解耦易测。

2. **订阅模式**（实时事件流，复用 web hub 逻辑后做）：
   - B 发 `subscribe` 订阅 A 的某会话；A 把事件逐条 `event` 消息推给 B。
   - 基于 §8.5.2 的 session pub-sub：A 侧只需 `sess.Subscribe(meshForwarder)`，
     forwarder 把 `agent.AgentEvent` 序列化成 `MsgEvent` 发给远端——
     和 TUI/Web 走同一条订阅路径，零特殊代码。
   - 事件序列化复用 `internal/web/hub.go` 已有逻辑（Web 控制台已做同样的事，
     只是走 WebSocket；这里换成 P2P stream）。
   - B 侧可渲染为只读视图，或 fork 成自己的会话介入。

订阅模式本质是"P2P 版的 web 控制台"——`web.Backend` 的事件广播从 WebSocket
换成 mesh stream，且因 §8.5.2 改造后 session 统一管理所有消费者（含 mesh 远端）。

**授权边界**：会话内容含代码与敏感上下文，**不是**同 token 就能拉。快照请求
（`session-snap`）与订阅（`subscribe`）都要求请求方 peer 在被请求方的
`shareSessions` 白名单内（mesh.json 配置，见 §7），否则回 `MsgError(denied)`。
从侧 fork 出的会话标记来源，默认不允许再向外二次共享（防转发泄露）。

### 5.5 配置协作（MCP / skills / models 下发）

doge 可动态加载配置（已有 `ReloadConfig` 读 auth/models/permission）。
mesh 上主侧可向从侧下发配置，使协作侧能力对齐（决策6）。

```go
// ConfigPushPayload 是 MsgConfigPush 的 payload。
type ConfigPushPayload struct {
    Skills  []SkillDef    `json:"skills"`   // skill 名 + 内容（或 URL 由从侧拉取）
    Models  []ModelEntry  `json:"models"`    // model 定义（或 model 集合 URL）
    Files   map[string]string `json:"files"` // 受限白名单文件（见下），非任意路径
    Reload  bool          `json:"reload"`    // 应用后是否触发 ReloadConfig
}
```

- **Files 白名单（安全关键）**：`Files` 只允许写 `skills/`、`models.json`、
  `mcp.json`（MCP 支持上线后）四类路径；**显式拒绝** `permission.json`、
  `auth.json`、`mesh.json`、任意绝对路径——否则一条 config-push 就能改权限配置
  提权（配合远端任务构成完整攻击链）。越界路径整体拒绝并回 `MsgError(denied)`。
- **MCP 降级首发**：doge 尚无 MCP 支持（§4.3 依赖注意），首发只下发
  skills/models；`MCPEndpoint` 结构与 profile 的 MCP 字段一样预留，待 doge
  MCP 立项后启用。
- **从侧应用**：写文件到 `~/.doge/agent/`（白名单内），按 `Reload` 触发
  `ReloadConfig`，然后重发 `MsgAdvertise` 让网络看到新能力（skills 进 profile）。
- **信任边界**：`config-push` 受 `allowRemoteConfig` 开关 + `trustedPeers` 约束，
  默认 deny。下发的是声明式配置（server 地址、skill 内容），不是可执行代码。
- **CLI 触发**：用户可显式推送（`doge mesh config push`），也可让主 agent 在派任务
  前自动检测目标能力缺口并补齐（见 §7 CLI）。
- **skills 共享**：主侧的 skill 目录可打包下发给从侧，使从侧具备相同工具能力——
  解决"派任务给远端但远端没装该 skill"的问题。

### 5.6 异步长任务与回调

remote_agent 默认同步（阻塞等 MsgResult），但远端任务可能跑很久——同步会占住
主 agent 的工具槽。`async:true` 时走异步回调：

- **派发即返回**：bridge 发 MsgTask（Envelope.ID=taskID）后**立即返回**工具结果
  （"已派发给 <peer>，task=<id>，完成时通知"），主 agent 继续干别的。
- **in-flight 注册表**：主侧 bridge 维护 taskID → {peer, 任务摘要, 状态, progress, 派发时间}，
  带超时清理。LLM 可经 `remote_agent_status` 工具查"我有哪些在途任务、各自状态"，
  便于编排多个并发远端任务。
- **result 到达**：bridge 收到 MsgResult（ReplyTo=taskID）→ 查表 → 组成
  "远端任务 <id> 完成：<result>" 作为 **followUp 消息**注入主 agent
  （`Agent.FollowUp`，doge 已有），触发主 agent 下一轮处理。无需新队列。
  followUp 消息带结构化标记 `Details.kind=remote-task`（taskId/peer/status/session），
  web 据此渲染成**独立可折叠的远端任务卡片**（状态徽章 + 节点 + 任务号，
  默认收起），不再伪装成普通 USER 消息；LLM 仍读到原文文本。
- **async 看门狗（0.5.0 新增）**：主侧对每条 in-flight 任务挂
  `timeout + 2min 宽限` 的看门狗——从侧自身在 task 超时点会自 abort 并回
  `aborted`，宽限期是给"结果在途"的余量；超过仍无 result（连接断了 / 从侧
  进程挂 / 卡死），看门狗把注册表标为 `timeout` 并回发 `MsgInterrupt`，
  mesh 面板不再出现永久 running 的死任务。宽限期内迟到的真实 result 仍会
  覆盖标记并正常注入（不丢数据）。
- **progress 到达**：只更新注册表状态 + 经 session pub-sub 广播给 TUI/Web 消费者
  显示进度；**不注入 agent 对话**（避免刷屏）。
- **question 到达**（异步下）：走 **§5.2 的旁路机制**（有人在线→TUI/Web 渲染待答
  提示、用户回复直写 answer channel；无人在线→bridge 独立调 LLM 自答；超时→
  `MsgError(timeout)`）。**不走 followUp**——异步下主 agent 可能在跑别的 turn，
  followUp 排队会饿死等待答案的从侧。
- **UI 通知**：bridge 是 session 的一个 consumer（§8.5.2 pub-sub），result/progress
  到达时同时 fan-out 给 TUI/Web，UI 可弹"远端任务完成"通知。

这套模型与 pi-subagents 的 background runs + result-intercom 一致：派发即返回 job id，
结果异步经通道交付。doge 复用已有 followUp 队列做交付通道，不引入新队列、不阻塞 agent loop。

**断线与重启的状态迁移**：

- **in-flight 持久化**：注册表随派发/完成作为自定义 entry 追加到主侧的会话 JSONL
  （doge 本就是 append-only 日志）。主侧重启后从会话恢复 in-flight 列表。
- **孤儿 result**：重启后到达的 result 若 taskID 无主（会话已换），按 From+taskID
  报告"收到未知任务结果"并丢弃，不崩溃。
- **stream 断开重连**：消息靠 `From + Envelope.ID(UUID)` 配对，重连后旧 task 的
  result 经新 stream 到达仍能对上；bridge 重连后对新 peers 重发 profile 握手。
- **订阅自愈**：订阅方（§5.4）检测 stream 断开后指数退避重连并重新 `subscribe`；
  被订阅方对断线订阅自动清理。

## 6. 模块结构（build tags 文件树）

```
cmd/doge/
  main.go            # 无 tag：解析 flag，调用 mesh 构造函数
  mesh_stub.go       # +build !mesh：newMesh()=nil, remoteTools=nil
  mesh_mesh.go       # +build mesh：构造 libp2p Mesh，返回 remoteTools

internal/mesh/
  mesh.go            # 无 tag：Mesh 接口、PeerInfo、Capability 类型
  envelope.go        # 无 tag：Envelope、MsgType、payload 结构体
  errors.go          # 无 tag：ErrMeshDisabled 等
  stub.go            # +build !mesh：New() 返回 disabled Mesh
  libp2p/            # +build mesh
    node.go          # libp2p host、Start/Close、Peers、Advertise
    discovery.go     # mDNS + DHT + rendezvous
    stream.go        # /doge/agent/0.1、/doge/forward/0.1 frame 读写
    forward.go       # 端口转发
    crypto.go        # PSK 认证/加密（libp2p 已有 TLS，PSK 做准入）
  collab/           # +build mesh（不叫 agent/，避免与 core internal/agent 撞名）
    bridge.go        # RemoteBridge：主侧状态机（task↔result，question 旁路）
    remote_tool.go   # remote_agent AgentTool 定义
    status_tool.go   # remote_agent_status：查 in-flight 异步任务状态
    handler.go       # 从侧 stream handler + fork 执行
    session.go       # 会话快照/订阅（含 shareSessions 授权）
    profile.go       # AgentProfile 构建/解析（从 core 读 tools/skills/cwds）
    config.go        # 配置协作：config-push handler、Files 白名单、重广播
    cwd.go           # cwd 别名解析 + 白名单校验（命中 mesh.json cwds）
    forward.go       # 声明式端口转发：规则加载、unix/TCP 桥接、热启停、单独重载
    forward_tool.go  # mesh_forward 工具：让 agent 自行 add/remove/enable/disable/reload 转发
```

## 7. 接入点

### CLI（仿 `--web` 体系，标准库 flag）

```
doge --mesh                              # 启动 mesh（加入 mesh.json 里的网络）
doge --mesh --mesh-token <tok>           # 用指定 token 加入
doge mesh new                            # 生成新网络 token（写 mesh.json）
doge mesh new --bootstrap /ip4/x.x.x.x/tcp/4001/p2p/<peerid>   # 种子节点地址嵌入 token
doge mesh peers                          # 列出已连接节点 + AgentProfile 概要
doge mesh profile [peer]                 # 看自己/指定节点的完整 AgentProfile

# 端口转发（声明式配置，动态启停；local 支持 unix:///path）
doge mesh forward add <name> --mode expose --local 127.0.0.1:8080        # 发布本地服务
doge mesh forward add <name> --mode expose --local unix:///var/run/docker.sock
doge mesh forward add <name> --mode listen --bind 0.0.0.0 --port 8090 --peer <id> --remote <svc>
doge mesh forward add <name> --mode listen --bind 127.0.0.1 --port 2375 --peer <id> --remote docker
doge mesh forward enable <name>          # 热启一条规则
doge mesh forward disable <name>         # 热停一条规则（关监听/注销 handler）
doge mesh forward list

# 工作目录隔离（声明从侧可达的项目目录）
doge mesh cwd add <alias> <absPath>      # 声明别名 → 绝对路径（写 mesh.json）
doge mesh cwd list                       # 列出已声明的工作目录
doge mesh cwd rm <alias>

# 配置协作（MCP/skills/models 下发与同步）
doge mesh config push <peer|--role X> --skills <dir|name> --mcp <def> --models <entry>
doge mesh config pull <peer>             # 拉取远端配置作为参考副本
doge mesh config ack                     # 查看待确认的下发
```

`--mesh` 与 `--web` 可共存（mesh 做传输，web 做本地 UI）。
`remote_agent` 工具参数也接受 `cwd`（别名/绝对路径）和 `roles`，LLM 派任务时
可指定远端工作目录与期望角色。

### 配置

`~/.doge/agent/mesh.json`：
```json
{
  "token": "<base64>",
  "name": "laptop-a",
  "roles": ["general", "researcher"],
  "allowRemoteTasks": true,
  "remoteTaskLevel": "readonly",
  "allowRemoteConfig": false,
  "trustedPeers": ["12D3Koo..."],
  "shareSessions": ["12D3Koo..."],
  "maxConcurrentTasks": 4,
  "bootstrapPeers": [],
  "privateNetwork": false,
  "cwds": {
    "projA": "/Users/devuser/workspace/projA",
    "projB": "/Users/devuser/workspace/projB"
  },
  "forwards": [
    {
      "name": "docker-daemon",
      "mode": "expose",
      "local": "unix:///var/run/docker.sock",
      "enabled": true
    },
    {
      "name": "remote-docker",
      "mode": "listen",
      "bind": "127.0.0.1",
      "port": 2375,
      "peer": "12D3Koo...",
      "remote": "docker-daemon",
      "enabled": true
    },
    {
      "name": "web",
      "mode": "expose",
      "local": "127.0.0.1:8080",
      "enabled": false
    }
  ]
}
```

- `cwds`：从侧愿意暴露的工作目录别名映射；`task.cwd` 必须命中此处（别名或值）。
- `bootstrapPeers`：种子节点地址列表（`/ip4/x.x.x.x/tcp/p/p2p/<id>`）。空 = 用
  libp2p 默认公共 DHT bootstrap；非空 = 自建种子（§4.2），公共 DHT 不可达时用它组私有 DHT。
- `privateNetwork`：false（默认，公网模式，PSK 走应用层 HMAC 互验）；
  true（私有/VPN 模式，启用 libp2p PrivateNetwork 传输层 PSK，节点须都在受信
  overlay 内且**网络内所有节点同模式**，见 §4.4）。
- `allowRemoteConfig`：是否接受 `config-push`，默认 false，显式开启后才受信任 peer 下发。
- `remoteTaskLevel`：远端任务权限分级（§8），`readonly`（默认）/ `write` / `full`。
- `shareSessions`：允许拉取/订阅本机会话的 peer 白名单（§5.4），空=谁都不许。
- `roles`：本节点声明的能力标签，写入 AgentProfile 供 role 路由。
- **热更新策略**：`forwards` 独立热重载（`ForwardReload`）；其余字段
  （trustedPeers/shareSessions/remoteTaskLevel 等）由 `doge mesh reload` 统一重读——
  只更新应用层状态，不重启 host、不断 peer 连接。
- `forwards`：端口转发规则，每条可热启停。
  - `expose`：`local` 是本地要 dial 的服务（`host:port` 或 `unix:///path`）；
  - `listen`：`bind`+`port` 是本地监听（`0.0.0.0` 对外 / `127.0.0.1` 仅本机 / 具体 IP），
    `peer`+`remote` 连到对端 expose 的 service；
  - `enabled`：`forward enable/disable` 改它，热启停单条规则。
  - 上例：本机把 docker.sock 暴露成 `docker-daemon`，另一节点 listen 到本地
    127.0.0.1:2375，`docker -H 127.0.0.1:2375` 即操作本机 docker。

### main.go 接线

`main.go`（无 tag）在构造 `AgentSessionOptions` 前调用：
```go
m, remoteTools := newMesh(opts)   // mesh_mesh.go / mesh_stub.go 提供
tools := tools.AllTools(ops)
if remoteTools != nil { tools = append(tools, remoteTools...) }
sess, _ := coding.NewAgentSession(...{Tools: tools}...)
```
mesh 启动后把从侧 handler 注册到 mesh 的 stream（`m.RegisterAgentHandler(...)`）。

## 8. 权限与信任

- **网络准入**：三层（§4.4）——libp2p TLS 传输加密、rendezvous 隐匿、PSK 应用层互验。
  未持 token 的节点过不了互验。
- **任务准入**：`allowRemoteTasks` 默认 false；开启后只接受 `trustedPeers` 的任务
  （或任意同 token 节点，按配置）。拒绝即时回 `MsgError(denied)`。
- **远端任务分级（修订新增）**：远端任务与本地会话**不共享**权限等级，
  由 `remoteTaskLevel` 控制（远端任务 = 不可信输入 + 无人值守，风险高于本地）：
  - `readonly`（默认）：只挂只读工具集（read/grep/find/ls）；
  - `write`：+ write/edit/code，仍无 bash；
  - `full`：全部工具 + 本地 permission.json，bash 高危仍走 ask（无人值守即 deny），
    `LLMApprover` 自动批准仅在此级可开，且建议只对 `trustedPeers` 开。
    **full 级还携带节点级 mesh 工具**（mesh_forward / mesh_peers /
    remote_agent_status，0.5.0 修复）：主侧可请从侧配置自己的转发或观察网格；
    readonly/write 级不带（转发会改动节点配置）。
  权限 decider 按 fork session 的 cwd 限定路径边界（permissions 已有 cwd 绑定能力），
  配合 `Cwds` 白名单形成"入口 + 边界"双层隔离。
- **远端 fork 拒装交互式 ask handler（0.6.0 加固）**：远端任务 fork 运行
  无人值守，权限契约 = 派发分级 + 本地 permission.json。因此 `AgentSession
  .SetPermissionAskHandler` 对 `Kind=remote` 会话直接拒绝——即使某 web 窗口
  正打开该 fork 观察（它只是观察者），也不会把 ask 变成"等那个浏览器弹窗
  回答"，避免 bash 命中 Ask 时整个远端任务卡到 ask 超时。无人值守自动判定
  （只读放行 / 高危拒绝）保持秒级。
- **数据边界**：fork 会话快照只含消息文本，不含本地文件；远端要读文件需走工具
  （受其本地权限约束）或端口转发。会话共享（快照/订阅）受 `shareSessions`
  白名单约束（§5.4），fork 出的会话不允许二次外发。
- **配置边界**：`config-push` 的 Files 走白名单（§5.5），permission/auth/mesh
  配置永不可被远端写入。

## 8.5 前置改造（P0，mesh 前必做）

mesh 的从侧要 fork 多个独立会话并发执行任务，主侧也要同时跟多个远端交互；
会话共享/订阅要求 TUI/Web/mesh 远端对等地消费同一会话的事件流。
当前 core 在这两点上都有阻碍，必须先改。这两个改造正交，都让现有功能
（web+TUI 并发）变得更正确，不是仅为 mesh 服务。

### 8.5.1 多会话并发：消除包级全局状态

**问题**：doge 已有 `ForkNew`/`ForkSession` 派生独立 AgentSession 的机制，
`CwdCell` 也已 per-session 隔离工作目录。但有两处包级全局状态会让并发会话串扰：

| 全局状态 | 位置 | 串扰 |
|---|---|---|
| `codeCaller` | `tools/code.go:25-41` | code 工具 RPC 桥接靠它路由脚本内工具调用回 host。A 跑 code 脚本时若 B 刚激活，脚本内工具调用会被路由到 B 的权限策略/cwd。 |
| `currentSessionID/File`（`sessionEnv`） | `tools/bash.go:100-119` | bash 取 `DOGE_SESSION_ID/FILE` 给子进程。A 跑 bash 时若 B 切会话，A 的 bash 拿到 B 的 session id。 |

mutex 只保证不崩，语义上是"拿到别人的"。web+TUI 当前没暴露只是因为人不会同时在两边都跑 code 工具。

**改造**：把这两处状态挪进 `Operations`（已经是 per-session 的，`CwdCell` 就是这么做的）：

1. `Operations` 加字段：`SessionID`、`SessionFile`、`CodeCaller`。
2. `CodeTool()` → `CodeTool(ops)`：从 `ops.CodeCaller` 取 caller，删全局 `codeCaller`。
3. bash 的 `sessionEnv()` → 从 `ops` 取 `SessionID`/`SessionFile`，删全局 `currentSessionID/File`。
4. `NewAgentSession` 不再调 `tools.SetCodeCaller` / `tools.SetSessionEnv`；
   改成在构造 ops 时填入（caller 闭包本就引用 session `s`，绑到 per-session ops 天然正确）。
5. `activateSession` 切会话时更新 ops 的 session 字段（仿 `cwdCell.Set`），不再调全局 setter。
6. 删除 `SetCodeCaller` / `SetSessionEnv` / 全局变量及其测试调整。

改动集中在 `tools/` 约 4 文件 + `agent_session.go` 3 处调用，中低风险。

### 8.5.2 事件 pub-sub：session 与 UI 的最后一点解耦

**问题**：当前事件分发割裂成两条路径、两套类型：

- **TUI**：走 `AgentSession.SetListener(fn func(AgentEvent))`（单消费者、回调无 error、session 层）。
  `handleAgentEvent` 持久化后转发给这一个 `listener`。
- **Web**：走 `Agent.Agent.Subscribe(l AgentListener)`（多消费者、带 error、agent 层），
  绕过 session 的 listener，session 不知道 web 订阅了。
- mesh 会话订阅按 §5.4 还得"再设计一套转发"。

后果：session 无法统一感知消费者数量；TUI 和 Web 不对等；mesh 要接第三套。

**目标**：`AgentSession` 作为唯一事件源（生产者），TUI / Web / mesh 远端都是对等
的消费者（subscriber），session 知道有几个消费者。这是 session 与 UI 的最后一点解耦。

**改造**：

1. `AgentSession` 内部维护 `consumers []*consumerEntry`（仿 `agent.Agent` 的
   `listenerEntry`/`Subscribe` 实现，已验证过多消费者 fan-out）。
2. 新增 `AgentSession.Subscribe(l AgentListener) (unsubscribe func())`：
   多消费者、idempotent unsubscribe、按 entry 指针移除。
3. `handleAgentEvent` 持久化后 **fan-out 给所有 consumers**（而非单个 listener）。
   每个 consumer 的 error 不中断其它（记录日志即可）。
4. 废弃 `listener` 字段 + `SetListener` 方法 + `AgentSessionOptions.Listener`
   构造选项（三处一起迁移）。
5. 统一消费者类型为 `agent.AgentListener`（带 error）。TUI 回调包一层忽略 error；
   Web 的 `sessionBackend.Subscribe` 改成转发到 `sess.Subscribe`（而非 `Agent.Subscribe`），
   web fork 后 re-subscribe 也走 `sess.Subscribe`。
6. **consumer 声明类型**（修订新增——`ConsumerCount` 会误判）：consumer 注册时声明
   `ConsumerInteractive`（TUI、有浏览器在线的 Web hub）或
   `ConsumerHeadless`（无浏览器在线的 hub、mesh forwarder、录制器）。
   Web hub 把浏览器在线状态（≥1 个 WebSocket 连接）映射为自己的 kind。
   暴露 `ConsumerCount()` 与 `InteractiveCount()`——question 分流（§5.2）用后者：
   Web hub 常驻订阅但没有浏览器打开时不算"有人在线"，避免 question 卡到超时。
7. `ui.sessionLike` 接口：`SetListener` → `Subscribe`。

**收益**：
- TUI/Web/mesh 对等订阅，mesh 会话共享的"订阅模式"退化为 `sess.Subscribe(forwarder)`，
  不再是特殊代码路径——§5.4 订阅模式从"复用 web hub 逻辑"升级为"直接复用 session pub-sub"。
- session 能感知消费者数量（mesh 可据此决定是否保留空闲会话、是否需要确认有人在线）。
- 顺带统一了 listener 类型，消除 session 层/agent 层两套回调签名。

## 9. 已定决策

1. **question 处理策略**（修订）：`InteractiveCount()>0` 时经 session pub-sub 推给
   TUI/Web、用户回复**直写 answer channel（旁路，不经 agent loop——原 steering 注入
   方案有死锁，见 §5.2）**；无人在线由工具/bridge 独立调 LLM 自答；带超时。
2. **寻址**：peer ID 唯一身份 + role 能力路由，两者都支持。详见 §4.3、§5.2。
3. **会话共享**：快照和订阅两种都做；订阅复用 web hub 逻辑后做。
   两者都受 `shareSessions` 白名单授权（§5.4）。
4. **DHT**：第一阶段就上（可借鉴 edgevpn discovery 代码），不局限于 mDNS 局域网。
   准入分层见 §4.4：公网模式默认不用 libp2p PrivateNetwork（PSK 走应用层互验，
   避免拒公网 bootstrap）；`privateNetwork: true` 可切私有/VPN 模式（传输层 PSK），
   网络内所有节点须同模式。bootstrap 支持自建种子节点（公共 DHT 不可达时），
   地址嵌入 token。
5. **版本锁定**：Go 版本不锁定（跟随 doge 现有 go 1.26.1）；libp2p 先锁定一个版本，
   避免上游 API 变动，后续按需升级。
6. **agent 发现与能力感知**：广播 AgentProfile（环境/工具/skills/model/cwds），
   供任务派发按能力路由；连接后互推握手（§4.3）。MCP 字段预留待 doge 支持后启用。
7. **配置协作**（修订）：主侧可下发 skills/models 到从侧，受 `allowRemoteConfig` +
   `trustedPeers` + **Files 白名单**约束（permission/auth/mesh 永不可写，§5.5）；
   MCP 待 doge 支持后启用。
8. **工作目录隔离**：task 可指定远端 cwd（别名/绝对路径，须在 profile.Cwds 内）；
   真正的边界是权限分级（§8 remoteTaskLevel），cwd 白名单只是入口校验。
9. **远端任务分级**（修订新增）：`remoteTaskLevel` = readonly（默认）/write/full，
   与本地会话权限分离；LLMApprover 自动批准仅 full 级可开。
10. **错误即时回执**（修订新增）：拒绝/失败一律 `MsgError`（ReplyTo 关联），
    主侧不等超时（§5.1）。

> 修订记录：第 1/4/7/8/9/10 条为设计审核后修正——原稿存在 question 死锁、
> PSK 私网与公网 DHT 互斥、config-push 任意文件写提权、cwd 白名单虚假安全感
> 四处硬伤，已在对应章节修订。

## 10. 实施阶段建议（进度跟踪）

| 阶段 | 范围 | 产出 | 状态 |
|---|---|---|---|
| P0 | core 前置：多会话并发（§8.5.1）+ 事件 pub-sub 解耦（§8.5.2，含 consumer kind/InteractiveCount）+ ai Message 并发锁（race 修复） | AgentSession 可安全并发执行；TUI/Web 对等订阅 session 事件；`-race` 全绿 | ✅ |
| W0 | web per-window 会话隔离 + 惰性控制权（配套，非 mesh 阶段）：control.go 控制权表、每窗口 sessionBackend、X-Doge-Client 路由、事件按窗口推送、`/api/session/claim`、前端 clientId/api() helper/请求去重/gzip 压缩 | 多窗口各自跟随会话；同一会话单写者 + 只读观察 + FIFO 排队接管 | ✅ |
| P1+P2 | Mesh 骨架 + 端口转发（同批实施）：build tags 双包、Mesh 接口/stub、libp2p host + mDNS + DHT 发现（借鉴 edgevpn）、PSK 应用层互验（+可选 PrivateNetwork）、Send/Subscribe、AgentProfile 握手与缓存；ForwardRule 声明式配置、expose/listen、unix/TCP 桥接、热启停、单独重载、`mesh_forward` 工具 | 两 doge 跨网络互相发现、互验、收发消息、看到彼此能力；`doge mesh forward` 与 agent 工具均可管理转发（unix socket 桥接） | ✅ 已完成 0.3.0–0.3.2（实现记录见 §11.12） |
| P3+P4 | agent 协作闭环（一起实施）：从侧 task handler（P3：fork 执行 + result/question/progress 上抛 + 超时 interrupt + MsgError 拒绝 + cwd 白名单/remoteTaskLevel 分级）+ 主侧 remote_agent 工具 + bridge 状态机（P4：peer/role 路由、question 旁路（answer channel + InteractiveCount）、async 模式 + in-flight 持久化） | 主侧 LLM 能派任务给远端执行并同步/异步拿结果，决策由人或 LLM 分流；两端互通闭环验收 | ✅ 已完成 0.4.0（实现记录见 §11.13）；协作层后续加固与稳定性修复 0.4.2–0.7.1（见 §11.14） |
| P6 | 配置协作：config-push/pull、skills/models 下发（Files 白名单）、应用后重广播 profile | 主侧可对齐协作侧能力 | ❌ **已关闭**（评估后不需要，见 §10 说明） |
| P5 | 会话共享：快照模式（shareSessions 授权） | 远端可经授权查看/加载会话 | ❌ **已关闭**（评估后不需要，见 §10 说明） |
| P7 | 订阅模式实时事件流：复用 web hub 序列化 + session pub-sub forwarder、断线自愈重订阅 | P2P 版 web 控制台，远端实时观察/介入会话 | ❌ **已关闭**（评估后不需要，见 §10 说明） |

### 已关闭项说明（评估）

以下三项当初为"完整分布式协作"设想，经实际使用评估后**不再需要完整实现**——
现有能力已覆盖真实需求，完整实现（含各白名单/断线自愈等安全与健壮性机制）
的维护成本高于收益。若未来需求变化（如节点规模扩大、跨节点完整会话迁移），
可按下述已覆盖部分做局部增强，无需重开整项。

- **P5 会话共享（快照/订阅）**：remote_agent 已返回 `session=<id>` 支持跨派发
  续接（`ForkRemoteTo`，仅限 Kind=remote）；远端任务结果以独立卡片（含完整
  输出）注入主会话，可再派发续谈。真正"跨节点按白名单拉取另一节点完整会话"
  的需求未出现——需要看对端会话时直接开对端 web 面板即可（每节点 web 本就
  可见本机全部会话，含远端 fork）。
- **P6 配置协作（config-push）**：协作网络规模小（2-3 节点），各节点直接配
  `models.json`/`settings.json`/`permission.json` 比 push 机制更简单、更可审计；
  `mesh_peers` / `remote_agent_status` 已提供能力感知（对端模型/负载/在途任务），
  主 agent 据此派发已够用。config-push 的安全边界（Files 白名单、防 permission/
  auth 提权）实现与维护成本高，收益不匹配。
- **P7 订阅模式（P2P 版 web 控制台）**：各节点 web 面板已能实时看本机会话 +
  在途远端任务 + 结果卡片；远端任务过程已有 progress/result 回传，无需完整
  会话事件流。跨节点实时订阅另一节点会话的需求未出现，断线自愈重订阅等机制
  成本高。

**Roadmap 顺序**（原推荐）：P3+P4（一起）→ P6（紧跟）→ P5（可并行/穿插）→ P7（最后）。
理由：P3/P4 是同一个 agent 协作闭环的两半，共享同一套协议（task/result/question/progress/error）
与 bridge 状态机，拆开无法端到端验收；P6 的 config-push 依赖从侧先落地；P5 只依赖 P1 的
Send/Subscribe；P7 是增强项放最后。
**现状**：P3+P4 已完成并打磨至 0.7.1；P5/P6/P7 经评估**关闭**（见上表说明），
roadmap 以 P3+P4 + 后续按需加固为终点。

## 11. P1+P2 实施清单（Mesh 骨架 + 端口转发，可独立执行）

> 目标：局域网/公网/私有 VPN 内两个 `doge --mesh`（共享同一 token）互相发现、
> PSK 互验、收发 Envelope 消息、`doge mesh peers` 看到彼此 AgentProfile；
> 且能通过 `doge mesh forward` / `mesh_forward` 工具配置端口转发（expose/listen、
> unix socket 桥接）。**不涉及** agent 协作（P3/P4）、配置下发（P6）。
> P2 建立在 P1 的 stream 层上，同批实施避免中间态。
> 前置依赖：P0 + W0 已完成（多会话并发、pub-sub、per-window 隔离）。

### 11.1 依赖引入（锁定版本）

```bash
go get github.com/libp2p/go-libp2p@v0.45.0
go get github.com/libp2p/go-libp2p-kad-dht@v0.36.0
```
- mDNS 与 rendezvous routing 在 go-libp2p 内部（`p2p/discovery/mdns`、`p2p/discovery/routing`），无需额外依赖。
- 避免引入：pubsub（Broadcast 用逐 peer fan-out）、WebRTC/WireGuard（不需要）。

### 11.2 文件结构（build tags 双包）

```
cmd/doge/
  main.go            # 无 tag：调用 newMesh(opts)（见 11.8）
  mesh_stub.go       # +build !mesh：newMesh()=nil, remoteTools=nil
  mesh_mesh.go       # +build mesh：构造 libp2p Mesh，返回 remoteTools
internal/mesh/
  mesh.go            # 无 tag：Mesh 接口、PeerInfo、Envelope、MsgType、错误类型
  errors.go          # 无 tag：ErrMeshDisabled 等
  stub.go            # +build !mesh：New() 返回 disabled Mesh（所有方法报错）
  token.go           # 无 tag：token 生成/解析（base64 JSON：rendezvous + psk）
  libp2p/            # +build mesh
    node.go          # host 生命周期、Start/Close/ID/Peers/Advertise、连接管理
    discovery.go     # mDNS（局域网）+ DHT rendezvous（公网）+ 自定 bootstrap
    stream.go        # /doge/agent/0.1 长度前缀 JSON frame、Send/Subscribe/连接互验
    crypto.go        # PSK → HMAC 互验 + record AES-GCM 加解密
    profile.go       # AgentProfile 构建（从 core 读 skills/version 等）+ 握手 + 缓存
```

**第一步**：建 `mesh.go`/`errors.go`/`stub.go`/`token.go`（无 tag），先保证
`go build`（无 mesh）编译通过、`doge --mesh` 报 "mesh disabled, build with -tags mesh"。

### 11.3 Mesh 接口（mesh.go，无 tag）

```go
type Mesh interface {
    Start(ctx context.Context) error
    Close() error
    ID() string
    Peers() []PeerInfo
    Advertise(cap Capability)          // 变更本地 profile 并重广播
    Send(peerID string, env Envelope) error
    Broadcast(env Envelope) error      // 仅小消息：逐已连 peer fan-out
    Subscribe(h EnvelopeHandler) (unsubscribe func())
    // 端口转发为 P2，P1 先返回 ErrNotImplemented：
    ForwardReload() error
    ForwardAdd(rule ForwardRule) error
    ForwardRemove(name string) error
    ForwardSetEnabled(name string, enabled bool) error
    ForwardList() []ForwardRule
}
type PeerInfo struct{ ID string; Addrs []string; Profile AgentProfile; Latency time.Duration }
```
`Envelope`/`MsgType`/`AgentProfile`/`Capability`/`ForwardRule` 类型定义见 §4.1/§4.3/§5.1（直接照抄，无 tag）。

### 11.4 libp2p host 启动（libp2p/node.go）

```go
h, err := libp2p.New(
    libp2p.ListenAddrStrings("/ip4/0.0.0.0/tcp/0", "/ip4/0.0.0.0/udp/0/quic-v1"),
    libp2p.DefaultTransports, // 含 QUIC；不要 EnableRelay/WebRTC（P1 不需要）
)
```
- 记录 `h.Addrs()` + `h.ID()`；`Start` 阻塞直到 ctx 取消（或启动后返回、由 CLI 持有）。
- `Peers()` 从缓存的 peer map 构建（见 11.7）。

### 11.5 发现（libp2p/discovery.go）

- **mDNS（局域网）**：
  ```go
  import "github.com/libp2p/go-libp2p/p2p/discovery/mdns"
  s, _ := mdns.NewMdnsService(host, serviceTag /* 从 token.rendezvous 派生 */, notifee)
  s.Start()
  ```
  `notifee` 实现 `HandlePeerFound(peer.AddrInfo)` → `host.Connect(ctx, ai)`。
- **DHT + rendezvous（公网）**：
  ```go
  import dht "github.com/libp2p/go-libp2p-kad-dht"
  import "github.com/libp2p/go-libp2p/p2p/discovery/routing"
  kdht, _ := dht.New(ctx, host, dht.Mode(dht.ModeServer))
  rd := routing.NewRoutingDiscovery(kdht)
  rd.Advertise(ctx, token.Rendezvous)   // 广播自己在 rendezvous
  rd.FindPeers(ctx, token.Rendezvous)   // 找到 → Connect
  ```
- **bootstrap**：`mesh.json.bootstrapPeers` 非空则用它（自建种子节点，见 §4.2）；
  空则用 libp2p 默认公网 bootstrap（`dht.DefaultBootstrapPeers`）。自建种子实现要点：
  - 种子节点：监听**固定地址**（如 `/ip4/0.0.0.0/tcp/4001`）+ DHT `ModeServer`，
    无需额外应用逻辑——其他节点连它后经它加入私有 DHT；
  - 种子地址通过 `doge mesh new --bootstrap <multiaddr>...` 嵌入 token（网络级），
    新节点仅凭 token 自动获得 bootstrap 列表；
  - 私有 VPN 模式（`privateNetwork: true`）下自建 bootstrap 同样持 PSK，兼容。
  参考 edgevpn `pkg/discovery/dht.go`。

### 11.6 PSK 应用层互验（libp2p/crypto.go）

**默认（公网模式）不要**用 `libp2p.PrivateNetwork`（会拒公网 bootstrap 节点）。
- 互验：连接建立后双方各发一条 `/doge/handshake/0.1`，payload = `HMAC-SHA256(psk, peerID)`；验证通过才继续，否则断连（§4.4）。
- record 加密：advertise/service 记录体用 `AES-GCM(psk→key, json)`；未持 token 的节点读不出。
- 密钥派生：`HMAC-SHA256(psk, "mesh-"+用途)` 派生不同用途 key。

**可选私有/VPN 模式**（`mesh.json.privateNetwork: true`，§4.4）：
- 启用 libp2p `PrivateNetwork`：`libp2p.PrivateNetwork(privatePsk)`，`privatePsk`
  为 32 字节，从 token 的 `psk` 派生（`HMAC-SHA256(psk, "libp2p-private-network")`）。
- 此模式下传输层已做 PSK 准入，应用层 HMAC 互验仍保留（纵深防御）。
- **约束**：同一 token 网络内所有节点须同模式；PrivateNetwork 下 bootstrap 也用
  自建种子（§11.5），不连公网 bootstrap。
- 实现：libp2p option 在 host 构造时按配置条件追加，`Start` 前根据
  `mesh.json.privateNetwork` 决定是否 `libp2p.PrivateNetwork(...)`。

### 11.7 通信与 AgentProfile 握手（libp2p/stream.go、profile.go）

- 协议：`/doge/agent/0.1`。frame = 4 字节大端长度 + JSON（`Envelope`）。
- `Send(peerID, env)`：`host.NewStream(ctx, peer, proto)` → 写 frame（点对点，不用 pubsub）。
- `Broadcast(env)`：对已连接 peer 逐个 Send（仅小消息，如 advertise）。
- `Subscribe(h)`：host.SetStreamHandler(proto, ...) 解析 Envelope → fan-out 给注册的 handler（多消费者，仿 `agent.Agent.Subscribe` 的 entry 指针模式）。
- **握手**：`host.Network().Notify(Connected)` 或在 Connect 成功后，双方互发 `MsgAdvertise`（profile）；定期（30s）+ profile 变更时重推。`Peers()` 读缓存的 profile。
- profile 构建：从 core 读 `version`/`skills`（`coding.LoadSkills` 的结果可传进来）/roles（mesh.json）/`Accepting`/`Load`；MCP 字段留空（§4.3 依赖注意）。

### 11.8 CLI 接入（cmd/doge）

- `--mesh` flag + `--mesh-token <tok>`：启动 mesh（token 来自参数或 `~/.doge/agent/mesh.json`）。
- `mesh new`：生成 token（写 mesh.json）。
- `mesh peers`：打印 `Peers()`（ID + roles + skills + Accepting）。
- `mesh_stub.go`（!mesh）：`newMesh(opts) (mesh.Mesh, []*agent.AgentTool)` 返回 `(nil, nil)`，`--mesh` 时报 "build with -tags mesh"。
- `mesh_mesh.go`（mesh）：构造 libp2p Mesh，启动；`remoteTools` P1 返回 nil（P3/P4 再注入）。
- `main.go`：`--mesh` 时调 `newMesh`，与 `--web` 可共存；mesh 的 Close 挂在退出路径。

### 11.9 测试

- `token_test.go`：生成 → 解析 → 字段一致；坏 token 报错。
- `crypto_test.go`：PSK 互验 HMAC 正确/错误；record 加解密。
- `node_test.go`（集成，`-tags mesh`）：同进程起两个 host（不同端口），共享同一 token；
  等待互发现（mDNS/DHT 或直接 `host.Connect` 指定地址），断言：
  1. `Peers()` 互相能看到对方 + profile 字段正确；
  2. `Send`/`Subscribe` 双向收发 Envelope 成功；
  3. 无 token 的第三 host 握手被拒。
- build 双包：`go build ./...`（无 mesh）与 `go build -tags mesh ./...` 都通过；
  `go vet ./...`、`go test ./...`（无 mesh）全绿，`go test -tags mesh ./...` 绿。

### 11.10 验收标准

两台机器（或同机两端口）运行 `doge --mesh`（同 token）：
1. `doge mesh peers` 各自看到对方（ID、roles、skills）；
2. 一方发消息（先用测试验证），另一方 Subscribe 收到；
3. 未持 token 的节点连不上（握手拒绝）；
4. 默认构建 `go build` 无 libp2p 依赖生效（二进制不膨胀）。

---

## 11.11 P2 端口转发实施（与 P1 同批）

### 依赖与文件

- 依赖 P1 的 stream 层（`host.NewStream` / `host.SetStreamHandler` / Envelope frame）。
- `internal/mesh/libp2p/forward.go`（+build mesh）：Forward API 实现。
- `internal/mesh/libp2p/forward_tool.go`（+build mesh）：`mesh_forward` 工具。
- `internal/mesh/mesh.go`（无 tag）：`ForwardRule` 类型 + Mesh 接口中的 Forward 方法
  （已在 §4.1 定义；P1 stub 返回 `ErrNotImplemented`，P2 实现）。

### 协议与地址（对照 §4.5）

- **每个 expose 规则一个独立协议**：`/doge/forward/<name>/0.1`。
  `host.SetStreamHandler(proto(name), handler)`，handler = `net.Dial(rule.Local)` +
  双向 `io.Copy`。用 per-name 协议避免"一条 handler 无法区分哪个 local"。
- **listen 规则**：`net.Listen(resolveAddr(bind, port))`（TCP）或 `net.Listen("unix", path)`
  （`unix://` 前缀）→ accept → `host.NewStream(peer, /doge/forward/<remote>/0.1)` →
  双向 `io.Copy`。`peer` 直接来自配置（不做 name→peer 广播路由，name 仅标识/日志）。
- 地址解析 helper：
  ```go
  func parseAddr(a string) (network, addr string) {
      if strings.HasPrefix(a, "unix://") { return "unix", strings.TrimPrefix(a, "unix://") }
      return "tcp", a
  }
  ```

### Forward API 实现要点

```go
func (m *meshNode) ForwardAdd(rule ForwardRule) error {
    // 校验 mode/name/地址；enabled=false 时只登记不启动
    switch rule.Mode {
    case "expose":  // 注册 stream handler → dial rule.Local
    case "listen":  // net.Listen → accept loop → NewStream(rule.Peer, remote proto)
    }
    // 追加到 m.forwards 表（name → 运行句柄）
}
func (m *meshNode) ForwardRemove(name string) error      // 停 + 移除
func (m *meshNode) ForwardSetEnabled(name string, e bool) // 热启停
func (m *meshNode) ForwardReload() error                  // 重读 mesh.json forwards，diff 应用
func (m *meshNode) ForwardList() []ForwardRule            // 返回 m.forwards
```
- 运行句柄：listen 持有 `net.Listener`；expose 持有注册的协议名。`enabled=false` 时
  停掉监听/不注册 handler。
- `ForwardReload`：对比新旧规则，停掉移除/禁用的、启动新增/启用的；不重启 host。

### mesh_forward 工具（agent 可调，P2）

- `ForwardTool(m Mesh) *agent.AgentTool`（+build mesh，放 forward_tool.go）：
  - `Name: "mesh_forward"`；参数 `{action: list|add|remove|enable|disable|reload,
    name, mode, local, bind, port, peer, remote, enabled}`；
  - Execute 调 `Mesh.Forward*` 方法，返回可读结果（list 打印规则表）。
- 注入：`cmd/doge/mesh_mesh.go` 的 `remoteTools` 追加
  `mesh.ForwardTool(mesh)`（P1 时 remoteTools 为空，P2 加入）。
- 工具与 core 解耦：forward_tool.go 只依赖 `Mesh` 接口 + `agent.AgentTool`，不碰 core 会话。

### CLI 子命令（P2）

`doge mesh forward add/list/enable/disable/remove`（`--mode expose|listen`、`--local`、
`--bind`/`--port`、`--peer`/`--remote`、`--name`）。在 `cmd/doge/mesh_mesh.go` 处理
`mesh` 子命令分发（首参为 `mesh` 时走子命令路由；标准库 flag 解析公共选项后 switch）。

### P2 测试

- 单元：`parseAddr`（unix:// 与 tcp）、ForwardRule 校验。
- 集成（`-tags mesh`，两节点）：
  1. A `expose` 本地 TCP echo server（`net.Listen` + echo），B `listen` 转发到
     `127.0.0.1:<bport>`；连 B 端口发送数据，收到 A 的 echo 响应；
  2. **unix socket**：A `expose local=unix://<tmp>/sock`（临时 unix socket），
     B `listen` TCP，连 B === A 的 unix socket；
  3. `ForwardReload` 重载后新增/移除规则生效；`enabled=false` 停用后连接被拒；
  4. `mesh_forward` 工具 Execute（list/add/remove）走真 mesh 生效。

### P2 验收

1. `doge mesh forward add` 配置后，两节点 A expose / B listen，连 B 本地端口
   === A 的服务（含 unix socket 桥接）；
2. `doge mesh forward enable/disable` 热启停单条规则，互不影响；
3. agent 可经 `mesh_forward` 工具自行增删/启停转发；
4. `ForwardReload` 单独重载 forwards，不影响 peer 连接与 agent 协作层。

## 11.12 实现记录（已完成 0.3.0–0.3.2）

> P1+P2 已实现并发布：`f920ecd feat: 0.3.0`（mesh 主功能）、`c95f9ea fix: 0.3.1`
> （持久化节点身份，stable PeerID/端口）、`3535f55 fix: 0.3.2`（web 会话控制修复、
> abort-then-400 修复、fork 保留 mesh 工具）。与 §11 设计一致，关键实现细节与差异如下。

### 结构（与 §11.2 一致）

- `internal/mesh/` 无 tag：`mesh.go`（接口+类型）、`envelope.go`（协议类型）、
  `token.go`、`config.go`、`errors.go`、`stub.go`(!mesh)。
- `internal/mesh/libp2p/`（+build mesh）：`node.go`、`identity.go`、`discovery.go`、
  `stream.go`、`crypto.go`、`forward.go`、`forward_tool.go`、`log.go`。
- `cmd/doge/mesh.go`（无 tag 子命令分发）、`mesh_mesh.go`(+mesh)、`mesh_stub.go`(!mesh)。
- `Mesh` 接口与设计**完全一致**（含 5 个 `Forward*` 方法）；`Envelope` 14 个 MsgType
  （P1+P2 仅用到 `MsgAdvertise`，其余为 P3+ 预留）。

### 实现要点与差异

- **传输：仅 TCP**。因 quic-go v0.55.0 在 Go 1.26.1 下服务端 session ticket panic
  （`node.go:124-129` 有注释），QUIC 被显式禁用（`libp2p.Transport(tcp.NewTCPTransport)`）。
  设计 §11.4 提到的 QUIC 选项**未启用**。
- **PrivateNetwork**：默认 `false`；`true` 时 `libp2p.PrivateNetwork(deriveKey(psk,
  "libp2p-private-network"))`（32 字节派生，与 §4.4 一致）。`privnet_test.go` 验证两节点互通。
- **发现**：mDNS（tag `_doge`+sha256(rendezvous)[:3]+`._udp`）+ DHT **恒 `ModeServer`**
  + `routing.NewRoutingDiscovery`（Advertise ttl/2 重发、FindPeers 60s）。注意：
  `discovery.go` 注释称私有网络模式只靠 mDNS+seeds，但 DHT **未按 `PrivateNetwork`
  门控**（恒启动）——文档与实现不一致，后续可修或补注释。
- **PSK 互验**：`/doge/handshake/0.1` 双向 `HMAC-SHA256(psk, peerID)`，错 token
  → `ClosePeer`；record 用 AES-GCM（purpose 派生 key）。密钥派生 `HMAC-SHA256(psk,
  "mesh-"+purpose)`。
- **身份持久化（0.3.1）**：Ed25519 私钥存 `~/.doge/agent/mesh-key`（0600，**不存
  mesh.json**——后者跨机共享）；固定 `ListenPort`（`mesh.json.listenPort`，0=随机）
  使 token 内嵌 bootstrap 地址跨重启有效。
- **握手与 profile**：`onConnected` → handshake → 标记 authed → ping 测延迟 →
  `sendProfile`（AES-GCM 加密 advertise）；profile 每 30s 重推 + `Advertise` 时重推
  （per-peer Send，非空 To 广播）。
- **重连（0.3.0）**：10s ticker `reconnectKnownPeers` 重拨已知 peer；未认证 peer
  断开即删，已认证保留隐藏（`Peers()` 只返回已认证）。
- **转发**：`/doge/forward/<name>/0.1` per-name 协议、raw byte pipe（无 frame）；
  `parseAddr` 支持 `unix://`；`ForwardReload` 3s 热载 poller（内容 hash `forwardsSig`，
  跨进程 `doge mesh forward ...` 生效）。
- **token/config**：`Token{Rendezvous, PSK, BootstrapPeers}`，base64 RawURLEncoding；
  `Config{..., PrivateNetwork, ListenPort, Forwards}` 存 mesh.json；`--mesh-token`
  覆盖不持久化。
- **CLI**：`doge mesh new|join|daemon|peers|profile|forward`；`--mesh`/`--mesh-token`；
  `--bootstrap` 可重复；`mesh daemon` headless 节点；Probe 模式供发现类命令不启转发。

### 测试（全部 `-tags mesh` 绿）

- `node_test.go`：互验+profile 交换、Send/Subscribe（含 unsub）、Broadcast、拒错 token、重连。
- `forward_test.go`：`parseAddr`、TCP echo 往返、unix socket、enable/disable、Reload 重载、工具 Execute。
- `privnet_test.go`、`discovery_test.go`（仅 HandlePeerFound 握手，macOS 组播套接字竞态故不做进程内 zeroconf）、
  `crypto_test.go`、`identity_test.go`、`token_test.go`（无 tag）。
- 双构建：`go build ./...`（无 mesh）与 `go build -tags mesh ./...` 均通过；
  `go test ./...`（无 mesh）与 `go test -tags mesh ./internal/mesh/...` 全绿。

### 版本线

- 0.3.0：P2P mesh（发现/互验/转发）+ 节点自动重连。
- 0.3.1：持久化 mesh 节点身份（stable PeerID/端口）。
- 0.3.2：web 会话控制修复、abort-then-400 修复、fork 保留 mesh 工具。

## 11.13 实现记录（P3+P4，0.4.0）

> P3+P4 已实现（agent 协作闭环，验收 1-6 全通，见 §11.13 测试清单）。
> 与 §5.1/§5.2/§5.3/§5.6/§8/§9 设计一致，关键实现细节与差异如下。

### 结构（与 §6 一致，落在 libp2p 包内）

- `internal/mesh/libp2p/collab.go`：`Collab`/`NewCollab`——把从侧 handler 与主侧
  bridge 合并为单条 Envelope 订阅（MsgTask/Interrupt/Answer→handler；
  Result/Progress/Question/Error→bridge）。
- `internal/mesh/libp2p/agent_handler.go`：从侧 `agentHandler`（fork 执行、
  校验、question/progress/result、中断/超时、并发上限）。
- `internal/mesh/libp2p/bridge.go`：主侧 `Bridge`（sync waiter / async in-flight
  注册表 + followUp 注入、question 旁路、inflight JSONL 持久化+恢复）。
- `internal/mesh/libp2p/remote_tool.go`、`status_tool.go`：`remote_agent` 与
  `remote_agent_status` 工具。
- gzip 快照 helper（原 snapshot.go，已并入 agent_handler.go）。
- 无 tag 侧：`internal/agent` 增 `EventQuestion`/`Question`/`EventMeshProgress`/
  `MeshProgress`；`internal/coding` 增 `AgentSession.AskQuestion/AnswerQuestion/
  EmitMeshProgress/ForkRemote/SummarizeContext/RecentMessages/AppendExtraTools`
  与 `RemoteToolLevel`；`internal/mesh/envelope.go` 增 Result/Question/Answer/
  Progress payload。

### 实现要点与差异

- **question 旁路（§5.2/§5.3）**：question 走 session 的 answer channel——`sess.
  AskQuestion` 注册 channel 并 fan-out `EventQuestion`；TUI/Web 渲染后用户回复
  经 `AnswerQuestion`（TUI 直接、Web 走 `POST /api/question/answer`）直写 channel，
  不经 agent loop（防死锁）。无人在线（`InteractiveCount()==0`）由 bridge 独立调
  LLM 自答（不进主 transcript）。超时（默认 5 分钟）回 `MsgError(timeout)`。
- **从侧提问**：不是靠事件而是靠注入 fork 会话的 `contact_supervisor` 工具——
  远端 agent 调用它即发 `MsgQuestion` 并阻塞等 `MsgAnswer`（按 taskID 路由）。
- **fork 会话（§5.3）**：`AgentSession.ForkRemote(cwd, level, model, extra)`
  在 coding 层按 `remoteTaskLevel` 构建工具集（readonly=read/grep/find/ls；
  write=+write/edit/code；full=全部）并绑定 fork 自身 cwd/会话身份的 ops；
  主侧 `AppendExtraTools` 让 current+forks 都拿到 remote 工具。
- **context 快照（§5.1）**：summary=fork 会话 LLM 摘要（复用 `summarize`，不
  改主会话）gzip；fork=近 `KeepRecentTokens` 消息 gzip；fresh=空。从侧按
  context 模式注入为初始 user 消息 / 初始 transcript。
- **拒绝即回（§9.10）**：非白名单发送方→`denied`；cwd 不在 `Cwds`→`bad-cwd`；
  并发满→`busy`；model hint 解析不了→`denied`。均 `MsgError` 且 `ReplyTo` 关联，
  主侧不等超时。
- **中断/超时（§5.3）**：`MsgInterrupt`（ReplyTo=taskID）或 task 超时 →
  fork session `Abort()`，回 `MsgResult(status=aborted)`。主侧同步超时先发
  `MsgInterrupt` 再报"timed out"。
- **async（§5.6）**：`async:true` 立即返回 task id；bridge in-flight 注册表
  （`remote_agent_status` 可查）随派发/完成以 `mesh_inflight` 自定义 entry 写主会话
  JSONL，重启后恢复为 `recovered`；result 到达 → `Agent.FollowUp`+`Continue`
  注入主 agent（agent 空闲时直接跑，运行中由 loop 在停点 drain）。
- **安全**：`permission.json` 仍是最终边界（fork 继承策略，无人值守 ask 默认
  deny）；cwd 白名单只是入口校验；`contact_supervisor`/`remote_agent` 只发送
  小字段（快照是 wire 字段，LLM 不可见）。

### 测试（全部 `-tags mesh`，`-race` 绿）

- `collab_test.go`：两节点真实 libp2p 传输 + 每节点独立 mock LLM——
  `TestSlaveRunsTaskFresh`（task→result）、`TestSlaveBadCwd`（bad-cwd 即时拒）、
  `TestSlaveUnauthorized`（denied）、`TestSlaveBusy`（并发上限 busy）、
  `TestSlaveInterrupt`（interrupt→aborted）、`TestRemoteAgentSync`、
  `TestRemoteAgentSyncBadCwd`、`TestRemoteAgentTimeout`（超时→interrupt→aborted）、
  `TestRemoteAgentAsync`（async ack + in-flight completed + followUp 注入）、
  `TestStatusTool`、`TestQuestionLLMAnswer`（无人值守 LLM 自答）、
  `TestQuestionHumanAnswer`（answer channel 用户回答）、
  `TestRemoteAgentSummarySnapshot` / `TestRemoteAgentForkSnapshot`、
  `TestRemoteAgentRoleRouting`（roles 路由）。
- `internal/coding/agent_session_remote_test.go`（无 tag）：`ForkRemote` 三级工具集
  （readonly 无 bash）、`AskQuestion/AnswerQuestion` 旁路。
- 双构建：`go build ./...` 与 `go build -tags mesh ./...` 通过；
  `go test ./...` 与 `go test -tags mesh ./...` 全绿；`go test -race ./...` 与
  `go test -race -tags mesh ./...` 全绿；`go vet` 双构建干净。

### 版本线

- 0.4.0：agent 协作闭环——从侧 task handler（P3）+ 主侧 remote_agent/bridge/
  question 旁路/async（P4），web/TUI 均支持远端任务提问。

### 0.4.1 修订：三处协作层设计补漏（评审反馈）

1. **任务连续性（§5.1/§5.3 修订）**：`TaskPayload`/`ResultPayload` 增 `Session`
   字段。从侧 `runTask` 支持 `session=<id>` 接续既有远端会话（`ForkRemoteTo`
   绑定已有会话，转录恢复后追加新任务），**仅限 Kind=remote 的会话可接续**
   （远端 client 不能续用户本地会话，否则 `MsgError(bad-session)`）；每次结果带
   `session=<id>`，主侧 `remote_agent` 传回即可跨多次派发保持对话。
2. **会话隔离（§8.5 修订）**：`SessionHeader.Kind`（""=用户 / `remote`=远端任务
   fork），`ForkRemote` 自动标记；webui/tui 会话列表默认过滤远端会话（web 收进
   "远端任务会话"折叠区、tui 按 `r` 切换显示），保留查看入口。
3. **mesh 感知入 UI（§4.3/§5.2 修订）**：`mesh_peers` 工具（含 `self=true` 报
   本节点身份）；web `GET /api/mesh` + Mesh 面板、TUI `/mesh` 命令展示本节点/
   对端/在途远端任务；`Mesh.Self()` 入接口、`Load` 由从侧 handler 实时上报
   （`SetTaskLoad`），角色路由负载感知。

## 11.14 实现记录（0.4.2–0.7.1：协作层加固与稳定性）

> 0.4.0/0.4.1 之后围绕真实联调反馈做的一轮加固与稳定性修复，全部涉及
> agent 协作链路（远端任务）与排查设施，与 §5/§8 设计对应。

### 0.4.2 — 提供商停摆兜底（core ai，远端/本地任务都受益）

- SSE 静默看门狗：goroutine-per-Read 包裹 `resp.Body`，超时无数据即取消
  请求并回 `ErrStallTimeout`；`ResponseHeaderTimeout` 兜底"连头都回不来"。
- 流空闲超时可配（settings `streamIdleSeconds` / 环境），atomic + 整体换
  client 免竞态。
- 首条消息即落盘：中途运行的可被另一窗口定位/接管；新窗口跟随实时实例；
  中断反馈与状态轮询改善。
- 提交：`671b43e`。

### 0.5.0 — 真实联调四问题修复 + 前端重设计 + 会话切换提速

1. **远端任务结果独立卡片**：followUp 注入消息带 `Details.kind=remote-task`
   结构化标记，web 渲染为独立卡片（§5.6，前端 0.7.1 起可折叠）。
2. **full 级任务带 mesh 工具**：`runTask` 在 full 分级下把节点级 mesh 工具
   （mesh_forward/mesh_peers/remote_agent_status）并入 fork 工具集；
   readonly/write 不带（§8）。
3. **async 在途任务看门狗**：主侧 `timeout+2min 宽限` 无结果则标记 timeout
   并回发 interrupt（§5.6）；从侧"无 assistant 输出"的空结果改报 error。
4. **mesh 面板防溢出**：注册表 progress 截断 400 字 + 前端限高可展开。
5. 前端全面重设计（深空仪表台风格）+ 会话切换提速（会话摘要缓存、
   id 直达解析、control 建会话移出全局锁、切换 loading 反馈）。
- 提交：`9d18545`。

### 0.6.0 — 远端任务停摆修复 + 事件日志系统（logx）

- **远端 fork 拒装交互式 ask handler**（§8）：web 观察窗口不再把权限弹窗装到
  无人值守的远端任务，bash 命中 Ask 不再卡 5 分钟等浏览器答复（真实事故根因）。
- runTask 不再吞 Prompt 错误：运行中途失败上报 `error`，不再用半截文本拼假
  completed；agent 运行 panic 保留非 error 值信息。
- **logx 诊断日志**：`pkg/logx`，debug/info/warn/error + off，
  `~/.doge/agent/logs/doge.log`（0.7.1 起按天滚动），`--log-level` flag /
  settings `logLevel`；埋点覆盖 agent（run/tool/流错误）、ai（请求/停摆）、
  perm（ask 判定全程）、mesh（任务生命周期）、session（持久化失败）、
  web（控制权）。
- 流式 partial 终态写入纳入消息锁（race 修复，见 §11.14 0.7.0）。
- 提交：`66f3423`。

### 0.7.0 — 流式消费死锁（线上事故根因，core ai）

- **根因（goroutine dump 实锤）**：三个流适配器（openai-completions /
  anthropic / responses）的 SSE handler 在持有 partial 消息写锁的状态下向
  EventStream 推送；通道满时生产者持锁阻塞，消费者（agent loop 经 web
  监听器 marshal 同一消息）等读锁、停止排空 → 锁序反转死锁。看门狗（无在
  读）与中断（无人取消通道发送）都解不开——与线上"LLM 200 后静默挂死、
  中断无效、远端任务永久 running"完全吻合（本机 + startvm-us 双节点同型）。
- **修复**：三适配器改为「锁内变更 + 事件排批次，解锁后统一 flush」——任何
  Push 不持锁；web 监听器 marshal 前 Clone 快照；回归测试
  （ProducerPushDoesNotHoldLock，completions + anthropic，构造通道满 + 并发
  marshal 的死锁场景，旧代码必挂）。
- debug 日志细化：ai 请求体（截断）、agent run end（耗时）/llm 响应汇总/
  工具参数、steering/followup 注入、perm 参数摘要；修复 defer 参数立即求值
  导致 run end 耗时恒为 0 的日志 bug。
- 提交：`d582622`。

### 0.7.1 — 交互与日志打磨

- 远端任务结果卡改原生 `<details>` 折叠：默认收起（仅头部徽章/节点/任务号），
  点击展开输出，减少长结果的纵向占用。
- logx 按天自动滚动：日期翻转归档为 `doge.log.YYYY-MM-DD` 并开新文件；
  按大小滚动同样带日期后缀（同日多次追加 `.1/.2…`），跨天重启按 mtime 归档。
- 提交：`55fe064`。

### 仍开放的已知边界（已随 P5/P6/P7 关闭收窄）

- 从侧权限 ask 不自动上抛主节点：worker 撞 ask 默认拒绝，需显式
  `contact_supervisor`。自动上抛原是 P6 候选，P6 关闭后此项一并**关闭**——
  无人值守任务按分级 + permission.json 自行决策已足够。
- 续写运行中不实时（fork 窗口不自动 re-attach 新活动实例）：低频场景，
  任务完成后刷新可见，保持现状（低优先级，不排期）。
- 问题弹窗不跨会话广播（只到派发会话）：人通常只在派发会话操作，接受现状。
- mDNS 同机长驻发现时序问题：跨机部署建议种子节点（§4.2），已按此落地。

### 已关闭项

P5 会话共享 / P6 配置协作 / P7 订阅模式 均**关闭**（§10 说明：现有能力已足够，
不再需要完整实现）。

## 11.15 实现记录（断网重连修复 + 远端工具直接调用）

### 断网重连失效的三个根因与修复（连接自愈加固）

真实部署反馈"断网后节点连不回来、只能重启"。排查确认三条叠加根因，全部修复：

1. **入站 peer 地址过期（主因）**：`reconnectKnownPeers` 仅按 `peer.ID` 空拨，
   依赖 peerstore 里的地址；但只有**主动 dial** 时地址才以 `PermanentAddrTTL`
   入库，被动连入的 peer 地址来自 identify 协议（`TempAddrTTL` 2 分钟 /
   `RecentlyConnectedAddrTTL` 15 分钟）。断网一旦超过该时长，空拨永远报
   "no addresses"（LAN 网络还可能因 mDNS browse 失效而没有任何再发现路径）。
   **修复**：`peerState` 本就记录每条连接的 `RemoteMultiaddr`，重连时把记录的
   历史地址解析出来随 `Connect` 显式传入（Connect 会以 Permanent TTL 重新吸收
   进 peerstore），重拨与断网时长解耦。
2. **种子连接不保活**：`bootstrapConnectLoop` 首次连上种子即 return，种子断线
   （或重启换端口）后该循环消失。**修复**：改为节点生命周期内持续循环——每 10 秒
   检查 `Connectedness`，未连接即重拨。
3. **多连接误清 authed**：同一 peer 存在多条连接时（双向同时重拨的竞态会留下
   两条），单条断开即把 `authed=false`，peer 从 `Peers()` 隐藏且永不恢复（另一条
   连接仍工作，不会再触发 Connected）。**修复**：`onDisconnected` 里先查
   `ConnsToPeer`——swarm 先移除连接再异步回调，此处计数准确；仍有存活连接则不动
   状态。

附带加固：`HandlePeerFound`（mDNS notifee）按 peer **single-flight**——zeroconf
browse 会反复通告同一 peer，旧实现每次通告都起一个无限退避重拨 goroutine，对离线
peer 无限堆积；现在同一 peer 同时只有一个重拨循环，且每轮先查 `Connectedness`
再拨。

测试：`TestNodeReconnectWithoutPeerstoreAddrs`（强制断连 + 清空 peerstore 地址
模拟 TTL 过期，重连循环必须凭历史地址恢复）、原 `TestNodeReconnect` 回归。

### 远端工具直接调用（remote_tool，不经远端 agent）

原设计里"精确操作远端节点"只有一条路：`remote_agent` 派任务（远端 fork 会话跑
LLM，再由远端 agent 调它自己的 mesh_forward）。对于"在 B 上加一条 expose/listen
绑定"这类确定性操作，远端跑一次 LLM 既慢又花 token 还引入不确定性。本轮增加
**直达通道**：

- **协议**：新增 `MsgToolCall`（主→从，payload=`ToolCallPayload{Tool, Args}`）与
  `MsgToolResult`（从→主，`ToolResultPayload{Status, Output, Error}`），
  `Envelope.ID` 作调用 ID、`ReplyTo` 关联，复用既有 `/doge/agent/0.1` stream。
- **从侧**：`toolcall.go` 的 `remoteToolHandler`——鉴权（`allowRemoteTools` +
  `trustedPeers`，新配置项 `AllowRemoteTools` **默认开**——token 即准入边界，
  同 token 节点本就受信，mesh_forward 是 mesh 的核心能力不应加默认闸；要收紧
  显式写 `false` 或配 `trustedPeers`（与远端任务共用 `peerTrusted` 判定）→ **节点级工具白名单**（仅 mesh_forward / mesh_peers /
  remote_agent_status；read/bash 等会话工具明确不走此通道，直接调用永远绕不过
  remoteTaskLevel 分级）→ 执行注册的 `AgentTool` → 回执。拒绝即时回 `MsgError`。
- **主侧**：`direct_tool.go` 的 `remote_tool` 工具——`{peer, tool, args}`，
  `Bridge` 复用任务结果的 waiter 通道等回执（60s 超时），输出原文回传。接入
  `attachMeshCollab`（本机所有会话可用）。
- **能力感知**：`allowRemoteTools` 开启时 profile 带标签 `tags["remoteTools"]=true`，
  `mesh_peers`/`doge mesh profile` 展示为 `DirectTools`，主侧 agent 据此决定
  用 `remote_tool` 还是 `remote_agent`。
- **无会话节点**：collab 的工具执行器与 agent session 解耦，`mesh daemon` 也挂上
  （`mesh_mesh.go`），headless 节点同样可被远端精确配置转发。
- **安全边界**：默认拒绝；白名单只含节点级配置/查看工具；`trustedPeers` 同时
  约束远端任务与直接调用；mesh_forward 的持久化与热加载路径与本地完全一致
  （远端 mesh.json 是唯一被写入面）。

测试：`TestRemoteToolDirectForward`（远端加 expose 规则即时生效）、
`TestRemoteToolDenied`（显式关闭 allowRemoteTools 即时拒绝）、
`TestRemoteToolUnknownTool`（白名单外主侧拦截 / 未注册从侧 not-found）、
`TestRemoteToolAllowlistBoundary`（**暴露边界回归**：白名单只含节点级 mesh
工具；就算把 guide 这类本地工具错误接进从侧 MeshTools，从侧白名单仍拒绝——
暴露 = 注册 ∩ 白名单，双闸缺一不可）。

### 工具暴露边界（哪些工具出网、哪些不出）

| 暴露面 | 工具集 | guide 等本地内置 |
|---|---|---|
| 本地会话（TUI/print/Web 窗口 fork） | 全部内置 + ExtraTools（guide、mesh 工具、remote_agent 族、MCP、skills） | ✓ 在（本地便利） |
| 远端任务 fork（remote_agent，按 remoteTaskLevel） | `remoteToolSet(level)`（readonly=read/grep/find/ls；write=+write/edit/code；full=全部）+ contact_supervisor +（仅 full）MeshTools | ✗ 不进（ForkRemote 用 Tools override 显式构造，**不继承 ExtraTools**） |
| remote_tool 直接调用 | 从侧注册的 `CollabOptions.MeshTools`（cmd 接线=mesh_forward/mesh_peers）∩ `directToolAllowlist`（mesh_forward/mesh_peers/remote_agent_status） | ✗ 两道闸都不在 |

区分是**显式 opt-in** 的：一个工具要对 peer 出网，必须同时（1）被接进
MeshTools 注册面、（2）名列 `directToolAllowlist`。会话工具（read/bash/…）、
本地便利工具（guide、skill）天然两处都不满足。新增可出网工具时：工具定义放
libp2p 包 → 加进白名单 → 接进 attachMeshCollab/daemon 的 MeshTools，三步都改
才生效（漏一步即拒绝，fail closed）。

## 11.16 实现记录（NAT 穿透：端口映射 + 自动中继 + 打洞）

> 目标：两侧都在 NAT 后（或一侧在 NAT 后无直连路径）时，mesh 仍能互通。
> 实现全部落在 libp2p 现成机制上，doge 只做接线与准入收口。

### 节点侧（默认启用，无需配置）

- **`libp2p.NATPortMap()`**：UPnP/NAT-PMP 网关端口映射（成功后对端经 identify
  学到公网映射地址可直接拨入；网关不支持则静默降级）。此前从未启用——
  FallbackDefaults 不含它，doge 的显式选项列表也没写。
- **`libp2p.EnableHolePunching()`**：DCUtR 协调打洞，把经中继的连接升级为直连
  （TCP 直打洞成功率有限，失败保持中继通路）。

### 自动中继（AutoRelay）

- relay **传输**（拨 `/p2p-circuit` 地址）libp2p 默认已启用（`DefaultEnableRelay`
  fallback），无需配置。
- **候选来源两模式**：`bootstrapPeers` 非空（推荐组网）→
  `EnableAutoRelayWithStaticRelays(种子)`；为空（公网 DHT）→
  `EnableAutoRelayWithPeerSource(rendezvous 发现结果)`。libp2p 默认 3 分钟
  bootDelay / 30 秒 minInterval，相对 10 秒重连节奏太慢，收紧为 3 秒 / 10 秒。
- **可达性事件**：autorelay 的 finder 只在收到 `EvtLocalReachabilityChanged`
  后启动，而 doge 不跑 autonat（永远没有事件）——构造完成时主动发一条
  Stateful 的 **Private** 事件。选 Private 而非 Unknown 有两个原因：finder 对
  两者等价，但 addrs 管理器只在 Private 下通告中继地址；且 doge 节点多数在
  NAT 后，无更优知识时声明私网是保守且保连通的选择。
- **种子 circuit 地址显式注入（关键补丁）**：autorelay 构造通告地址时用
  `cleanupAddressSet` 过滤中继在 peerstore 里的地址——只保留**公网**地址。
  公网种子没问题，但 VPN 内网种子（Tailscale/overlay IP）会被过滤成空、
  完全通告不出中继路径。补法：种子模式下经 `libp2p.AddrsFactory` 直接追加
  `<种子地址>/p2p/<种子ID>/p2p-circuit/p2p/<本机ID>`（autorelay 仍负责预约与
  刷新，此处只保证通告确定性）。实现细节：factory 在 host 构造期间就会被
  holepunch 服务调用，闭包里用私钥派生的 `selfID` 而非 `m.host.ID()`（彼时
  host 尚未赋值）。

### 中继服务（cfg.Relay，种子节点开启）

- `relay.New(host, ...)` 启动 circuit v2 hop（Start 里非 Probe 时；失败仅降级
  记日志，不影响其余能力），Close 时一并关闭。
- **`WithInfiniteLimits()`**：库默认限制每条中继电路 2 分钟 / 128KB，会把端口
  转发的长连接拦腰斩断；准入已由 ACL + PSK 把关，限制放开。
- **`WithReservationAddressFilter(恒真)`**：默认只向客户端通告中继的公网地址，
  VPN 内网种子必须通告其内网地址，否则客户端拿不到可用 circuit 地址。
- **ACL（relay.ACLFilter）**：`AllowReserve`/`AllowConnect` 只放行 mesh 内节点
  ——`knownPeer` = 已互验，或连接建立 2 分钟内（握手在途宽限；错 token 节点
  握手即断，其预留随连接失效，陌生人无法占住中继槽位）。

### 组合语义

能直连时 libp2p 优先直连，中继是兜底路径；`reconnectKnownPeers` 重拨时记录的
地址里本就含 circuit 地址（经中继的连接同样记录），断网恢复路径对中继连接
一致适用。未启 autonat（后续如有需要可加：公网节点可停止无谓的中继预约）。

### 测试

- `TestRelayHopEndToEnd`：三节点——R 起 hop 服务，B/C 以 R 为静态中继；
  断言 circuit 地址通告、C 经中继拨通 B、PSK 互验 + profile 交换 + Envelope
  收发全部走中继连接完成。
- `TestRelayACLDeniesStrangers`：错 token 节点连接后被握手拒绝并从 peers 表
  清除（其预留随连接失效）。

## 11.17 实现记录（peer 别名：节点名寻址）

LLM 写全 peer ID（`12D3Koo...`）既长又易抄错；节点本就有人类可读名
（mesh.json `name`，随 AgentProfile.Name 广播）。本轮让所有 LLM 侧的 peer 入参
都接受节点名：

- **`resolvePeerRef(mesh, ref)`**（peers_tool.go）：可解析成 peer ID → 在已连
  列表里则用之，不在也原样透传（让 Send 给出明确的 "not connected"）；否则按
  `profile.Name` 在**已连接且已互验**的 peer 里匹配——唯一命中返回其 ID；
  零命中即时报 `no connected peer named ...`；多命中即时报
  `peer name "x" is ambiguous (id1, id2); target one by its exact peer id`
  ——**列出候选 ID**，agent 下一步可直接精确指定，不用再查一遍。
- **接入点**：`remote_agent`（peer 参数，与 roles 路由共存）、`remote_tool`
  （peer 参数）、`mesh_forward`（listen 规则的 peer——添加时解析成真实
  peer ID 再落盘 mesh.json，配置里存别名无法拨号）。`mesh_peers` 描述注明
  名字可用作 peer 引用及其唯一性约束。
- **注意**：CLI `doge mesh forward add --peer` 直接写配置文件、无运行时上下文，
  不做名字解析（只收 peer ID）；解析发生在 LLM 工具层，规则最终落盘的永远是
  真实 peer ID。

测试：`TestRemoteToolPeerNameAlias`（唯一名解析 + 未知名即时报错）、
`TestRemoteToolPeerNameAmbiguous`（重名即时报错并列出两个候选 peer ID）、
`TestRemoteAgentPeerNameAlias`（remote_agent 按名字派发端到端）。

## 11.16 实现记录（数据流互验门禁 + 文件传输加固，复审修复）

对未提交变动的全面复审发现：**互验状态没有纳入数据流的授权判定**——PSK 握手
是连接建立后异步执行的，而 agent / file / forward 三类 stream handler 只查配置
开关（allowRemoteTools / allowRemoteFiles / forward 规则），不查 `st.authed`。
错 token 节点连接后扣住握手流不回应，即可在窗口期内经 `MsgToolCall` 调
`mesh_forward` 持久化转发规则（含 expose docker.sock），或直接开
`/doge/forward/<name>` 流打到暴露的本地服务。修复：

- **`waitPeerAdmitted` 门禁**（node.go）：三类数据流入口统一判定——**等待互验
  完成**（最长 `streamHandshakeGrace` 15s，覆盖对端握手刚完成、本侧 authed
  标记尚未落地的调度竞态），等不到即 reset。刻意不做"刚连接即放行"的宽限：
  复核指出宽限会被攻击者反复重连刷新（onConnected 每次重置计时），等价于
  持续可达 forward/expose 高危面；等待语义下未互验的流一帧都不会被处理。
- **握手限期**：`handshake` / `handleHandshakeStream` 补 `SetDeadline`
  （10s）——静默不答的对端到点被 establish 断开，未互验连接不再常驻。错 token
  回错值的仍由 hmacEqual 失败即时断连。**残余面**：未互验流会让 handler
  goroutine 等待至多一个 grace 周期（且随连接断开提前退出），可被 flood 短暂
  堆积 goroutine——不构成数据面暴露；relay 准入（`knownPeer`）仍是宽限语义，
  其暴露面只有中继槽位，维持原样。
- 测试：`TestPeerAuthedAndWait`（判定矩阵 + 等待语义）、
  `TestUnauthedStreamRejected`（裸 host 模拟静默攻击者：门禁等待超时后
  agent / file 流被 reset、无 envelope 分发，等待期不靠"刚连接"放行）、
  `TestRemoteToolBusy`（MsgToolCall 并发占满立即回 busy）。

文件传输同批加固（file-transfer-design §12 复审修复）：

- 文件流收发两侧补 `SetDeadline`（发起侧随 10 分钟 ctx，接收侧 15 分钟）——
  对端挂住流不再永久阻塞 `mesh_file` / `remote_agent(files=…)`。
- 单文件上限口径统一：`effectiveFileMax = min(fileMaxBytes, uploads
  MaxFileBytes)` 前置校验，100~256MiB 区间的文件回干净的 `too-large` 而非落盘
  失败的 `failed`；pull 路径同口径（此前无检查）。
- `hdr.Size` 为负拒绝；发送侧只发 size 字节（两次读文件之间的变更不污染对端
  ack 帧解析）；pull 响应端打开/复制失败 reset 流（截断不再被误读成
  bad-hash）；`ErrQuota` 映射为 ack code `quota`。
- MsgToolCall 执行加并发上限（8，溢出回 busy）；任务路径本就有 maxConcurrent。
- headless daemon 补接 uploads store（设计 §7.4 声称可收文件，此前未接线），
  清理随进程。
