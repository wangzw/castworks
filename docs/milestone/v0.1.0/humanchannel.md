# humanchannel（v0.1.0）

v0.1.0 中，`humanchannel` 是控制面进程里的一组 HTTP 接口和一个命令行工具，用来创建运行实例、发消息、查看输出和结束运行实例。版本范围见 [v0.1.0](README.md)。

## 职责

- 在开发机的 `127.0.0.1` 上提供 HTTP API，默认端口 8080，可配置。
- 校验输入，再转交 `runcontroller`。
- 提供命令行 `castworks`，演示场景的每一步都通过它执行。

## 接口

HTTP API，请求与响应都是 JSON：

| 方法与路径 | 请求 | 响应 | 对应 |
| --- | --- | --- | --- |
| `POST /v1/runs` | `{prompt}` | `{run_id}` | `RunService.Create` |
| `GET /v1/runs` | 无 | `[{run_id, state, substate, created_at}]` | `RunService.List` |
| `GET /v1/runs/{run_id}` | 无 | `{run_id, state, substate, failure_reason, turns[{turn_id, started_at, ended_at, output, is_error}], suspensions[{started_at, snapshot_ms, restore_ms, hello_ms, error}]}` | `RunService.Get` |
| `POST /v1/runs/{run_id}/messages` | `{text}` | `{inbox_seq}` | `AppendInbox` |
| `POST /v1/runs/{run_id}/end` | 无 | `{}` | `RunService.End` |

`prompt` 与 `text` 不能为空，也不能超过 64 KiB，违反时返回 400；`run_id` 不存在时返回 404；向已结束或已失败的运行实例发消息返回 409。

命令行：

| 命令 | 作用 |
| --- | --- |
| `castworks run create <prompt>` | 创建运行实例，打印 `run_id` |
| `castworks run send <run_id> <text>` | 发一条消息 |
| `castworks run get <run_id>` | 打印状态、各回合输出与挂起记录 |
| `castworks run watch <run_id>` | 每秒查询一次，状态变化或有新回合输出时打印 |
| `castworks run list` | 列出运行实例 |
| `castworks run end <run_id>` | 结束运行实例 |

`--server` 指定 API 地址，默认 `http://127.0.0.1:8080`。

## 与设计的差异

- 没有用户认证与项目角色。只有一个用户，所有运行实例互相可见。
- 设计中，创建、查询与结束经 `UserService` 的 `CreateRun`、`GetRun`、`EndRun`，人类消息经聊天界面适配器写入收件箱。v0.1.0 把两者合成一组 HTTP 接口。
- 没有界面、即时通讯、Issue 评论、审批、终端与文件访问。
- 回合输出取自 `turn_ended.output`，见 [agentprotocol](agentprotocol.md)。设计没有定义这条路径。

## 测试

- 各接口的成功路径，以及 400、404、409 三种错误。
- 命令行对假的 API 服务端：每条命令的请求与输出格式。
