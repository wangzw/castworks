# Substrate 上每个运行实例只用一个 actor，每代是它的一次冷启动

## 背景

运行实例跨越多代沙箱（[ADR-run-outlives-sandbox](adr-run-outlives-sandbox.md)），工作区必须在代之间保留，并承载 Agent 的会话记录与投递意图日志（[ADR-workspace-persistence-per-provider](adr-workspace-persistence-per-provider.md)、[ADR-delivery-intent-log](adr-delivery-intent-log.md)）。新代只在沙箱提供者确认旧代沙箱实例已结束，或旧租约到期之后接管工作区；旧代的令牌、消息与出口按代拒绝（[ADR-generation-fencing](adr-generation-fencing.md)）。以挂起为目的的销毁发生在回合边界，新代在新的进程中以 Claude Code 的 `--resume` 续接会话（[ADR-turn-boundary-destroy](adr-turn-boundary-destroy.md)）。

Substrate v1.0.0（[REF-substrate](../references.md)）中与此相关的行为如下：

- 外部 CSI 卷（`Volume.external`，预览门 `ExternalVolumes`）按 actor 供给，只属于该 actor，随 actor 删除。卷在 actor 首次 `ResumeActor` 时由 CSI 创建；挂起、`RevertActor` 与崩溃只卸载卷，只有 `DeleteActor` 删除卷。卷内容不进快照，`RevertActor` 不回滚卷内容。
- `durableDir` 的内容保存在 actor 的快照里（gVisor 下为 tmpfs）。每个 actor 只保留一份快照，每次挂起成功后替换为新的一份，`DeleteActor` 时删除。比 actor 存续更久的只有快照标签（Tag）；带外部卷的模板不允许以 `source_tag` 创建 actor，从 MEMORY 保真度的标签创建的 actor 恢复的是进程内存。
- 快照保真度（`VOLUMES`、`ROOTFS`、`MEMORY`）在 actor 模板上设定。actor 处于 `SUSPENDED` 时可以用 `UpdateActor` 改用另一个模板，新模板的卷与挂载必须完全相同；此后的下一次 `ResumeActor` 只恢复卷与 `durableDir`，容器从镜像冷启动。
- `RevertActor` 接受处于 `RUNNING` 或 `CRASHED` 的 actor，以及最近快照只在节点本地的 `SUSPENDED` actor：它终止沙箱、卸载卷、丢弃节点本地的检查点与未完成的上传，使 actor 停在最近一次已完成的持久快照并处于 `SUSPENDED`。`CRASHED` 的 actor 必须先退回才能恢复；没有持久快照时，下一次恢复为冷启动。
- 一个 actor 同一时刻至多有一个沙箱；每个 actor 至多有一个出口策略（`EgressPolicy`，名为 `default`）。

外部 CSI 卷承载工作区时，actor 崩溃后卷上的会话记录与投递意图日志比最近一次快照里的进程状态新。以退回后的内存继续运行，进程看到的会话与投递进度落后于工作区。

## 考虑过的选项

### 每代删除旧 actor、新建 actor

采用它意味着每代沙箱是一个新 actor：整代销毁删除该代 actor，下一代以新的 actor 名创建。**否决**：`DeleteActor` 删除该 actor 的外部 CSI 卷与保存 `durableDir` 的快照，两种工作区都不能跨代保留。

### 以快照标签跨代携带工作区

采用它意味着销毁前给 actor 的快照打标签，下一代以 `source_tag` 创建新 actor。**否决**：带外部卷的模板不允许以 `source_tag` 创建 actor，外部 CSI 卷无法这样携带；`durableDir` 虽能随标签携带，但 MEMORY 保真度的标签恢复的是旧进程的内存，新代得不到从头启动的看守进程。

### 保留 NFS 子目录并挂到新 actor

采用它意味着把 StorageClass 的回收策略设为保留，删除 actor 之后 NFS 子目录留在导出上，再让下一代 actor 的卷指向它。**否决**：外部卷由 Substrate 按 actor 在 CSI 中创建，卷的标识与子目录随每个 actor 各自生成，Substrate API 没有把已有卷交给另一个 actor 的途径；保留与复用依赖 CSI 驱动在 Substrate API 之外的行为与按 actor 的卷命名。

### Substrate 上不做以挂起为目的的销毁

采用它意味着 Substrate 路径只有不挂起与快照，一代覆盖 actor 的全部存续，回合边界不换新进程。**否决**：外部 CSI 卷承载工作区时，崩溃恢复仍须冷启动并以 `--resume` 续接，冷启动的机制省不掉；长等待之后也失去在新进程中续接的选择，只能恢复保存旧进程的内存快照。

### 每运行实例一个 actor，以两份内容相同的模板交替实现每代冷启动

采用它意味着运行实例的首次预配创建 actor，`ReleaseRun` 才删除它；工作区是它的外部 CSI 卷或 `durableDir`，随 actor 存续；每代沙箱是该 actor 的一次冷启动，冷启动由 actor 挂起时改用另一份内容相同的模板触发。**采纳**，见决策。

## 决策

采用 Substrate 上每个运行实例只用一个 actor、每代是该 actor 的一次冷启动：

- actor 由运行实例的首次预配创建，只由 `ReleaseRun` 删除，即运行实例显式结束或放弃之后、工作区不再需要时。工作区是该 actor 的外部 CSI 卷（首选，`sync` 持久化语义，actor 存在期间可在沙箱外经同一导出读取）或 `durableDir`（退化配置，`on_suspend` 持久化语义），两者都与 actor 同寿命，因而跨代保留。k8s 提供者不受本决策影响。
- 提供者为每个模板版本维护两份只差槽位标记的 actor 模板，称为模板槽位（template slot）。结束一代时，提供者使 actor 改挂与其最近一次持久快照所用模板不同的槽位，下一次 `ResumeActor` 因而是冷启动。新的一代从不新建 actor：首代是 actor 的首次激活（有模板快照时从模板快照恢复），此后每代是一次冷启动，新的看守进程经引导接口领取，Agent 以 `claude -p --resume` 续接会话。
- 以挂起为目的的销毁（回合边界，I9 安全点）：Agent 正常退出后，提供者同步挂起 actor（`durableDir` 在这次挂起中上传），再切换槽位。该代的销毁确认是“同步挂起完成且槽位已切换”，actor 不删除。
- 不以挂起为目的的销毁（本代沙箱作废、进入 `Degraded`、`fence_and_bump` 对旧代的销毁、`sandbox_id` 核对不符等）：actor 处于 `RUNNING` 或 `CRASHED` 时以 `RevertActor` 终止当前沙箱，已处于 `SUSPENDED` 时不需终止，随后切换槽位。旧代进程由 Substrate 一个 actor 只有一个沙箱的约束停止，它与按 `(run_id, generation)` 撤销出口策略共同构成 Substrate 上对旧代的失效判定。
- 崩溃恢复：`CRASHED` 的 actor 经 `RevertActor`、切换槽位后以 `ResumeActor` 冷启动，Agent 从工作区以 `--resume` 续接。外部 CSI 卷承载工作区时，崩溃之后不恢复内存；`durableDir` 承载工作区时，工作区回退到最近一次快照，按“回合被中断”重投。
- 同一代内的 MEMORY 快照（回合中快照与快照挂起）不变：同一 actor、同一代、同一沙箱实例。
- Substrate 上的 `sandbox_id` 标识 actor 的一次激活（首代为首次激活，此后为每次冷启动），不标识 actor 本身。actor 名与 actor JWT 跨代不变，引导接口把领取绑定到运行实例的当前代与当前沙箱实例，每个 `(actor, 代)` 只领取成功一次。

## 后果

本决策使项目承担以下义务，穷举如下：

- 提供者以 `run_id` 派生 actor 名，每个运行实例一个，并记录 actor 当前承载的代与 `sandbox_id`。整代销毁与单个销毁只在 actor 仍承载该代时结束其沙箱；迟到的旧代销毁不触动已承载更新代的 actor。
- 提供者必须能区分以挂起为目的的销毁与其他销毁：前者同步挂起，后者以 `RevertActor` 终止，`runcontroller` 在以挂起为目的的整代销毁中携带挂起尝试的 `suspend_id`。
- 每一代的预配先使 actor 处于 `SUSPENDED` 且改挂合适的槽位，遗留的沙箱以 `RevertActor` 结束；随后把 actor 的出口策略替换为本代副本、写入本代引导记录，再调用 `ResumeActor`。出口策略在撤销某一代时只在它仍承载该代副本时删除。
- 两个槽位的模板声明相同的卷、挂载、快照位置与 `preferredFidelity: MEMORY`。模板版本升级同样成对提供，新的一代改挂当前版本的槽位；改变卷或挂载的模板版本不能用于已有的运行实例。
- 外部 CSI 卷承载工作区时，同一代的快照恢复遇到 `CRASHED` 的 actor 不退回重试，以确定性错误返回，由新一代冷启动承接。
- 槽位切换强制冷启动、冷启动之后的快照仍为 MEMORY 保真度、外部 CSI 卷在挂起、退回、崩溃与冷启动之后保留且只随 `DeleteActor` 删除，以及看守进程（PID 1）退出后 actor 的状态，都须在目标集群实测；验证通过前 Substrate 路径不上线。

承担的代价穷举如下：

- 每次以挂起为目的的销毁都同步挂起 actor：写入对象存储的内存快照不会再被恢复，销毁时延与对象存储写入高于直接删除。每次挂起成功替换上一份快照，存储占用不随代数累积。
- 销毁态的运行实例保留 actor、最近一份快照与外部卷，直到 `ReleaseRun`；`ReleaseRun` 删除 actor 后，工作区随之删除，此后不可读。
- 旧代沙箱的停止依赖 Substrate 一个 actor 只有一个沙箱，以及换模板后的恢复只能是冷启动；这两点来自源码阅读，尚未在目标集群实测。
- actor JWT 不区分代：旧代在途的领取请求若在新代引导记录写入之后才被处理，新代看守进程的领取得到 409，运行实例按沙箱丢失再重建一代。
- 入站请求经路由器唤醒销毁态的 actor 时得到一次冷启动：看守进程领取不到引导记录，不启动 Agent，但占用 worker 容量，直到对账器回收或下一代预配结束它。
- 外部 CSI 卷仍是预览特性，不在 Substrate 的兼容承诺内。
- 每个模板版本维护两份模板。

## 失效条件

- Substrate 提供在 `DeleteActor` 之后保留、可交给另一个 actor 的卷，或支持带外部卷的模板以标签创建 actor，使每代新建 actor 也能保留工作区。
- 实测表明挂起状态下换模板之后的 `ResumeActor` 不是冷启动，或冷启动之后的快照不再是 MEMORY 保真度。
- Substrate 允许一个 actor 同时存在多个沙箱，或外部 CSI 卷在挂起、退回或崩溃时丢失内容。
