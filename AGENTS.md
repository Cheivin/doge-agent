# AGENTS.md

doge：交互式编码 agent CLI（Go 1.26+，macOS/Linux，单二进制零依赖）。TUI / Web 控制台 /
桌面客户端三种界面共享同一 agent 内核，可选 P2P mesh（`-tags mesh` 编译开关）。

## 常用命令

```bash
make build            # 默认构建 → dist/doge（不含 mesh，不引入 libp2p）
make build-mesh       # mesh 构建 → dist/doge-mesh
make test             # go test ./...（默认配置）
go vet ./...
go test -race ./...   # 合并前完整回归

# mesh 回归（默认 + mesh 双配置都要过）
go vet -tags mesh ./...
go test -tags mesh -race ./internal/mesh/...

# 桌面客户端（desktop/ 是独立 Go module，macOS 需 CGO）
make desktop
cd desktop && go vet ./... && go test ./...
```

## 目录地图

- `cmd/doge/` — 入口
- `internal/agent/` — agent 循环内核：流式 turn、steering/follow-up 双队列、工具编排、并发控制
- `internal/ai/` — 多模型网关：OpenAI Responses / OpenAI Chat Completions 兼容 / Anthropic Messages
- `internal/coding/` — coding agent 层：会话（append-only JSONL）、compaction、附件、配置
  - `coding/tools/` — 内置工具（read / write / edit / bash / grep / code PTC）
  - `coding/permissions/` — 权限层（策略、风险分级、通配匹配）
  - `coding/uploads/` — 上传暂存区
- `internal/mcp/` — MCP 接入：mcp 代理工具、directTools 提升、磁盘缓存
- `internal/mesh/` — P2P 层，仅在 `-tags mesh` 下参与编译（`mesh/libp2p/`）
- `internal/app/` — 装配层；mesh 边界用 `mesh_stub.go` / `mesh_mesh.go` 按 build tag 拆分
- `internal/ui/` — bubbletea TUI
- `internal/web/` — Web 控制台（API + WebSocket）；`web/frontend/` 是原生 HTML/JS/CSS 三件套，go:embed 内嵌，无构建步骤
- `internal/guide/` — 使用指南 `guide.md` 随二进制分发（go:embed），以 `guide` 工具暴露
- `pkg/logx/` — 日志
- `desktop/` — wails v3 桌面客户端，独立 Go module（replace 指回仓库根）
- `docs/` — 设计文档；`docs/internal/` 内部设计稿；`research/` 是参考资料，不属于 doge 代码

## 硬性约定

- **默认构建不引入 libp2p**：mesh 相关代码全部藏在 `-tags mesh` 之后，跨包边界走
  stub/real 文件对（参考 `internal/app/mesh_stub.go`）。新增 mesh 能力时默认构建必须照常编译通过。
- **改用法/配置必须同步 `internal/guide/guide.md`**：它 go:embed 进二进制，是 agent 回答
  用法问题的唯一来源；`docs/usage.md` 只是指向说明。
- **会话惰性创建**：启动不得产生任何磁盘痕迹（连会话目录都不建），首条消息落盘时才创建目录与文件。
- **配置与数据都在 `~/.doge/agent/`**（`DOGE_CODING_AGENT_DIR` 可覆盖）。测试不要依赖真实
  用户目录，用该环境变量指到临时目录。
- **版本发布**：更新 Makefile 的 `VERSION`，release commit 用 `doge x.y.z` 格式；日常 commit
  用 `type: 中文描述`（feat/fix/docs/perf/refactor/build/chore）。
- `.zcodeignore` 上半部分同步自 `.gitignore`，改了 `.gitignore` 记得同步。

## 代码风格

- 注释中文、一行式（`// 标识符 中文短语`），步骤注释四到八字动宾短语；不写 @param 式文档。
- 错误消息英文小写；日常错误直接 `return err` 不层层包装；启动失败 panic，运行期可降级
  错误记日志继续、影响正确性的向上返回。
- 日志走 slog 键值对，消息格式 `模块 中文短语`，动作三态（开始/完成/失败），错误键统一 `err`。
- 测试只用标准库 testing，断言 `t.Fatalf`；合并前跑 `-race`。
- struct 用 `type (...)` 分组，字段行尾中文注释，receiver 单字母。

## 文档

- 各子系统设计：`docs/*-design.md`（mcp / mesh / skills / file-transfer / web-api）
- 内部设计稿（agent 设计、loop 护栏等）：`docs/internal/`
- 动大模块先补设计文档再动代码（mermaid 图 + 取舍表）。
