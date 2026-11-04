# 冻结由节点冻结代理对 Agent 子 cgroup 执行

## 背景

冻结要停止 Agent 进程树的 CPU 调度而保留内存，同时不能停止看守进程：看守进程要继续应答探针、续约工作区租约、维持出站流并持有本地代理的上游连接。被冻结的必须只是 Agent 进程树。

本项目的沙箱以非特权容器运行，宿主不向容器委派 cgroup，容器内的看守进程无法自行写 `cgroup.freeze`。用 SIGSTOP 逐个停止进程不是原子操作：扫描期间进程可以继续 fork，SIGCHLD 与定时器会在停止过程中产生噪声，长冻结下这些问题会放大。

cgroup v2 提供 `cgroup.freeze` 接口（[REF-linux-cgroup-v2](../references.md)），把 Agent 进程树放进容器 cgroup 下的子 cgroup 后，冻结与解冻都是对子 cgroup 的一次写入。定位 Pod 与容器的 cgroup 路径需要容器运行时的信息，节点级特权 DaemonSet 具备这一能力。k8s 提供者的 Sandbox CR 由 agent-sandbox 管理（[REF-agent-sandbox](../references.md)），节点冻结代理与它并列部署。

## 考虑过的选项

### 容器内看守进程自行写 cgroup

采用它意味着看守进程在容器内创建子 cgroup 并写 `cgroup.freeze`。**被拒**：本项目的沙箱以非特权容器运行，宿主不向容器委派 cgroup；要让容器可写就必须把沙箱提权，破坏隔离边界。

### 仅使用 SIGSTOP 冻结

采用它意味着看守进程按运行实例的 uid 扫描 `/proc`，对进程集合迭代发送 SIGSTOP，直到连续两次扫描没有新 PID 且全部处于 T 态。**被拒**：不是原子操作，扫描期间新 fork 的进程会逃逸；SIGCHLD 与定时器信号在停止过程中产生噪声；轮次上限与收敛依赖进程行为；只允许短冻结，无法支撑长等待。

### 冻结整个 Pod

采用它意味着对容器或 Pod 的整个 cgroup 执行冻结，看守进程与 Agent 一起停止。**被拒**：看守进程停止后无法应答探针、续约租约、维持出站流或持有本地代理的上游连接；探针失败会触发 kubelet 重启；工作区租约过期会让其他副本接管；出站流断开使控制面判定沙箱丢失。

### 节点冻结代理对 Agent 子 cgroup 执行冻结，SIGSTOP 仅作降级

采用它意味着沙箱节点池运行节点冻结代理（特权 DaemonSet），经容器运行时定位 Pod 与容器的 cgroup 路径，在容器 cgroup 下创建子 cgroup `agent`；看守进程以停止态启动 Agent 主进程，节点冻结代理把它移入子 cgroup 后再继续，后续 fork 自动继承；冻结写 `agent/cgroup.freeze`，解冻写回；无节点冻结代理时退回 SIGSTOP，只允许短冻结。**采用**——见决策。

## 决策

采用节点冻结代理执行冻结：沙箱节点池的每个节点运行一个节点冻结代理（特权 DaemonSet），由它定位 Pod 与容器的 cgroup 路径，在容器 cgroup 下维护子 cgroup `agent`，看守进程留在父 cgroup；冻结与解冻以 `agent/cgroup.freeze` 执行，冻结与解冻以 `freeze_id` 标识；无节点冻结代理时退回看守进程的 SIGSTOP 扫描，只允许短冻结。

## 后果

本决策使项目承担以下义务（穷举）：

- 沙箱节点池必须允许特权 DaemonSet 并授予其访问容器运行时的权限；每个节点一个节点冻结代理 Pod。
- 看守进程必须以停止态启动 Agent 主进程，并与节点冻结代理协调把它移入 `agent` 子 cgroup 后再继续；后续 fork 必须继承该子 cgroup。
- 探针只配置在看守进程上；看守进程自身不得被冻结；preStop 必须先解冻再终止，否则 SIGTERM 会送给冻结进程。
- 冻结、解冻与原地缩小 CPU request 必须以 `freeze_id` 幂等；已执行过解冻的 `freeze_id` 之后到达的冻结与缩小请求不得生效。
- 冻结态的 Pod 必须标注 `safe-to-evict=false`（[REF-cluster-autoscaler-faq](../references.md)），但节点内存压力下仍可能被驱逐；状态机必须把「冻结态且沙箱已死」转入重建路径。
- SIGSTOP 降级路径必须按运行实例 uid 扫描，迭代到连续两次扫描无新 PID 且全部处于 T 态，设置轮次上限，超限回 `Busy`；该路径只允许短冻结。
- 节点冻结代理不可用时，策略必须把可用冻结限制为 SIGSTOP 短冻结，或选择不挂起。

承担的代价穷举如下：

- 特权 DaemonSet 扩大集群权限面与运维面；cgroup 路径发现依赖容器运行时。
- 看守进程与节点冻结代理的启动协调增加一个失败模式；节点冻结代理不可达时只能降级或放弃冻结。
- SIGSTOP 降级路径非原子，存在已接受的进程逃逸、SIGCHLD 噪声与定时器集中触发缺陷。
- 冻结态 Pod 被驱逐时仍会丢失内存状态，只能走重建路径。
- 冻结与原地缩放的 `freeze_id` 语义要求提供者与看守进程共同维护，跨模块约束更强。

## 失效条件

- Kubernetes 为非特权 Pod 提供 cgroup 委派或原生冻结接口，容器内看守进程可以安全执行冻结时，节点冻结代理不再必需。
- 沙箱运行时提供原子、无需宿主特权的进程树冻结能力时，本决策的节点冻结代理路径可被替换。
