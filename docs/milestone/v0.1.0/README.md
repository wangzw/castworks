# v0.1.0 MVP 范围

v0.1.0 是概念验证，不上生产。它只证明一件事：在 Substrate 上，Agent 等人回复时，castworks 能把沙箱挂起、释放计算资源，人回复后再恢复，Agent 带着原有上下文继续工作。只覆盖“等人”这一种等待，演示场景之外的功能一律不做。

[设计文档](../../design/README.md)仍然是长期目标。v0.1.0 只实现其中很小的一部分，并且有意偏离设计中的若干做法，偏离之处在下文逐条写明；v0.1.0 的代码不声称满足[架构不变量](../../design/architecture.md#架构不变量)。各模块在 v0.1.0 中的细化文档放在本目录下。

## 演示场景

1. 用命令行创建一个运行实例，并附上第一条提示。控制面在 Substrate 上创建 actor，看守进程启动 Claude Code 执行这条提示，回合的输出回显到命令行。
2. 回合结束后空闲满 30 s，控制面对 actor 做快照挂起。actor 进入 `SUSPENDED`，占用的 worker 容量被释放。
3. 用命令行再发一条消息，消息只引用上一轮的结果，不重复上下文，例如“把刚才那个文件改成……”。控制面恢复 actor，看守进程重新连上控制面并把消息交给 Claude Code，Claude Code 在原有上下文中完成这一回合。
4. 用命令行结束运行实例，actor 被删除。

满足下列条件即视为 v0.1.0 完成：

- 场景在开发集群上完整跑通，第 3 步的回复表明 Claude Code 保留了上一轮的上下文，上一轮写出的文件仍然存在。
- 同一运行实例连续挂起、恢复 10 次，每条消息恰好得到一次处理，不丢失，不重复。
- 运行实例处于挂起状态时重启控制面，之后发来的消息仍能恢复该运行实例并得到处理。

## 挂起机制

挂起只用 Substrate 的 MEMORY 快照，时机是回合边界。快照保存了 Claude Code 进程的内存和 actor 的文件系统，恢复后进程从挂起处继续运行，所以 v0.1.0 不需要工作区持久化、Claude Code 的 `--resume` 续接、跨代去重与引导接口。

设计在回合边界默认采用销毁重建，v0.1.0 只用快照，销毁重建留给后续版本。快照恢复不递增代，v0.1.0 中每个运行实例只有一代。

快照采用同步模式：Substrate 等对象存储中的快照写完才返回，与设计一致。

## 组成

| 组成部分 | 内容 |
| --- | --- |
| 控制面进程 | 一个进程，包含 `runcontroller`、`sandboxprovider` 与 `humanchannel` 的 HTTP API，部署在 `castworks-system` 命名空间 |
| PostgreSQL | 保存运行实例与收件箱 |
| 命令行 | 调用控制面 HTTP API |
| actor 镜像 | 看守进程与 Claude Code，只构建 arm64 |

设计中这些模块是各自独立的进程，v0.1.0 把它们合进一个进程，模块之间仍按[模块依赖图](../../design/architecture.md#模块依赖图)的方向调用。

## 各模块的范围

| 模块 | v0.1.0 做 | v0.1.0 不做 |
| --- | --- | --- |
| `runcontroller` | 运行实例记录与简化状态机：预配中、运行中、空闲、挂起中、已挂起、恢复中、已结束、失败；人类消息的收件箱；空闲满 30 s 即挂起的固定规则；挂起状态下收到消息即恢复 | 唤醒谓词的一般形式、代与失效判定、`Accepted` 与 `Consumed` 两级确认、效果记录、对账器、定时器、订阅 |
| `suspendpolicy` | 不单独实现，挂起规则写在 `runcontroller` 中 | 全部 |
| `agentprotocol` | 看守进程到控制面的一条 gRPC 双向流，消息只有 `Hello`、回合开始、回合结束（携带输出）与投递消息四种 | 协议版本协商、重放、去重、令牌 |
| `warden` | 作为 PID 1 运行；以 stream-json 输入输出启动常驻的 `claude -p`，Claude Code 以非 root 用户运行；转发回合开始与结束；把投递的消息写入 Claude Code 的 stdin；快照恢复后重新连接控制面 | 活动度采集、本地代理、本地 DNS 与透明拦截器、平台 MCP 服务器、hooks、后台作业登记、自唤醒源镜像、等待意图 |
| `sandboxprovider` | 只实现 Substrate 提供者：直接调用 ate-api 完成 actor 模板、actor 的创建、挂起、恢复与删除，以及每个 actor 的 EgressPolicy | k8s 提供者、能力集合、引导接口、路由器唤醒、出口策略的按代落地与撤销 |
| `humanchannel` | 控制面 HTTP API 与命令行：创建运行实例、发消息、查看输出、结束运行实例 | 界面、OIDC 认证、项目角色、即时通讯与 Issue 评论、审批、终端与文件访问 |
| `llmgateway` | 不实现，Claude Code 直接连接 Anthropic | 全部 |
| `connectorhub` | 不实现 | 全部 |
| `secrets` | 不实现，LLM 密钥存放在 `castworks-system` 的 k8s Secret 中 | 全部 |
| `workspace` | 不实现，Claude Code 的工作目录就在 actor 自己的文件系统中，随快照保存 | 全部 |

## 消息与挂起并发时不丢消息

设计中的并发正确性，v0.1.0 只保留这一条，它是这次概念验证能否成立的前提。

- 挂起的决定与收件箱检查在同一个事务中完成：锁住运行实例行，确认没有未投递的消息，再把状态改为挂起中。
- 写入消息的事务同样锁住运行实例行。运行实例已挂起时，同一事务把状态改为恢复中并发起恢复；运行实例正在挂起中时，只写入消息，挂起完成后发现有未投递的消息，立即发起恢复。
- 消息只在空闲状态下投递。看守进程确认已写入 Claude Code 的 stdin 后，控制面才把它标为已投递；挂起只在全部消息都已确认之后发生。

## 网络

Substrate 把 actor 的全部 TCP 出站送到出口网关，EgressPolicy 没有放行的连接一律拒绝；没有 EgressPolicy 的 actor 连域名解析都得不到应答（[REF-substrate](../../references.md)）。因此控制面为每个 actor 创建 EgressPolicy，只放行两类出站：

- `api.anthropic.com:443` 以 `tls_passthrough` 规则透传，出口网关不终止 TLS，Claude Code 不需要信任出口网关的 CA。
- 控制面的 gRPC 端口以 `http` 规则放行，看守进程与控制面之间是明文 gRPC（h2c）。

Claude Code 以 `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` 运行，不发出遥测一类的非必要流量。演示任务只在 actor 本地读写文件，不访问其他主机。

## 模板快照

Substrate 的新 actor 默认从[模板快照](../../glossary.md)恢复，模板快照制作时看守进程已经启动，但还不属于任何运行实例。所以看守进程每次连接控制面时，都从 `systemInfo` 卷投射的 `actor-name` 读取当前 actor 名，控制面按 actor 名找到对应的运行实例；Claude Code 在看守进程得知所属运行实例、收到第一条消息之后才启动，模板快照中没有 Claude Code 进程。

## 安全

v0.1.0 只保留以下三项：

- Substrate 自带的 gVisor 隔离。
- 控制面 API 只在集群内可达，Service 为 ClusterIP，本机经 `kubectl port-forward` 访问。
- LLM 密钥不进代码仓库，不打进镜像。

以下让步在生产环境中都不可接受：

- LLM 密钥由控制面在 `Hello` 的应答中交给看守进程，看守进程以环境变量 `ANTHROPIC_API_KEY` 交给 Claude Code，Agent 能读到它。
- 控制面 API、看守进程与控制面之间都不做认证。
- 没有审批流程，Claude Code 以跳过权限确认的模式运行，全部工具调用直接执行。

## 已知限制

- actor 崩溃后，该运行实例无法恢复，状态置为失败。
- 控制面在挂起中、恢复中这类过渡状态重启时，运行实例可能停在该状态，需要人工处理。通过标准只要求已挂起状态下的重启。
- Claude Code 的后台任务与会话内定时随快照一起停住，到点不会唤醒 actor，要等下一条消息恢复 actor 之后才继续。
- 恢复后，Claude Code 在挂起前建立的 HTTPS 连接已经失效。v0.1.0 依靠 Claude Code 自身的重试处理，第一次 LLM 请求可能因此失败一次。
- 只有一个用户，所有运行实例互相可见。

## 不在范围内

演示场景之外的设计内容都不在 v0.1.0 中，主要包括：等待 LLM、外部系统与审批时的挂起，冻结与销毁重建，k8s 提供者与预热池，GitHub 与其他外部系统的接入，SPIFFE mTLS、运行实例令牌、实例密钥与接入票据，OIDC 与界面，恢复准入与优先级，等待时长预测，多租户。

## 开发环境需求

v0.1.0 只需要开发集群上的以下条件：

- `castworks-system` 命名空间中的 PostgreSQL。
- 镜像仓库中可推送的 castworks 路径。
- atespace `castworks-dev`，以及在其中持有 `editor` 角色、供控制面调用 ate-api 的 ServiceAccount。
- 镜像内的 Claude Code 版本固定，不自动更新。
