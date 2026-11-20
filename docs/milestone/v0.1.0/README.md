# v0.1.0 MVP 范围

v0.1.0 是概念验证，不上生产。它只证明一件事：在 Substrate 上，Agent 等人回复时，castworks 能把沙箱挂起、释放计算资源，人回复后再恢复，Agent 带着原有上下文继续工作。只覆盖“等人”这一种等待，演示场景之外的功能一律不做。

[设计文档](../../design/README.md)仍然是长期目标。v0.1.0 只实现其中很小的一部分，并且有意偏离设计中的若干做法，偏离之处在下文与各模块文档中逐条写明；v0.1.0 的代码不声称满足[架构不变量](../../design/architecture.md#架构不变量)。

各模块在 v0.1.0 中的细化：[runcontroller](runcontroller.md)、[agentprotocol](agentprotocol.md)、[warden](warden.md)、[sandboxprovider](sandboxprovider.md)、[humanchannel](humanchannel.md)。

## 演示场景

1. 用命令行创建一个运行实例，并附上第一条提示。控制面在 Substrate 上创建 actor，看守进程启动 Claude Code 执行这条提示，回合的输出回显到命令行。
2. 回合结束后空闲满 30 s，控制面对 actor 做快照挂起。actor 进入 `SUSPENDED`，占用的 worker 容量被释放。
3. 用命令行再发一条消息，消息只引用上一轮的结果，不重复上下文，例如“把刚才那个文件改成……”。控制面恢复 actor，看守进程重新连上控制面并把消息交给 Claude Code，Claude Code 在原有上下文中完成这一回合。
4. 用命令行结束运行实例，actor 被删除。

满足下列条件即视为 v0.1.0 完成：

- 场景在开发集群上完整跑通，第 3 步的回复表明 Claude Code 保留了上一轮的上下文，上一轮写出的文件仍然存在。
- 同一运行实例连续挂起、恢复 10 次，每条消息恰好得到一次处理，不丢失，不重复。
- 运行实例处于挂起状态时重启控制面，之后发来的消息仍能恢复该运行实例并得到处理。

## 验收结果

2026-11-19（UTC）在开发集群上重跑验收。actor 镜像为 `registry.zhanwei.wang/castworks/actor@sha256:8347ea189ec39bf58d9ceec7b788f3a67431fe62b9b25e8df89ee54f4dd2ff5c`，来源提交记为 `430f63a`：这个值取自 `measurements.json` 的 `repo_commit` 字段，该字段是操作者经 `ACTOR_COMMIT` 环境变量传入的，没有独立的构建记录。控制面与看守进程代码与提交 `0add82d` 相同；当时的验收代码还没有 `hello_at` 放宽，`hello_ms` 为空的挂起记录按失败计，判定比现在更严。`430f63a` 之后，看守进程自身的生产代码只有 `819141d` 一处改动：`internal/warden/claudecode/stream.go` 增加 3 行，把 `rate_limit_event` 按信息类消息处理，不再落入未知消息分支。所以镜像与验收时代码的看守进程行为相同，只差这条日志的级别。已采集的日志中 `ignored stdout message` WARN 出现 3 次，类型都是 `rate_limit_event`，原因就是镜像尚未包含这条改动；行为不受影响，没有重建镜像。另有运行目录 `20261119T234309Z` 单独跑过一次 `TestSuspendResume/Demo` 并通过，两个回合的 `is_error` 均为 false，第二个回合在恢复后完成；该次未计入[技术验证](../verification.md)的 12 个样本。`TestSubstrateTwoTurns` 一次通过（18.84 s），`TestSuspendResume` 的三个子测试全部通过。各项耗时、快照大小与样本数见[技术验证](../verification.md)。

| 通过标准 | 结论 | 依据 |
| --- | --- | --- |
| 场景在开发集群上完整跑通，第 3 步的回复表明 Claude Code 保留了上一轮的上下文，上一轮写出的文件仍然存在 | 通过 | `TestSuspendResume/Demo`（54.66 s）。两个回合都经 `castworks` 命令行操作，`is_error` 均为 false。第一个回合写入含随机标记的文件，回复不回显文件内容；第二个回合在恢复后完成，输出 `file=8c7ab221bfa93786`、`pass=pass-dmzl5esbn0ks`、`mtime=1795131905`，与第一个回合写入的文件一致。回合结束到挂起开始间隔 34580 ms，不小于 `G_idle` 的 30 s；actor 进入 `SUSPENDED` 后不再占用 worker。挂起记录 1 条，`snapshot_ms=1040`、`restore_ms=270`、`hello_ms=939`。 |
| 同一运行实例连续挂起、恢复 10 次，每条消息恰好得到一次处理，不丢失，不重复 | 通过 | `TestSuspendResume/TenCycles`（449.36 s）。11 个回合全部 `is_error=false`，10 次挂起都在 `suspended` 状态下由新消息触发恢复，`sent_while_suspended` 都为 true。第 k 次恢复后的回合输出 `count=k+1`、`last=k`（k=1～10）；最后一个回合回复的 `counter.txt` 为 `0,1,2,3,4,5,6,7,8,9,10`。收件箱的 11 条消息与 11 个回合按 `delivery_id` 一一对应，没有重复投递，也没有丢失；计数放在运行实例再次进入挂起之后，给重复投递留出一个扫描周期的机会。 |
| 运行实例处于挂起状态时重启控制面，之后发来的消息仍能恢复该运行实例并得到处理 | 通过 | `TestSuspendResume/RestartWhileSuspended`（55.52 s）。第一个回合结束后 actor 为 `SUSPENDED`，控制面收到 `SIGTERM` 以 0 退出，actor 保持挂起。用同一数据库重启控制面后，发消息前运行实例仍为 `suspended`；消息触发恢复并完成第二个回合，输出 `file=9d80471d6b669804`、`pass=pass-dmzlbu97ln0i`。挂起记录 1 条，`snapshot_ms=975`、`restore_ms=237`、`hello_ms=968`。 |

## 挂起机制

挂起只用 Substrate 的 MEMORY 快照，时机是回合边界。快照保存了 Claude Code 进程的内存和 actor 的文件系统，恢复后进程从挂起处继续运行，所以 v0.1.0 不需要工作区持久化、Claude Code 的 `--resume` 续接、跨代去重与引导接口。

设计在回合边界默认采用销毁重建，v0.1.0 只用快照，销毁重建留给后续版本。快照恢复不递增代，v0.1.0 中每个运行实例只有一代。

快照采用同步模式：Substrate 等对象存储中的快照写完才返回，与设计一致。

## 组成

| 组成部分 | 运行位置与内容 |
| --- | --- |
| 控制面进程 | 开发机上的普通进程，包含 `runcontroller`、`sandboxprovider` 与 `humanchannel` 的 HTTP API |
| PostgreSQL | 开发机上的 docker 容器，保存运行实例与收件箱；控制面用 55432 的 `postgres`，测试用 55433 的 `postgres-test` |
| 命令行 | 在开发机上运行，调用控制面 HTTP API |
| actor 镜像 | 看守进程与 Claude Code，只构建 arm64，由 Substrate 运行 |

只有 actor 运行在集群里，控制面不构建镜像，也不部署进集群。设计中这些模块是各自独立的进程，v0.1.0 把它们合进一个进程，模块之间仍按[模块依赖图](../../design/architecture.md#模块依赖图)的方向调用。

看守进程也能在开发机上作为普通进程运行，直接连接本机的控制面，用于调试协议与 Claude Code 的驱动，不必每次构建镜像、创建 actor。挂起与恢复只能在 Substrate 上测试。

## 各模块的范围

| 模块 | v0.1.0 做 | v0.1.0 不做 |
| --- | --- | --- |
| `runcontroller` | 运行实例记录与简化状态机：预配中、运行中、空闲、挂起中、已挂起、恢复中、已结束、失败；人类消息的收件箱；空闲满 30 s 即挂起的固定规则；挂起状态下收到消息即恢复 | 唤醒谓词的一般形式、代与失效判定、`Accepted` 与 `Consumed` 两级确认、效果记录、对账器、定时器、订阅 |
| `suspendpolicy` | 不单独实现，挂起规则写在 `runcontroller` 中 | 全部 |
| `agentprotocol` | 看守进程到控制面的一条 gRPC 双向流，消息为 `Hello`、`HelloAck`、`AgentEvent`（回合开始与回合结束，结束时携带输出）、`Deliver`、`Accepted` 与 `Shutdown` | 协议版本协商、流上的序号与重放、令牌 |
| `warden` | 作为 PID 1 运行；以 stream-json 输入输出启动常驻的 `claude -p`，Claude Code 以非 root 用户运行；转发回合开始与结束；把投递的消息写入 Claude Code 的 stdin；快照恢复后重新连接控制面 | 活动度采集、本地代理、本地 DNS 与透明拦截器、平台 MCP 服务器、hooks、后台作业登记、自唤醒源镜像、等待意图 |
| `sandboxprovider` | Substrate 提供者调用 ate-api 完成 actor 模板、actor 的创建、挂起、恢复与删除，以及每个 actor 的 EgressPolicy；另有本地提供者，只用于实现顺序第 4 步的本地模式，不创建资源，不挂起 | k8s 提供者、能力集合、引导接口、路由器唤醒、出口策略的按代落地与撤销 |
| `humanchannel` | 控制面 HTTP API 与命令行：创建运行实例、发消息、查看输出、结束运行实例 | 界面、OIDC 认证、项目角色、即时通讯与 Issue 评论、审批、终端与文件访问 |
| `llmgateway` | 不实现，Claude Code 直接连接 Anthropic | 全部 |
| `connectorhub` | 不实现 | 全部 |
| `secrets` | 不实现，LLM 凭据存放在开发机上控制面读取的本地文件中，控制面配置写明它是 API 密钥还是 OAuth 令牌 | 全部 |
| `workspace` | 不实现，Claude Code 的工作目录就在 actor 自己的文件系统中，随快照保存 | 全部 |

## 消息与挂起并发时不丢消息

设计中的并发正确性，v0.1.0 只保留这一条，它是这次概念验证能否成立的前提。

- 挂起的决定与收件箱检查在同一个事务中完成：锁住运行实例行，确认没有未投递的消息，再把状态改为挂起中。
- 写入消息的事务同样锁住运行实例行。运行实例已挂起时，同一事务把状态改为恢复中并发起恢复；运行实例正在挂起中时，只写入消息，挂起完成后发现有未投递的消息，立即发起恢复。
- 消息只在空闲状态下投递。看守进程确认已写入 Claude Code 的 stdin 后，控制面才把它标为已投递；挂起只在全部消息都已确认之后发生。

## 网络

Substrate 把 actor 的全部 TCP 出站送到出口网关，EgressPolicy 没有放行的连接一律拒绝；没有 EgressPolicy 的 actor 连域名解析都得不到应答（[REF-substrate](../../references.md)）。因此控制面为每个 actor 创建 EgressPolicy，只放行两类出站：

- `api.anthropic.com:443` 以 `tls_passthrough` 规则透传，出口网关不终止 TLS，Claude Code 不需要信任出口网关的 CA。
- 控制面的 gRPC 端口以 `http` 规则放行，看守进程与控制面之间是明文 gRPC（h2c）。actor 的域名解析只应答出口策略中列出的主机，所以控制面在集群里以一个不带 selector 的 Service 出现，全限定名为 `castworks-dev.castworks-system.svc.cluster.local`，它的 EndpointSlice 指向开发机的地址；规则与看守进程拨号都使用这个全限定名，见 [sandboxprovider](sandboxprovider.md#网络)。

Claude Code 以 `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` 运行，不发出遥测一类的非必要流量。验收只在 `oauth_token` 凭据下观察到 Claude Code 只访问 `api.anthropic.com`，`api_key` 凭据下尚未观察；出现其他主机时加入出口策略。演示任务只在 actor 本地读写文件，不访问其他主机。

## 模板快照

Substrate 的新 actor 默认从[模板快照](../../glossary.md)恢复。v0.1.0 在 actor 模板中关闭模板快照，新 actor 冷启动，理由见 [sandboxprovider](sandboxprovider.md#actor-模板)。看守进程启动时从 `system-info` 卷投射的 actor 名得知所属的运行实例。

## 实现顺序

每一步结束时都有可以运行的结果。第 1 步最先完成，它决定控制面留在开发机还是部署进集群。此后各模块按冻结的接口并行开发，各自以替身测试；在真实环境上的验收按编号顺序进行，前一步通过之后再做下一步：

1. 验证网络：在 `castworks-dev` 中起一个临时 actor，经出口网关与不带 selector 的 Service 访问开发机上的一个测试端口。不通时先改为把控制面部署进集群，再继续。
2. `agentprotocol` 的定义与代码生成，以及编解码测试。
3. `runcontroller` 与 `humanchannel`：在假的沙箱提供者与假的看守进程上通过 [runcontroller](runcontroller.md#测试) 的全部测试，命令行可以操作。
4. 看守进程的本地模式：在开发机上连接真实控制面，驱动真实的 Claude Code 跑完两个回合，同时录制驱动测试用的 stream-json 样本。
5. Substrate 提供者与 actor 镜像：在 Substrate 上创建运行实例，完成两个回合并结束，此时还不挂起。
6. 挂起与恢复：打开挂起扫描，跑通演示场景，并逐条检查通过标准。

## 安全

v0.1.0 只保留以下三项：

- Substrate 自带的 gVisor 隔离。
- 控制面的 HTTP API 只监听开发机的回环地址，即 `127.0.0.1` 或 `[::1]`。
- LLM 凭据不进代码仓库，不打进镜像。

以下让步在生产环境中都不可接受：

- LLM 凭据由控制面在 `Hello` 的应答中交给看守进程，看守进程按凭据种类以环境变量 `ANTHROPIC_API_KEY` 或 `CLAUDE_CODE_OAUTH_TOKEN` 交给 Claude Code，二者只设其一，Agent 能读到它。
- 控制面的 gRPC 端口监听开发机的网卡地址，集群 Pod 与内网中的其他机器都能连接。
- 控制面 API、看守进程与控制面之间都不做认证。
- 没有审批流程，Claude Code 以跳过权限确认的模式运行，全部工具调用直接执行。

## 已知限制

- actor 崩溃后，该运行实例无法恢复。挂起、恢复与预配过程中的崩溃分别经 `Snapshot` 失败、`RestoreSnapshot` 失败与等待 `Hello` 超时转为失败；运行实例处于 `active` 时 actor 崩溃不会被发现，出站流断开后不再有看守进程重连，运行实例停在原状态，只能人工结束。这项检测由 [v0.2.0](../v0.2.0/runcontroller.md#actor-崩溃的检测) 补上。
- 控制面在挂起中、恢复中这类过渡状态重启时，运行实例可能停在该状态，需要人工处理。通过标准只要求已挂起状态下的重启。
- Claude Code 的后台任务与会话内定时随快照一起停住，到点不会唤醒 actor，要等下一条消息恢复 actor 之后才继续。
- 恢复后，Claude Code 在挂起前建立的 HTTPS 连接已经失效。v0.1.0 依靠 Claude Code 自身的重试处理，第一次 LLM 请求可能因此失败一次。
- `turn_ended` 在断线中丢失，而重连前 Claude Code 已开始自发回合时，`Hello` 的 `phase` 是 `in_turn`，不带 `last_turn`，上一回合的结果补不回来。
- 只有一个用户，所有运行实例互相可见。

## 不在范围内

演示场景之外的设计内容都不在 v0.1.0 中，主要包括：等待 LLM、外部系统与审批时的挂起，冻结与销毁重建，k8s 提供者与预热池，GitHub 与其他外部系统的接入，SPIFFE mTLS、运行实例令牌、实例密钥与接入票据，OIDC 与界面，恢复准入与优先级，等待时长预测，多租户。

## 开发环境需求

v0.1.0 需要以下条件：

- 开发机上能运行 docker，用于 PostgreSQL。
- 镜像仓库中可推送的 castworks 路径。
- atespace `castworks-dev`；ServiceAccount `castworks-system/sandboxprovider`，在 `castworks-dev` 的访问策略中持有 `editor` 角色。控制面以 TokenRequest 为它签发令牌，经端口转发调用 ate-api，所以开发机的 kubeconfig 需要 `ate-system` 中的端口转发权限，以及 ClusterTrustBundle 的读取权限。
- 命名空间 `castworks-system` 中的 gVisor WorkerPool，带标签 `castworks.dev/pool: castworks`，单个 worker 的资源上限至少容纳一个 2 CPU、2 GiB 的 actor。
- 命名空间 `castworks-system` 中不带 selector 的 Service `castworks-dev` 及其 EndpointSlice，指向开发机的地址与控制面的 gRPC 端口 9090。
- 从 actor 经出口网关到开发机地址的网络可达。这一条要在实现之前最先验证；不可达时，控制面改为部署进集群。
- 镜像内的 Claude Code 版本固定，不自动更新。
