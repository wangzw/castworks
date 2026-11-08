# 看守进程作为沙箱 PID 1 并以管道驱动 Agent

## 背景

沙箱要运行完整的第三方 Agent 进程，同时要有组件驱动它、监督它、冻结它并向控制面报告。Agent 以无头模式通过 stream-json 输入输出与会话续接运行（[REF-claude-code-headless](../references.md)）。回合开始与结束由 stream-json 的 `result`（含 `queued_turn_count`）与 `session_state_changed` 的 `idle` 状态表达；`requires_action` 表示会话阻塞在权限询问等请求上；工具开始与结束由 `assistant` 与 `user` 消息中的 `tool_use`/`tool_result` 块表达；权限询问经 `control_request`/`control_response` 消息与 `canUseTool` 回调表达；后台作业由 `task_started`、`task_progress`、`task_notification` 与 `background_tasks_changed` 等消息表达（[REF-claude-agent-sdk-typescript](../references.md)）。这些信号必须被翻译成协议事件，投递必须经管道交给 Agent。

驱动、冻结与监督组件不能与 Agent 一起被冻结：它要应答探针、续约工作区租约、维持出站流并持有本地代理的上游连接。它也不能在崩溃后被就地重启：内存中的活动度摘要、本地代理缓冲与出站流状态无法一致重建，Agent 可能正处于回合中，重启后的监督关系不清晰。

同类运行时（例如 AX）以 PID 1 守护进程作为沙箱入口，并以就绪探针门控恢复（[REF-google-ax](../references.md)）；其 runner 契约中的进程组、探针与回环元数据可以借鉴，但沙箱生命周期与恢复语义按本系统的设计重写。

## 考虑过的选项

### subreaper 加外部 init 监督重启

采用它意味着看守进程作为普通守护进程，由外部 init 以 subreaper 方式监督；看守进程崩溃时由 init 就地重启。**被拒**：重启丢失活动度摘要、本地代理缓冲、出站流与投递在途状态；Agent 可能处于回合中，重启后的监督关系与会话续接不清晰；两个监督者竞争进程树；k8s 的就绪探针与 PID 1 语义不在同一处。

### 双容器 Pod：看守进程 sidecar 加 Agent 容器内 launcher

采用它意味着看守进程与 Agent 分属两个容器，Agent 容器内的 launcher 中继 stdio，看守进程经容器间通道驱动。**被拒**：多一个容器与一个中继；stdio 与信号跨容器传递；冻结粒度要同时覆盖两个容器；Pod 生命周期与 PID 1 归属复杂；Agent 容器仍需要一个监督者，监督链更长。

### 看守进程作为 PID 1，直接以管道驱动 Agent

采用它意味着看守进程是沙箱容器的 PID 1，Agent 是它直接 spawn 并以管道驱动的子进程；看守进程负责进程监督、Agent 驱动器、本地代理、本地 DNS 与透明拦截器、平台 MCP 服务器、活动度采集、租约续约、投递意图日志与出站流；看守进程崩溃即沙箱崩溃，走重建与「回合被中断」路径。**采用**——见决策。

## 决策

采用看守进程作为沙箱容器的 PID 1：它直接以管道驱动 Agent 子进程，承担进程监督、Agent 驱动器、本地代理、本地 DNS 与透明拦截器、平台 MCP 服务器、活动度采集、工作区租约续约、投递意图日志与出站流；看守进程崩溃即沙箱崩溃，运行实例走重建与「回合被中断」路径。

## 后果

本决策使项目承担以下义务（穷举）：

- 看守进程必须是沙箱容器的 PID 1，回收子进程、处理信号并监督 Agent 进程树；Agent 以运行实例 uid 启动，设置 `no_new_privs` 并清空 capability bounding set。
- 看守进程必须实现 Agent 驱动器接口、本地代理、本地 DNS 与透明拦截器、平台 MCP 服务器、活动度采集、租约续约、投递意图日志与出站流；这些职责不能拆分给一个会被冻结或独立重启的进程。
- 探针只配置在看守进程上：PID 1 是沙箱的稳定入口，k8s 的就绪探针与 PID 1 语义必须同处，探针不能挂在会被冻结或随回合重建的 Agent 进程上；解冻与 preStop 的顺序由冻结粒度决定，见 [ADR-freeze-via-node-agent](adr-freeze-via-node-agent.md)。
- 看守进程崩溃时，控制面必须按沙箱丢失处理，重建沙箱并把被中断的回合通知 Agent。
- 销毁执行前必须把恢复句柄落库；重建后以恢复句柄续接会话。
- 看守进程留在父 cgroup，Agent 进程树位于节点冻结代理维护的 `agent` 子 cgroup；看守进程以停止态启动 Agent 主进程，并与节点冻结代理协调把它移入该子 cgroup 后再继续（[ADR-freeze-via-node-agent](adr-freeze-via-node-agent.md)）。

承担的代价穷举如下：

- 看守进程是沙箱内的单点；它的崩溃会杀死 Agent 并强制重建。
- 看守进程的 CPU 与内存开销计入沙箱，在沙箱资源预算内单列配额，不得挤占 Agent 的配额。
- 看守进程的缺陷影响所有运行实例，其测试必须覆盖看守进程承担的全部职责路径，包括 PID 1 的僵尸回收、信号处理与进程组管理，并达到为该组件设定的覆盖率下限。
- 看守进程不能就地重启，短期故障也会触发完整重建。

## 失效条件

- 沙箱运行时提供等价的监督者，能冻结 Agent 进程树、维持探针与出站流，并在看守进程崩溃后就地恢复一致状态时，PID 1 的职责可以外移。
- Agent 能以库的形式嵌入控制面，不再需要沙箱内进程与 stdio 通道时，本决策不再适用。
