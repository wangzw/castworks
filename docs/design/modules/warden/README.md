# 看守进程

看守进程是沙箱容器的 PID 1，驱动 Agent、执行冻结、采集活动度、续约工作区租约、维护投递意图日志，并提供本地代理、本地 DNS 与透明拦截器、平台 MCP 服务器、托管 hook、`ToolExec`、Agent 驱动器与入站端点。

## 索引

| 文档 | 内容 |
| --- | --- |
| [看守进程](design-warden.md) | 模块主文档：进程监督与 `StartAgent`、冻结与解冻、入站数据唤醒、活动度采集、工作区租约续约、投递意图日志、平台 MCP 服务器与等待方式的选择、托管 hook 及其 IPC 对端认证、沙箱侧配置应用、`ToolExec`、出站流与状态对账、入站端点 |
| [本地代理](design-warden-llmproxy.md) | Agent 出站 HTTP 流量的回环代理与透明拦截器：请求分类、本地 DNS 与透明拦截、模式选择、直通与缓存、保活、单次回放、令牌轮换、恢复取回、LLM 请求时刻、连接用途记录、冻结期间的入站载荷通知、周期性连接观测与宿主侧分类请求转发 |
| [Claude Code 驱动器](design-warden-driver-claude-code.md) | 无头 `stream-json` 驱动 Claude Code：启动与环境、回合边界、事件翻译、审批 defer、安全点、会话续接、后台作业登记、自唤醒源镜像、等待意图解析与情境提示、终止 |
| [控制面循环驱动器](design-warden-driver-native.md) | 两种 Agent 循环共用的 Agent 驱动器接口与控制面循环实现：事件集合、`Deliver` 翻译、挂起与恢复语义、`ToolExec` 的使用 |

## 实现状态

| 子模块 | 状态 |
| --- | --- |
| [本地代理](design-warden-llmproxy.md) | 未实现 |
| [Claude Code 驱动器](design-warden-driver-claude-code.md) | 未实现 |
| [控制面循环驱动器](design-warden-driver-native.md) | 未实现 |
