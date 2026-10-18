# doge 首跑配置引导设计（first-run setup / 密钥管理）

状态：**P1 已实施**（2026-10-16，同 automation-design 一批）。实施范围：
setup 信号（/api/config `setup` 节）+ `GET /api/config/providers` + EnvKeys
导出 + Web/桌面首跑向导 + 设置「密钥」tab + 默认模型下拉（双写）+ 模型下拉
"未配置密钥"徽标 + `MissingAPIKeyError` 文案 + `--list-models` 只列可用 +
桌面 shim 接线。P2（key 真实验证）/P3（`doge setup` CLI）未实施。
关联：`automation-design.md`（同批评审，同批实施）；`web-api.md` §4.7（config
端点既有规范）；`doge-agent-design.md`（配置体系全景）。

## 1. 目标与场景

一句话：**新用户（新机器）打开桌面端或 Web 控制台，即被引导完成
"选服务商 → 粘贴 API key → 选默认模型"三步，写盘后立即可对话；
密钥与默认模型从此在设置中心可管，全程无需手工编辑任何文件。**

| # | 场景 | 现状体验 | 目标体验 |
|---|---|---|---|
| S1 | 全新安装，无任何 key | 界面正常启动，模型下拉靠兜底显示一项（openai/gpt-5.2），发第一条消息才报 `MissingAPIKeyError`，且文案只指路环境变量与 `--api-key` | 首跑向导自动出现，三步配完即用 |
| S2 | 有 key（环境变量或 auth.json）但没设默认模型 | 启动兜底 openai/gpt-5.2——用户 key 若是 deepseek 的，首次请求即失败 | 轻量提示"选个默认模型"（横幅/一步向导，可跳过） |
| S3 | 自定义 OpenAI 兼容网关（聚合站） | 要手工编辑 models.json + auth.json 两个文件 | 向导"自定义服务商"路径：baseUrl + key + 模型 id 一次配完 |
| S4 | 想跳过（打算用环境变量 / local 端点 / 稍后再配） | — | 明确的"跳过"出口；设置中心随时重入 |

**非目标**：不做账号体系/登录（桌面无登录门；Web 的 token 门保持现状）；不做
key 的云同步；不做多套 profile 切换。

## 2. 现状盘点（调查结论）

后端能力**基本齐备**，缺口集中在前端入口与引导逻辑：

### 2.1 已有、可直接复用

- **写 auth.json + 热重载**：`AuthStore.SetAPIKey/DeleteAPIKey`（`internal/coding/auth_store.go:80-93`，
  0600、自动建目录）；Web `POST /api/config/auth` / `DELETE /api/config/auth/{provider}`
  （`internal/web/config.go:152-195`，写完自动 `ReloadConfig` 立即生效）；
  桌面 Go 侧 `ConfigService.AuthSet/AuthDelete`（`desktop/services/config.go:123-153`）同款。
- **读打码密钥**：`GET /api/config/auth`（maskKey 末 4 位，永不回明文）+ 桌面 `AuthGet`。
- **写默认模型**：`PUT /api/config/settings` 与桌面 `SettingsPut` 可写
  `defaultProvider/defaultModel`（启动时生效，响应 `restartRequired:true`）。
- **切当前会话模型**：`POST /api/model`（`api.go:165-187`）/ 桌面 `SessionService.SetModel`
  ——补上"引导完立即用"的最后一环，无需重启。
- **目录数据**：内置 9 provider + 20 模型（`models_catalog.go:14-136`）；
  `GET /api/models?all=1` / `SessionService.Models(true)` 列全目录；
  `AvailableModels()`（`ai/models.go:146`，能解析到 key 或本地端点）即"配完立即可用"判定。
- **自定义模型**：`POST /api/config/models` 单模型 upsert + 热重载（config.go:259-292）。

### 2.2 缺失

1. **两个前端都没有任何配 key 的 UI**：Web 前端从未调用 `/api/config/auth` 与
   `/api/config/settings`（grep 无匹配）；模型表单（web/desktop 的 `openModelForm`）
   没有 key 字段。桌面前端 shim（`desktop/frontend/app.js:51-159`）连
   `/api/config`、`/api/config/auth`、`/api/config/settings` 都未接线——Go 侧
   方法齐备但前端调不到（落到"未接线的 API 调用"分支）。
2. **无首次运行检测**：`app.Prepare`/`desktop/main.go` 没有"配置不存在→引导"逻辑；
   README 明文"首次运行只创建目录结构，配置文件需要手工编辑"——本设计推翻这条。
3. **设置面板只管自定义模型**：`loadSettingsModels` 只展示 models.json 里配过的
   provider（内置未配置的不出现），无密钥区、无默认模型设置。
4. **无 key 的界面误导**：模型下拉靠兜底追加当前活跃模型（`backend.go:655-665`），
   看起来"有模型"实则不可用；`MissingAPIKeyError`（`ai/auth.go:70-79`）文案只提
   环境变量与 `--api-key`，不提 auth.json 也不提设置界面（三端共用此文案）。
5. 小项：provider 的环境变量名表 `defaultEnvKeys` 是私有 map，未导出给前端
   （引导页想显示"也可用 OPENAI_API_KEY 环境变量"需导出）；`--list-models` 列
   全目录不按可用性过滤（`main.go:341` 用 `AllModels`，名不符实）。

## 3. 总体设计

### 3.1 就绪判定：从真实状态推导，不设哨兵文件

后端在 `GET /api/config` 总览响应中新增 `setup` 节（桌面 `Overview` 同步）：

```json
"setup": {
  "ready": true,            // 有可用模型（AvailableModels 非空）且已设默认模型
  "hasUsableModel": true,   // len(AvailableModels()) > 0（key 或本地端点任一）
  "hasDefaultModel": false  // settings.defaultProvider + defaultModel 均非空
}
```

前端据 `setup` 决定形态：`!hasUsableModel` → 首跑向导（S1）；
`hasUsableModel && !hasDefaultModel` → 一步式"选默认模型"横幅（S2，可关）；
`ready` → 无感。不写"已完成引导"标记文件——**就绪与否永远由配置真实状态推导**，
清掉配置或换机器自然重现引导，状态永不漂移。

### 3.2 首跑向导（桌面 / Web 同一流程，两份前端各自实现）

```
┌ 首跑向导（全屏遮罩，可跳过）─────────────────────────────┐
│ ① 选服务商                                               │
│    [DeepSeek] [通义千问] [智谱 GLM] [Z.ai] [Moonshot]     │
│    [OpenAI] [Anthropic] [OpenRouter]                     │
│    ── 或 ──                                              │
│    [自定义 OpenAI 兼容服务]（baseUrl + 模型 id，S3 路径） │
│ ② 粘贴 API key                                           │
│    密码框（掩码）·"存入 ~/.doge/agent/auth.json（0600，  │
│    仅本机）· 也可改用环境变量 OPENAI_API_KEY"（按所选     │
│    provider 显示对应变量名，需后端导出 EnvKeys）          │
│ ③ 选默认模型                                              │
│    该 provider 的内置模型单选（来自 ?all=1 目录）          │
│    自定义路径：模型 id + API 类型（默认 openai-completions）│
│ ④ 完成                                                   │
│    POST /api/config/auth          → key 落盘 + 热重载     │
│    [自定义路径先 POST /api/config/models → 模型落盘]      │
│    PUT  /api/config/settings      → 默认模型（下次启动）   │
│    POST /api/model                → 当前会话立即切换       │
│    关闭向导 → 模型下拉已列出可用模型                       │
└──────────────────────────────────────────────────────────┘
```

关键点：

- **向导是纯前端编排**——四个写入口全部是既有 API，后端零新增写路径；
- **双写默认模型**：settings 管下次启动、`POST /api/model` 管当前会话， bridging
  既有 `restartRequired` 语义，完成后无重启无等待；
- **不做 key 有效性验证请求**（P1）：写入后拉一次 `GET /api/models`，所选模型
  出现在可用列表即"本地配置生效"；key 是否真的对，等首次真实请求检验。真验证
  （发最小测试请求）为 P2 可选，各家端点不一（OpenAI 兼容有 `GET /v1/models`，
  Anthropic 无便宜端点），不值得 P1 复杂化；
- **跳过**：向导右上角"稍后再配"；`local` 端点用户与环境变量用户有明确出口。

### 3.3 密钥管理与默认模型进设置中心（长期能力，不止首跑）

首跑向导只解决"第一次"，日常增删改 key 是持续需求（现状改个 key 要手工编辑
auth.json）：

- 设置中心新 tab「密钥」（或并入模型 tab 顶部的"服务商与密钥"区，倾向前者——
  与模型管理职责分明）：
  - provider 行：名称 + baseUrl + key 掩码（`GET /api/config/auth`）+ 更新/删除
    （`POST` / `DELETE`，热重载即生效）；
  - "＋ 添加密钥"：选内置 provider 或输入自定义 provider 名 → 粘 key；
  - 行内徽标：该 provider 当前可用模型数（`?all=1` 目录交叉）。
- 模型管理 tab 顶部加"默认模型"下拉（可用模型中选）→ `PUT /api/config/settings`，
  提示"当前会话不受影响，已同步切换/下次启动生效"（同 3.2 双写策略）。
- 桌面端为上述全部先补 shim 接线（`/api/config`、`/api/config/auth` GET/POST/DELETE、
  `/api/config/settings` GET/PUT 各一行映射，模式同 `/api/config/models`）。

### 3.4 错误文案指路（三端共用一处修复）

`MissingAPIKeyError`（`ai/auth.go:70-79`）改为（以 openai 为例）：

> no API key for provider "openai". 在设置 → 密钥中添加，或设置环境变量
> OPENAI_API_KEY，或将密钥写入 ~/.doge/agent/auth.json（见 /help）。

桌面/Web 的转录错误渲染天然带出此文案；TUI 同。另将无 key 时的模型下拉兜底项
标注"未配置密钥"徽标（避免"看着有模型"的误导）。

## 4. 后端改动清单（很小）

| 文件 | 改动 |
|---|---|
| `internal/web/backend.go` | `configOverview` 组装处加 `setup` 节（`hasUsableModel`/`hasDefaultModel`/`ready`） |
| `internal/web/config.go` | `handleConfigOverview` 响应加 `setup`；（可选）加 `GET /api/config/providers`：内置 provider 列表 + baseUrl + 是否需要 key + 环境变量名 |
| `internal/ai/models.go` 或 `models_catalog.go` | `RegisterBuiltinProviders` 填 `Provider.EnvKeys`（`defaultEnvKeys` 表搬运），供上项导出 |
| `desktop/services/config.go` | `Overview` 响应同步 `setup` 节 |
| `desktop/frontend/app.js` | shim 接线 `/api/config`、`/api/config/auth`、`/api/config/settings` |
| `internal/ai/auth.go` | `MissingAPIKeyError` 文案改写（§3.4） |
| `cmd/doge/main.go` | `--list-models` 改用 `AvailableModels`（标注不可用项或只列可用，名实相符） |

## 5. 前端改动清单

| 端 | 改动 | 参照 |
|---|---|---|
| Web `internal/web/frontend/` | 向导组件（全屏遮罩 + 四步卡，`setup.ready` 判定入口）；设置 tab「密钥」；模型 tab 默认模型下拉；模型下拉"未配置密钥"徽标 | 设置 tab 容器（index.html:88-91）、`openMcp` 面板四件套、`openModelMenu` |
| 桌面 `desktop/frontend/` | 同上四项（两份前端代码相似，逐项对齐）+ shim 接线 | 同上 |

向导组件保持原生 JS 零构建风格（一个 overlay div + 步骤状态机 + 三四个 API 调用），
不引框架。

## 6. 安全

- key 输入用密码框；显示永远走 `maskKey`（末 4 位）；任何 API 不回明文；
- auth.json 0600（`persistLocked` 现状）；key 不写日志、不进 telemetry（本就无 telemetry）;
- Web 端向导走既有 `requireAuth`（配好 web token 前提下才出现向导——token 门是
  前置安全层，不受影响）；桌面无端口无 token，本机即边界。

## 7. 分期实施

| 阶段 | 范围 | 产出 | 状态 |
|---|---|---|---|
| P1 | `setup` 信号 + 桌面 shim 接线 + 设置中心「密钥」tab + 默认模型下拉 + 首跑向导（内置 provider 路径 + 自定义网关路径）+ 错误文案 + `--list-models` 修正 | 桌面/Web 开箱三步即用；key 与默认模型全程 UI 可管 | 未开始 |
| P2 | key 真实验证（可选按钮，best-effort 测试请求）；`GET /api/config/providers` 独立端点与 EnvKeys 导出（若 P1 用内联方式则顺延） | 验证体验增强 | 未开始 |
| P3 | `doge setup` CLI 交互向导（TUI 内跑同流程，服务无界面场景）；guide.md/README 配置章节改写（删"配置文件需要手工编辑"表述） | 全端覆盖 | 未开始 |

## 8. 设计决策（建议，待评审）

| # | 决策 | 理由 | 放弃的方案 |
|---|---|---|---|
| O1 | 就绪判定从真实配置推导（AvailableModels + settings 默认模型），不设哨兵文件 | 状态永不漂移；清配置/换机自然重现引导 | "已完成引导"标记文件（与真实状态会脱节） |
| O2 | 向导=纯前端编排，复用四个既有写 API，后端只加只读 `setup` 信号 | 写路径零新增=零新攻击面；三端（Web/桌面/未来 CLI）共享同一套 API 语义 | 新增 `/api/setup` 聚合写端点（多一个写路径，无收益） |
| O3 | 密钥管理做成设置中心常驻能力，向导只是入口之一 | 改 key/换 key 是日常需求，不该锁死在首跑；也修复"设置面板只认自定义模型"的既有缺口 | 只做一次性向导 |
| O4 | 默认模型双写：settings（下次启动）+ `POST /api/model`（当前会话） | 既有 `restartRequired` 语义不动，向导完成后零重启零等待；两 API 均已存在 | 改 settings 为热生效（动启动时读取的既有契约，影响面大） |
| O5 | P1 不做 key 有效性验证请求 | 各家端点不一致，最小请求也有成本/副作用；可用列表出现即本地生效，key 对错让首次真实请求检验 | P1 即做测试连接 |
| O6 | 向导同时覆盖自定义网关路径（baseUrl+key+模型 id） | 聚合站是 doge 主要用户群（models.json 的 impersonate/sessionAffinity 皆为此设）；三个既有 POST 串联，成本低 | P1 只做内置 provider |

## 9. 开放问题

| # | 问题 | 倾向 |
|---|---|---|
| Q1 | 「密钥」独立 tab vs 并入模型 tab 顶部 | 独立 tab：职责清楚，模型 tab 已很长 |
| Q2 | 首跑向导是否也进 TUI（`/setup` 命令或启动横幅） | P3 与 `doge setup` CLI 一起评估；TUI 用户环境变量习惯强，优先级低 |
| Q3 | `hasUsableModel` 把环境变量里的 key 也算上（ResolveAPIKey 全链）——用户用环境变量时不弹向导 | 正确行为：环境变量可用=已就绪，不打扰 |
| Q4 | 多 provider 都有 key 时向导默认选中谁 | 按内置目录顺序取第一个有 key 的 provider；用户可改 |
