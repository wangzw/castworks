# agentprotocol（v0.1.0）

v0.1.0 中，看守进程与控制面之间只有一条 gRPC 双向流，消息是设计中[消息闭集](../../design/modules/design-agentprotocol.md#消息闭集)的一个子集，名称与设计相同。版本范围见 [v0.1.0](README.md)。

## 职责

- 定义看守进程到控制面、控制面到看守进程两个方向的消息。
- 规定流的建立、替换与断开后的对账方式，使挂起恢复之后，消息仍然恰好被处理一次。

## 接口

看守进程主动连接控制面的 gRPC 端口，使用明文 HTTP/2（h2c）。服务只有一个方法：

```proto
service AgentStream {
  rpc Connect(stream WardenFrame) returns (stream ControlFrame);
}

message WardenFrame {
  string run_id = 1;
  oneof body {
    Hello hello = 2;
    AgentEvent event = 3;
    Accepted accepted = 4;
  }
}

message ControlFrame {
  string run_id = 1;
  oneof body {
    HelloAck hello_ack = 2;
    Deliver deliver = 3;
    Shutdown shutdown = 4;
  }
}
```

看守进程到控制面：

| 消息 | 字段 | 语义 |
| --- | --- | --- |
| `Hello` | `agent_state{phase, turn_id}`、`last_accepted_seq`、`last_turn{turn_id, output, is_error}` | 每条流的首帧。`phase` 取 `starting`、`in_turn`、`boundary`、`exited`；`last_accepted_seq` 是看守进程已写入 Claude Code 的最大 `inbox_seq`，尚未写入过为 0；`phase=boundary` 时 `last_turn` 携带最近一个回合的结果 |
| `AgentEvent` | `turn_id`，`kind` 为 `turn_started{delivery_id, session_id}` 或 `turn_ended{output, is_error, agent_exited}` | 回合开始与结束。`turn_id` 由看守进程生成，同一运行实例内唯一；`output` 是该回合 `result` 消息的结果文本，超过 1 MiB 时截断并在末尾注明 |
| `Accepted` | `delivery_id`、`through_seq` | 一批投递已写入 Claude Code 的 stdin |

控制面到看守进程：

| 消息 | 字段 | 语义 |
| --- | --- | --- |
| `HelloAck` | `llm_credential{kind, value}` | 接受这条流。`llm_credential` 是 v0.1.0 独有的字段，`kind` 取 `api_key` 或 `oauth_token`，见 [v0.1.0 的安全让步](README.md#安全) |
| `Deliver` | `delivery_id`、`batch[{inbox_seq, text}]`、`through_seq` | 投递一批人类消息，`batch` 按 `inbox_seq` 升序，`through_seq` 是批内最后一条的序号；只在回合边界发出 |
| `Shutdown` | `reason` | 运行实例已结束；看守进程关闭 Claude Code 的 stdin 并退出 |

每条消息的 `text` 不超过 64 KiB。

## 行为

### 建立与替换

- 看守进程发出的首帧必须是 `Hello`，控制面校验 `run_id` 之后回 `HelloAck`。同一条流上后续帧的 `run_id` 与首帧不同时，控制面以 `InvalidArgument` 关闭该流。
- `run_id` 不存在时，控制面以 `NotFound` 关闭流，看守进程退避后重试。从[模板快照](../../glossary.md)启动、尚未属于任何运行实例的看守进程就走这条路径。
- 运行实例已经结束时，控制面以 `FailedPrecondition` 关闭流，看守进程收到后退出。
- 同一运行实例已有一条流时，新到的 `Hello` 取代旧流，控制面关闭旧流。快照恢复后，旧流所在的连接已经失效，但控制面未必察觉，取代规则保证新流立即生效。

### 断线检测与重连

- 看守进程开启 gRPC keepalive，每 10 s 发一次 ping，5 s 内没有应答即判定连接失效并重连。快照恢复后，旧连接靠这一机制在 15 s 内被发现。
- 重连按 1 s、2 s、4 s 退避，上限 5 s，不设次数上限。

### 对账

流上没有序号与重放。断线期间丢失的消息，由下一条流的 `Hello` 补齐：

- `Accepted` 丢失：`Hello.last_accepted_seq` 告诉控制面哪些消息已经写入，控制面据此推进已投递位置。
- `Deliver` 丢失：控制面在 `Hello` 之后发现仍有未投递的消息，重新投递。
- `turn_ended` 丢失：`Hello.agent_state.phase=boundary` 告诉控制面回合已经结束，`last_turn` 补上该回合的结果。
- 重复投递：看守进程只把 `inbox_seq` 大于 `last_accepted_seq` 的消息写入 Claude Code，其余直接以 `Accepted` 确认。

`last_accepted_seq` 保存在看守进程的内存中。v0.1.0 只用 MEMORY 快照挂起，内存随快照保留，不需要写入文件。

## 与设计的差异

- 信封只有 `run_id`。设计中的 `generation`、`msg_seq`、`ack_msg_seq`、`sent_at` 与 `protocol_version` 都不实现：v0.1.0 每个运行实例只有一代，流上不重放，也不协商协议版本。
- `Hello` 不携带令牌、租约、实例公钥与持有证明，不做认证；不实现 `Challenge`，控制面不先发挑战。
- 不实现 `Fenced`、`Close` 等关闭帧，流的关闭以上文的 gRPC 状态码表达；看守进程到控制面方向也不跨连接重放，`turn_ended` 的丢失由 `Hello.last_turn` 补齐。
- keepalive 与重连退避取上文的取值，不取设计中的 `T_keepalive`、`T_keepalive_timeout`、`T_reconnect_min` 与 `T_reconnect_max`。
- `HelloAck.llm_credential` 与 `turn_ended.output` 是设计中没有的字段。前者是安全让步，后者让命令行能显示回合结果。
- `Deliver` 的投递目标固定为下一条用户消息，批内只有文本。
- 不实现 `Consumed`、`DeliveryFailed`、`ReadyToSuspend`、`PrepareSuspend`、`Resumed` 等其余消息。挂起由控制面直接调用 Substrate 完成，看守进程不参与，也不知道自己被挂起过。

## 测试

- 编解码往返与字段校验。
- 首帧不是 `Hello`、`run_id` 不一致、`run_id` 不存在、运行实例已结束四种情形下，流的关闭方式与状态码符合上文。
- 同一运行实例的第二条流取代第一条。
- 以假的看守进程分别丢弃 `Accepted`、`Deliver` 与 `turn_ended`，断线重连后，每条消息恰好被写入一次，回合结果完整。
