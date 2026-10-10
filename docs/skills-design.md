# doge Skills 闭环设计：渐进式披露 + 三条调用路径

状态：已实现（0.9.0）；API 与 WebUI 面板已适配（见 §7），
      含单元/集成测试与真实模型端到端验证
前置调研：Claude Code / OpenAI Codex / Hermes Agent / DeepSeek Harness（见 §2）

## 1. 目标

把 doge 的 skills 功能从"半截"补齐成闭环：

- 模型能**发现**已安装的技能（系统提示里只给 name + description，渐进式披露）。
- 模型能**主动加载**技能正文并照着执行（`skill` 工具）。
- 用户能**显式触发**技能且内容必然送达（`/skill:name` 直注正文）。
- 模型/用户发出的 `<skill name location/>` 调用块被 harness 识别并回填内容。

## 2. 调研结论（行业现状）

调研了 Claude Code、OpenAI Codex CLI、Hermes Agent（NousResearch/hermes-agent）、
DeepSeek Harness（deepseek-ai/deepseek-harness）的 skills 机制，结论高度收敛：

1. **磁盘格式统一**：都是"技能目录 + SKILL.md"，frontmatter 必填 `name`（kebab-case）
   与 `description`，可选 `disable-model-invocation`、`allowed-tools` 等（agentskills.io 规范）。
2. **渐进式披露是主流共识**：启动时只把 name + description（约 100 tokens）放上下文，
   正文（SKILL.md）按需加载——Claude Code 用 Skill 元工具，Codex 目录进 developer 消息，
   DeepSeek Harness 用 `<available_skills>` 目录 + `skill` 工具，Hermes 用 skills_list/skill_view。
3. **模型调用 = 结构化工具调用**，不是原始文本块：`skill(command=name)` /
   `skill(name=name)`，harness 读 SKILL.md 把渲染正文作为一条消息注入对话，正文在会话内常驻。
4. **用户显式调用 = 斜杠命令** `/skill-name`，harness 把完整正文并进用户消息。
5. **`disable-model-invocation: true`** 关闭模型自动加载（description 也不进上下文），
   只留用户显式入口。
6. **`Skill` 工具的权限/校验**：unknown / disabled 都会给出明确错误并列出可用技能。

doge 原实现（v0.1.0 起）只完成了发现 + 广告（`<available_skills>` name+description+location）
+ `/skill:` 展开为惰性 `<skill name location/>` 块；`SkillBlockRe` 从创建起就是死代码，
`Skill.Content` 解析后无人消费，系统提示也没有任何使用说明——即"技能正文永远不会到达模型"。

## 3. 闭环设计

三条调用路径全部闭合，模型主动或用户显式都能把技能正文送进上下文：

```
发现：SkillsDirs → LoadSkills → <available_skills>(name+description+location)
                      │
      ┌───────────────┼───────────────────────┐
      │               │                       │
 ① skill 工具     ② /skill:name 斜杠     ③ <skill name location/> 块
 (模型主动)       (用户显式)              (模型/用户发出的调用块)
 返回正文         正文直注用户消息         prepareNextTurn 扫描 → FollowUp 注入
```

- **① `skill` 工具**（新增 `internal/coding/skill_tool.go`）：`skill(name=...)` 返回该技能
  完整正文 + location + 资源提示；unknown 列出可用技能；`disable-model-invocation` 拒绝并
  提示用户用 `/skill:`。有技能时注册进会话工具集。
- **② `/skill:` 直注**（`ExpandSkillCommand` 改造）：展开为用户消息，顶部保留
  `<skill name location/>` 标记，随后直接内嵌完整正文 + 参数，不依赖模型二次配合。
- **③ 块解析回填**（新增 `ResolveSkillBlocks` + `AgentSession.prepareNextTurn`）：
  每轮 turn 结束后扫描 assistant 文本里的 `<skill name="..." location="..."/>` 块，
  按 name 在已加载技能里解析（绝不按模型给的任意路径读文件），正文作为 FollowUp
  用户消息注入下一轮。去重、跳过 unknown/disabled。
- **提示词说明**（`FormatSkillsForPrompt` 改造）：`<available_skills>` 末尾加一行
  "用 skill 工具按 name 加载完整说明"；disabled 技能不进列表。

安全约束：`ResolveSkillBlocks` 只按 name 从会话已加载技能解析，location 仅作展示，
不成为任意文件读取通道。

## 4. 实现清单

| 文件 | 改动 |
|---|---|
| `internal/coding/skill_tool.go` | 新增：`skill` 工具定义 |
| `internal/coding/skills.go` | `FormatSkillsForPrompt` 加调用说明；`ExpandSkillCommand` 内嵌正文；新增 `ResolveSkillBlocks`/`renderSkillInvocation` |
| `internal/coding/agent_session.go` | 有技能时注册 `skill` 工具；`prepareNextTurn` 扫描调用块并 FollowUp 注入 |
| `internal/coding/skills_loop_test.go` | 新增：块解析/工具/提示词/命令/集成注入/工具注册 6 个测试 |

未改通用 agent 循环（`internal/agent`）：`prepareNextTurn` 钩子已存在，块解析全部收在
coding 包内，零侵入。

## 5. 测试

`go test ./...` 全绿（含 coding / agent / ai / mesh / mcp / web）。新增：

- `ResolveSkillBlocks`：单块、去重、unknown/disabled 跳过、不带回读标记。
- `SkillTool`：返回正文、unknown 报错列可用技能、disabled 拒绝。
- `FormatSkillsForPrompt`：含调用说明、disabled 不广告。
- `ExpandSkillCommand`：标记 + 正文 + 参数。
- `TestAgentSessionSkillBlockInjection`：mock 模型输出 `<skill>` 块 → 正文以用户消息
  注入 → 下一轮模型拿到内容（集成级）。
- `TestSkillToolRegistered`：有技能时工具注册、无技能时不注册。

## 6. 端到端验证

- 安装：任选一个社区 SKILL.md 技能仓库，克隆后整体放入
  `~/.doge/agent/skills/<name>/`（SKILL.md 技能根 + references/scripts/templates/assets）。
- 运行：`doge -p --no-mcp "用 <技能名> 技能做一份 doge 介绍 PPT…"`，接任意可用模型。
- 结果：日志确认模型**主动调用 `skill` 工具**加载技能正文 → 读 references →
  按模板与主题生成 deck。
- **产物规范**：首轮生成把 deck 写到会话 cwd，资源用技能树外的绝对/跨目录相对
  引用——本地可打开但不符合技能约定（技能 Quick start 规定 deck 建在技能树内、
  引用 `../assets`）。第二版 prompt 明确要求按技能约定工作后，模型在
  `~/.doge/agent/skills/<name>/examples/…/` 脚手架生成完整 deck，资源引用与
  自带示例完全一致（`../../assets/...`），脚本核验全部相对引用解析成功、无绝对
  路径；headless Chrome 截图 + vision 复核渲染正常。
- 经验：技能正文自带使用约定，prompt 里点明"按技能要求的目录/引用方式产出"
  即可让模型生成符合规范的、可携带的 deck。

## 7. API 与 WebUI 适配

- **API（只读）**：`GET /api/skills`（已装技能摘要）、`GET /api/skills/{name}`
  （详情：正文 + 文件树）、`GET /api/session/skills`（会话已加载技能）、
  `/api/config` 的 `skills` 段（dirs/count/names）。列表与详情每次请求即时重扫
  磁盘，增删无需重启。接口文档见 `docs/web-api.md` §4.12。
- **WebUI**：状态栏新增 `✦ Skills` 按钮 → 模态面板：本会话技能（skill 工具背后
  的集合，标注"可模型调用/仅手动"）+ 已安装技能卡片（description、正文大小、
  「查看正文 / 文件」展开 SKILL.md 正文与 references/scripts/templates 文件树、
  发现目录）。纯 vanilla JS，复用 MCP 面板的 modal/section/badge 样式与委托事件模式。
- 验证：`go test ./...` 全绿；起真实 web 服务 curl 验证 4 个端点 + 401/404；
  Puppeteer 驱动 headless Chrome 登录并点击面板，确认本会话/已安装两节渲染、
  详情展开出正文与 211 个文件条目。

## 8. 后续可做（不在本次范围）

- frontmatter 支持 `allowed-tools` / `user-invocable` / `when_to_use`（现仅 name/description/disable-model-invocation）。
- 会话级"已加载技能"去重提示（Claude Code 的 already-loaded 语义）。
- `/skills` 列表/管理命令；技能安装/卸载入口（Skills Hub 风格）。
- mesh 远端任务按技能下发（当前 profile 只广播技能名）。
