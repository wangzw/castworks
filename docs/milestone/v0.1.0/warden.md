# warden（v0.1.0）

v0.1.0 中，看守进程在 actor 里作为 PID 1 运行，维持到控制面的出站流，并以 stream-json 驱动一个跨回合常驻的 Claude Code 进程。它也能在开发机上作为普通进程运行，用于调试。版本范围见 [v0.1.0](README.md)。

## 职责

- 作为 PID 1 监督子进程：回收僵尸进程，收到 `SIGTERM` 时按[关停](#关停)的步骤退出。
- 确定自己属于哪个运行实例。
- 在端口 8081 的 `/readyz` 上应答 Substrate 的 `wakeupProbe`：进程启动后即返回 200，不以连上控制面为条件。`ResumeActor` 因此只等看守进程启动，连上控制面的时间由 `runcontroller` 的 `T_hello` 约束。
- 作为 `agentprotocol` 的客户端维持出站流，见 [agentprotocol](agentprotocol.md)。
- 启动 Claude Code，把投递的消息写入它的 stdin，从它的 stdout 识别回合的开始与结束。

## 运行方式

| 方式 | 身份来源 | 控制面地址 | 用途 |
| --- | --- | --- | --- |
| actor 模式 | `systemInfo` 卷投射的 `/run/ate/actor-name` 文件 | 环境变量 `CASTWORKS_CONTROL_PLANE`，取不带 selector 的 Service 的全限定名加端口 | 演示场景与挂起恢复测试 |
| 本地模式 | 命令行参数 `--run-id` | 命令行参数 `--control-plane`，例如 `127.0.0.1:9090` | 在开发机上调试协议与 Claude Code 的驱动 |

两种方式的代码路径只在身份与地址的来源上不同。本地模式下看守进程不是 PID 1，Claude Code 以当前用户运行。

## 身份

actor 名由 `run_id` 派生，形如 `cw-<run_id>`，见 [sandboxprovider](sandboxprovider.md)。actor 模式下，看守进程启动时读取 `/run/ate/actor-name`（内容没有结尾换行），从中取出 `run_id`。v0.1.0 关闭了模板快照，同一 actor 的快照恢复不改变 actor 名，所以读一次即可。文件内容不符合 `cw-<run_id>` 的形式时，看守进程记录错误并以非零状态退出。

## Claude Code 的启动

看守进程收到第一条 `Deliver` 时才启动 Claude Code，actor 冷启动时只需启动看守进程。启动命令：

```text
claude -p --output-format stream-json --input-format stream-json --verbose --dangerously-skip-permissions
```

| 环境变量 | 取值 |
| --- | --- |
| `ANTHROPIC_API_KEY` 或 `CLAUDE_CODE_OAUTH_TOKEN` | 二选一，取 `HelloAck.llm_credential.value`：`kind=api_key` 时设置前者，`kind=oauth_token` 时设置后者，另一个不设置 |
| `CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS` | `1`，产生回合边界判定所需的 `session_state_changed` |
| `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` | `1` |
| `DISABLE_AUTOUPDATER` | `1` |
| `HOME` | Claude Code 运行用户的主目录 |
| `PATH` | actor 模式为镜像里的固定 `PATH`（与 Dockerfile 的 `ENV PATH` 一致），本地模式为看守进程启动时的 `PATH` |

只注入上表中的变量，看守进程自己的环境变量不传给 Claude Code。

actor 模式下，看守进程与 Claude Code 都以镜像的 `USER`，即非 root 用户 `agent` 运行：Substrate 的模板没有 `runAsUser` 字段，看守进程也不切换用户。Claude Code 的工作目录是 `/home/agent/work`，由看守进程在启动 Claude Code 之前创建。它位于根文件系统中，根文件系统的写入随 MEMORY 快照保存。

## 回合

### 写入消息

收到 `Deliver` 时：

1. 取出批内 `inbox_seq` 大于 `last_accepted_seq` 的消息。没有这样的消息时，直接回 `Accepted`，到此结束。
2. Claude Code 尚未启动时先启动。
3. 把这些消息的文本以空行连接，作为一条 stream-json 用户消息写成 stdin 上的一行，例如 `{"type":"user","message":{"role":"user","content":"……"}}`。一批消息因此只开始一个回合。
4. `last_accepted_seq` 取 `through_seq`，回 `Accepted`。
5. 生成新的 `turn_id`，发出 `turn_started{delivery_id, session_id}`。`session_id` 取自 Claude Code 启动时输出的 `system` 初始化消息。

stdin 在回合之间保持打开，下一批消息写入同一进程。

### 识别回合结束

回合边界的判定沿用设计中 [Claude Code 驱动器](../../design/modules/warden/design-warden-driver-claude-code.md#回合边界判定)的规则：`session_state_changed: idle` 与 `queued_turn_count=0` 的 `result` 消息都到达，两者先后不限。判定之后：

- 发出 `turn_ended{output, is_error, agent_exited=false}`。`output` 取 `result` 消息的结果文本，`is_error` 取它的同名字段。
- 把这个回合的 `turn_id`、`output` 与 `is_error` 保存为 `last_turn`，供下一次 `Hello` 携带。

Claude Code 在回合中退出，或者写入 stdin 失败时，发出 `turn_ended{agent_exited=true}`，`phase` 变为 `exited`。v0.1.0 不重启 Claude Code。

## 出站流

- 连接、keepalive 与重连的规则见 [agentprotocol](agentprotocol.md)。
- 每次连接成功后先发 `Hello`，`agent_state.phase` 取当前状态：Claude Code 尚未启动为 `starting`，回合进行中为 `in_turn`，回合边界为 `boundary`，已退出为 `exited`。
- 收到 `Shutdown`，或者流以 `FailedPrecondition` 关闭时，按[关停](#关停)的步骤退出。

快照恢复后，看守进程不做任何特殊处理：keepalive 发现旧连接失效，看守进程重连并发送 `Hello`。

## 关停

关闭 Claude Code 的 stdin，等待 10 s；仍未退出时发 `SIGTERM`，再等 5 s；仍未退出时发 `SIGKILL`。然后看守进程退出。

## 日志

看守进程的日志按行写到标准输出，Claude Code 的标准错误原样转发，两者都可以用 `kubectl ate logs` 查看，但只在 actor 运行时可读。Claude Code 的 stream-json 输出逐行另存到文件 `/home/agent/.castworks/stream.jsonl`，它随快照保存，供排查问题，也可以作为驱动测试的录制样本。

## 镜像

- 只构建 arm64。
- Claude Code 的版本固定为不低于 2.1.318 的某个版本，镜像构建时写死。
- `USER` 为非 root 用户 `agent`，主目录为 `/home/agent`；包含演示任务所需的基本工具，例如 git。
- 入口是看守进程。
- 推送到镜像仓库后按 digest 引用，见 [sandboxprovider](sandboxprovider.md#actor-模板)。

## 与设计的差异

- 没有本地代理、本地 DNS 与透明拦截器，Claude Code 经出口网关直接连接 Anthropic。
- 没有平台 MCP 服务器与托管 hooks；没有审批，Claude Code 跳过权限确认。
- 不采集活动度，不登记后台作业与自唤醒源，不解析等待意图。
- 不经引导接口领取令牌与租约，没有实例密钥。
- 快照恢复后不校时，也不主动断开 Claude Code 的旧连接，见 [v0.1.0 的已知限制](README.md#已知限制)。
- 不处理挂起相关的消息：挂起由控制面直接调用 Substrate 完成，看守进程不参与。

## 测试

- 回合边界判定：用录制的 stream-json 样本覆盖 `result` 与 `idle` 两种到达顺序，以及 `queued_turn_count` 大于 0 的情形。
- 去重：同一批消息重复投递时只写入一次，并照常回 `Accepted`。
- 身份：`actor-name` 的内容符合 `cw-<run_id>` 时以其中的 `run_id` 连接，不符合时以非零状态退出。
- `wakeupProbe`：进程启动后、连上控制面之前，`/readyz` 已返回 200。
- 本地模式端到端：在开发机上连接真实控制面，用真实的 Claude Code 跑完两个回合，第二个回合引用第一个回合的结果。
- 关停：Claude Code 不响应 stdin 关闭时，按顺序升级到 `SIGTERM` 与 `SIGKILL`。
