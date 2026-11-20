# connectorhub（v0.2.0）

v0.2.0 中，`connectorhub` 在控制面进程中运行，接收外部系统的 webhook，保存[连接器事件日志](../../glossary.md)，登记 `once` [订阅](../../glossary.md)，并把订阅完成事件经 `runcontroller` 的 `AppendInbox` 写入收件箱。唯一接入的外部系统是 [extsim](extsim.md)。版本范围见 [v0.2.0](README.md)。

## 职责

- 装载外部系统定义与项目绑定配置。
- 接收 webhook：校验签名与投递标识，确定事件所属项目，写入连接器事件日志，再匹配活动订阅。
- 经[面向沙箱的端点](../../glossary.md) `connector` 提供事件目录与订阅登记，登记时回溯匹配连接器事件日志，未命中时核对外部对象的当前状态。
- 为完成的订阅构造订阅完成事件，经 `AppendInbox` 写入收件箱，失败时重投。
- 取消已结束或已失败的运行实例的活动订阅。

通用部分（定义装载、连接器事件日志、订阅匹配、完成事件的构造与投递）不含针对某个外部系统的代码。每个外部系统另有一个连接器，负责设计中“由连接器规范化”的部分：签名头、事件类型、结论字段与状态查询。v0.2.0 只有 [extsim 连接器](#extsim-连接器)。

## 接口

### connector 端点

明文 HTTP，请求与响应都是 JSON，监听控制面启动参数 `--connector-addr`。Substrate 提供者下它必须是开发机的网卡地址，与 `--grpc-addr` 的要求相同；actor 经不带 selector 的 Service `castworks-dev` 的 9091 端口到达它。请求不带令牌，以路径中的 `run_id` 标识运行实例。

| 方法与路径 | 请求 | 响应 |
| --- | --- | --- |
| `GET /v1/runs/{run_id}/events?system=` | `system` 可省略，省略时返回全部外部系统 | 200，`EventDescriptor` 数组 |
| `POST /v1/runs/{run_id}/subscriptions` | `{"system": "...", "event": "...", "correlation": {"<键>": "<值>"}}` | 201，`{"sub_id", "since_cursor", "status"}`；`status` 为 `active`，或登记时已完成为 `completed` |

`EventDescriptor` 的字段与取值规则见设计中的[事件目录接口](../../design/modules/connectorhub/design-connectorhub.md#对平台-mcp-服务器的事件目录接口)，`Lifetime` 只有 `once`。订阅的 `event` 对应设计中的 `completes_on.event`，`lifetime` 固定为 `once`，没有 `filter` 与 `timeout_at`。

| 情形 | 状态码 |
| --- | --- |
| 运行实例不存在 | 404 |
| 运行实例已结束或已失败 | 409 |
| 外部系统不可用、事件不在完成条件目录中、`correlation` 的键不属于关联键、不含关联键身份键、项目归属键的取值不属于本项目的绑定值 | 400，`error` 说明哪一条不满足，面向 Agent 可读 |

### webhook 接收端

只监听回环地址，控制面启动参数 `--webhook-addr`，默认 `127.0.0.1:9092`。路径取外部系统定义的 `events.sources[].path`。

| 情形 | 状态码 | 连接器事件日志 |
| --- | --- | --- |
| 路径未注册 | 404 | 不写 |
| 签名密钥不可读 | 503 | 不写 |
| 签名不通过 | 401 | 不写 |
| 缺少投递标识，或请求体不是 JSON | 400 | 不写 |
| 新事件，写入并完成匹配 | 200 | 写入 |
| 投递标识已存在 | 重新匹配后 200 | 不重复写入 |
| 写入或匹配时数据库出错 | 500 | 写入未提交时不写 |

### 对 runcontroller 的调用

进程内函数调用，见 [runcontroller](runcontroller.md#接口)：

- `RunService.Get`：确认运行实例存在，读取它是否已结束或已失败。
- `AppendInbox`：写入 `Kind=subscription_completed` 的事件，以 `(run_id, dedup_key)` 幂等。

`runcontroller` 不调用本模块。

### 配置

控制面启动参数 `--connectorhub-config` 指向一个 YAML 文件；不给出时不启动本模块，也不监听上述两个地址。

```yaml
project: default
systems:
  - definition: deploy/dev/connectorhub/extsim.yaml
    signing_key_file: /path/to/extsim-signing-key
    api_base_url: http://127.0.0.1:9100
bindings:
  extsim: [demo]
```

- `project`：唯一的项目，所有运行实例都属于它。
- `systems[].definition`：外部系统定义文件，内容见 [extsim](extsim.md#外部系统定义)；`signing_key_file`：webhook 签名密钥；`api_base_url`：连接器调用状态查询的地址。
- `bindings`：项目绑定配置，外部系统名到项目归属键取值的列表。

## 外部系统定义的装载

装载时按设计中[外部系统定义](../../design/modules/connectorhub/design-connectorhub.md#外部系统定义)的字段闭集解析，校验其中 v0.2.0 使用的字段：`name`；`webhook` 类型的 `events.sources`；`correlation_keys`、`identity_keys`、`project_binding`、`dedup_key`、`ordering.occurred_at`；`completions`。`correlation_normalization` 只支持 `lowercase`。`auth` 可以省略；`access` 与 `wait_signatures` 解析后不使用；含 `poll` 事件源的定义拒绝装载。定义名没有对应连接器时拒绝装载。任一定义不能装载时控制面启动失败。

## extsim 连接器

| 职责 | 规则 |
| --- | --- |
| 签名 | 请求头 `X-Extsim-Signature-256`，取值 `sha256=` 加请求体 HMAC-SHA256 的十六进制值，以常数时间比较 |
| 事件类型 | 请求头 `X-Extsim-Event` 的值，即 `job.completed` |
| 结论字段 | 负载中的 `job.conclusion` |
| 状态查询 `extsim-job` | 以订阅关联键中的 `job.id` 调用 `GET {api_base_url}/v1/jobs/{id}`，超时 5 s；`status` 为 `completed` 时终态取值为 `conclusion`，状态内容取响应体；其他 `status` 视为未到终态；404、超时与连接失败视为不可达 |

## 数据

本模块的表与 `runcontroller` 的表在同一数据库中，迁移各自管理，互不读写对方的表。

| 表 | 主要列 | 说明 |
| --- | --- | --- |
| `connector_event` | `append_seq`、`system`、`delivery_id`、`event_type`、`project_ids`、`correlation`、`occurred_at`、`payload`、`received_at` | 主键 `append_seq`；`(system, delivery_id)` 唯一；`correlation` 是按 `correlation_keys` 从负载取出的键值，规范化后保存，带 GIN 索引 |
| `connector_event_counter` | `next_seq` | 单行，分配 `append_seq` |
| `subscription` | `sub_id`、`run_id`、`project_id`、`system`、`event`、`correlation`、`status`、`terminal`、`since_cursor`、`dedup_spec`、`created_at`、`completed_at` | `status` 取 `active`、`completed`、`cancelled` |
| `subscription_claim` | `sub_id`、`delivery_id` | 主键为两列 |
| `completion_delivery` | `sub_id`、`run_id`、`event_id`、`dedup_key`、`event`、`status`、`attempts`、`last_error`、`created_at`、`delivered_at` | `(sub_id, dedup_key)` 唯一；`status` 取 `pending`、`delivered`、`rejected`；`event` 是待写入收件箱的订阅完成事件 |

`sub_id` 为 `sub-` 加 12 位小写 base32 随机串。

## 行为

### 追加序号

设计以按关联键的咨询锁串行化订阅登记与匹配，并要求两者先加锁、再读取，见设计中的[并发与事务边界](../../design/modules/connectorhub/design-connectorhub.md#并发与事务边界)。v0.2.0 不用咨询锁，改为让追加序号的分配顺序等于提交顺序：`append_seq` 不取自数据库序列，而由写入事件的事务锁住 `connector_event_counter` 的唯一一行分配，事件的写入因此串行。某个快照能看到计数器取值 N，就能看到全部 `append_seq < N` 的事件。没有咨询锁时，数据库序列不满足这一点：序号较小的事件可能晚提交，同时漏过订阅登记的回溯与登记之后的重扫。v0.2.0 只有一个外部系统、事件量小，串行写入的代价可以接受。

### webhook 摄入

1. 按路径找到定义与连接器，路径未注册时返回 404。
2. 由连接器校验签名，读取投递标识，解析 JSON 请求体。
3. 由连接器取得事件类型；按 `correlation_keys` 的字段路径从负载取出关联键，按 `ordering.occurred_at` 取发生时间；项目归属键的取值属于 `bindings` 时，`project_ids` 为配置中的项目，否则为空。
4. 在一个事务中分配 `append_seq` 并写入事件；`(system, delivery_id)` 冲突时不写入，跳到第 6 步。
5. 提交。
6. 匹配活动订阅（见下一节），完成后返回 200。

投递标识已存在时仍执行第 6 步。匹配以认领记录的唯一键与订阅状态的条件更新保证幂等，重复执行不产生第二个完成事件；第 6 步失败时返回 500，发送方重试时由这一规则补上匹配。

### 匹配规则

订阅与事件匹配，当且仅当：事件的 `system` 与订阅相同，`project_ids` 包含订阅的 `project_id`，`event_type` 等于订阅的 `event`，且订阅的 `correlation` 是事件 `correlation` 的子集。v0.2.0 的订阅都是 `once`：一个事务内认领该事件，以条件更新把订阅从 `active` 改为 `completed` 并写入终态取值，写入 `pending` 的完成事件投递记录。条件更新不成功表示订阅已由其他路径完成或取消，事务不写任何记录。

### 订阅登记

1. `RunService.Get`：运行实例不存在返回 404，已结束或已失败返回 409。订阅的 `project_id` 取配置中的项目。
2. 校验参数，不满足时返回 400，条件见 [connector 端点](#connector-端点)。
3. 开启可重复读事务：读取计数器的 `next_seq` 作为 `since_cursor`，插入 `active` 的订阅，按匹配规则回溯 `append_seq < since_cursor` 的事件；命中时取 `occurred_at` 最早的一条，按匹配规则完成订阅。提交。
4. 提交后重扫 `append_seq >= since_cursor` 且本订阅尚无认领记录的事件，命中时同样按匹配规则完成。
5. 订阅仍为 `active` 时调用连接器的状态查询。已到终态时，以条件更新完成订阅并写入投递记录，`dedup_key` 取 `H(sub_id, verify, 终态取值)`；未到终态或不可达时订阅保持 `active`。
6. 返回 `sub_id`、`since_cursor` 与订阅当前的 `status`。

事件与订阅的先后有三种情形：

- 事件在订阅登记的快照之前提交，第 3 步的回溯看到它。
- 事件在快照之后、第 4 步之前提交，第 4 步看到它。
- 事件在第 4 步之后提交，它的第 6 步匹配看到已提交的订阅。

三种情形下订阅都会完成（设计中的 I2）。回调丢失时，只有第 5 步的状态查询能补救；登记之后丢失的回调没有补救，见 [v0.2.0 的已知限制](README.md#已知限制)。

### 订阅完成事件

字段的构造规则沿用设计中的[完成事件构造与投递](../../design/modules/connectorhub/design-connectorhub.md#完成事件构造与投递)：`event_id` 为 `(sub_id, 终态取值)`，webhook 触发的 `dedup_key` 为 `H(sub_id, H(dedup_spec 的字段值))`，编码与哈希按该节规定。写入收件箱的事件内容为：

| 字段 | 取值 |
| --- | --- |
| `sub_id`、`system`、`event` | 订阅的值 |
| `subscription_status` | `completed` |
| `terminal` | webhook 触发取连接器给出的结论字段；状态查询触发取查询到的结论 |
| `correlation` | 订阅的关联键 |
| `occurred_at` | webhook 触发取事件的发生时间；状态查询触发取查询时刻 |
| `source` | `webhook` 或 `verify` |
| `detail` | webhook 触发为原始负载，状态查询触发为状态查询的响应体；超过 48 KiB 时截断，并置 `detail_truncated` 为真 |

没有附件；Agent 需要更多信息时自己调用外部系统查询。

### 投递

完成事件投递记录提交后立即投递。投递按记录调用 `AppendInbox`：成功或返回重复时置 `delivered`；返回运行实例不存在、已结束或已失败时置 `rejected`；其他错误保持 `pending`，`attempts` 加一并记录 `last_error`。后台扫描在控制面启动时与之后每 `P_deliver` 执行一次，重投全部 `pending` 记录。

同一扫描对仍有 `active` 订阅的运行实例逐个调用 `RunService.Get`，运行实例不存在、已结束或已失败时，把它的 `active` 订阅以条件更新置为 `cancelled`。

## 参数

| 参数 | 取值 | 含义 |
| --- | --- | --- |
| `P_deliver` | 5 s | 投递重试与取消扫描的周期 |
| 状态查询超时 | 5 s | 连接器调用外部系统状态查询的上限 |
| `detail` 上限 | 48 KiB | 订阅完成事件中原始内容的截断长度 |

## 与设计的差异

- 只有 `once` 订阅，订阅没有 `filter` 与 `timeout_at`；没有状态式轮询与订阅对账扫描，登记之后丢失的回调不会被补上。
- 投递标识重复的 webhook 会重新匹配，设计中不重复匹配；这是没有订阅对账扫描时补上匹配失败的办法。
- `append_seq` 由计数行串行分配，事件写入串行，代替设计中按关联键的咨询锁。
- 只有一个项目，项目绑定配置随控制面启动装载，不随运行实例变化。
- 端点不校验运行实例令牌，不查询代；只凭 `run_id` 识别运行实例。
- 没有附件、出口策略与沙箱侧配置的生成和推送、等待签名、工具接入声明。
- 没有核对的合并与公平调度，没有每运行实例的投递令牌桶。
- 签名密钥取自本地文件，不经 `secrets`；外部系统定义可以省略 `auth`。

## 测试

在控制面进程内，以真实的 `extsim`、真实的 `runcontroller`、假的沙箱提供者与假的看守进程测试：

- 回调先于订阅登记到达：登记时回溯命中，订阅直接完成，收件箱得到一条订阅完成事件。
- 作业已完成而回调被丢弃：登记时的状态查询生成完成事件，`source` 为 `verify`。
- 回调与订阅登记并发：分别让事件提交落在登记快照之前、快照与重扫之间、重扫之后，三种情形下订阅都完成，且只完成一次。
- 同一投递标识重发与投递标识不同、内容相同的重复投递：收件箱只有一行。
- 第一次匹配时数据库出错并返回 500，发送方重试后订阅完成。
- `AppendInbox` 暂时失败：记录保持 `pending`，下一次扫描投递成功；控制面重启后，启动时的扫描投递遗留的 `pending` 记录。
- 运行实例结束后：它的 `active` 订阅置为 `cancelled`，其后到达的匹配事件不再写入收件箱。
- 参数校验：各种 400 情形的 `error` 说明不满足的条件；运行实例不存在为 404，已结束为 409。
- webhook：签名不通过为 401，缺少投递标识为 400，均不写连接器事件日志；项目归属键取值未绑定的事件照常写入，`project_ids` 为空，不参与匹配。
