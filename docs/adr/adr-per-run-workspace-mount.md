# 沙箱容器内只挂载本运行实例的工作区子目录

## 背景

[ADR-workspace-persistence-per-provider](adr-workspace-persistence-per-provider.md) 决定 k8s 提供者以 RWX 共享文件系统承载[工作区](../glossary.md)：共享文件系统没有卷挂接步骤，预热 Pod 在创建时即可获得工作区挂载点，认领不需要挂接卷。k8s [沙箱](../glossary.md) Pod 由 agent-sandbox 的 Sandbox CR 管理（[REF-agent-sandbox](../references.md)），预热 Pod 来自 SandboxWarmPool，预热池的分片键是镜像、规格与节点池或可用区，不含项目维度；预热 Pod 创建时尚不知道将服务哪个[运行实例](../glossary.md)。共享文件系统的挂载根在部署内为单组织共享（[ADR-tenant-dimension-without-enforcement](adr-tenant-dimension-without-enforcement.md)），覆盖全组织所有运行实例的目录。

运行实例子目录的属主是该运行实例的 uid、权限为 0700，创建这样的子目录需要以 root 写共享文件系统，共享文件系统因此要求 `no_root_squash`。挂载根进入 Pod 时，跨运行实例的隔离只依赖运行实例 uid 与目录权限。容器内一旦取得 root（内核提权，或攻破以 root 运行并解析 Agent 可控输入的[看守进程](../glossary.md)），即可 `seteuid` 到任意运行实例 uid，读写全组织任一运行实例的工作区，包括会话记录、[投递意图日志](../glossary.md)与 Agent 配置；`root_squash` 只映射 uid 0，客户端上报的其他 uid 照样被信任，挡不住这一路径。

Substrate 提供者的工作区卷按 actor 划分：外部 CSI 卷每 actor 一卷，`durableDir` 随 actor 保存；actor 内看不到其他运行实例的工作区。

## 考虑过的选项

### 维持挂载根加 uid 与目录权限隔离

采用它意味着沙箱 Pod 与预热 Pod 挂载整个挂载根，认领后看守进程切换到运行实例子目录，隔离只依赖 uid 与目录权限。**被拒**：容器内取得 root 之后没有第二道防线，越权范围覆盖全组织的工作区。

### 只对冷启动 Pod 使用 subPath

采用它意味着冷启动 Pod 以 subPath 只挂载运行实例子目录，预热 Pod 仍挂载整个挂载根。**被拒**：预热 Pod 创建时不知道运行实例，仍须挂载整个挂载根；预热池是 k8s 路径的主要预配方式，越权面没有收窄。

### hostUsers: false 加看守进程在 mount namespace 中只绑定子目录

采用它意味着 Pod 以独立的用户命名空间运行，看守进程在自己的 mount namespace 中把运行实例子目录绑定到工作区挂载路径并卸载挂载根。**不作为基线**：要求共享文件系统支持 idmapped mount；看守进程需要 `CAP_SYS_ADMIN`，扩大其权限；挂载根在绑定之前仍进入 Pod，攻破看守进程即可访问挂载根。保留为可选加固。

### 每个运行实例独立卷，认领时挂接

采用它意味着每个运行实例有独立的卷，预热 Pod 在认领时挂接该卷。**被拒**：认领路径须等待卷挂接，失去共享文件系统预挂载的时延优势，与 ADR-workspace-persistence-per-provider 的选择冲突。

### 挂载根只由节点侧组件挂载，Pod 只带按 Pod 的挂载点，运行实例子目录由节点侧组件绑定进 Pod

采用它意味着挂载根只在节点上挂载；Pod 以 CSI 内联临时卷获得按 Pod 的挂载点；冷启动在卷发布时直接发布运行实例子目录（效果与 subPath 相同），预热 Pod 在认领时由节点侧组件把运行实例子目录绑定挂载进去。**采用**——见决策。

## 决策

采用沙箱容器内只挂载本运行实例的工作区子目录：

- k8s 共享文件系统的挂载根只由[节点挂载代理](../glossary.md)在节点上挂载，不进入任何沙箱 Pod 或预热 Pod；节点挂载代理是特权 DaemonSet，同时是 CSI 节点插件。
- 沙箱 Pod 与预热 Pod 以节点挂载代理提供的 CSI 内联临时卷在工作区挂载路径获得按 Pod 的挂载点，挂载传播为 `HostToContainer`。
- 冷启动 Pod 在卷发布时，由节点挂载代理把运行实例子目录直接发布到该卷。
- 预热 Pod 创建时发布空挂载点；认领时 `sandboxprovider` 调用节点挂载代理的 `BindWorkspace`，把运行实例子目录绑定挂载到该 Pod 的卷发布目录，经挂载传播出现在容器内；绑定完成之后才写引导记录并唤醒 Pod。
- 运行实例子目录的创建、属主与配额由节点挂载代理在发布或绑定之前执行。
- 块存储退化配置、Substrate 的外部 CSI 卷与 `durableDir` 的卷本身按运行实例划分，已满足本决策；这些卷上的工作区准备（`Ensure`）由看守进程执行。

## 后果

本决策使项目承担以下义务（穷举）：

- 沙箱 Pod 与预热 Pod 只以节点挂载代理提供的 CSI 内联临时卷挂载工作区，使用 `HostToContainer` 传播，不挂载共享文件系统的挂载根。
- 认领在写引导记录之前完成绑定；绑定以 Pod 与运行实例为键幂等，Pod 删除时随卷卸载解除。
- 节点挂载代理只接受 `sandboxprovider` 的调用，并核对目标 Pod 位于本节点、属于沙箱、运行实例与代的标注与请求一致且尚未绑定过。
- `no_root_squash` 只为节点挂载代理以 root 创建运行实例子目录所需。
- 节点挂载代理的地址不出现在沙箱 Pod 与预热 Pod 的网络放行列表中。

承担的代价穷举如下：

- 需要部署一个特权 DaemonSet 与 CSI 驱动，扩大集群权限面与运维面。
- 认领路径多一次绑定调用，计入预热认领时延。
- 依赖挂载传播的可靠性，需要在目标集群验证。
- 节点挂载代理不可用时，该节点无法预配与认领沙箱。
- 挂载根仍存在于节点上，节点被攻破时不受本决策保护。

## 失效条件

- 共享文件系统与容器运行时普遍支持 idmapped mount 与按 Pod 的用户命名空间时，可由 Pod 内绑定子目录替代节点挂载代理。
