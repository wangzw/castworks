# humanchannel（v0.1.0）

v0.1.0 中，`humanchannel` 是控制面进程里的一组 HTTP 接口和一个命令行工具，用来创建运行实例、发消息、查看输出和结束运行实例。版本范围见 [v0.1.0](README.md)。

## 职责

- 在开发机的回环地址（`127.0.0.1` 或 `[::1]`）上提供 HTTP API，默认 `127.0.0.1:8080`，可配置。
- 校验输入，再转交 `runcontroller`。
- 提供命令行 `castworks`，演示场景的每一步都通过它执行。

## 接口

### HTTP API

控制面在 `127.0.0.1` 上提供 HTTP API，请求与响应都是 JSON。`Host` 的主机部分必须是 `127.0.0.1`、`localhost` 或 `[::1]`（端口不限，`localhost` 大小写不敏感），否则返回 403；所有 `POST` 请求（含 `/end`）的 `Content-Type` 必须是 `application/json`，可带 `charset` 等参数，否则返回 415。两项检查都在路由之前，且先检查 `Host`。

| 方法与路径 | 请求 | 响应 | 对应 |
| --- | --- | --- | --- |
| `POST /v1/runs` | `{prompt}` | `{run_id}` | `RunService.Create` |
| `GET /v1/runs` | 无 | `[{run_id, state, substate, created_at}]` | `RunService.List` |
| `GET /v1/runs/{run_id}` | 无 | `{run_id, state, substate, created_at, failure_reason, turns[{turn_id, started_at, ended_at, output, is_error}], suspensions[{started_at, snapshot_ms, restore_ms, hello_ms, error}]}` | `RunService.Get` |
| `POST /v1/runs/{run_id}/messages` | `{text}` | `{inbox_seq}` | `AppendInbox` |
| `POST /v1/runs/{run_id}/end` | 无 | `{}` | `RunService.End` |

`POST /v1/runs` 成功时返回 201，其余接口成功时返回 200；`POST /v1/runs/{run_id}/end` 的响应体是 `{}`。

所有时间字段编码为 UTC、固定三位毫秒的 RFC 3339 字符串，例如 `2026-11-19T06:30:00.123Z`；解码接受任意合法 RFC 3339。

`ended_at`、`snapshot_ms`、`restore_ms`、`hello_ms` 未完成时是 JSON null，键始终存在。`substate`、`failure_reason`、`error`、`output` 为空时是空字符串，不省略。`GET /v1/runs`、`turns` 与 `suspensions` 为空时是 `[]`，不是 null。响应 JSON 关闭 HTML 转义，一个响应一行。

`prompt` 与 `text` 不能为空，也不能超过 64 KiB，违反时返回 400；只含空白的文本不算空。请求体超过 1 MiB、不是合法 JSON、含未知字段或有多余内容也返回 400。`run_id` 的格式不做校验，不存在时返回 404；向已结束或已失败的运行实例发消息返回 409；`end` 幂等，对已结束或已失败的运行实例仍返回 200。`end` 与 `GET` 接口忽略请求体，不要求请求体存在。

错误响应体是 `{"error": "<说明>"}`，`Content-Type` 为 `application/json`。400 对应请求体不合法或文本校验失败：文本为空或超过 64 KiB 时说明固定为 `text empty or longer than 64 KiB`，请求体不合法时说明固定为 `invalid request body`；403 对应 `Host` 的主机部分不是回环地址，说明固定为 `forbidden host`；404 对应不存在的运行实例，说明固定为 `run not found`；409 对应已结束或已失败的运行实例，说明固定为 `run ended or failed`；415 对应 `POST` 请求的 `Content-Type` 不是 `application/json`，说明固定为 `unsupported media type`；其余错误返回 500，说明固定为 `internal error`，不含内部错误文本。通过上述检查后，没有匹配的路径返回 404，方法不对返回 405，这两种响应的响应体是 `net/http` 的默认纯文本，不是 JSON。

### 命令行

| 命令 | 作用 |
| --- | --- |
| `castworks run create <prompt>` | 创建运行实例，打印 `run_id` |
| `castworks run send <run_id> <text>` | 发一条消息，打印 `inbox_seq` |
| `castworks run get <run_id>` | 打印状态、各回合输出与挂起记录 |
| `castworks run watch <run_id>` | 每秒查询一次，状态变化或有新回合输出时打印 |
| `castworks run list` | 列出运行实例 |
| `castworks run end <run_id>` | 结束运行实例 |

`--server` 指定 API 地址，默认 `http://127.0.0.1:8080`；它既可写在 `run` 之前，也可写在子命令之后、位置参数之前，两处都写时子命令之后的生效。`--` 之后的参数一律当作位置参数。`-h` 与 `--help` 把用法打印到标准输出，退出码 0。

`create` 取一个位置参数，`send` 取两个，`get`、`watch` 与 `end` 各取一个，`list` 取零个；数目不对、命令未知，或 `--server` 不是带主机的 http 或 https 地址时，退出码 2，错误与用法写到标准错误。

退出码：0 表示成功，1 表示运行错误（服务端返回错误、连不上服务端、`watch` 看到 `failed`），2 表示用法错误，130 表示 `watch` 被中断。错误信息写标准错误，一行，以 `castworks:` 和一个空格开头；服务端错误的文案是 `server returned <状态码>: <响应体 error 字段>`，响应体不是 JSON 时改用 HTTP 状态文本，连不上服务端的文案含服务端地址。

所有时间按 UTC、固定三位毫秒输出，例如 `2026-11-19T06:30:00.123Z`。空的 `substate` 显示为 `-`。

`run create` 只打印一行 `run_id`。`run send` 只打印一行 `inbox_seq` 的十进制数。`run end` 成功时不打印。

`run list` 先打印表头，再为每个运行实例打印一行，列为 `RUN_ID`、`STATE`、`SUBSTATE`、`CREATED_AT`，用 `text/tabwriter` 以空格填充、列间留两个空格，顺序与接口返回一致（创建时间降序）；没有运行实例时只打印表头。

`run get` 按下列格式输出：

```text
run_id: <id>
state: <state>
substate: <substate>
failure_reason: <原因>          仅在非空时输出这一行
turns: <N>
turn <序号从 1 起>  <turn_id>  started <时间>  ended <时间或->  is_error <true|false>
  <output 的每一行，前缀两个空格>
suspensions: <M>
suspension <序号从 1 起>  started <时间>  snapshot_ms <数或->  restore_ms <数或->  hello_ms <数或->
  error: <文本>                  仅在非空时输出这一行
```

序号字段与说明词之间、各字段之间都是两个空格。`output` 末尾的一个换行先去掉，其余每一行前缀两个空格，空行保持为空行；`output` 为空时不输出缩进行。

`run watch` 每次轮询调用一次 `GET /v1/runs/{run_id}`。每次轮询先输出新结束的回合，再输出状态行。已结束且没有输出过的回合按 `turn_id` 判重、按 `started_at` 升序输出，每行是 `turn <turn_id> is_error=<true|false>`，后接与 `run get` 相同的两空格缩进输出行；状态行是 `state=<state> substate=<substate>`，在第一次轮询以及 `state` 或 `substate` 变化时输出。进入 `failed` 时，状态行之后输出 `failure_reason: <原因>`，退出码 1，标准错误写 `castworks: run failed`；进入 `ended` 时退出码 0。挂起记录不在 `run watch` 中输出，用 `run get` 查看。

`run watch` 轮询出错时，只有连接错误与 5xx 响应重试：向标准错误写一行 `castworks: <错误>, retrying`，连续出错只在第一次写，然后继续轮询。其余错误立即以退出码 1 退出，包括 4xx、2xx 但响应体解码失败等。

## 与设计的差异

- 没有用户认证与项目角色。只有一个用户，所有运行实例互相可见。
- 设计中，创建、查询与结束经 `UserService` 的 `CreateRun`、`GetRun`、`EndRun`，人类消息经聊天界面适配器写入收件箱。v0.1.0 把两者合成一组 HTTP 接口。
- 没有界面、即时通讯、Issue 评论、审批、终端与文件访问。
- 回合输出取自 `turn_ended.output`，见 [agentprotocol](agentprotocol.md)。设计没有定义这条路径。

## 测试

- 各接口的成功路径，以及 400、404、409 三种错误。
- 命令行对假的 API 服务端：每条命令的请求与输出格式。
