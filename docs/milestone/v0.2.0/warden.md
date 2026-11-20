# warden（v0.2.0）

v0.2.0 的看守进程在 [v0.1.0](../v0.1.0/warden.md) 的基础上增加平台 MCP 服务器，维护 Claude Code 的后台作业集合，并把订阅完成事件写成 Claude Code 的用户消息。身份、出站流、回合边界的判定与关停都不变，下文只写变化。版本范围见 [v0.2.0](README.md)。

## 运行方式

在 v0.1.0 的基础上，看守进程还需要 `connector` 端点的地址：

| 方式 | `connector` 端点地址 | 平台 MCP 服务器的监听地址 |
| --- | --- | --- |
| actor 模式 | 环境变量 `CASTWORKS_CONNECTOR`，取 `castworks-dev.castworks-system.svc.cluster.local:9091`，由 actor 模板设置 | 固定为 `127.0.0.1:8082` |
| 本地模式 | 命令行参数 `--connector`，例如 `127.0.0.1:9091` | 命令行参数 `--mcp-addr`，默认 `127.0.0.1:0`，即由系统分配端口；本地验收会并行运行多个看守进程 |

本地模式下 Claude Code 沿用看守进程的 `PATH`，开发机上的 `extsim` 命令行要在这个 `PATH` 中。

## 平台 MCP 服务器

看守进程启动时在上表的地址上启动平台 MCP 服务器，早于 Claude Code。服务器使用 MCP 的 Streamable HTTP 传输（[REF-mcp](../../references.md)），路径 `/mcp`，服务器名 `castworks`，只提供两个工具：

| 工具 | 输入 | 处理 | 结果 |
| --- | --- | --- | --- |
| `list_events` | `system`，可省略 | `GET /v1/runs/{run_id}/events` | `EventDescriptor` 数组的 JSON 文本 |
| `subscribe_event` | `system`、`event`、`correlation`（键与值都是字符串的对象），可选的 `expected_wait` 与 `reason` | `POST /v1/runs/{run_id}/subscriptions`，只转发 `system`、`event` 与 `correlation` | `sub_id`、`status`，以及“事件到达后作为下一回合的输入，现在可以结束回合”的说明；`status` 为 `completed` 时说明事件已经写入收件箱 |

- 两个工具都调用 [connectorhub 的 connector 端点](connectorhub.md#connector-端点)，超时 10 s。端点返回 4xx 时，工具结果标为错误，内容是端点给出的 `error`；5xx、超时与连接失败时，工具结果标为错误，说明平台暂时不可用、可以重试。
- `expected_wait` 与 `reason` 不转发。按设计中的[平台 MCP 工具](../../design/modules/warden/design-warden.md#平台-mcp-工具)，看守进程把它们连同 `run_id`、工具名与调用标识写入日志。
- 初始化应答中的 `instructions` 与两个工具的描述承载同一组规则，内容取设计中的[等待方式的选择](../../design/modules/warden/design-warden.md#等待方式的选择)，去掉与 `wait_for`、`sleep_until` 有关的条目：先用 `list_events` 查看可订阅的事件；等外部系统时调用 `subscribe_event` 并结束回合，事件到达后作为下一回合的输入；需要继续当前工作时可以登记订阅后继续；需要人类输入时结束回合；不要用 Bash、curl 或其他外部工具轮询外部系统；会话内定时、`Monitor` 与后台 Bash 照常可用；能估计等待时长或说明原因时可以填写 `expected_wait` 与 `reason`。这些文字面向 Agent，用英文写。

## Claude Code 的启动

启动命令在 v0.1.0 的基础上增加两个参数：

```text
--mcp-config '{"mcpServers":{"castworks":{"type":"http","url":"http://127.0.0.1:8082/mcp"}}}' --strict-mcp-config
```

本地模式下 `url` 中的端口取平台 MCP 服务器实际监听的端口。`--strict-mcp-config` 使 Claude Code 只使用这里给出的服务器，工作目录中的 `.mcp.json` 与用户级 MCP 配置都不生效。Agent 看到的工具名为 `mcp__castworks__list_events` 与 `mcp__castworks__subscribe_event`；Claude Code 以跳过权限确认的模式运行，调用这两个工具不需要确认。

环境变量表不变。`CLAUDE_CODE_BG_TASKS_REPORT_RUNNING` 不设置：本地模式录制的样本中，后台 Bash 任务存活时 Claude Code 2.1.328 照常在回合结束时报告 `idle`，回合边界的判定不受影响。

## 后台作业

看守进程按设计中[后台作业登记](../../design/modules/warden/design-warden-driver-claude-code.md#后台作业登记)的部分规则维护后台作业集合：

1. `background_tasks_changed` 推送完整的存活任务集合，以它替换当前集合。
2. 两次推送之间，`is_backgrounded` 为真的 `task_started` 加入任务，报告终态的 `task_notification` 移除任务。
3. `ambient` 为真的任务不计入。2.1.328 的录制样本中任务没有 `ambient` 字段，缺少该字段按假处理。
4. Claude Code 启动时集合为空；Claude Code 退出时清空集合。

集合变化时发出 `bg_jobs_changed`，每次 `Hello` 携带当前集合，消息定义见 [agentprotocol](agentprotocol.md)。看守进程不终止后台任务，也不为挂起关闭它们。

## 订阅完成事件的写入

`Deliver` 中的 `subscription_completed` 项写成如下文本，其余处理与 v0.1.0 的[写入消息](../v0.1.0/warden.md#写入消息)相同：一批中各项的文本以空行连接，作为一条用户消息写入 stdin。

```text
[castworks] Subscription <sub_id> completed.
system: <system>
event: <event>
outcome: <terminal>
correlation: <键>=<值>, ...（按键排序）
occurred_at: <RFC 3339>
source: <webhook 或 verify>
detail:
<detail>
```

`detail_truncated` 为真时，末尾加一行 `(detail truncated)`。

## 镜像

- 加入 `extsim` 命令行 `/usr/local/bin/extsim`，以及写有 `extsim` 服务地址的 `/etc/extsim.json`，见 [extsim 的命令行](extsim.md#命令行)。
- Claude Code 的版本不变。

## 与设计的差异

在 v0.1.0 所列差异之外：

- 平台 MCP 服务器只有 `list_events` 与 `subscribe_event`，没有 `wait_for`、`sleep_until` 与附件取回；请求不带运行实例令牌，以 `run_id` 标识运行实例。
- 不发出 `subscription_created`、`tool_started` 与 `tool_ended`，不解析等待意图，不给情境提示，不向子代理注入等待方式的规则。
- 不下调 `MCP_TIMEOUT` 与 `MCP_CONNECT_TIMEOUT_MS`：平台 MCP 服务器在 Claude Code 启动之前已经就绪。
- 后台作业不按进程表关联 PID，不判定孤儿进程，不以 hook 输入核对。
- 订阅完成事件的文本格式由看守进程固定，设计中由驱动器决定。

## 测试

- 平台 MCP 服务器：以假的 `connector` 端点验证两个工具的转发、4xx 与 5xx 的错误结果；`expected_wait` 与 `reason` 只进日志，不出现在转发的请求中。
- 启动参数：`--mcp-config` 中的端口与平台 MCP 服务器实际监听的端口一致。
- 后台作业：用录制样本重放，`local_turn03_deliver` 之后集合为该回合启动的后台任务，`local_turn04_spontaneous` 的空集推送之后集合为空，`bg_jobs_changed` 的发出次数与集合变化次数一致；Claude Code 退出后集合为空。
- 订阅完成事件的文本：字段按模板写出，`correlation` 按键排序，截断时有末行说明；与人类消息混合的一批写成一条用户消息。
- 本地模式端到端：连接真实的控制面与 `extsim`，用真实的 Claude Code 完成提交作业、登记订阅、结束回合、收到订阅完成事件并据此改正重交的循环；同时录制含平台 MCP 调用与后台任务的 stream-json 样本。
