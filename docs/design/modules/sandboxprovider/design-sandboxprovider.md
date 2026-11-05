# sandboxprovider

本模块实现[沙箱提供者](../../../glossary.md)：把控制面选定的预配、认领、冻结、解冻、快照、快照恢复、销毁与资源释放翻译为具体提供者层的调用，并把提供者能力与出口策略落地结果暴露给控制面。k8s 子模块基于 kubernetes-sigs/agent-sandbox（[REF-agent-sandbox](../../../references.md)），覆盖冻结层；substrate 子模块基于 agent-substrate/substrate（[REF-substrate](../../../references.md)），覆盖快照层；k8s 子模块与 substrate 子模块共用看守进程、协议与控制面（[ADR-dual-sandbox-providers](../../../adr/adr-dual-sandbox-providers.md)）。本模块不决定何时挂起，不维护运行实例状态机，不生成出口策略内容，不拥有工作区绑定；这些分别归 `suspendpolicy`、`runcontroller`、`connectorhub` 与 `workspace`。心智模型：本模块是控制面与提供者层之间的适配层，把「把这一代沙箱冻结、快照、销毁或重建」翻译成各提供者的原语，并以代、标识与墓碑保证重复调用、迟到调用与旧代调用不产生第二次副作用。

文档较长，不宜顺序通读。第一次阅读依次取：职责、外部接口契约中的调用语义、核心数据结构中的能力集合与降级、走查、恢复时延分解；术语表、依赖、UML 图与核心数据结构作为核对接口与字段的参考；错误与失败语义、并发与事务边界、扩展点留到最后，按墓碑与幂等规则自测迟到调用与旧代调用的处置。

## 职责

职责穷举如下，每条先写本模块执行的动作，再写产生的结果或边界；能力集合是闭合集合，机制选择由 `suspendpolicy` 完成（[ADR-capability-tiered-suspension](../../../adr/adr-capability-tiered-suspension.md)）。

- **能力自描述**：返回能力集合的闭合取值 `{Freeze{cgroup | signal}, Snapshot, Destroy, InPlaceResize, WarmPool, EgressInject}`；能力集合是 `suspendpolicy` 选择机制的唯一依据，本模块不替调用方选择机制。
- **预配与认领**：按 `(run_id, generation, attempt_id)` 幂等创建沙箱或从预热池认领沙箱，调用 `workspace` 绑定工作区，按代落地出口策略，返回 `sandbox_id`、放置位置与工作区持久化语义；创建请求在整代销毁或按运行实例释放的墓碑之后被拒绝。
- **冻结与解冻**：按 `freeze_id` 执行 `Freeze{cgroup}`，并在 `InPlaceResize` 能力允许时缩小 CPU request；`Freeze{signal}` 的冻结与解冻由看守进程执行，本模块只声明该能力；已解冻的 `freeze_id` 不再接受冻结与缩小，冻结只作用于 Agent 子 cgroup，看守进程保持可运行（[ADR-freeze-via-node-agent](../../../adr/adr-freeze-via-node-agent.md)）。
- **快照与快照恢复**：按 `suspend_id` 执行快照并报告快照保留期限；按 `resume_id` 在同一代内恢复快照并返回沙箱标识；快照不存在或已过期时明确报告；按 `suspend_id` 撤销尚未确认的快照尝试，撤销即墓碑。
- **销毁与释放**：按 `(run_id, generation)` 整代销毁并返回携带被删除代的确认；按 `(generation, sandbox_id)` 单个销毁；整代销毁与单个销毁都只作用于该代的沙箱实例，不删除更新的代；按 `run_id` 幂等释放运行实例级资源，释放之后拒绝该运行实例的一切创建（[ADR-generation-fencing](../../../adr/adr-generation-fencing.md)）。
- **查询**：批量列举沙箱并返回每个沙箱的 `run_id`、代、`sandbox_id` 与放置位置；按 `sandbox_id` 查询单个沙箱；按节点查询冻结态内存、可分配内存与节点内存:CPU 比；查询原地放大的 `Deferred` 状态；按机制给出提供者侧的恢复时延估计。
- **出口策略**：保存 `connectorhub` 推送的每运行实例出口策略；在预配与认领时按 `(run_id, generation)` 落地本代副本；推送的更新只同步到尚未撤销的代；按 `(run_id, generation)` 撤销该代副本，撤销即墓碑，已撤销的代拒绝此后的落地与更新。
- **预热池**：维护按镜像、规格与节点池或可用区分片的预热池容量；认领时返回可用的沙箱；容量按 `N = λL + z_{1−ε}·√(λL) + B` 计算，分片的容量各自计算。
- **恢复触发**：为 substrate 子模块经 Substrate 路由器调用 `/wake` 触发恢复；为 k8s 子模块执行解冻、把 Sandbox CR 改回 `Running` 或从预热池认领；解冻以 `Thawed` 为完成观察，快照恢复与销毁态重建以同代或新代 `Hello` 为完成观察。
- **串行化与幂等**：对同一运行实例的 `Suspend`、`Resume` 与 `Destroy` 按到达顺序串行；每个调用以 `freeze_id`、`suspend_id`、`resume_id`、`(run_id, generation)` 或 `(generation, sandbox_id)` 幂等，重复调用返回首次结果。

### 非职责

- 挂起决策归 `suspendpolicy`：等待时长预测、机制选择、冻结中复评与升级梯、恢复准入的限速与优先级、抖动预算。本模块只提供能力集合、生命周期调用与恢复时延估计。
- 运行实例状态机归 `runcontroller`：状态、子状态、代递增、游标、唤醒谓词、投递与对账。本模块只执行状态机选定的提供者操作，并返回确认、删除代与错误。
- 工作区绑定与持久化归 `workspace`：租约、绑定、持久化确认与沙箱外访问。本模块在预配与认领时调用绑定，不持有租约表。
- 出口策略内容生成归 `connectorhub`：允许主机、占位符替换规则与平台出口代理的请求级校验。本模块只保存、落地与撤销策略副本。
- 沙箱内进程监督、Agent 驱动器与 `Freeze{signal}` 的看守进程侧执行归 `warden`；本模块只提供节点冻结代理的 `Freeze{cgroup}` 执行与提供者层调用。
- 以挂起为目的的销毁的安全点检查归 `runcontroller` 与 `warden`（I9）；本模块不判断 `ReadyToSuspend` 的前置条件。
- 协议信封、消息定义与出站流归 `agentprotocol`；本模块只使用 `/readyz` 与 `/wake` 入站端点触发恢复。

## 术语表

| 术语 | 英文对照 | 适用范围 | 定义 |
| --- | --- | --- | --- |
| 能力集合 | capability set | sandboxprovider | 提供者自描述的闭合能力取值，`suspendpolicy` 只在其中选择机制 |
| 创建墓碑 | creation tombstone | sandboxprovider | 整代销毁或按运行实例释放之后，提供者据以拒绝该代或该运行实例后续创建的持久记录 |
| 撤销墓碑 | revocation tombstone | sandboxprovider | 出口策略按代撤销之后，提供者据以拒绝该代后续落地与更新的持久记录 |
| 快照撤销墓碑 | snapshot cancellation tombstone | sandboxprovider | 按 `suspend_id` 撤销未确认的快照尝试之后，同一 `suspend_id` 的迟到快照请求不生效的持久记录 |
| 冻结墓碑 | freeze tombstone | sandboxprovider | 按 `freeze_id` 解冻之后，同一 `freeze_id` 的迟到冻结与缩小请求不生效的持久记录 |
| 出口策略副本 | egress policy copy | sandboxprovider | 以 `(run_id, generation)` 为键、由 `connectorhub` 推送策略生成的本代落地对象 |
| 冻结标识 | freeze_id | sandboxprovider | 一次冻结尝试的标识，冻结、解冻与缩小 CPU request 以它幂等 |
| 预热池分片 | warm pool shard | sandboxprovider | 按镜像、规格与节点池或可用区划分的独立预热池，容量各自计算 |

## 外部接口契约

稳定性承诺按接口类别给出：生命周期接口对 `runcontroller` 稳定；出口策略接口对 `connectorhub` 稳定；提供者在能力集合内声明的能力可以增加，扩展能力集合的闭合取值、删除能力或改变已有能力的语义是破坏性变更；墓碑与幂等语义是提供者必须满足的契约，不满足的实现不是合规提供者。生命周期调用经 `runcontroller` 的效果记录在数据库事务提交后发出；出口策略推送由 `connectorhub` 直接调用。`sandboxprovider` 不读写控制面的表，也不参与控制面事务。`Provider.Thaw` 是提供者方法；协议消息 `Thaw{suspend_id}` 由 `runcontroller` 经出站流发送给看守进程，不调用本接口。

```go
type Provider interface {
    Capabilities(ctx context.Context) CapabilitySet
    Provision(ctx context.Context, req ProvisionRequest) (SandboxHandle, error)
    Claim(ctx context.Context, req ProvisionRequest) (SandboxHandle, error)
    Freeze(ctx context.Context, req FreezeRequest) (FreezeReceipt, error)
    Shrink(ctx context.Context, req ShrinkRequest) error
    Thaw(ctx context.Context, req ThawRequest) error
    Snapshot(ctx context.Context, req SnapshotRequest) (SnapshotReceipt, error)
    RevokeSnapshot(ctx context.Context, suspendID string) error
    RestoreSnapshot(ctx context.Context, req RestoreRequest) (SandboxHandle, error)
    Destroy(ctx context.Context, ref GenerationRef) (DestroyReceipt, error)
    DestroySandbox(ctx context.Context, ref SandboxRef) error
    ReleaseRun(ctx context.Context, runID RunID) error
    ListSandboxes(ctx context.Context, filter SandboxFilter) ([]SandboxInfo, error)
    GetSandbox(ctx context.Context, ref SandboxRef) (SandboxInfo, error)
    NodeMemory(ctx context.Context, sel NodeSelector) ([]NodeMemory, error)
    ResizeStatus(ctx context.Context, ref SandboxRef) (ResizeStatus, error)
    EstimateResume(ctx context.Context, mechanism ResumeMechanism) (LatencyEstimate, error)
    PutEgressPolicy(ctx context.Context, policy EgressPolicyDocument) error
    RevokeEgressPolicy(ctx context.Context, ref GenerationRef) error
}

type CapabilitySet struct {
    FreezeCgroup  bool
    FreezeSignal  bool
    Snapshot      bool
    Destroy       bool
    InPlaceResize bool
    WarmPool      bool
    EgressInject  bool
}

type ProvisionRequest struct {
    RunID      RunID
    Generation int
    AttemptID  string              // 取 resume_id
    Token      string              // 运行实例令牌，经沙箱创建的注入参数交给看守进程
    LeaseID    string              // 工作区租约标识，交给看守进程用于首次续约与 Hello
    Workspace  WorkspaceBinding
}

type SandboxHandle struct {
    RunID      RunID
    Generation int
    SandboxID  string
    Placement  string
    Durability string              // sync 或 on_suspend
    State      string              // creating、running、frozen、snapshotted、destroyed、released
}

type FreezeRequest struct {
    RunID      RunID
    Generation int
    SandboxID  string
    FreezeID   string              // 取冻结尝试的 suspend_id
    Variant    FreezeVariant       // cgroup；signal 由看守进程执行，提供者收到 signal 以参数非法拒绝
}

type FreezeReceipt struct {
    FreezeID string
    FrozenAt time.Time
}

type ShrinkRequest struct {
    RunID      RunID
    Generation int
    SandboxID  string
    FreezeID   string              // 必须是已冻结且未解冻的标识
}

type ThawRequest struct {
    RunID      RunID
    Generation int
    SandboxID  string
    FreezeID   string
    Variant    FreezeVariant       // cgroup；signal 由看守进程执行，提供者收到 signal 以参数非法拒绝
}

type SnapshotRequest struct {
    RunID      RunID
    Generation int
    SandboxID  string
    SuspendID  string
}

type SnapshotReceipt struct {
    SnapshotID        string
    SnapshotExpiresAt time.Time
}

type DestroyReceipt struct {
    RunID      RunID
    Generation int
}

type RestoreRequest struct {
    RunID      RunID
    Generation int
    SuspendID  string
    AttemptID  string              // 取 resume_id
}

type GenerationRef struct {
    RunID      RunID
    Generation int
}

type SandboxRef struct {
    Generation int
    SandboxID  string
}

type EgressPolicyDocument struct {
    RunID  RunID
    Policy json.RawMessage        // connectorhub 序列化后的出口策略，提供者不解释其字段
}
```

### 调用语义与稳定承诺

| 调用 | 幂等键 | 结果 | 失败分类 |
| --- | --- | --- | --- |
| `Provision`、`Claim` | `(run_id, generation, attempt_id)` | `SandboxHandle` | 墓碑拒绝与参数非法为确定性；提供者不可达与超时为暂时性 |
| `Freeze` | `freeze_id` | `FreezeReceipt` | 已解冻标识的迟到冻结为确定性拒绝 |
| `Thaw` | `freeze_id` | 空 | 幂等；重复调用返回成功 |
| `Shrink` | `freeze_id` | 空 | 已解冻标识的迟到缩小为确定性拒绝；提供者不具备 `InPlaceResize` 时返回能力缺失 |
| `Snapshot` | `suspend_id` | `SnapshotReceipt`，含保留期限 | 快照撤销墓碑之后的迟到请求为确定性拒绝 |
| `RevokeSnapshot` | `suspend_id` | 空 | 幂等；重复撤销返回成功，撤销即快照撤销墓碑 |
| `RestoreSnapshot` | `(run_id, generation, attempt_id)` | `SandboxHandle` | 快照不存在或已过期为确定性；提供者不可达为暂时性 |
| `Destroy` | `(run_id, generation)` | `DestroyReceipt`，携带被删除代 | 幂等；重复调用返回首次确认 |
| `DestroySandbox` | `(generation, sandbox_id)` | 空 | 幂等；目标不存在返回成功 |
| `ReleaseRun` | `run_id` | 空 | 幂等；重复调用返回成功 |
| `PutEgressPolicy` | `run_id` | 空 | 已撤销代的落地为确定性拒绝 |
| `RevokeEgressPolicy` | `(run_id, generation)` | 空 | 幂等；重复调用返回成功 |
| `ListSandboxes`、`GetSandbox`、`NodeMemory`、`ResizeStatus`、`EstimateResume`、`Capabilities` | 无 | 观察结果 | 只读调用，失败不改变提供者状态 |

- **稳定承诺**：`runcontroller` 可以依赖生命周期调用、墓碑语义与幂等语义；`connectorhub` 可以依赖出口策略的保存、按代落地与按代撤销。新增提供者实现同一接口，不修改调用方；提供者在闭合集合内新增一项已声明能力是兼容变更，扩展闭合取值、删除能力或改变能力语义需要同时修改调用方与 `suspendpolicy`。
- **顺序**：对同一运行实例的 `Suspend`、`Resume` 与 `Destroy` 不并发；调用方按效果记录的先后发出，提供者按到达顺序串行执行。不同运行实例的调用可以并发。
- **只读调用**：`ListSandboxes`、`GetSandbox`、`NodeMemory`、`ResizeStatus`、`EstimateResume` 与 `Capabilities` 不改变提供者状态，可在控制面事务之外调用；调用方把结果与 `run` 行核对，查询失败时不据以判定。
- **不持有控制面状态**：`sandboxprovider` 不读取 `runcontroller` 的表，不持有运行实例状态；提供者侧记录只服务于幂等、墓碑、预热池与观察。

### 与 suspendpolicy 的能力协商

- `runcontroller` 在挂起决策前读取 `Capabilities`，把能力集合作为上下文传给 `suspendpolicy.decide` 与 `review`；`suspendpolicy` 只返回能力集合允许的机制，没有可用机制时返回不挂起。
- `Freeze{signal}` 只允许短冻结，冻结时长上限与升级梯由 `suspendpolicy` 定义；`FREEZE_SHRINK` 只与 `Freeze{cgroup}` 和 `InPlaceResize` 组合；`Snapshot` 只在提供者声明该能力时进入候选；`Destroy` 只在提供者声明该能力时进入候选。
- `WarmPool` 与 `EgressInject` 不是挂起机制：`WarmPool` 影响销毁重建的恢复路径，`EgressInject` 影响出口路径；`WarmPool` 与 `EgressInject` 不进入 `decide` 的机制选择。
- 能力缺失时由 `suspendpolicy` 降级，`sandboxprovider` 不把能力缺口暴露为运行时错误：substrate 子模块没有冻结，回合中只有不挂起与快照；k8s 子模块没有快照，回合中冻结到上限后由 `suspendpolicy` 选择 `DRAIN`，回合边界仍可销毁重建。
- 调用方把能力集合之外的机制传给 `sandboxprovider` 时，提供者以参数非法拒绝；这属于调用方缺陷，不是提供者的降级路径。

### 与 connectorhub 的出口策略接口

- `PutEgressPolicy` 保存每运行实例的出口策略；`EgressPolicyDocument` 是提供者不解释的信封，`connectorhub` 的 `EgressPolicySink` 由一层适配把其 `EgressPolicy` 序列化后调用本接口，`sandboxprovider` 不导入 `connectorhub` 的类型。提供者只按 `run_id` 保存，并在预配与认领时按 `(run_id, generation)` 生成本代副本。推送的更新只同步到该运行实例尚未撤销的代副本。
- `RevokeEgressPolicy` 按 `(run_id, generation)` 撤销该代副本，撤销即墓碑；已撤销的代拒绝此后的落地与更新，重复撤销幂等。
- Substrate 路径把出口策略落地为 EgressPolicy，以 MITM 与 `replaceHeaders` 替换请求头凭据占位符，默认拒绝未列入的主机；actor 镜像信任出口网关的 CA，凭据来源为 credential provider。
- k8s 路径的占位符替换由 `connectorhub` 的平台出口代理完成，`sandboxprovider` 只落地平台出口代理配置与 NetworkPolicy，并按运行实例保存策略；平台出口代理对每个出口请求按附带的运行实例令牌经代查询接口校验代、终态与作废状态，不使用缓存，按代撤销为空操作。
- 出口策略的撤销由 `runcontroller` 在代递增、进入 `Degraded`、放弃、显式结束、本代沙箱作废与回合边界销毁的提供者确认的事务内写 `revoke_egress` 效果；提供者在效果执行时撤销，撤销生效前的残余窗口以效果记录的截止为上界。

## 依赖

依赖穷举如下，方向与[模块依赖图](../../architecture.md#模块依赖图)一致：本模块依赖基础层的 `workspace` 与 `agentprotocol`，以及各提供者层的控制器、代理与存储；`runcontroller` 与 `connectorhub` 依赖本模块；本模块不依赖 `runcontroller`、`suspendpolicy` 与 `connectorhub`。

- `workspace`：在预配与认领时绑定工作区，取得挂载路径、`RunUID`、`StorageMode`、`Durability` 与 `Placement`，把绑定交给沙箱；`lease_id` 取自 `ProvisionRequest.LeaseID`，由 `runcontroller` 建立租约后随请求传入，不来自绑定；工作区持久化按提供者选择，k8s 子模块以共享文件系统为默认、块存储为退化配置，substrate 子模块优先外部 CSI 卷、`durableDir` 为退化配置（[ADR-workspace-persistence-per-provider](../../../adr/adr-workspace-persistence-per-provider.md)）。本模块不持有租约，续约与接管由 `workspace` 与看守进程完成；`on_suspend` 语义下，销毁路径在同步挂起返回之后调用 `workspace.Confirm(Mode=suspend_upload)`，确认成功才视为工作区已持久化。
- `agentprotocol`：恢复触发使用 `/readyz` 与 `/wake` 入站端点；`/readyz` 是就绪探针，`/wake` 返回表示看守进程的出站流已重建。入站端点不承载事件内容，投递只在出站流上进行。
- k8s 提供者层：agent-sandbox 控制器管理 Sandbox、SandboxClaim 与 SandboxWarmPool；节点冻结代理是特权 DaemonSet，按节点定位 Pod 与容器 cgroup，在容器 cgroup 下维护子 cgroup `agent`，写 `agent/cgroup.freeze` 执行冻结与解冻（[REF-linux-cgroup-v2](../../../references.md)）；原地缩放依赖 Kubernetes In-Place Pod Resize（[REF-k8s-in-place-pod-resize](../../../references.md)），需要 `pods/resize` 子资源的 patch 权限；k8s 入站路由 wake-router 只服务 `/terminal` 与界面文件访问，事件驱动的恢复由控制面直调提供者。
- substrate 提供者层：Substrate 控制面执行 actor 的 Suspend 与 Resume；路由器以 `ate-target-actor` 头调用 `/wake`，恢复 actor 并停泊请求直到 `/readyz` 就绪；出口网关落地 EgressPolicy 并实现占位符替换；对象存储承载快照与 `durableDir` 数据。控制面调用路由器的身份是 SPIFFE 身份，且必须在路由器的入站白名单内（[REF-spiffe](../../../references.md)）。
- 部署前提：k8s 提供者要求节点为 cgroup v2、集群允许特权 DaemonSet、In-Place Pod Resize 可用、agent-sandbox 已安装、NetworkPolicy 由集群网络实施；substrate 提供者要求 k8s 具备 PodCertificateRequest 与 ClusterTrustBundle API、节点运行 gVisor、节点内核 6.7 及以上、PostgreSQL 与对象存储。
- 本模块与 `warden` 之间没有代码依赖；`/readyz` 与 `/wake` 是运行时 HTTP 入口，不是模块间的代码调用。

## UML 图

类图固定提供者接口与数据类型的边界，时序图固定预配与恢复的调用顺序，能力映射图固定能力集合到挂起机制与恢复路径的对应关系。图中不表达的约束由每张图后的正文给出。

### 提供者接口类图

```mermaid
classDiagram
    class Provider {
        <<interface>>
        +Capabilities() CapabilitySet
        +Provision(req) SandboxHandle
        +Claim(req) SandboxHandle
        +Freeze(req) FreezeReceipt
        +Shrink(req) error
        +Thaw(req) error
        +Snapshot(req) SnapshotReceipt
        +RevokeSnapshot(suspendID) error
        +RestoreSnapshot(req) SandboxHandle
        +Destroy(ref) DestroyReceipt
        +DestroySandbox(ref) error
        +ReleaseRun(runID) error
        +ListSandboxes(filter) SandboxInfoList
        +GetSandbox(ref) SandboxInfo
        +NodeMemory(sel) NodeMemoryList
        +ResizeStatus(ref) ResizeStatus
        +EstimateResume(mechanism) LatencyEstimate
        +PutEgressPolicy(policy) error
        +RevokeEgressPolicy(ref) error
    }
    class K8sProvider
    class SubstrateProvider
    class CapabilitySet {
        +bool FreezeCgroup
        +bool FreezeSignal
        +bool Snapshot
        +bool Destroy
        +bool InPlaceResize
        +bool WarmPool
        +bool EgressInject
    }
    class SandboxHandle {
        +RunID RunID
        +int Generation
        +string SandboxID
        +string Placement
        +string Durability
    }
    class FreezeRequest {
        +string FreezeID
        +string Variant
    }
    class ShrinkRequest {
        +string FreezeID
    }
    class SnapshotReceipt {
        +string SnapshotID
        +time SnapshotExpiresAt
    }
    class EgressPolicyDocument {
        +RunID RunID
        +json Policy
    }
    Provider <|.. K8sProvider
    Provider <|.. SubstrateProvider
    Provider --> CapabilitySet
    Provider --> SandboxHandle
    Provider --> FreezeRequest
    Provider --> SnapshotReceipt
    Provider --> EgressPolicyDocument
```

`Provider` 是控制面调用提供者的唯一入口，`K8sProvider` 与 `SubstrateProvider` 是同一接口的可互换实现；能力集合是数据而不是行为，调用方读取它后自行选择机制。`SandboxHandle` 是预配、认领与快照恢复的共同结果，携带代与放置位置；`FreezeRequest` 与 `SnapshotReceipt` 分别承载冻结标识与快照保留期限；`EgressPolicyDocument` 只保存 `connectorhub` 推送的内容，提供者不解释其语义。图中不表达墓碑、幂等记录与串行化，这些由核心数据结构与核心算法与流程定义。

### 预配与恢复时序图

```mermaid
sequenceDiagram
    autonumber
    participant C as connectorhub
    participant R as runcontroller
    participant P as sandboxprovider
    participant W as workspace
    participant L as 提供者层
    participant D as 看守进程

    C->>P: PutEgressPolicy(run_id, policy)
    R->>P: Provision(run_id, generation, attempt_id, token, lease_id)
    P->>W: 绑定工作区
    W-->>P: 挂载路径、RunUID、StorageMode、Durability、Placement
    P->>L: 创建沙箱或认领预热池
    L-->>P: sandbox_id, placement
    P->>P: 落地该代出口策略
    P-->>R: SandboxHandle
    D->>R: Hello(generation, sandbox_id)

    Note over R,P: 回合中冻结
    R->>P: Freeze(freeze_id, cgroup)
    P->>L: 节点冻结代理写 agent/cgroup.freeze
    P-->>R: FreezeReceipt
    opt 具备 InPlaceResize
        R->>P: Shrink(freeze_id)
        P-->>R: 缩小返回
    end
    R->>P: Thaw(freeze_id)
    P->>L: 解冻
    P-->>R: 解冻返回
    R->>D: Thaw(suspend_id)
    D-->>R: Thawed

    Note over R,P: 恢复按 resume_mode 分派
    alt snapshot_restore，substrate 提供者
        R->>P: RestoreSnapshot(run_id, generation, suspend_id, resume_id)
        P->>L: 经路由器调用 /wake
        L->>D: 恢复 actor 并停泊请求直到 /readyz
        D-->>L: /readyz 就绪
        L-->>P: /wake 返回
        P-->>R: SandboxHandle
        D->>R: Hello(generation, sandbox_id)
    else thaw，k8s 提供者
        R->>P: Thaw(freeze_id)
        P->>L: 节点冻结代理解冻
        P-->>R: 解冻返回
        D-->>R: Thawed
    else warm_claim 或 cold_start，k8s 提供者
        R->>P: Claim 或 Provision(run_id, generation, attempt_id, token, lease_id)
        P->>L: 认领预热池、把 Sandbox CR 改回 Running，或创建沙箱
        L-->>P: sandbox_id, placement
        P-->>R: SandboxHandle
        D->>R: Hello(generation, sandbox_id)
    end
```

时序图固定预配、冻结与恢复的消息顺序：预配先保存出口策略并绑定工作区，再创建或认领沙箱；冻结先由提供者执行 `Freeze{cgroup}`，解冻返回后由 `runcontroller` 经出站流发送协议消息 `Thaw{suspend_id}`；恢复按提供者分叉，substrate 子模块经路由器 `/wake` 恢复 actor 并等待 `/readyz`，k8s 子模块解冻、把 Sandbox CR 改回 `Running` 或认领预热池，由看守进程自行重连。`/wake` 返回即表示出站流已重建；k8s 路径的事件驱动恢复由控制面直调提供者，不经入站路由。每步之后的持久化结果见走查。

### 能力集合与机制映射图

```mermaid
flowchart LR
    subgraph caps["能力集合（闭合）"]
        FC["Freeze{cgroup}"]
        FS["Freeze{signal}"]
        SN["Snapshot"]
        DE["Destroy"]
        IR["InPlaceResize"]
        WP["WarmPool"]
        EI["EgressInject"]
    end
    subgraph mech["挂起机制（闭合）"]
        NONE["不挂起"]
        F["冻结"]
        FSR["冻结 + 原地缩小 CPU request"]
        S["快照"]
        DR["销毁重建"]
    end
    subgraph path["恢复与出口路径"]
        COLD["冷启动"]
        WARM["预热池认领"]
        EGR["出口策略按代落地"]
    end
    FC --> F
    FC --> FSR
    FS --> F
    SN --> S
    DE --> DR
    IR --> FSR
    WP --> WARM
    WARM -.->|池空| COLD
    EI --> EGR
    K8S["k8s 提供者"] -.-> FC
    K8S -.-> FS
    K8S -.-> DE
    K8S -.-> IR
    K8S -.-> WP
    SUB["Substrate 提供者"] -.-> SN
    SUB -.-> DE
    SUB -.-> WP
    SUB -.-> EI
```

能力集合与挂起机制都是闭合集合。`Freeze{cgroup}` 与 `Freeze{signal}` 都映射到冻结，`Freeze{cgroup}` 与 `InPlaceResize` 组合映射到冻结加原地缩小；`Freeze{signal}` 只允许短冻结，不能与原地缩小组合；`Snapshot` 映射到快照；`Destroy` 映射到销毁重建。`WarmPool` 不产生新的挂起机制，它把销毁重建的恢复路径分为预热池认领与冷启动；`EgressInject` 也不产生挂起机制，它只影响出口策略的落地方式。k8s 提供者声明冻结、销毁、原地缩小与预热池；Substrate 提供者声明快照、销毁、预热池与出口注入。缺失的能力由 `suspendpolicy` 降级，图中不画不可选的机制。

## 核心数据结构

提供者侧的数据结构不是控制面的表：运行实例状态仍以 PostgreSQL 中的 `run` 行为唯一事实源，提供者记录只服务于幂等、墓碑、预热池、出口策略副本与观察结果。下列记录在返回确认之前持久化，否则重驱动可能产生第二次副作用。

### 状态一览

下表描述提供者侧沙箱实例与运行实例级资源的状态，不描述运行实例状态机；运行实例状态机归 `runcontroller`。第三列的当前动作均由 `sandboxprovider` 执行。

| 状态 | 一句话含义 | 当前动作 | 直觉画面 |
| --- | --- | --- | --- |
| `creating` | 预配、认领或快照恢复已受理，沙箱尚未就绪 | 创建沙箱或认领预热池，绑定工作区，落地出口策略 | 房间正在搭建，门还没开 |
| `running` | 沙箱实例存在并可执行 | 等待冻结、快照、销毁或查询 | 房间在使用中 |
| `frozen` | Agent 子 cgroup 已冻结，内存保留 | 重复 `Freeze` 返回首次 `FreezeReceipt` 且不重复写 cgroup；解冻之后到达的冻结与缩小由冻结墓碑拒绝 | 房间灯关了，家具原位 |
| `snapshotted` | 内存已持久化，快照有保留期限 | 保存快照标识与到期时刻，等待恢复或销毁 | 房间拍照存档，房间清空 |
| `destroyed` | 沙箱实例已删除，运行实例级资源保留 | 保留创建墓碑，拒绝该代及更早代的创建 | 房间退租，仓库里还留着柜子 |
| `released` | 运行实例级资源已释放 | 保留释放墓碑，拒绝该运行实例的一切创建 | 仓库清空，钥匙收回 |

状态转移由提供者操作驱动：`creating` 成功后进入 `running`；`Freeze{cgroup}` 使 `running` 进入 `frozen`，`Thaw` 使其回到 `running`；`Snapshot` 使 `running` 进入 `snapshotted`；`frozen` 先经 `Thaw` 回到 `running` 才能快照；快照恢复经 `creating` 回到 `running`；整代销毁使当前状态进入 `destroyed`；`ReleaseRun` 使 `destroyed` 进入 `released`。`destroyed` 之后的创建只允许更大的代，同一代与更早代由创建墓碑拒绝；`released` 是运行实例级资源的终态。

### 能力集合与降级

| 能力 | 语义 | k8s 子模块 | substrate 子模块 | 缺失时的降级 |
| --- | --- | --- | --- | --- |
| `Freeze{cgroup}` | 由节点冻结代理对 Agent 子 cgroup 写 `cgroup.freeze`，看守进程不冻结 | 具备 | 不具备 | 回合中不冻结；有快照能力时用快照，否则不挂起；回合边界仍可销毁重建 |
| `Freeze{signal}` | 由看守进程对 Agent 进程树执行 SIGSTOP，只允许短冻结 | 具备 | 不具备 | 没有短冻结；回合中按其他能力处置 |
| `Snapshot` | 持久化沙箱内存并释放资源，恢复在同一代内进行 | 不具备 | 具备 | 回合中冻结到上限后由 `suspendpolicy` 选择 `DRAIN`；长等待释放内存只能走回合边界销毁重建 |
| `Destroy` | 按代销毁与按运行实例释放，携带墓碑 | 具备 | 具备 | 无法销毁重建；k8s 子模块与 substrate 子模块都具备该能力 |
| `InPlaceResize` | 冻结后缩小 CPU request，恢复时放大；不缩内存 request | 具备（启动自检通过时） | 不具备 | 冻结只释放 CPU 用量，不释放调度额度；恢复不涉及放大 |
| `WarmPool` | 预创建可认领的沙箱，缩短销毁重建的恢复时延 | 具备 | 具备 | 恢复退化为冷启动，受冷启动并发上限约束 |
| `EgressInject` | 提供者按运行实例落地允许主机与请求头凭据占位符替换 | 不具备 | 具备 | k8s 路径由 `connectorhub` 的平台出口代理做占位符替换与逐请求代校验，提供者只落地代理配置与 NetworkPolicy |

能力集合是闭合集合，`suspendpolicy` 不得选择提供者未声明支持的机制。`Freeze{signal}` 只允许短冻结；`FREEZE_SHRINK` 只与 `Freeze{cgroup}` 和 `InPlaceResize` 组合。`WarmPool` 与 `EgressInject` 不是挂起机制，不出现在挂起机制闭集中。

### 沙箱句柄与操作记录

| 记录 | 键 | 字段 | 用途 |
| --- | --- | --- | --- |
| `SandboxHandle` | `(run_id, generation, sandbox_id)` | `placement`、`durability`、`state` | 预配、认领与快照恢复的结果 |
| `FreezeRecord` | `freeze_id` | `variant`、`frozen_at`、`thawed_at`、`shrink_applied` | 冻结与解冻的幂等和冻结墓碑 |
| `SnapshotRecord` | `suspend_id` | `snapshot_id`、`expires_at`、`cancelled`、`restored` | 快照保留期限、撤销与恢复 |
| `OperationRecord` | `(run_id, generation, attempt_id)` 或调用自身的幂等键 | `result`、`completed_at` | 重复调用返回首次结果 |
| `EgressCopy` | `(run_id, generation)` | `policy`、`landed_at`、`revoked` | 出口策略的按代落地与撤销 |
| `WarmPoolShard` | `(image, spec, node_pool 或可用区)` | `capacity`、`available` | 预热池容量与分片 |
| `NodeMemory` | `node` | `frozen_mem`、`allocatable_mem`、`node_mem_cpu` | 节点内存压力查询；`node_mem_cpu` 供 `suspendpolicy` 计算 ρ |
| `LatencyEstimate` | `mechanism` | `p95`、`sampled_at` | 恢复准入的时延估计 |

`SandboxHandle` 不携带运行实例状态；`FreezeRecord` 与 `SnapshotRecord` 只记录提供者侧的执行结果；`OperationRecord` 是幂等依据，必须在返回确认之前写入；`EgressCopy` 以 `revoked` 标记撤销墓碑；`NodeMemory` 与 `LatencyEstimate` 是观察结果，不是权威状态。

### 墓碑与保留

| 墓碑 | 写入时机 | 拒绝的对象 | 保留期 |
| --- | --- | --- | --- |
| 创建墓碑 | `Destroy(run_id, generation)` 返回确认之前 | 该代及更早代的 `Provision`、`Claim` 与 `RestoreSnapshot` | 不短于控制面效果记录的重驱动窗口 |
| 释放墓碑 | `ReleaseRun(run_id)` 返回成功之前 | 该运行实例的一切创建，含沙箱、运行实例级资源与出口策略 | 随提供者的运行实例级记录保留 |
| 撤销墓碑 | `RevokeEgressPolicy(run_id, generation)` 返回成功之前 | 该代的出口策略落地与更新 | 直到该代沙箱与副本不再存在 |
| 快照撤销墓碑 | 撤销未确认的快照尝试返回之前 | 同一 `suspend_id` 的迟到快照请求 | 不短于快照保留期 |
| 冻结墓碑 | `Thaw(freeze_id)` 返回成功之前 | 同一 `freeze_id` 的迟到冻结与缩小请求 | 不短于挂起尝试的记录窗口 |

## 核心算法与流程

### 走查

本条走查把按代销毁、创建墓碑、释放墓碑与幂等落到具体值上。初始状态：`run_id=r-1`、`generation=5`、`sandbox_id=sb-5`，k8s 子模块，`EgressConfig(r-1)` 已保存（k8s 路径按运行实例保存出口策略，不维护按代副本），创建墓碑与释放墓碑为空，`freeze_id=f-5` 尚未使用。下表只列提供者侧状态，`run` 行由 `runcontroller` 维护。

| 步骤 | 调用或事件 | 提供者侧状态 |
| --- | --- | --- |
| 1 | `Freeze(f-5, cgroup)`，随后 `Shrink(f-5)` | `FreezeRecord(f-5)` 写入，`frozen_at=t1`；节点冻结代理写 `agent/cgroup.freeze=1`；`Shrink` 成功后 `shrink_applied=true`；沙箱状态 `frozen` |
| 2 | `Thaw(f-5)` | `FreezeRecord(f-5).thawed_at=t2`；节点冻结代理写 `agent/cgroup.freeze=0`；沙箱状态 `running`；同一 `freeze_id` 的迟到冻结与缩小被冻结墓碑拒绝 |
| 3 | `Destroy(r-1, 5)` | 删除 Pod，保留 PVC 与 Service；写入创建墓碑 `(r-1, ≤5)`；沙箱状态 `destroyed`；返回 `DestroyReceipt{generation=5}` |
| 4 | 迟到的 `Provision(r-1, 5, a-9)` | 被创建墓碑拒绝；不创建沙箱，沙箱状态仍为 `destroyed` |
| 5 | `Provision(r-1, 6, a-10)` | 创建 `sb-6`，沿用 `EgressConfig(r-1)`（k8s 路径不生成按代副本）；沙箱状态 `running`，`placement` 写入 |
| 6 | 迟到的 `DestroySandbox(5, sb-5)` | 幂等返回；不删除 `sb-6`，`sb-6` 状态仍为 `running` |
| 7 | `RevokeEgressPolicy(r-1, 5)` | k8s 路径为空操作；平台出口代理按运行实例令牌逐请求校验代、终态与作废状态；`EgressConfig(r-1)` 不受影响 |
| 8 | `ReleaseRun(r-1)` | 删除 Sandbox CR、Service 与 PVC；写入释放墓碑 `(r-1)`；沙箱状态 `released`；此后对该 `run_id` 的一切创建被拒绝 |

走查结束时 `generation` 由 `runcontroller` 决定，提供者不递增代；创建墓碑证明第 5 代此后不再出现沙箱实例，释放墓碑使该运行实例不再接受创建。第 6 步说明销毁的幂等依据是调用携带的 `(generation, sandbox_id)`，不是到达顺序；第 7 步说明 k8s 路径的出口撤销为空操作，放行由平台出口代理的逐请求代校验决定。

### 能力协商与机制选择

1. `runcontroller` 读取 `Capabilities`，把能力集合作为上下文传给 `suspendpolicy.decide` 与 `review`。
2. `suspendpolicy` 只在能力集合允许的机制中返回一个机制；没有可用机制时返回不挂起。机制闭集为不挂起、冻结、冻结加原地缩小、快照、销毁重建。
3. `runcontroller` 把机制映射为提供者调用：不挂起不调用；冻结调用 `Freeze{cgroup}`，或在只有 `Freeze{signal}` 时由看守进程执行短冻结；冻结加原地缩小先调用 `Freeze{cgroup}`，再以同一 `freeze_id` 调用 `Shrink`；快照调用 `Snapshot`；销毁重建先经看守进程的安全点检查，再调用 `Destroy`。
4. 能力缺失时 `suspendpolicy` 选择降级机制：substrate 子模块在回合中只有不挂起与快照，回合边界可销毁重建；k8s 子模块在回合中冻结到上限后选择 `DRAIN`，回合边界可销毁重建；没有 `InPlaceResize` 时冻结只释放 CPU 用量；没有 `WarmPool` 时恢复退化为冷启动；没有 `EgressInject` 时出口路径由平台出口代理与 NetworkPolicy 承担。
5. `WarmPool` 与 `EgressInject` 不进入 `decide` 的机制选择：`WarmPool` 影响恢复路径，`EgressInject` 影响出口路径。把能力集合之外的机制传给提供者属于调用方缺陷，提供者以参数非法拒绝。

### 子模块共性与差异

| 维度 | k8s 子模块 | substrate 子模块 |
| --- | --- | --- |
| 底座 | agent-sandbox 的 Sandbox、SandboxClaim 与 SandboxWarmPool | Substrate actor 模板与预热 worker |
| 能力集合 | `Freeze{cgroup}`、`Freeze{signal}`、`Destroy`、`WarmPool`、`InPlaceResize` | `Snapshot`、`Destroy`、`WarmPool`、`EgressInject` |
| 冻结执行 | 节点冻结代理对 Agent 子 cgroup 执行，看守进程留在父 cgroup；`Freeze{signal}` 由看守进程执行 | 不具备冻结能力 |
| 恢复触发 | 解冻、Sandbox CR 改回 `Running` 或认领预热池；看守进程自行重连 | 经路由器 `/wake` 恢复 actor，请求停泊到 `/readyz` 就绪 |
| 工作区 | 共享文件系统为默认，块存储为退化配置；预热 Pod 预挂项目根，认领时切换到运行实例子目录 | 外部 CSI 卷优先，`durableDir` 为退化配置；退化配置下回合边界销毁使用同步挂起 |
| 出口 | 平台出口代理配置加 NetworkPolicy；平台出口代理逐请求校验代，按代撤销为空操作 | EgressPolicy 以 MITM 与 `replaceHeaders` 落地；按代撤销即墓碑 |
| 快照 | 不具备 | 内存快照，恢复在同一代内进行；恢复后校时并清理旧连接 |
| 预热 | SandboxWarmPool 与 SandboxClaim；认领过的 Pod 不回收复用 | 预热 worker 多路复用；不存在预挂项目根的问题 |

共性：k8s 子模块与 substrate 子模块实现同一 `Provider` 接口，共用看守进程、协议与控制面，遵循同一套墓碑、幂等与按代销毁语义，接受同一出口策略推送与工作区绑定抽象。差异留在各自子模块：k8s 的 Sandbox CR、节点冻结代理、原地缩放、NetworkPolicy 与 wake-router 见 [k8s 子模块](design-sandboxprovider-k8s.md)；substrate 的 actor 模板、Suspend 与 Resume、路由器唤醒、EgressPolicy 与 credential provider 见 [substrate 子模块](design-sandboxprovider-substrate.md)。

### 预配与认领

1. `runcontroller` 在效果记录提交后调用 `Provision` 或 `Claim`，请求携带 `run_id`、`generation`、`attempt_id`、运行实例令牌、`lease_id` 与工作区绑定；`attempt_id` 取 `resume_id`。提供者在创建沙箱时把令牌经沙箱创建的注入参数交给看守进程，`lease_id` 写入看守进程的启动配置，二者都不进入提供者的持久记录。
2. 提供者先检查创建墓碑：代不大于该运行实例最近整代销毁的代，或释放墓碑已存在时，返回确定性拒绝，不创建沙箱。
3. 提供者调用 `workspace` 绑定工作区，取得挂载路径、`RunUID`、`StorageMode`、`Durability` 与 `Placement`；`lease_id` 取自请求，工作区租约仍归 `workspace`，提供者把绑定与租约标识交给沙箱。
4. 提供者创建沙箱或认领预热池：k8s 子模块创建 Sandbox CR 或发起 SandboxClaim；substrate 子模块从 actor 模板创建 actor 或从预热 worker 认领。
5. 提供者在 `(run_id, generation)` 上落地出口策略副本；该代已被撤销时跳过落地，已撤销的代拒绝此后的落地与更新。
6. 提供者在创建或认领之前持久化以 `(run_id, generation, attempt_id)` 为键的占有记录，或把该键标注到预热沙箱上使认领可按同一键重放；创建与认领按该键执行，重驱动识别已认领的沙箱而不重复认领。完成后补全 `OperationRecord` 并返回 `SandboxHandle`；看守进程随后连接并发送 `Hello`，`runcontroller` 在 `Hello` 通过校验后完成预配。提供者不等待 `Hello`，也不把 `Hello` 作为预配成功的前提。
7. 同一 `(run_id, generation, attempt_id)` 的重复调用返回首次结果；响应丢失后的重驱动不会创建重复沙箱。认领的预热 Pod 在认领之后不回收复用；k8s 预热 Pod 预挂项目根，看守进程在认领时切换到运行实例子目录。

### 冻结与解冻

1. `runcontroller` 选定冻结后调用 `Freeze`，请求携带 `freeze_id`、`generation` 与 `sandbox_id`；`freeze_id` 取冻结尝试的 `suspend_id`。
2. `Freeze{cgroup}` 由节点冻结代理执行：代理定位 Pod 与容器的 cgroup 路径，在容器 cgroup 下维护子 cgroup `agent`，写 `agent/cgroup.freeze=1`；看守进程留在父 cgroup，继续应答探针、续约工作区租约、维持出站流并持有本地代理的上游连接。`Freeze{signal}` 由看守进程对 Agent 进程树执行 SIGSTOP，只允许短冻结，提供者不执行该变体。
3. `Freeze` 返回之后，`runcontroller` 以同一 `freeze_id` 调用 `Shrink`；提供者具备 `InPlaceResize` 时只缩小 CPU request，不缩内存 request；沙箱 Pod 从创建起即 Burstable，原始 request 存注解，容器 `resizePolicy` 为 `NotRequired`；缩小立即生效，恢复时的放大可能进入 `Deferred`。
4. 提供者持久化 `FreezeRecord` 并返回 `FreezeReceipt`；`frozen_at` 取节点冻结代理的确认时刻。`Freeze{signal}` 的冻结确认由看守进程的 `ReadyToSuspend` 承担。
5. `Thaw(freeze_id)` 由提供者执行 `Freeze{cgroup}` 的解冻：节点冻结代理写 `agent/cgroup.freeze=0`；返回后 `runcontroller` 经出站流发送协议消息 `Thaw{suspend_id}`，看守进程观察到 Agent 子 cgroup 未冻结后上报 `Thawed`。`Freeze{signal}` 的解冻由看守进程自行执行，协议消息仍由 `runcontroller` 发送。
6. 已解冻的 `freeze_id` 不再接受冻结与缩小；迟到的冻结与缩小请求不生效，CPU request 不会停在缩小值。冻结中升级先解冻；解冻与恢复时的原地放大并行，不等待放大完成。
7. 冻结态 Pod 标注 `cluster-autoscaler.kubernetes.io/safe-to-evict=false`（[REF-cluster-autoscaler-faq](../../../references.md)）；`preStop` 先解冻再终止，否则 SIGTERM 会送给冻结进程。节点内存压力下 Pod 仍可能被驱逐，`runcontroller` 把「冻结态且沙箱已死」转入重建路径。
8. 原地放大进入 `Deferred` 时，解冻与放大并行；`Deferred` 超过阈值且处于回合边界时销毁重建以迁移，回合中则以低权重继续运行。提供者经 `ResizeStatus` 暴露 `Deferred` 状态。

### 快照与快照恢复

1. `runcontroller` 选定快照后调用 `Snapshot`，请求携带 `run_id`、`generation`、`sandbox_id` 与 `suspend_id`；只有具备 `Snapshot` 能力的提供者会收到该调用。
2. 提供者持久化快照并返回 `SnapshotReceipt`，其中 `SnapshotExpiresAt` 是快照保留期限；`runcontroller` 在保留期临近前作废该代，避免恢复落在已过期快照上。
3. 撤销尚未确认的快照尝试按 `suspend_id` 执行，提供者写入快照撤销墓碑；撤销确认之后到达的同一 `suspend_id` 的快照请求不生效。
4. `RestoreSnapshot` 在同一代内恢复，不递增代；请求携带 `run_id`、`generation`、`suspend_id` 与 `attempt_id`，返回 `SandboxHandle`。快照不存在或已过期时返回确定性错误，`runcontroller` 转重建意图，不把该错误计入恢复失败次数。
5. 恢复完成后，看守进程校时并主动断开 Agent 进程的旧 TCP 连接；在途 LLM 请求由本地代理按 `request_id` 向 `llmgateway` 取回已完成响应。工作区按提供者持久化，快照不替代工作区持久化；同一代的出口策略副本沿用，不重新生成。
6. substrate 子模块的快照恢复会重置 guest 时钟并切断旧连接；k8s 子模块不具备快照能力，`suspendpolicy` 不选择快照。

### 恢复触发

恢复按提供者分述如下；`runcontroller` 按 `resume_mode` 选择调用，`sandboxprovider` 不决定恢复方式。

- **substrate 子模块**：`runcontroller` 的 `resume` 效果按 `resume_mode` 调用 `RestoreSnapshot`、`Claim` 或 `Provision`。快照恢复经 Substrate 路由器以 `ate-target-actor` 头调用 `/wake`，路由器恢复 actor 并停泊请求直到 `/readyz` 就绪；`/wake` 返回即表示看守进程的出站流已重建。预热池认领与冷启动由提供者取得或创建 actor，看守进程随后连接并发送 `Hello`。调用方必须持有 SPIFFE 身份且在路由器的入站白名单内；`parked-request-budget` 按冷恢复时延调大。
- **k8s 子模块，冻结态**：`runcontroller` 调用 `Thaw`，节点冻结代理清除 `agent/cgroup.freeze`；看守进程不重新创建，出站流保持或重连。
- **k8s 子模块，销毁态**：`runcontroller` 调用 `Provision` 新建沙箱、把 Sandbox CR 的 `operatingMode` 改回 `Running`，或调用 `Claim` 从预热池认领；看守进程自行重连并发送新代 `Hello`。事件驱动的恢复由控制面直调提供者，k8s 入站路由只服务 `/terminal` 与界面文件访问。
- 解冻以 `Thawed` 为完成观察；快照恢复与销毁态重建以同代或新代 `Hello` 为完成观察；恢复准入的放行由 `runcontroller` 与 `suspendpolicy` 完成，提供者只执行调用并返回确认或确定性错误。`EstimateResume` 返回提供者侧的恢复时延估计，`runcontroller` 把它与排队位置一起用于 `AdmissionState.EstimatedResume`。

### 按代销毁、墓碑与幂等

1. **整代销毁**：`Destroy(run_id, generation)` 删除该代的沙箱实例。k8s 子模块把 Sandbox CR 的 `operatingMode` 置为 `Suspended`，删除 Pod、保留 PVC 与 Service；substrate 子模块挂起 actor。提供者在返回删除确认之前写入创建墓碑，拒绝该代及更早代的 `Provision`、`Claim` 与 `RestoreSnapshot`；删除确认携带被删除的代，证明该代此后不再出现沙箱实例。
2. **单个销毁**：`DestroySandbox(generation, sandbox_id)` 只删除该代的指定沙箱实例，携带代，不删除更新的代；它用于完成事务校验失败后的清理、对账器按列举结果回收，以及 `sandbox_id` 核对不符的单个销毁。单个销毁不写入该代的创建墓碑，因为它不证明该代不再出现沙箱实例。
3. **按运行实例释放**：`ReleaseRun(run_id)` 删除运行实例级资源，k8s 子模块删除 Sandbox CR、Service 与 PVC，substrate 子模块释放 actor 相关资源；释放幂等，释放之前写入释放墓碑，释放之后拒绝该运行实例的一切创建，含沙箱、运行实例级资源与出口策略。
4. **正确性不依赖到达顺序**：整代销毁与单个销毁都携带代，只作用于该代的沙箱实例，不作用于更新的代；新代沙箱不会被迟到执行的旧代销毁删除。整代销毁之后的迟到预配被创建墓碑拒绝；释放之后的迟到预配被释放墓碑拒绝；出口策略撤销之后的迟到落地与更新被撤销墓碑拒绝。
5. **幂等**：同一 `(run_id, generation)` 的整代销毁返回首次确认；同一 `(generation, sandbox_id)` 的单个销毁重复执行返回成功；同一 `run_id` 的释放重复执行返回成功；同一 `freeze_id` 的解冻、同一 `suspend_id` 的快照撤销重复执行不产生第二次副作用。提供者按到达顺序串行执行同一运行实例的调用，重复调用返回首次结果。
6. **创建墓碑与工作区租约**：删除确认携带的代等于工作区租约所属的代时，可作为 `workspace` 接管租约的 `proof`；创建墓碑保证删除确认之后该代不再出现沙箱实例，因而该证明成立。

### 预热池容量与分片

预热池容量按 `N = λL + z_{1−ε}·√(λL) + B` 计算。λ 为峰值认领率，L 为补充前置时间，ε 为允许的缺池概率，`z_{1−ε}` 为标准正态分布的 `1−ε` 分位数，B 为需吸收的最大相关唤醒突发。补充前置时间包括镜像预拉与节点扩容：镜像预拉通常为 5–30 s，需要扩节点时达到分钟级。

池按 `(image, spec, node_pool 或可用区)` 分片，每个分片的容量各自计算，总容量为各分片容量之和。分片使镜像、规格与放置约束不同的沙箱不互相挤占；k8s 子模块使用 SandboxWarmPool 与 SandboxClaim，substrate 子模块使用预热 worker 多路复用。认领失败即池空，恢复退化为冷启动，受冷启动并发上限约束；`pool_miss_rate` 上升时按公式重算池容量。

### 恢复时延分解

下表是端到端目标，覆盖 `runcontroller`、`sandboxprovider` 与看守进程；`EstimateResume` 只返回提供者侧的部分，`runcontroller` 另加排队时间。数值取自性能与容量目标。

| 机制 | 组成步骤 | p95 目标 | 前提 |
| --- | --- | --- | --- |
| 解冻 | 准入放行，提供者解冻返回，看守进程观察到 Agent 子 cgroup 未冻结，Agent 可接收 | 决策完成到 Agent 可接收小于 100 ms；事件到达至 Agent 可接收小于 200 ms | 冻结机制；看守进程未被冻结 |
| 快照恢复 | 准入放行，快照读取，actor 恢复，校时与旧连接清理，看守进程重连或同代 `Hello`，在途响应取回 | 同节点本地快照且无在途请求小于 2 s；通用小于 5 s | 快照机制；快照未过期 |
| 预热池认领 | 准入放行，认领，工作区子目录切换，看守进程启动与 `Hello`，重投 | 小于 10 s | 共享文件系统；Agent 二进制与依赖缓存在镜像内；启动期外联快速失败；池非空 |
| 冷启动 | 准入放行，放置与资源挂接，沙箱启动，工作区挂载，看守进程启动与 `Hello`，重投 | 共享文件系统小于 20 s；块存储小于 60 s | 共享文件系统；块存储退化配置有节点或可用区亲和 |

### 出口策略落地与按代撤销

1. `connectorhub` 经 `PutEgressPolicy` 推送每运行实例出口策略；`sandboxprovider` 按 `run_id` 保存，策略内容不解释。
2. 预配与认领时，提供者按 `(run_id, generation)` 生成本代副本并落地；快照恢复沿用同代副本；推送的更新只同步到尚未撤销的代。
3. substrate 子模块把副本落地为 EgressPolicy，以 MITM 与 `replaceHeaders` 替换请求头凭据占位符，默认拒绝未列入的主机；actor 镜像信任出口网关的 CA，凭据来源为 credential provider。
4. k8s 子模块落地平台出口代理配置与 NetworkPolicy；NetworkPolicy 默认拒绝出口，只放行控制面地址、LLM 网关与平台出口代理；平台出口代理对每个出口请求按附带的运行实例令牌经代查询接口校验代、终态与作废状态，不使用缓存；按代撤销为空操作。
5. `RevokeEgressPolicy(run_id, generation)` 写入撤销墓碑并返回；已撤销的代拒绝此后的落地与更新。撤销由 `runcontroller` 在代递增、进入 `Degraded`、放弃、显式结束、本代沙箱作废与回合边界销毁的提供者确认的事务内写入 `revoke_egress` 效果，提供者在效果执行时撤销。
6. 撤销在效果记录的截止之前生效时，残余窗口不超过该截止；截止前未能生效时，例如 Substrate 控制面不可达，窗口延续到撤销生效或该代沙箱删除完成。k8s 路径的逐请求代校验不依赖撤销，按代撤销为空操作。

### 串行化与幂等

- 同一运行实例的 `Suspend`、`Resume` 与 `Destroy` 不并发；提供者按到达顺序串行执行，调用方不依赖到达顺序保证正确性，正确性由代、墓碑与幂等键给出。
- 提供者在返回确认之前持久化 `OperationRecord` 与相应墓碑；响应丢失后的重驱动返回首次结果，不产生第二次副作用。侧效果已完成但记录未持久化时，提供者不得返回成功。
- 同一 `freeze_id` 的冻结、解冻与缩小幂等；同一 `suspend_id` 的快照与撤销幂等；同一 `(run_id, generation, attempt_id)` 的预配、认领与快照恢复幂等；同一 `(run_id, generation)` 的整代销毁与出口策略撤销幂等；同一 `(generation, sandbox_id)` 的单个销毁幂等；同一 `run_id` 的释放幂等。
- 预热池认领是原子操作；同一预热沙箱不会被多个运行实例认领。出口策略的落地与撤销按 `(run_id, generation)` 串行；策略更新只作用于尚未撤销的代。
- 提供者可以并发处理不同运行实例的调用；同一运行实例的并发重复调用由串行化与幂等键吸收，调用方不需要额外的全局锁。

## 错误与失败语义

错误按是否可重试分为确定性与暂时性：确定性错误不重试，调用方丢弃并计数或转重建；暂时性错误按同一幂等键退避重试，提供者返回首次结果。下表是已明确处置路径的失败，各行是举例。

| 失败 | 处置 |
| --- | --- |
| 提供者不可达或超时 | 暂时性；`runcontroller` 以同一幂等键重驱动，提供者若已完成则返回首次结果 |
| 沙箱不存在 | 确定性；解冻与快照恢复转重建意图，销毁返回幂等成功，查询返回空结果 |
| 快照不存在或已过期 | 确定性；转重建意图，不计入恢复失败次数 |
| 创建墓碑或释放墓碑拒绝 | 确定性；调用方丢弃并计数，不创建沙箱，不产生第二次副作用 |
| 撤销墓碑拒绝出口策略落地或更新 | 确定性；调用方丢弃并计数，不恢复该代的放行 |
| 快照撤销之后的迟到快照 | 确定性；快照撤销墓碑使其不生效 |
| 已解冻 `freeze_id` 的迟到冻结或缩小 | 确定性；冻结墓碑使其不生效，CPU request 不停在缩小值 |
| 预热池为空 | 认领返回池空；恢复退化为冷启动，受冷启动并发上限约束，`pool_miss_rate` 上升 |
| 节点内存压力或 Pod 被驱逐 | 提供者报告沙箱丢失；`runcontroller` 把冻结态或运行中的丢失转入重建路径；`preStop` 先解冻再终止 |
| 原地放大进入 `Deferred` | 解冻与放大并行；`Deferred` 超过阈值且处于回合边界时销毁重建迁移，回合中以低权重继续运行 |
| `ReleaseRun` 之后的迟到预配 | 释放墓碑拒绝；不创建沙箱或运行实例级资源 |
| 预配已创建沙箱但响应丢失 | 重驱动返回首次 `SandboxHandle`；完成事务校验失败时 `runcontroller` 写单个销毁，遗留沙箱由列举结果回收 |
| 出口策略撤销在截止前未生效 | 残余窗口延续到撤销生效或该代沙箱删除完成；调用方按效果截止告警，不用缓存的代放行 |
| 提供者能力缺失 | `suspendpolicy` 不选择该机制；调用方仍传入能力之外的机制时，提供者以参数非法拒绝 |

提供者不把能力缺失暴露为运行时错误；能力缺失是设计输入，由 `suspendpolicy` 选择降级机制。删除确认、快照撤销与释放的重复调用都返回首次结果，调用方据此区分「已完成」与「未执行」。

## 并发与事务边界

- **不参与控制面事务**：生命周期调用经 `runcontroller` 的效果记录在数据库事务提交后发出；出口策略推送由 `connectorhub` 直接调用。提供者不读写控制面的表，也不持有 `run` 行锁。提供者侧的 `OperationRecord`、墓碑与出口策略副本在返回确认之前持久化。
- **按运行实例串行**：同一运行实例的 `Suspend`、`Resume` 与 `Destroy` 不并发；提供者按到达顺序串行执行，不同运行实例的调用可以并发。并发重复调用由幂等键与串行化吸收。
- **顺序与墓碑**：`runcontroller` 保证新代预配排在旧代销毁确认或旧租约到期之后；提供者不依赖这一顺序保证正确性，整代销毁与单个销毁携带的代、创建墓碑、释放墓碑与撤销墓碑使迟到的旧代调用不删除新代沙箱，也不在释放之后创建资源。
- **完成事务校验**：`runcontroller` 在调用提供者之前于 `run` 行锁内复核运行实例未终态、代与尝试标识未变；调用期间显式结束、降级或代递增已提交时，完成事务校验失败，提供者结果含 `sandbox_id` 时同一事务写单个销毁的效果记录。执行者在提供者返回之后、完成事务之前崩溃时，遗留沙箱由对账器按 `ListSandboxes` 的反向求差回收。
- **只读调用**：`ListSandboxes`、`GetSandbox`、`NodeMemory`、`ResizeStatus` 与 `EstimateResume` 在事务外调用；调用方把结果与 `run` 行核对，查询失败时不据以判定，节点内存压力查询失败不触发升级。
- **出口策略的并发**：`PutEgressPolicy` 与 `RevokeEgressPolicy` 按 `run_id` 与 `(run_id, generation)` 串行；策略更新只同步到尚未撤销的代，撤销之后的落地与更新被墓碑拒绝。k8s 路径的逐请求代校验由平台出口代理完成，不依赖提供者的撤销顺序。
- **预热池认领**：认领是原子操作，同一预热沙箱不会被多个运行实例认领；认领过的 Pod 不回收复用，池容量按分片独立计算。

## 扩展点

### 新增提供者

新增提供者实现 `Provider` 接口并声明 `CapabilitySet`，满足墓碑、幂等、按代销毁与恢复触发语义，提供 `ListSandboxes`、`GetSandbox`、`NodeMemory`、`ResizeStatus` 与 `EstimateResume`，并接入 `workspace` 的绑定接口与 `agentprotocol` 的 `/readyz`、`/wake` 端点。满足这些条件时不修改 `runcontroller`、`suspendpolicy` 与 `connectorhub`；提供者特有行为留在其子模块，子模块文档与集成测试随提供者新增。能力集合必须是闭合集合的子集；需要新能力时按「新增能力或机制」处理。

### 新增能力或机制

新增能力须同时扩展 `CapabilitySet`、挂起机制闭集、`suspendpolicy` 的决策函数、恢复准入的机制限速与测试矩阵；只增加能力而不增加机制没有意义。新增恢复触发方式须扩展 `runcontroller` 的 `resume_mode` 闭集、`AdmissionState` 的机制字段与 `EstimateResume` 的机制取值，并提供对应的提供者调用与完成观察。删除能力或改变已有能力的语义是破坏性变更，须同时修改调用方与 `suspendpolicy`。出口策略内容对提供者不透明，新增策略字段不改变提供者接口。

## 实现状态

| 能力 | 状态 | 代码 |
| --- | --- | --- |
| 能力自描述 | 未实现 | — |
| 预配与认领 | 未实现 | — |
| `Freeze{cgroup}` | 未实现 | — |
| `Freeze{signal}` | 未实现 | — |
| 快照与快照恢复 | 未实现 | — |
| 按代销毁与单个销毁 | 未实现 | — |
| 按运行实例释放资源 | 未实现 | — |
| 沙箱列举与查询 | 未实现 | — |
| 节点冻结态内存查询 | 未实现 | — |
| 原地缩放与放大状态查询 | 未实现 | — |
| 按机制恢复时延估计 | 未实现 | — |
| 出口策略落地与按代撤销 | 未实现 | — |
| 预热池容量与分片 | 未实现 | — |
| 串行化与幂等 | 未实现 | — |
