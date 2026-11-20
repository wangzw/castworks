# extsim（v0.2.0）

`extsim` 是[模拟外部系统](../../glossary.md)的实现，供开发与验收使用，不属于设计中的模块。它在开发机上作为独立进程运行，模拟“发起异步作业，作业结束后回调”这一类外部系统，经外部系统定义接入 [connectorhub](connectorhub.md)。版本范围见 [v0.2.0](README.md)。

## 职责

- 接受 Agent 提交的作业，异步执行，按提交的内容给出结论。
- 作业结束时向 `connectorhub` 回调 `job.completed`，收不到 2xx 时重试。
- 提供作业状态查询，供 Agent 与 `connectorhub` 使用。
- 提供命令行，供 Agent 在沙箱内调用。
- 提供测试控制接口，供测试设定作业耗时与回调方式，并读取回调的投递记录。

## 作业

| 字段 | 含义 |
| --- | --- |
| `id` | 作业标识，`job-` 加 12 位小写 base32 随机串 |
| `project` | 项目，提交时给出；是外部系统定义中的项目归属键 |
| `status` | `queued`、`running`、`completed` |
| `conclusion` | `status` 为 `completed` 时取 `succeeded` 或 `failed`，此前为空 |
| `reason` | `conclusion` 为 `failed` 时的原因，此前为空 |
| `created_at`、`updated_at` | 创建与最后一次状态变化的时刻，RFC 3339 格式，精确到毫秒 |

作业提交后为 `queued`，随即转为 `running`，经过作业耗时后转为 `completed`，然后发出回调。作业耗时取测试控制接口为该作业设定的值，没有设定时取启动参数 `--job-duration`，默认 60 s。默认值长于运行实例的空闲挂起阈值 `G_idle`（30 s）加一次快照的耗时，演示中回调到达时 actor 已经挂起。

结论只由提交的内容决定，校验规则固定：

1. 内容必须是一个 JSON 对象，否则 `failed`，原因为 `content is not a JSON object`。
2. 对象必须含有值为非空字符串的字段 `owner`，否则 `failed`，原因为 `missing required field "owner"`。
3. 两条都满足时为 `succeeded`。

演示场景的初始内容不含 `owner`，第一次提交因此不通过。

## 接口

### 作业接口

监听启动参数 `--listen` 指定的地址，默认 `0.0.0.0:9100`，明文 HTTP，请求与响应都是 JSON，不鉴权。

| 方法与路径 | 请求 | 响应 |
| --- | --- | --- |
| `POST /v1/jobs` | `{"project": "...", "content": "..."}`；`project` 非空，`content` 不超过 64 KiB | 201，作业对象，`status` 为 `queued` |
| `GET /v1/jobs/{id}` | 无 | 200，作业对象；作业不存在时 404 |

请求不合法时返回 400，响应体为 `{"error": "..."}`。

### 回调

作业转为 `completed` 后，`extsim` 向启动参数 `--callback-url` 发出 `POST`，默认 `http://127.0.0.1:9092/hooks/extsim`，即 `connectorhub` 的 webhook 接收端。

| 部分 | 内容 |
| --- | --- |
| `X-Extsim-Event` | `job.completed` |
| `X-Extsim-Delivery` | 投递标识，每次投递一个随机 UUID；重试沿用同一投递标识 |
| `X-Extsim-Signature-256` | `sha256=` 加请求体的 HMAC-SHA256 十六进制值，密钥取自启动参数 `--signing-key-file` 指向的文件 |
| 请求体 | `{"job": <作业对象>}` |

收不到 2xx，或者连接失败时，按 1 s 起、每次加倍、上限 30 s 的间隔重试，直到收到 2xx。签名密钥文件与控制面共用，见[开发环境需求](README.md#开发环境需求)。

### 测试控制接口

只监听启动参数 `--control-listen` 指定的回环地址，默认 `127.0.0.1:9101`。

| 方法与路径 | 作用 |
| --- | --- |
| `POST /control/plans` | 追加一个作业计划 `{"duration_ms": n, "callback": "deliver" \| "drop" \| "duplicate", "callback_delay_ms": n}`，各字段可省略；计划按追加顺序依次用于此后提交的作业 |
| `DELETE /control/plans` | 清空尚未使用的计划 |
| `GET /control/jobs/{id}/deliveries` | 该作业每次回调尝试的投递标识、尝试序号、发出时刻、响应状态码或错误 |

`callback` 的取值：

- `deliver`：默认值，按上文发出回调并重试。
- `drop`：作业照常完成，状态查询可见，但不发出回调。
- `duplicate`：先完成一次正常投递，收到 2xx 之后，再以新的投递标识发出请求体相同的第二次投递。

`callback_delay_ms` 是作业完成到第一次发出回调之间的等待。

## 命令行

镜像中的 `extsim` 命令行供 Agent 调用：

| 命令 | 作用 | 退出码 |
| --- | --- | --- |
| `extsim job submit [--file F]` | 提交作业，内容取自文件 `F`，省略时取标准输入；输出作业对象 | 成功 0，失败 2 |
| `extsim job get <id>` | 输出作业对象 | 成功 0，失败 2 |
| `extsim job watch <id> [--interval 2s]` | 按间隔查询，直到作业完成，输出最终的作业对象 | `succeeded` 为 0，`failed` 为 1，其他错误为 2 |

服务地址与项目依次取命令行参数 `--server` 与 `--project`、环境变量 `EXTSIM_URL` 与 `EXTSIM_PROJECT`、配置文件 `/etc/extsim.json` 中的 `url` 与 `project`，都没有时取 `http://127.0.0.1:9100` 与 `demo`。actor 镜像在 `/etc/extsim.json` 中写入 `http://extsim.castworks-system.svc.cluster.local:9100`。看守进程只把固定的几个环境变量交给 Claude Code，所以 actor 中靠配置文件确定地址；开发机上的本地模式没有这个文件，取默认的回环地址。

## 外部系统定义

```yaml
kind: ExternalSystem
name: extsim
access:
  mode: passthrough
  upstreams: [extsim.castworks-system.svc.cluster.local]
events:
  sources:
    - type: webhook
      path: /hooks/extsim
      signature: hmac-sha256
      delivery_id: X-Extsim-Delivery
      event_types: [job.completed]
  correlation_keys: [job.project, job.id]
  identity_keys: [job.id]
  project_binding: job.project
  dedup_key: [event_type, job.id, job.conclusion]
  ordering: {occurred_at: job.updated_at}
  completions:
    - event: job.completed
      verify: {api: extsim-job, terminal: [succeeded, failed]}
```

定义没有 `auth`，因为 Agent 调用 `extsim` 不鉴权；设计要求 `auth.methods` 非空，v0.2.0 的 `connectorhub` 允许省略。`access` 照写，但 v0.2.0 的出口策略由 [sandboxprovider](sandboxprovider.md) 按配置生成，不读这一段。事件类型的规范化、结论字段与 `extsim-job` 状态查询由 `connectorhub` 中的 extsim 连接器实现，见 [connectorhub](connectorhub.md#extsim-连接器)。

## 运行

| 启动参数 | 默认值 |
| --- | --- |
| `--listen` | `0.0.0.0:9100` |
| `--control-listen` | `127.0.0.1:9101` |
| `--callback-url` | `http://127.0.0.1:9092/hooks/extsim` |
| `--signing-key-file` | 无，必填 |
| `--job-duration` | `60s` |

作业、计划与待重试的回调只保存在内存中，进程重启后全部丢失。日志按行写到标准输出，每次状态变化与每次回调尝试各一行。

## 测试

- 校验规则：非 JSON 对象、缺少 `owner`、`owner` 为空串与合规内容，结论与原因符合上文。
- 作业生命周期：在可控时钟下，作业按设定耗时完成；未设定计划时取 `--job-duration`。
- 回调：签名可用同一密钥验证；接收端返回 500 或拒绝连接时按间隔重试，重试沿用同一投递标识，收到 2xx 后停止。
- 计划：`drop` 不发回调而状态查询可见结论；`duplicate` 在第一次投递成功后发出投递标识不同、请求体相同的第二次投递；`callback_delay_ms` 推迟第一次回调；计划按追加顺序使用。
- 命令行：`job watch` 的退出码与结论对应；服务地址按命令行参数、环境变量、配置文件、默认值的顺序确定。
