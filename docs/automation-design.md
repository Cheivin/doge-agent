# doge 定时任务设计（scheduled automations）

状态：**P1–P3 已实施**（2026-10-16）。P1：存储与调度核心（automations.json
flock 读写 + 结构化调度引擎）+ 执行器（fork 会话 + runs.jsonl 台账 + reconcile
+ 会话回注）+ `automation` 工具 + 防递归两层 + `settings.automation` 节 +
TUI/Web 会话归组。P2：REST `/api/automations*` + Web 设置「定时任务」tab。
P3：`doge automation` CLI 子命令 + TUI `/automations` 命令 + 桌面 Automations
Service 与前端 tab 对齐。P4（绑定会话续跑、系统通知、mesh 远端执行）未实施。
评审结论（2026-10-16）：D1（进程内调度）、D2（JSON+flock）**已确认**，见 §14。
调度设计要点与防呆清单（§2.1）；本仓库现状盘点（§2.2）。
关联：`doge-agent-design.md`（分层铁律、F6 远端任务执行链）、
`mesh-design.md`（forwardsSig 跨进程热重载先例）、`web-api.md`（P2 API 规范落点）、
`file-transfer-design.md`（"配置写盘 → 热生效"先例）。

## 1. 目标与场景

一句话：**用户或模型创建持久化的定时任务，到点后 doge 自动拉起一个无人值守的
agent 会话执行任务 prompt，结果留存可查、可回注当前会话。**

| # | 场景 | 例子 | 主路径 |
|---|---|---|---|
| S1 | 会话内自然语言建任务 | "每 20 分钟提醒我喝水"、"每天早上 9 点拉一遍 RSS 存到 inbox.md" | 模型经 `automation` 工具创建（§7） |
| S2 | 一次性延时任务 | "2 小时后跑一遍 `go test ./...`，失败就把失败包名发我" | `delay` 调度（§5.2） |
| S3 | 绝对时刻循环 | "每周一 09:00 生成本周周报草稿" | `cron` 调度（§5.2） |
| S4 | 管理与观察 | 查看/启停/手动触发/看历史结果 | Web 面板 / CLI / 工具（P2/P3，§12） |

**非目标**（明确不做，理由见 §14）：

- 不做服务端闲时队列（doge 无后端，无对应物）；
- 不安装系统 crontab / launchd（方向相反——crontab 在权限层属 `RiskSystem` 级，见
  `internal/coding/permissions/risk.go`，无人值守默认拒绝）；
- 不做"进程退出后仍能触发"的守护（doge 无通用 daemon；边界见 §3.1"已知边界"）。

## 2. 现状盘点（调查结论）

### 2.1 调度设计要点

定时任务系统的通用形态：持久化任务定义 + 调度状态为唯一事实源，常驻宿主周期
轮询认领到期任务，派发到独立会话执行，执行终态回写运行台账并通知。以下是要点
设计（细节在后续各节展开）：

| 设计 | 要点 |
|---|---|
| 结构化调度规则为权威 | `scheduleRule{unit, interval, anchorAt, …}` 承载真实间隔，cron 字符串仅展示。原因：5 字段 cron 表达不了"每 50 小时"（字段步长上限），且 `*/N` 会对齐墙钟（11:46 建"每 10 分钟"会在 11:50 提前触发） |
| 相对时间由服务端时钟锚定 | 模型只传 `delayMinutes`，宿主用真实时钟算 `anchorAt + N*60s`。模型的"现在"是陈旧的——自算一次性 cron 刚过期会静默滚到下一年 |
| misfire 显式化 | 错过 >5min 记 `skipped` 不补跑；一次性任务错过即终态（通用重算会让一次性任务"复活"到下一周期） |
| runId 幂等台账 | `{id}:{scheduledAt}` / `{id}:manual:{uuid}`；派发重试复用同一 runId（认领期间 nextRunAt 不动） |
| 租约认领 + 僵尸回收 | `running + claimed_at`，10min 过期自动回收；优雅停机主动释放 |
| 退避分类 | transient（30s 指数退避→15min 上限、5 次；循环任务超限跳到下一周期而非 failed）/ permanent（直接 failed） |
| 防递归三层 | scheduled run 内禁止再建 automation（否则无人值守自我复制）；三层：工具从模型视野剔除 → 策略门 denylist → handler 按"本轮事实"兜底拒绝 |
| manual run 不占调度节奏 | 手动触发不推进 nextRunAt / 不计 maxRuns，但复用单飞锁防并发投递 |
| 契约与服务层双重校验 | 工具 schema 的互斥规则（recurring vs maxRuns、delayMinutes vs cron…）在 service 层重复实现，防旧客户端/内部调用绕过 |

定时系统的常见坑（实现须内建防呆）：

1. `null`（显式清除）与"未传"必须区分，`??` 合并把 null 当未传导致用户无法恢复普通 cron；
2. 切无限循环时要主动清残留 `maxRuns`，否则形成"无限循环+有限上限"矛盾隐藏态；
3. `enabled` 必须由 lifecycle 推导同步——出现过 completed 仍被认领；
4. 退避中的任务认领条件只看 `retry_at`，否则每 tick 立即重试击穿退避；
5. 一次性 delayMinutes 落成的兼容 cron 在错过窗口后会被通用引擎推出下一周期——必须 `isOneShot` 判定 + 终态；
6. 删除后迟到的回报不能复活任务（对 missing row 直接丢弃）；
7. sendPrompt 被接受 ≠ 在执行，单飞锁要覆盖排队+执行全程。

### 2.2 doge 侧可复用件

- **headless 执行链（最佳同构）**：mesh 远端任务 `runTask`
  （`internal/mesh/libp2p/agent_handler.go:213-338`）——fork 独立会话 →
  `RegisterActive` → `Subscribe(…, ConsumerHeadless)` → `go sess.Prompt(...)` +
  超时/中断 select → `resultFromSession` 提取结果。定时任务到点执行几乎照抄。
- **fork 家族**：`ForkRemote` / `ForkRemoteTo`（`agent_session.go:1116-1177`）按
  readonly/write/full 物理裁剪工具集（`remoteToolSet` :1187），且源会话在跑也能 fork。
- **无人值守权限现成**：不装 AskHandler 的会话 `ask` 自动走 `autoDecide` 风险分级
  （`permissions/decider.go` resolveAsk→autoDecide），`SetPermissionAskHandler` 对
  `Kind==remote` 已拒装（`agent_session.go:305-312`），automation 同构即可。
- **后台 goroutine 模板**：uploads `StartSweep`（`uploads/store.go:127-140`，先跑
  一轮 + ticker + ctx.Done 退出），挂载点在 `app.Assemble`（`app.go:174-178`），
  生命周期由 `App` 的 cancel 字段收口（`app.go:60-61`、`Close` :245-257）。
- **跨进程配置热重载先例**：mesh forward 的 CLI 写 `mesh.json`、运行节点 3s ticker
  内容 hash 对比热加载（`forward.go:300` 的 `forwardsSig`）。
- **action 多态管理工具模板**：`mesh_forward` 工具（`forward_tool.go:19-99`），
  一个 schema + `strParam/intParam` helper + 结果渲染函数。
- **会话回注通道**：mesh bridge 的 `injectFollowUp`（`bridge.go:493-519`）——
  `Agent.FollowUp(msg) + Continue()`，空闲即跑、运行中由 loop 停点 drain。
- **modernc.org/sqlite 已是直接依赖**（纯 Go 无 CGO）——但本设计选择 JSON 存储，
  理由见 §14 决策 D2。
- **配置读写封装**：`mesh/config.go` 的 Load（不存在返回默认不报错）/Save（0600）。

### 2.3 缺口

- **无常驻 daemon**：TUI/print 进程退出即一切停止；仅有的常驻形态是
  `doge mesh daemon`（无会话）与 `doge --web --no-tui`（headless web）。
- **无跨进程写协调**：配置文件无文件锁（单进程持有 + 写段互斥的约定）；
  多个 doge 进程并存（如 TUI + 桌面）时调度权归属需要机制。
- **无通知通道**：无系统通知封装（终端应用）。

## 3. 总体设计

```
 用户/模型                          任意常驻 doge 进程（TUI / --web / 桌面）
 ─────────────                      ─────────────────────────────────────────
 automation 工具（S1）──┐
 Web 面板（S4，P2）─────┤           ┌──────────────────────────────────┐
 CLI（S4，P3）──────────┼──写──→    │ automations.json   (定义+调度状态) │ ← flock 原子写
 手工编辑文件 ──────────┘           │ automations/runs.jsonl (运行台账)  │ ← append-only
                                   └───────────────┬──────────────────┘
                                                   │ 20s tick：reload(hash) → 回收僵尸 → 认领
                                                   ▼
                                          Scheduler goroutine（每进程一个，
                                          文件认领保证全局单飞，§5.3）
                                                   │ due
                                                   ▼
                                          Runner：ForkAutomation 会话（§6）
                                          无人值守 autoDecide · 无 automation 工具
                                                   │
                              ┌────────────────────┼─────────────────────┐
                              ▼                    ▼                     ▼
                     runs.jsonl 终态回写   来源会话 FollowUp 回注（§8）  Web 面板/下次 tick
```

### 3.1 宿主进程与已知边界（决策 D1）

调度器是**进程内 goroutine**（仿 StartSweep），挂在 `app.Assemble`——TUI、
`--web`（含 `--no-tui`）、桌面客户端都自然成为调度宿主。**多个进程并存时靠
文件认领单飞**（§5.3），同一任务同一时刻只被一个进程执行。

诚实边界（写进 README「局限与说明」）：**所有 doge 进程退出后任务不触发**，
恢复运行时按 misfire 语义处理（§5.4）。需要 7×24 触发的部署用
`doge --web --no-tui` 常驻（服务器/容器），或桌面客户端托盘驻留（0.11.0 关窗驻留，
天然常驻宿主）。print 模式（`-p`）跑完即退、不挂调度器；`mesh daemon` 无会话装配，
P1 不挂（开放问题 Q3）。

不引入独立 scheduler 进程/守护：doge 单二进制里 goroutine + 文件认领即可达到
"任一进程存活即调度"，少一个进程形态与 IPC 层。

## 4. 数据模型与存储

### 4.1 文件布局

```
~/.doge/agent/automations.json          # 任务定义 + 调度状态（合一，见决策 D3）
~/.doge/agent/automations/runs.jsonl    # 运行台账（append-only，每行一个 Run）
```

`automations.json` 沿用配置文件惯例：0600、`MarshalIndent` 可读格式、写盘走
tmp+rename 原子替换、**flock（`syscall.Flock`，LOCK_EX）保护跨进程读-改-写**。
无任务不建文件（惰性，与"配置文件需要手工编辑"的哲学一致——文件本身仍可手工编辑，
scheduler 每 tick hash 对比感知外部变更，照 `forwardsSig` 模式）。

### 4.2 Schedule（权威调度规则，结构化）

```go
// ScheduleKind 三选一；互斥校验在工具层与 store 层双重实现（防旧客户端/内部调用绕过）
const (
    ScheduleDelay    = "delay"    // 一次性：创建后 N 分钟触发（宿主真实时钟锚定）
    ScheduleInterval = "interval" // 循环：每 interval 个 intervalUnit，从 AnchorAt 推导
    ScheduleCron     = "cron"     // 循环：5 字段本地时区 cron，绝对墙钟时刻
)

type Schedule struct {
    Kind         string   // delay | interval | cron
    DelayMinutes int      // delay：1–525600；一次性（recurring 恒 false）
    Cron         string   // cron：5 字段（分 时 日 月 周），本地时区
    IntervalUnit string   // interval：minute | hour | day | week | month
    Interval     int      // interval：1–200
    At           string   // interval 日/周/月级的当日时刻 "HH:MM"（可空）
    Weekdays     []int    // interval=week 的星期（0–6，可空=每天）
    MonthDays    []int    // interval=month 的日期（1–31，可空）
    AnchorAt     int64    // 创建/修改锚点（Unix 秒）——interval 从锚点推导，不对齐墙钟
}
```

设计原则：**"每 N 单位"一律走 interval（锚点推导）**，cron 只用于"每天 9 点/
每周一 09:00"这类墙钟时刻——规避 `*/N` 提前触发与字段步长上限两个坑。一次性相对
时间只允许 delay（禁止模型换算成 cron）。文件里不存兼容 cron 字符串（doge 无历史包袱，不需要兼容层）。

### 4.3 Automation（定义 + 调度状态合一张表）

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` | string | `auto-<8 字节 hex>`，同 session id 风格 |
| `title` | string | 展示标题，**必须保留用户原话中的时间短语**（"每20分钟喝水提醒"而非"喝水提醒"） |
| `prompt` | string | 每次触发投递的完整指令，原文不包装（§6.3） |
| `cwd` | string | 执行工作目录（默认=创建时会话 cwd） |
| `model` | string | 可空=跟随全局默认模型 |
| `level` | string | 工具级：`readonly` / `write` / `full`（默认 `full`，照 remote task 分级裁剪，§6.1） |
| `schedule` | Schedule | 见 §4.2 |
| `recurring` | bool | delay 恒 false；interval/cron 恒 true |
| `maxRuns` | int | 仅有限次循环用（recurring=false 不存在于 doge 的 interval/cron——doge 的循环恒 recurring=true，maxRuns 限制总次数，0=无限） |
| `enabled` | bool | 入调度开关（由 status 推导同步，防 completed 仍被认领） |
| `status` | string | `active` / `paused` / `completed` / `failed` |
| `nextRunAt` / `lastRunAt` | int64 | Unix 秒 |
| `scheduledRunCount` | int | 定时触发计数（maxRuns 生命周期口径；manual 不计入） |
| `runCountTotal` | int | 含 manual 的展示总数 |
| `running` / `claimedAt` / `ownerPID` | | 单飞认领锁 + 租约（§5.3） |
| `retry` | object | 退避状态 `{attempts, retryAt, lastError}`（退避中认领只看 retryAt，防 tick 击穿退避） |
| `sourceSessionID` | string | 会话内创建时记录来源（回注通知用，§8） |
| `createdAt` / `updatedAt` | int64 | |

约束（store 层强制）：上限 50 条；delay ⇒ recurring=false；interval/cron ⇒
recurring=true；maxRuns>0 ⇒ recurring=true 且非 delay。

### 4.4 Run（运行台账）

runs.jsonl 每行一条，append-only：

```go
type Run struct {
    ID           string // "{automationID}:{scheduledAt}" 或 "{automationID}:manual:{8字节hex}"
    AutomationID string
    Trigger      string // schedule | manual
    ScheduledAt  int64  // 认领时的 nextRunAt（重试复用同一 runId 的依据）
    StartedAt    int64
    FinishedAt   int64
    Outcome      string // running | succeeded | failed | stopped | skipped
    SessionID    string // fork 出的会话 id（可跳转查看完整转录）
    Summary      string // 结果摘要（末条 assistant 文本截 4KB）
    Error        string
}
```

清理：每小时随 uploads sweep 同款 ticker 按 `historyRetentionDays`（默认 30 天）
裁剪；skipped 也记行（misfire 审计）。**Run 的完整转录就是会话 JSONL 本身**，
runs.jsonl 只存台账与摘要——不复制会话内容（对齐 mesh"结果卡片注入主会话"的取舍）。

## 5. 调度器

### 5.1 形态

`internal/coding/automation/scheduler.go`：`Start(ctx)` 仿 `StartSweep`——先跑
一轮、`time.Ticker` 20s、`ctx.Done` 退出；挂载在 `app.Assemble`，cancel 存入
`App`（照 `sweepCancel` 模式）。文件不存在或无 active 任务时 tick 只做一次
`os.Stat`（近乎零成本，惰性哲学）。外部变更（CLI/手工编辑/另一进程写入）靠
tick 首步的内容 hash 对比感知，无跨进程通知需求。

### 5.2 下次触发计算（`computeNextRun(schedule, from)`）

- **delay**：`AnchorAt + DelayMinutes*60`，一次性；
- **interval**：
  - minute：`AnchorAt + ceil((from−AnchorAt)/step)·step`（锚点推导，不对齐墙钟）；
  - hour：分钟对齐 `At`（缺省取 AnchorAt 的分钟），小时从锚点步进；
  - day / week / month：日历枚举搜索（day ≤36600、week ≤5220、month ≤1200 次，
    溢出守卫：2/30 滚月跳过该年），week/month 结合 `Weekdays`/`MonthDays`/`At`；
- **cron**：5 字段（分 时 日 月 周）本地时区，支持 `* , - /`；next 用逐字段进位
  回溯算法（实现约 150 行，无新依赖；备选 robfig/cron/v3，见开放问题 Q1）。
  **一律本地时区，不做 UTC 换算**。

### 5.3 认领（跨进程单飞）

每 tick 在 flock 内完成，顺序固定为：

1. **reload**：hash 变了则重读文件，重算受影响任务的 nextRunAt（用户改 schedule 即热生效）；
2. **终态推进**：`scheduledRunCount ≥ maxRuns`（有限次）→ `completed` + 出调度；
3. **回收僵尸**：`running=1 且 (now−claimedAt)>10min 或 ownerPID 不存活` → 复位可认领；
4. **认领 due**：`enabled && !running && status=active && (退避中 ? retryAt≤now : nextRunAt≤now)`
   → 原子置 `running=1, claimedAt=now, ownerPID=$PID` 写回；
5. 被本进程认领的任务交给 Runner（§6），goroutine 执行。

单飞语义：同一 automation 上次 run 未结束又到点 → 本轮跳过（记 skipped run，
nextRunAt 推进到下一周期）；全局并发上限 `maxConcurrentRuns`（默认 1）超限时
顺延本 tick（不记 skipped，下 tick 重试认领）。

### 5.4 misfire 与一次性终态

认领时发现 `nextRunAt ≤ now−5min`（进程全退了/休眠错过）：

- 记一条 `skipped` run（error="进程未运行，错过触发窗口"），不计 scheduledRunCount；
- 循环任务：nextRunAt 前推到下一个未来触发点；
- **一次性（delay）：错过即终态** `completed`（附 skipped run）——绝不能按
  delay 公式重算出"下一个周期"让提醒复活（一次性终态原则）。

### 5.5 退避

派发/执行启动失败分两类：**transient**（模型网关 5xx/超时、目录暂时不可达）→
`retryAt = now + min(30s·2^attempts, 15min)`，5 次为限，循环任务超限则放弃本轮
跳到下一正常周期（回 idle 清退避态），有限次任务转 `failed`；**permanent**
（cwd 已删除、模型不存在、auth 缺失）→ 直接 `failed` 终态停用。

## 6. 执行器（Runner）

### 6.1 会话构造

新增 `SessionKindAutomation = "automation"`（`session.go` 的 Kind 常量旁）与
`ForkAutomation(cwd, model, level)` —— 结构照 `ForkRemote`（`agent_session.go:1116`）：

- 工具集按 `level` 物理裁剪（复用 `remoteToolSet` 分级：readonly=只读、write=无
  bash、full=全量），ops 绑定 fork 自身的 cwd/会话身份；
- **ExtraTools 构建时排除 `automation` 工具**（防递归的核心，§10.2）；
- `SetPermissionAskHandler` 对 `Kind==automation` 拒装（照 remote 的
  `agent_session.go:305-312`）→ ask 全部走 `autoDecide` 无人值守语义；
- 会话显示名标注任务标题（Web 会话列表归组"定时任务会话"，照远端任务会话先例）。

### 6.2 执行流程（照 `runTask` 同构）

```
run := Run{ID: runID, Outcome: "running", …}          // 先写台账（fork 前落盘）
appendRuns(run)
fork := sess.ForkAutomation(task.Cwd, task.Model, task.Level)
fork.SessionManager().RegisterActive(fork)            // web 可实时观察
unsub, _ := fork.Subscribe(noopOrProgress, coding.ConsumerHeadless)
done := make(chan error, 1)
go func() { done <- fork.Prompt(task.Prompt) }()
select {
case <-done:                                          // 正常/模型侧错误结束
case <-time.After(runTimeout):  fork.Abort(); <-done  // 看门狗，默认 60min（0=不限）
case <-ctx.Done():            fork.Abort(); <-done    // 进程退出
}
outcome/summary = resultFromSession 同款提取           // 末条 assistant + StopReason
unregister + unsub + runs.jsonl 回写终态
```

启动时 reconcile：runs.jsonl 里 `running` 且所属进程已死的记录标 `stopped`
（error="宿主进程退出"）。任务本身不重跑
（认领时 nextRunAt 已推进，错过即过）。

### 6.3 prompt 投递

**任务 prompt 原文就是用户消息，不包装**——prompt 必须在创建时就写成自包含指令（工具描述硬约束，§7）。
执行上下文（cwd/模型/工具级）全部来自 Automation 字段，不进 prompt。

## 7. 模型侧工具

### 7.1 形态：单工具 action 多态

不拆成 create/list/update/delete 四个独立工具，照 doge 自己的
`mesh_forward` 惯例——一个 `automation` 工具（`internal/coding/automation/tool.go`，
schema/Execute 仿 `forward_tool.go`）：

```
automation(action, …)
  action=create   → {title, prompt, delayMinutes?|cron?|intervalUnit+interval?,
                     cwd?, model?, level?, maxRuns?}
  action=list     → 概要表（id/标题/调度/下次/状态/计数）
  action=get      → {id} 单条详情 + 最近 3 次 run
  action=update   → {id, …可变字段}（schedule 改动即重算 nextRunAt，AnchorAt 重锚）
  action=delete   → {id}
  action=enable|disable → {id}（暂停/恢复；enable 仅 paused 可恢复）
  action=run      → {id} 手动立即触发（不占调度节奏，复用单飞锁）
  action=history  → {id, limit} 运行台账
```

工具注入点：`app.Assemble` 的 ExtraTools（`app.go:203`）。**无 automation 定义
文件时工具仍注册**（list 返回空、create 可用）——工具的存在即功能的可发现性，
惰性成本只是 schema 约 300 token（与 mesh_forward 同级，上下文成本可接受；
若嫌大可后续并入 `guide` 式按需加载，开放问题 Q4）。

### 7.2 工具描述硬规则（写入 description）

1. **相对时间必须 `delayMinutes`**："8 分钟后 / 2 小时后"绝不允许换算成 cron 或
   绝对时刻——模型的"现在"是陈旧的；自算的一次性 cron 刚过期会静默滚到下一年。
2. **"每 N 分钟/小时"用 interval**（宿主从创建时刻锚定推导）；**cron 只用于用户
   点名的墙钟时刻**（"每天 9 点"、"每周一早上"），本地时区，不做 UTC 换算。
3. **title 保留用户原话里的时间短语**（"每20分钟喝水提醒"，不能是"喝水提醒"）。
4. **prompt 写成完整自包含指令**：无人值守执行，没有当前对话上下文，不能引用
   "上面提到的文件"、"刚才的输出"；需要的路径/规格/验收标准全部写进 prompt。
5. **prompt 不得指示 run 创建/修改/删除 automation**（防自我复制；执行侧另有
   物理裁剪兜底，见 §10.2）。
6. recurring 与 maxRuns 互斥语义：循环任务默认无限，要限次数传 maxRuns；
   delay 恒为一次性。
7. 尊重用户给的精确时刻，不加抖动、不提前。

### 7.3 双重校验

§7.1 括号里的互斥规则（delay vs cron vs interval 三选一、intervalUnit 与 interval
配对、maxRuns 仅循环、delayMinutes ≤ 525600、interval ≤ 200）在工具参数校验与
store 写入前**重复实现**——防未来 Web/CLI 入口或内部调用绕过工具层。

## 8. 结果留存与通知

| 通道 | 时机 | 实现 |
|---|---|---|
| runs.jsonl 台账 | 每次终态 | §4.4，audit 与面板数据源 |
| 会话回注 | run 结束时来源会话仍活着 | `injectFollowUp` 模式：`FollowUp(结果卡片) + Continue()`（`bridge.go:493-519`）；卡片含任务标题、outcome、摘要、session id（可 `doge --open` 或 Web 切过去看全程） |
| Web 面板 | P2 | Automations 面板：下次运行/最近结果/历史（§12） |
| 系统通知 | P3 可选 | 桌面客户端托盘气泡（wails 侧）或 TUI 终端 bell |

回注是 best-effort：来源会话已关/在跑，直接跳过（台账里已有完整记录），**不因
通知失败影响调度结算**。

## 9. 配置

`settings.json` 新增 `automation` 节（全局+项目合并照旧，但**调度器与存储只在
全局生效**——定时任务本身带 cwd，不随项目拆分）：

```json
{
  "automation": {
    "enabled": true,
    "maxConcurrentRuns": 1,
    "runTimeoutMinutes": 60,
    "historyRetentionDays": 30,
    "maxAutomations": 50
  }
}
```

## 10. 安全与权限

### 10.1 无人值守执行

automation run 是 headless 会话：不装 AskHandler → `ask` 决策全部走 `autoDecide`
风险分级（readOnly 放行、projectLocal/system/network/unknown 按用户配置，默认
projectLocal=拒）、`humanOnly` 命令一律拒、内置高危库照拒。**这套语义现成且久经
远端任务检验，零新代码**。用户想要更宽的自动化，调 `permissions.json` 的
autoDecide 即可，与远端任务共享同一套心智模型。

### 10.2 防递归（doge 版两层）

无需"可见性剔除 → 策略门 → handler 兜底"的三层防护：**doge 的工具集是
per-session 物理构造的**，两层即可：

1. **物理裁剪**：`ForkAutomation` 构造工具集时排除 `automation` 工具——模型
   视野里根本没有该工具（对齐 denylist 层，且更彻底）；
2. **store 层断言**：store 的 create/update/delete 入口检查调用来源——
   `Kind==automation` 的会话上下文直接拒绝（兜底未来入口绕过裁剪）。

automation 轮照常保留 `mesh_forward` 等其它工具（只禁自我复制，不禁正常工作）。

### 10.3 其它边界

- 任务 `cwd` 不存在：创建时校验 + 运行时 permanent 失败；
- prompt 是持久化数据，run 中模型读到的一切照常过权限层，无额外信任提升；
- `automation` 工具本身过 `checkToolPermission`，可被 `permissions.json` 的
  tools 规则 deny/ask；
- 文件权限 0600（automations.json 含 prompt 可能带敏感上下文）。

## 11. P1 接入点清单（file → 改动）

| 文件 | 改动 |
|---|---|
| `internal/coding/automation/store.go` | 新增：Automation/Schedule/Run 类型、flock 读-改-写、双重校验、hash 感知 reload |
| `internal/coding/automation/schedule.go` | 新增：computeNextRun（interval 锚点引擎 + cron 解析器）、一次性终态判定 |
| `internal/coding/automation/scheduler.go` | 新增：Start(ctx) 20s tick（reload→终态推进→僵尸回收→认领）、misfire、退避 |
| `internal/coding/automation/runner.go` | 新增：fork 执行链 + runs 台账 + reconcile + 会话回注 |
| `internal/coding/automation/tool.go` | 新增：`automation` 工具（action 多态，仿 forward_tool） |
| `internal/coding/session.go` | `SessionKindAutomation` 常量 |
| `internal/coding/agent_session.go` | `ForkAutomation`；`SetPermissionAskHandler` 拒装 automation kind |
| `internal/coding/config.go` | `AutomationsFile()`/`AutomationsDir()`；`EnsureAgentDir` 补目录 |
| `internal/coding/settings.go` | `Automation` 节字段 + 默认值 |
| `internal/app/app.go` | Assemble 挂 scheduler.Start（cancel 字段收口）、ExtraTools 注入工具 |
| `internal/web/`（会话列表） | automation 会话归组显示（照远端任务会话） |

P1 验收：单进程内 create→tick 认领→fork 执行→台账+回注闭环；双进程并存单飞；
进程重启 reconcile；一次性错过终态；`-race` 全绿。

## 12. P2/P3 界面

| 面 | 内容 | 参照 |
|---|---|---|
| Web API（P2） | `GET/POST /api/automations`、`PATCH/DELETE /api/automations/{id}`、`POST …/{id}/run|enable|disable`、`GET …/{id}/runs`；web-api.md 新增 §4.14 | meshapi.go / View 接口注入（nil=面板隐藏） |
| Web 面板（P2） | 设置中心新 tab「定时」：任务卡片（标题/调度徽标/下次/最近结果/启停/立即运行）+ 历史 | MCP 面板四件套（openMcp/renderMcpModal/onMcpAction） |
| guide.md（P2） | `<!-- module: automations -->` 用法 | 内嵌 guide 模块 |
| README（P2） | 新章节「定时任务」（Mesh 后） | — |
| TUI（P3） | `/automations` overlay：列表 + 启停/删除 | `/mesh` overlay + cmdMenuItems |
| CLI（P3） | `doge automation list/add/rm/enable/run`（跨进程写文件，调度器 tick 热生效） | mesh 子命令模式（自建 FlagSet） |
| 桌面（P3） | `desktop/services/automations.go` + 前端 tab | skills.go 同构 |

## 13. 分期实施

| 阶段 | 范围 | 产出 | 状态 |
|---|---|---|---|
| P1 | 存储与调度核心 + 执行器 + `automation` 工具 + 防递归 + 台账 + 会话回注（§4–§8、§11） | 任意常驻 doge 进程内定时任务闭环，模型可创建/查询/管理 | 未开始 |
| P2 | Web API + 面板 + guide/README 文档（§12） | 可视化管理与历史 | 未开始 |
| P3 | TUI `/automations` + CLI 子命令 + 桌面对齐（§12） | 全界面覆盖 | 未开始 |
| P4（可选） | 绑定会话续跑（sourceSession 的会话 resume 而非新会话）、系统通知、mesh 远端执行（任务派给远端节点跑） | 增强 | 评估 |

## 14. 设计决策（D1/D2 已确认 2026-10-16；其余为建议，待评审）

| # | 决策 | 理由 | 放弃的方案 |
|---|---|---|---|
| D1 ✅ | 调度器=进程内 goroutine，文件认领单飞；不引入独立 daemon | doge 单二进制无 IPC 层负担；TUI/Web/桌面任一存活即调度；桌面托盘驻留天然覆盖常驻场景 | 独立 scheduler 进程（多一个进程形态与 IPC 层负担）；systemd/cron 系统集成（违背单二进制自包含） |
| D2 ✅ | JSON 文件 + flock，不用 SQLite（尽管 modernc.org/sqlite 已是直接依赖） | 配置文件哲学：人可读、可手工编辑、可备份、可 git；2–3 进程 × 20s 级轮询的并发度 flock 足够；避免"用户资产（定义）"与"状态"两处事实源 | SQLite + WAL（并发更强但引入二进制状态文件，doge 负载用不上） |
| D3 | 定义与调度状态合一在 automations.json（不拆两个文件） | 手工编辑/备份一个文件即全部；认领字段的写频 = 每 run 两次（认领+结算），远低于需要拆分的量级 | 定义/状态分文件（一致性成本） |
| D4 | 循环任务恒 recurring=true + 可选 maxRuns（不引入"recurring=false 有限次"组合） | 该组合态易产生"无限循环+有限上限"矛盾隐藏态；doge 干净起步：一次性=delay，循环=interval/cron（maxRuns 限总次数） | 完整的四态组合 |
| D5 | 单工具 action 多态（`automation`），不拆 create/update/delete/list | doge 既有惯例（mesh_forward/code/mcp 代理均 action 多态），上下文占用减半 | 四工具拆分（CLI 契约风格） |
| D6 | 防递归=工具集物理裁剪 + store 断言两层 | doge 工具集 per-session 构造，裁剪即根除；无需协议信封层 | 三层防护（为全局工具注册架构所设） |
| D7 | run 的完整记录=会话 JSONL 本身，台账只存摘要 | 会话即转录，天然可跳转可导出；不复制内容 | 独立 run transcript 存储 |
| D8 | 不做闲时队列 | 无服务端组件，语义不成立 | — |

## 15. 开放问题

| # | 问题 | 倾向 |
|---|---|---|
| Q1 | cron 解析器：手写 ~150 行（`* , - /` + 进位 next）vs 引入 robfig/cron/v3（MIT、零传递依赖） | 手写：只需 parse+next 两个函数，可控无依赖；robfig 功能过剩（daemon/chain 等） |
| Q2 | delay/interval 粒度认领窗口：tick 20s 意味着分钟级任务的触发误差 ±20s | 可接受；要求精确到秒的场景本就不适合 agent 任务 |
| Q3 | `mesh daemon` 是否挂调度器（服务器常驻形态，但无会话装配） | P1 不挂；若用户有"服务器跑定时任务"需求再评估（需在 daemon 里 Assemble 一个无 UI 会话工厂） |
| Q4 | `automation` 工具 schema 约 300 token 常驻 system prompt 是否可接受 | 可接受（mesh_forward 同级）；否则并入按需加载 |
| Q5 | 桌面客户端（独立进程）与 CLI 进程并存时的认领公平性：抢跑/饥饿 | flock 认领天然先到先得；20s 级任务无饥饿场景，不引入领导选举 |
| Q6 | 任务执行时的 uploads 附件引用（prompt 引用上传文件）暂不支持 | P1 不做；需要时在 Automation 加 `attachments []uploadID`，fork 后 place 到 cwd |
