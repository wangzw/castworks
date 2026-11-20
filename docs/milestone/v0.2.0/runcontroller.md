# runcontroller（v0.2.0）

v0.2.0 的 `runcontroller` 在 [v0.1.0](../v0.1.0/runcontroller.md) 的基础上做三处修改：收件箱除人类消息外还接收订阅完成事件；后台作业集合非空的运行实例不挂起；`active` 状态下 actor 崩溃时运行实例转为失败。状态与状态转移的集合、挂起与恢复的规则都不变，下文只写变化。版本范围见 [v0.2.0](README.md)。

## 接口

`RunService.AppendInbox` 的参数由文本改为收件箱事件，是 `humanchannel` 与 [connectorhub](connectorhub.md) 共用的汇入入口：

```go
type InboxKind string

const (
    KindHumanMessage          InboxKind = "human_message"
    KindSubscriptionCompleted InboxKind = "subscription_completed"
)

type InboxEvent struct {
    Kind         InboxKind
    Text         string                  // human_message：1 字节至 64 KiB
    EventID      string                  // subscription_completed：必填
    DedupKey     string                  // subscription_completed：必填
    Subscription *SubscriptionCompletion // subscription_completed：必填
}

type SubscriptionCompletion struct {
    SubID, System, Event, SubscriptionStatus, Terminal string
    Correlation     map[string]string
    OccurredAt      time.Time
    Source          string // webhook 或 verify
    Detail          string // 不超过 48 KiB
    DetailTruncated bool
}

AppendInbox(ctx context.Context, runID string, ev InboxEvent) (inboxSeq int64, duplicate bool, err error)
```

- 字段的含义与取值由 `connectorhub` 构造，见 [connectorhub 的订阅完成事件](connectorhub.md#订阅完成事件)。
- `(run_id, dedup_key)` 已存在时，返回已有行的 `inbox_seq`，`duplicate` 为真，不改变运行实例的任何状态。
- 错误：运行实例不存在为 `ErrNotFound`，已结束或已失败为 `ErrTerminated`，事件字段不满足上述约束为 `ErrInvalidEvent`。`ErrInvalidEvent` 取代 v0.1.0 的 `ErrInvalidText`，`humanchannel` 照旧把它映射为 400。

`humanchannel` 只把调用改为传入 `Kind=human_message` 的事件，行为不变。`RunService` 的其余方法不变。

## 数据

`inbox` 表：

| 列 | 变化 |
| --- | --- |
| `kind` | 新增，取 `human_message` 或 `subscription_completed`；已有的行取 `human_message` |
| `text` | `human_message` 的约束不变；`subscription_completed` 为空串 |
| `event_id`、`dedup_key` | 新增，`subscription_completed` 必填；`(run_id, dedup_key)` 在 `dedup_key` 非空时唯一 |
| `payload` | 新增，`subscription_completed` 以 JSON 保存 `SubscriptionCompletion` |

`run` 表新增 `bg_jobs`，文本数组，默认为空，是看守进程最近一次上报的后台作业集合。

## 行为

### 写入收件箱

在 v0.1.0 的写入事务中，锁住运行实例行之后、分配 `inbox_seq` 之前，先按 `(run_id, dedup_key)` 查找；找到时返回已有行，不分配序号。其余处理与 v0.1.0 相同，与事件的种类无关：运行实例已挂起时转为恢复中并在提交后发起恢复，挂起中时由挂起完成后的检查接手，回合边界上提交后投递。

### 投递

`Deliver` 的每一项按 `inbox_seq` 升序取自收件箱：`human_message` 填 `text`，`subscription_completed` 填 `subscription_completed`，消息定义见 [agentprotocol](agentprotocol.md)。一批可以同时包含两种事件。

### 后台作业与挂起

- 运行实例处于非终态时，每次处理 `Hello` 都以其中的 `bg_jobs` 替换 `run.bg_jobs`，包括只登记流的 `suspending` 与 `suspended`；`phase` 为 `exited` 的 `Hello` 按 v0.1.0 把运行实例转为 `failed`，不更新集合。
- 收到 `bg_jobs_changed` 时以 `job_ids` 替换 `run.bg_jobs`。运行实例处于 `active`、`boundary`，且集合由非空变为空时，`idle_since` 置为当前时刻。
- 挂起扫描的条件在 v0.1.0 的基础上增加 `bg_jobs` 为空：挂起的候选查询与事务内的重新检查都加上这一条。

后台作业存活时，设计只允许静止冻结，Substrate 不提供冻结，所以不挂起，见 [v0.2.0 的后台作业](README.md#后台作业)。集合变空时重新开始空闲计时，是因为后台任务结束后 Claude Code 通常紧接着开始一个[自发回合](../../glossary.md)处理任务通知；重新计时避免在两者之间挂起。

### actor 崩溃的检测

actor 在 `active` 状态下崩溃时，出站流断开，之后不会再有看守进程重连。v0.2.0 按以下规则发现这种情形：

- 控制面在内存中记录每个 `active` 运行实例的出站流最近一次断开的时刻；控制面启动时，所有 `active` 运行实例都以启动时刻记为断开。收到新的 `Hello` 时清除记录。
- 挂起扫描每 `P_scan` 执行一次时，同时选出出站流断开超过 `T_reconnect` 的 `active` 运行实例，逐个调用 `GetSandbox`。
- `GetSandbox` 报告 actor 已崩溃或不存在时，在事务内确认运行实例仍为 `active`、出站流仍未重连，然后转为 `failed`，`failure_reason` 写明 actor 的状态。actor 不自动删除，留作排查；用户结束运行实例时按 v0.1.0 的规则调用 `Destroy`。
- `GetSandbox` 报告其他状态或调用出错时，状态不变，下一轮扫描再查。
- 本地提供者的 `GetSandbox` 恒报运行中，本地模式下看守进程退出不会转为失败。

## 参数

在 v0.1.0 的参数之外增加：

| 参数 | 取值 | 含义 |
| --- | --- | --- |
| `T_reconnect` | 30 s | `active` 运行实例的出站流断开多久之后查询 actor 状态；大于看守进程发现断线的上限 15 s（keepalive 每 10 s 一次，5 s 超时）与重连退避上限 5 s 之和 |

## 与设计的差异

在 v0.1.0 所列差异之外：

- actor 崩溃后运行实例转为失败。设计中崩溃之后重建新的一代并以 `--resume` 续接，v0.2.0 没有代，也没有 `--resume`。

- 收件箱只有人类消息与订阅完成事件两种；`runcontroller` 不保存订阅，唤醒条件仍然只有“已挂起且有未投递的事件”，没有等待行，也不按订阅判定唤醒集合是否为空。
- 后台作业集合只用于禁止挂起。设计中它还决定回合边界能否销毁重建、能否静止冻结，v0.2.0 没有这两种机制。
- 汇入入口是进程内函数，不校验调用方身份，`InboxEvent.Source` 不记录。

## 测试

在假的沙箱提供者与假的看守进程上：

- 去重：同一 `dedup_key` 写入两次，收件箱只有一行，第二次返回同一 `inbox_seq` 且 `duplicate` 为真；运行实例已挂起时，第二次写入不再发起恢复。
- 订阅完成事件唤醒已挂起的运行实例，恢复后以 `subscription_completed` 项投递，字段与写入时一致。
- 人类消息与订阅完成事件交替写入，同一批 `Deliver` 按 `inbox_seq` 排列两种事件。
- 后台作业：回合边界空闲超过 `G_idle` 而 `bg_jobs` 非空时不挂起；`bg_jobs_changed` 报告空集之后从当时起再空闲满 `G_idle` 才挂起。
- 断线期间丢失 `bg_jobs_changed`：重连的 `Hello` 携带的集合替换 `run.bg_jobs`。
- 迁移：v0.1.0 已有的收件箱行迁移后为 `human_message`，人类消息的既有测试照常通过。
- actor 崩溃：出站流断开后假提供者报告 actor 已崩溃，运行实例在 `T_reconnect` 加一个 `P_scan` 之内转为 `failed`，`failure_reason` 写明 actor 状态；actor 未被删除。
- 出站流在 `T_reconnect` 之内重连：不调用 `GetSandbox`，状态不变。
- 假提供者报告运行中，或 `GetSandbox` 返回错误：状态不变，下一轮扫描再查。
- 控制面重启时有 `active` 运行实例，其 actor 已崩溃、看守进程不再重连：启动后 `T_reconnect` 加一个 `P_scan` 之内转为 `failed`。
