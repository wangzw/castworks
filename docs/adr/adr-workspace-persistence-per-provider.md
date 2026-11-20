# 工作区持久化按沙箱提供者选择

## 背景

工作区必须在沙箱销毁与重建之间保留，并承载 Agent 的会话状态（`CLAUDE_CONFIG_DIR`，[REF-claude-code-env-vars](../references.md)）与投递意图日志。各代沙箱以逐字一致的路径挂载工作区。会话状态与投递意图日志都需要同步写入的持久化语义：接受记录同步写入完成后才回 `Accepted` 并交给 Agent，消费记录同步写入完成后才发 `Consumed`；投递确认的级序见 [ADR-delivery-intent-log](adr-delivery-intent-log.md)。`durableDir` 配置下，工作区回退会连同会话记录一起回退。

k8s 提供者的沙箱 Pod 由 agent-sandbox 的 Sandbox CR 管理（[REF-agent-sandbox](../references.md)）。共享文件系统（RWX）无挂接步骤、无节点亲和，预热 Pod 在创建时即可获得工作区挂载点，认领时由节点侧组件绑定运行实例子目录，无需卷挂接。块存储（RWO）有节点或可用区亲和，不能预绑定预热池，并发挂接与强制分离存在等待。共享文件系统没有 RWO 的单节点挂接限制；但 RWO 只限制挂接节点，不阻止同节点多个 Pod 或旧代沙箱写入，不能替代工作区租约；共享文件系统与块存储都由工作区租约串行化写者。

Substrate 提供者的 actor 只支持 `durableDir`、`external`（CSI 动态供给）、`image` 与 `systemInfo` 卷，不能直挂 PVC 或透传 worker Pod 的卷（[REF-substrate](../references.md)）。外部 CSI 卷写穿持久、崩溃不丢、可在沙箱外经同一导出访问；`durableDir` 只在挂起时上传到对象存储，actor 崩溃会丢失上次快照之后的全部写入，沙箱外没有读取接口。两者都只随 actor 存续：外部 CSI 卷按 actor 供给，删除 actor 时随之删除；`durableDir` 保存在 actor 的快照里，同样随 actor 删除。工作区能跨代保留，依赖每个运行实例只用一个 actor（[ADR-substrate-actor-per-run](adr-substrate-actor-per-run.md)）。

## 考虑过的选项

### 块存储作为默认

采用它意味着所有提供者都以 RWO 块存储承载工作区。**否决**：有节点或可用区亲和，不能预绑定预热池；并发挂接与强制分离存在等待；块存储冷启动重建只能达到 p95 < 60 s，作为默认承载时无法满足共享文件系统路径 p95 < 20 s 的冷启动目标。

### k8s 共享文件系统，Substrate 只用 durableDir

采用它意味着 k8s 用 RWX 共享文件系统，Substrate 统一用 `durableDir`。**否决**：`durableDir` 在 actor 崩溃时丢失上次快照之后的全部写入；沙箱外没有读取接口，从界面读取工作区时必须唤醒运行实例；回合边界销毁的同步挂起包含 `durableDir` 的上传，销毁时延随工作区大小增长；不能作为默认持久化方式。

### 按提供者选择持久化方式

采用它意味着 k8s 用共享文件系统并以块存储为退化配置，Substrate 优先用外部 CSI 卷、以 `durableDir` 为退化配置。退化配置指首选持久化机制不可用或不适配时的后备承载方式。Substrate 的退化配置下，工作区以回合边界销毁的同步挂起上传完成为持久化，回合中崩溃按“回合被中断”重投事件并接受该回合文件改动丢失；工作区模块向两个提供者提供同一套抽象。**采纳**，见决策。

## 决策

采用按沙箱提供者选择工作区持久化：k8s 提供者使用 RWX 共享文件系统并以块存储为退化配置，Substrate 提供者优先使用运行实例所属 actor 的外部 CSI 卷并以 `durableDir` 为退化配置，两者随该 actor 跨代保留（[ADR-substrate-actor-per-run](adr-substrate-actor-per-run.md)）。`durableDir` 下工作区以回合边界销毁的同步挂起上传完成为持久化，回合中崩溃按“回合被中断”重投事件并接受该回合文件改动丢失；工作区模块向两个提供者提供同一套绑定、持久化确认、沙箱外访问与租约抽象。

## 后果

本决策使项目承担以下义务，穷举如下：

- k8s 提供者必须在每个沙箱与预热 Pod 内以逐字一致的路径挂载本运行实例的工作区子目录，挂载根不进入 Pod，挂载方式见 [ADR-per-run-workspace-mount](adr-per-run-workspace-mount.md)；块存储退化配置必须遵守节点或可用区亲和、挂接上限与强制分离等待。
- 工作区写者必须以工作区租约串行化，与持久化方式无关：k8s 的共享文件系统与块存储、Substrate 的外部 CSI 卷与 `durableDir` 皆同；租约续约失败时看守进程必须冻结或终止 Agent 进程树并上报。
- Substrate 提供者必须优先使用外部 CSI 卷；`durableDir` 退化配置下，回合边界销毁的同步挂起必须在上传完成后才返回，上传确认之后工作区才算已持久化；回合中崩溃必须重投事件，已接受的事件带 `redelivery=true`。
- 工作区必须承载 Agent 的会话状态与投递意图日志，会话状态与投递意图日志不得放在 rootfs；同步写入必须在 `Accepted`、`Consumed` 之前完成。
- 沙箱外文件访问在外部 CSI 卷的导出可直接读取时读取导出（只在 actor 存在期间），其余经入站路由到达看守进程的文件 API；后者可能唤醒快照态或销毁态的运行实例。
- 共享文件系统必须满足目录权限、uid 与配额；`no_root_squash` 仅用于[节点挂载代理](../glossary.md)以 root 创建运行实例子目录；Agent 二进制与依赖缓存放在镜像内，不放在工作区。

承担的代价穷举如下：

- 共享文件系统是共享失败域，需要 RWX 存储；性能、配额与 `no_root_squash` 的安全约束由部署承担，其中 `no_root_squash` 只对节点侧组件生效。
- 块存储退化配置有挂接与亲和限制，重建时延高于共享文件系统。
- Substrate 的 `durableDir` 退化配置在崩溃时丢失上次快照之后的写入；`durableDir` 的上传计入回合边界销毁的同步挂起时延；沙箱外读取需要唤醒运行实例。
- Substrate 的外部 CSI 卷处于预览阶段，可用性、性能与沙箱外访问需要验证。
- 两套持久化模型增加测试与运维复杂度；工作区抽象必须隐藏差异，否则上层模块会出现提供者分支。
- 工作区租约引入续约、过期与接管路径，租约失败会终止 Agent 进程树并触发重建。

## 失效条件

- 出现一种存储原语同时提供同步持久化、跨提供者挂载、沙箱外访问与崩溃安全，且不需要工作区租约时，按提供者分别选择持久化方式不再必要。
- Substrate 的外部 CSI 卷成为唯一受支持的路径且 `durableDir` 被移除，或 k8s 提供者获得等价的外部 CSI 卷能力时，退化配置与补偿路径可以删除。
