# 架构决策记录

本目录记录在用的架构决策：一条记录对应一个难以逆转或影响模块边界、公共接口、数据模型、部署形态、外部依赖与安全态势的选择，陈述约束、权衡过的选项、采用的决策与接受的代价。

## 运行实例生命周期

| 决策 |
| --- |
| [运行实例独立于沙箱存续](adr-run-outlives-sandbox.md) |

## 等待与唤醒

| 决策 |
| --- |
| [在持久化收件箱上等待](adr-wait-on-durable-inbox.md) |
| [收件箱事件跨代保留、唤醒信号按代失效、代范围等待与定时器随代作废](adr-generation-scoped-waits.md) |

## 唤醒源与流量

| 决策 |
| --- |
| [外部唤醒源与外部流量由托管侧持有，Agent 自唤醒源由托管侧镜像](adr-host-owns-external-wake-sources.md) |
| [平台不执行第三方操作，只提供出口凭据替换、连接器事件日志与订阅](adr-platform-does-not-execute-external-operations.md) |
| [LLM 流量经沙箱内本地代理与 LLM 网关两级](adr-two-tier-llm-proxy.md) |

## 并发与所有权

| 决策 |
| --- |
| [每运行实例单写者与汇入例外](adr-single-writer-per-run.md) |

## 投递与一致性

| 决策 |
| --- |
| [投递幂等状态随工作区持久化](adr-delivery-intent-log.md) |
| [外部副作用经效果记录在提交后执行](adr-effect-record-after-commit.md) |
| [以持久化状态与纠正性对账收敛](adr-state-based-reconciliation.md) |

## 进程与循环

| 决策 |
| --- |
| [同时支持沙箱内完整 Agent 与控制面外置循环](adr-dual-agent-loop-location.md) |
| [看守进程作为沙箱 PID 1 并以管道驱动 Agent](adr-warden-is-pid1.md) |

## 沙箱提供者

| 决策 |
| --- |
| [同时实现 k8s 与 Substrate 两个沙箱提供者](adr-dual-sandbox-providers.md) |
| [冻结由节点冻结代理对 Agent 子 cgroup 执行](adr-freeze-via-node-agent.md) |

## 挂起与恢复

| 决策 |
| --- |
| [回合位置随运行实例持久化](adr-turn-position-persisted.md) |
| [挂起与恢复策略以进程内库调用](adr-suspendpolicy-as-library.md) |
| [控制面发起的恢复经恢复准入放行](adr-single-recovery-admission.md) |
| [以挂起为目的的销毁只在回合边界](adr-turn-boundary-destroy.md) |
| [挂起机制按提供者能力分层](adr-capability-tiered-suspension.md) |

## 工作区

| 决策 |
| --- |
| [工作区持久化按沙箱提供者选择](adr-workspace-persistence-per-provider.md) |

## 失效判定

| 决策 |
| --- |
| [按代拒绝旧沙箱的消息与副作用](adr-generation-fencing.md) |

## 租户

| 决策 |
| --- |
| [保留租户维度而不实现跨租户隔离](adr-tenant-dimension-without-enforcement.md) |
