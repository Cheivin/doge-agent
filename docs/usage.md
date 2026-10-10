# doge 使用指南（已内置二进制）


用法、功能与配置的完整指南不再放这里，而是**随二进制内置**（源文件
`internal/guide/guide.md`，`go:embed` 编译进产物），以**内嵌 `guide` 工具**暴露给
agent：不带参数返回模块列表（构建/启动命令/各配置文件/TUI/mesh/环境变量…），
`module=<key>` 取该模块正文——渐进式披露，不占常驻 context。独立部署、无仓库
文档的机器上同样可用，内容与本二进制版本一致。

- **问 agent**：直接问任何用法/配置/功能问题（“mesh 怎么组网？”
  “permission.json 支持哪些规则？”），agent 调 `guide` 工具查模块作答，
  无需执行 help 命令、无需找文档。
- **模块 key 稳定**（如 `models-json`、`mesh-json`、`permissions-json`、`tui`），
  以指南内 `<!-- module: key | 一行说明 -->` 标记声明（markdown 渲染不可见），
  说明即模块列表的目录项，可跨会话引用。

设计概念文档仍在 `docs/`（如 `mesh-design.md`、`web-api.md`），面向开发者，
不随二进制分发。
