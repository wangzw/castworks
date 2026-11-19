# castworks 与同类项目

## 对照范围

对照的一端是 `../design/architecture.md` 描述的 castworks 需求与设计，另一端是设计目标与之重合的开源项目与商业产品。所列项目是示例，不是穷举。castworks 与 Google AX 的逐项对照见 [castworks 与 Google AX](castworks-vs-google-ax.md)，这里不重复。

对照只覆盖需求与设计，不覆盖实现与成熟度。各项目的设计取自其公开文档；文档没有写明的地方标为“文档未说明”，不作推断。

项目按覆盖的层次分组：

- 托管完整 Agent 的项目：除沙箱外，还管理会话、挂起决策与恢复后的续接。
- 可挂起沙箱：只提供隔离的计算实例及其暂停与恢复，不解释沙箱内运行的程序。对应 castworks 的 `sandboxprovider` 层。
- 沙箱内的 Agent 驱动层：在沙箱内启动第三方 Agent 并对外提供统一接口。对应 castworks 的看守进程与 Agent 驱动器。

## 结论

| 层次 | 项目 | 与 castworks 的关系 |
| --- | --- | --- |
| 托管完整 Agent | kagent、Claude Managed Agents | 问题域相同：会话比沙箱存续更久，等待人类时释放计算资源，凭据不进入沙箱。挂起只发生在回合边界 |
| 托管完整 Agent | Amazon Bedrock AgentCore Runtime、Foundry hosted agents | 每会话一个隔离的计算实例，空闲超时后释放并保留文件系统。Agent 是使用方自带的容器，平台不解释回合 |
| 托管完整 Agent | Open-Inspect、cullenmcdermott/sandbox | 每会话一个沙箱运行现成的编码 Agent，空闲后停止沙箱、保留文件系统 |
| 可挂起沙箱 | E2B、Daytona、Blaxel、Sprites、阿里云 Agent Sandbox、Cube Sandbox、OpenSandbox | 提供 castworks 沙箱提供者能力集合中的快照或销毁能力，挂起由超时、连接断开或显式调用触发 |
| 沙箱内的 Agent 驱动层 | Sandbox Agent | 在沙箱内以统一接口驱动多种第三方 Agent，不管理沙箱生命周期 |

所列项目的挂起触发点是回合结束、空闲超时、连接断开或显式调用。castworks 的挂起由控制面从持久化事实推导的等待触发，回合中等待 LLM 响应、外部系统或审批时同样可以冻结或快照；Agent 的自唤醒源由托管侧镜像进唤醒集合。回合中的挂起与自唤醒源的镜像，在所列项目的公开文档中没有对应的机制。

## 哪些项目托管完整的 Agent

| 项目 | 托管的 Agent | 挂起触发 | 挂起后保留 | 恢复触发 |
| --- | --- | --- | --- | --- |
| castworks | 完整的第三方 Agent（Claude Code），以进程运行在沙箱内 | 控制面从收件箱、唤醒信号与等待行推导等待，挂起策略选择机制与时长 | 冻结与快照保留内存；销毁重建保留工作区与恢复句柄 | 唤醒谓词成立：LLM 响应完成、外部系统事件、人类消息、审批决定或定时器到期 |
| kagent | Harness 的类型包括内置引擎、`claude`、`codex` 与自带镜像的 `byo` | 回合边界：任务进入终态，或等待人类输入与授权；不等待空闲超时 | 持久卷；进程内存丢弃 | 会话的下一条消息到达 |
| Claude Managed Agents | 内置 Agent 循环，由模型、系统提示词、工具、MCP 服务器与 skills 配置 | 会话进入 `idle`：回合结束、调用自定义工具或工具调用需要确认 | 沙箱检查点，含文件系统与已安装的包；进程内存是否保留，文档未说明 | 向会话发送用户消息、自定义工具结果或工具确认 |
| Amazon Bedrock AgentCore Runtime | 使用方自带的容器，不限框架 | 空闲超时，默认 900 秒；`/ping` 返回 `HealthyBusy` 时不计为空闲 | 内存不保留；配置会话存储时保留文件系统 | 以同一 `runtimeSessionId` 再次调用 |
| Foundry hosted agents | 使用方自带的容器，不限框架 | 空闲超时，默认 15 分钟，可配置为 2～240 分钟 | `$HOME` 与 `/files` | 同一会话再次收到请求 |
| Open-Inspect | OpenCode 或 Claude Agent SDK | 空闲或心跳过期 | 文件系统快照，或暂停的沙箱，取决于沙箱后端 | 下一条提示词到达 |
| cullenmcdermott/sandbox | Claude Code、OpenCode | 使用方命令；或回合结束、客户端已断开且空闲 15 分钟 | PVC；Pod 删除 | 使用方命令，或客户端重连 |

kagent（[REF-kagent](../references.md)）把 Agent 运行为 Agent Substrate（[REF-substrate](../references.md)）的 actor，自身承担会话与平台层。挂起由 kagent 在回合边界发起，恢复由会话的下一条消息触发，凭据由 Substrate 的出口网关持有，Agent 发出的占位头在网关处替换。castworks 的 Substrate 提供者路径采用同样的分层与凭据取向；差别在等待的建模：kagent 的等待只有“回合已结束”，castworks 的等待是条件集合，回合中的等待也在其中。

Claude Managed Agents（[REF-claude-managed-agents](../references.md)）把 Agent 循环、会话历史与凭据放在沙箱之外，沙箱只执行工具调用。沙箱可以由使用方自托管：使用方运行环境工作进程，以出站 HTTPS 轮询工作队列，在本地执行工具调用并回传结果。自定义工具调用与需要确认的工具调用以同一机制暂停会话：会话进入 `idle`，停止原因为 `requires_action`，全部阻塞事件得到回应后回到 `running`。castworks 的控制面循环驱动器与这一位置相同；castworks 的进程驱动器让循环留在沙箱内，等待 LLM 响应的是沙箱内的进程，因此需要回合中的冻结与快照。

Amazon Bedrock AgentCore Runtime（[REF-bedrock-agentcore-runtime](../references.md)）为每个会话预配独立的 microVM。空闲判定依赖 Agent 代码自报：`/ping` 返回 `Healthy` 表示等待请求，返回 `HealthyBusy` 表示后台任务进行中。castworks 不要求 Agent 自报，空闲由后台任务集合、自唤醒条目与沙箱活动度推导。

Foundry hosted agents（[REF-foundry-hosted-agents](../references.md)）的会话是逻辑上的隔离边界，计算实例是其运行时载体；空闲超时后平台释放计算实例并持久化会话状态，会话再次被引用时在新的计算实例上还原。这与 castworks 中运行实例与沙箱的分离相同。对话历史与键值状态存储由平台另行持久化，不随计算实例释放。

Open-Inspect（[REF-open-inspect](../references.md)）的控制面为每个会话维护一个带 SQLite 的 Cloudflare Durable Object，沙箱由可替换的后端提供，面向单一组织内互信的使用者。cullenmcdermott/sandbox（[REF-cullenmcdermott-sandbox](../references.md)）基于 kubernetes-sigs/agent-sandbox（[REF-agent-sandbox](../references.md)），每会话一个 Sandbox 资源与 PVC，恢复后以 `claude --resume` 续接会话，进程与进行中的回合不保留。它的恢复路径与 castworks 的 k8s 提供者在回合边界销毁重建后的续接方式相同。

## 哪些项目只提供可挂起的沙箱

| 项目 | 隔离 | 挂起触发 | 挂起后保留 | 恢复触发 |
| --- | --- | --- | --- | --- |
| E2B | Firecracker microVM | 显式调用，或超时后自动暂停 | 文件系统与内存，含运行中的进程 | 显式连接；启用自动恢复后，HTTP 流量与 SDK 操作 |
| Daytona | 默认为容器，可选虚拟机 | 无活动 15 分钟后自动停止；后台脚本不计为活动 | 容器停止后只保留文件系统；虚拟机暂停保留内存 | 文档未说明 |
| Blaxel | 轻量虚拟机 | 没有活动连接约 15 秒后进入待机 | 进程与文件系统；外部网络连接不保留 | 重新连接沙箱 |
| Sprites | Linux 虚拟机 | 不活动时自动休眠 | 以对象存储为后端的持久存储；内存是否保留，文档未说明 | 文档未说明 |
| 阿里云 Agent Sandbox | MicroVM | 按需休眠；触发者，文档未说明 | 内存状态 | 文档未说明；唤醒耗时 1～10 秒 |
| Cube Sandbox | 基于 RustVMM 与 KVM 的 MicroVM | 空闲超过超时后由 sidecar 触发 | 完整的虚拟机状态，含内存与文件系统 | 请求到达；请求阻塞到恢复完成 |
| OpenSandbox | runc、gVisor 或 Kata | 显式调用；Fast Sandbox 后端在没有入口流量时自动暂停 | Kubernetes 后端把根文件系统提交为 OCI 镜像，不保留内存与进程；agent-sandbox 后端保留 PVC | 显式调用；Fast Sandbox 后端由下一条 HTTP 请求唤醒 |

E2B（[REF-e2b](../references.md)）、Blaxel（[REF-blaxel](../references.md)）、阿里云 Agent Sandbox（[REF-aliyun-agent-sandbox](../references.md)）与 Cube Sandbox（[REF-cube-sandbox](../references.md)）的暂停保留内存，对应 castworks 沙箱提供者能力集合中的 `Snapshot`。Daytona（[REF-daytona](../references.md)）的容器停止与 OpenSandbox（[REF-opensandbox](../references.md)）的 Kubernetes 后端只保留文件系统，对应 `Destroy` 加工作区持久化。Sprites（[REF-sprites](../references.md)）另外提供文件系统级的检查点与回滚。Cube Sandbox 的快照可以写入 S3，在另一个节点上恢复。

这一层的项目以连接或请求的有无判定空闲。暂停期间沙箱对外的连接断开：E2B 的客户端全部断连，Blaxel 的外部网络连接在远端超时关闭。castworks 的沙箱内进程在等待 LLM 响应或外部系统时同样持有出站连接：Agent 的 LLM 调用与外部系统流量全部经托管侧代理，在途请求由 LLM 网关登记，冻结期间这些连接上的入站数据记为唤醒信号。

Cloudflare Sandbox SDK（[REF-cloudflare-sandbox](../references.md)）不属于这一层：沙箱不活动 10 分钟后容器停止，文件、进程与 shell 状态全部丢失，下一个请求启动新的容器。

## 哪些项目在沙箱内驱动第三方 Agent

Sandbox Agent（[REF-sandbox-agent](../references.md)）是运行在沙箱内的服务进程，负责安装、启动与停止 Agent 进程，路由提示词并转发事件，另外提供文件、进程与终端接口。它以同一套 HTTP 接口驱动 Claude Code、Codex、OpenCode、Cursor、Amp 与 Pi，并把 Agent 的权限请求转给调用方应答。

| 方面 | Sandbox Agent | castworks 的看守进程 |
| --- | --- | --- |
| 运行位置 | 沙箱内的服务进程 | 沙箱内，PID 1 |
| 与 Agent 的关系 | 以统一接口转发提示词与事件 | Agent 驱动器把 Agent 原生信号翻译为协议事件，把投递翻译为原生输入 |
| 会话持久化 | 由调用方 SDK 的持久化驱动承担，默认存于内存 | 运行实例状态由控制面持久化，投递意图日志随工作区持久化 |
| 恢复后续接 | 新建会话并重放最近的事件，不恢复进程 | 冻结直接解冻；快照恢复同代续接；销毁重建以 Agent 自身的会话恢复机制续接 |
| 沙箱生命周期 | 交给沙箱后端 | 挂起与解冻的沙箱侧执行由看守进程承担，决策在控制面 |

## 与 castworks 的设计差异

| 方面 | 所列项目 | castworks |
| --- | --- | --- |
| 挂起的依据 | 回合结束、空闲超时、连接断开或显式调用 | 等待由收件箱、唤醒信号与等待行推导；没有依赖时不得挂起 |
| 回合中的等待 | kagent 与 cullenmcdermott/sandbox 只在回合边界挂起；Claude Managed Agents 的循环在沙箱之外 | 回合中冻结，有快照能力时可快照；以挂起为目的的销毁只在安全点执行 |
| 后台工作 | AgentCore Runtime 由 Agent 代码自报忙碌；Daytona 不把后台脚本计为活动 | 后台作业存活时只在静止证据成立时冻结，不终止后台作业 |
| Agent 自身的定时与唤醒 | 文档未说明 | 托管侧镜像自唤醒源，保证到点时 Agent 进程可运行 |
| 外部系统事件 | 入站请求或消息触发恢复 | 连接器登记订阅，订阅完成事件写入收件箱并派生唤醒信号 |
| 凭据 | kagent 由出口网关替换占位头；Claude Managed Agents 的凭据在沙箱之外 | 沙箱内零长期凭据，出口代理默认拒绝并以占位符替换凭据 |
