# castworks 设计文档

castworks 是一个托管 AI Agent 环境。完整的第三方 Agent（当前托管 Claude Code）作为进程运行在挂载持久化工作区的沙箱内；所有 LLM 调用与外部系统流量经托管侧代理；托管环境在 Agent 等待 LLM、外部系统或人类时挂起沙箱释放计算资源，并在依赖解除后恢复，以同时达到计算资源利用率与响应延迟目标。

## 索引

- [架构](architecture.md) — 系统级结构、跨切面设计与架构不变量
- [术语表](../glossary.md) — 项目级术语，所有文档类型共用
- [引用](../references.md) — 文档引用的公开外部来源

## 模块

| 模块 | 文档 |
|---|---|
| `runcontroller` | [runcontroller](modules/design-runcontroller.md) |
