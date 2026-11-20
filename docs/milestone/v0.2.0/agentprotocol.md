# agentprotocol（v0.2.0）

v0.2.0 的 `agentprotocol` 在 [v0.1.0](../v0.1.0/agentprotocol.md) 的基础上增加后台作业的上报，并让投递项可以是订阅完成事件。流的建立、替换、断线检测与对账规则都不变，下文只写变化。版本范围见 [v0.2.0](README.md)。

## 接口

新增与修改的消息如下，其余消息与 v0.1.0 相同：

```proto
message Hello {
  AgentState agent_state = 1;
  int64 last_accepted_seq = 2;
  LastTurn last_turn = 3;
  repeated string bg_jobs = 4;            // 新增
}

message AgentEvent {
  string turn_id = 1;
  oneof kind {
    TurnStarted turn_started = 2;
    TurnEnded turn_ended = 3;
    BgJobsChanged bg_jobs_changed = 4;    // 新增
  }
}

message BgJobsChanged {
  repeated string job_ids = 1;
}

message InboxMessage {
  int64 inbox_seq = 1;
  oneof content {                         // text 移入 oneof，字段编号不变
    string text = 2;
    SubscriptionCompleted subscription_completed = 3;
  }
}

message SubscriptionCompleted {
  string sub_id = 1;
  string system = 2;
  string event = 3;
  string subscription_status = 4;
  string terminal = 5;
  map<string, string> correlation = 6;
  google.protobuf.Timestamp occurred_at = 7;
  string source = 8;
  string detail = 9;
  bool detail_truncated = 10;
}
```

| 消息 | 语义 |
| --- | --- |
| `Hello.bg_jobs` | 看守进程当前的后台作业集合，任务标识按字典序排列；Claude Code 尚未启动或已退出时为空 |
| `bg_jobs_changed` | 后台作业集合变化时发出，`job_ids` 是变化之后的完整集合，排序同上；`turn_id` 在回合中取当前回合，在回合边界上为空，控制面不使用它 |
| `InboxMessage` | `text` 与 `subscription_completed` 恰好有一个；人类消息填 `text`，订阅完成事件填 `subscription_completed` |
| `SubscriptionCompleted` | 字段与 `runcontroller` 的 `SubscriptionCompletion` 一一对应，取值见 [connectorhub 的订阅完成事件](connectorhub.md#订阅完成事件) |

`text` 仍不超过 64 KiB，`detail` 不超过 48 KiB。

## 行为

后台作业沿用 v0.1.0 的对账方式：流上没有序号与重放，`bg_jobs_changed` 只在流连通时送达；断线期间的变化由下一条流的 `Hello.bg_jobs` 补齐，控制面以它替换已保存的集合。

`text` 移入 `oneof` 后字段编号不变，人类消息的编码与 v0.1.0 相同。v0.1.0 的看守进程不认识 `subscription_completed`，也不上报后台作业，所以切换到 v0.2.0 的控制面之前，先结束 v0.1.0 的全部运行实例。

## 与设计的差异

在 v0.1.0 所列差异之外：

- `AgentEvent` 只增加了 `bg_jobs_changed`。设计中的 `subscription_created`、`tool_started`、`tool_ended`、`wait_started` 与 `turn_ended.bg_procs` 不实现，订阅只在 `connectorhub` 中登记。
- 投递项按种类分为两种消息，订阅完成事件不带附件引用。

## 测试

- 编解码往返：`Hello.bg_jobs`、`bg_jobs_changed`、两种 `InboxMessage` 与 `SubscriptionCompleted` 的全部字段。
- `InboxMessage` 的两个字段都为空，或者 `text` 超过 64 KiB、`detail` 超过 48 KiB 时，校验不通过。
- 以假的看守进程丢弃 `bg_jobs_changed` 后断线重连，控制面以 `Hello.bg_jobs` 得到正确的集合。
