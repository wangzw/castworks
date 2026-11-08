# 术语表

项目级术语，在全部文档类型中含义一致。文档正文使用中文术语，「英文对照」列是源语言或代码中的写法；模块 slug、文件路径、ADR 标识与协议字段名保留代码中的英文写法。

| 术语 | 英文对照 | 定义 |
| --- | --- | --- |
| 运行实例 | Run | 一次 Agent 任务的执行实体；拥有工作区、收件箱、等待状态与当前代，生命周期跨越沙箱的多次创建与销毁 |
| 沙箱 | Sandbox | 承载 Agent 进程的隔离计算实例（容器或 actor），生命周期短于运行实例 |
| 工作区 | Workspace | 运行实例的持久化文件系统目录，在沙箱销毁与重建之间保留 |
| 收件箱 | Inbox | 运行实例的追加写事件队列，只存放可投递给 Agent 的事件 |
| 唤醒信号 | wake signal | 不进入收件箱的内部信号，用于满足阻塞条件与代范围的等待条件，例如 LLM 响应完成或失败、定时器触发；审批决定与订阅完成写入收件箱时同一事务派生对应的唤醒信号 |
| 等待 | Wait | 运行实例当前等待的条件集合；由控制面从持久化事实推导，协作式 Agent 可追加显式条件 |
| 唤醒谓词 | wake predicate | 判定运行实例是否应恢复的条件：阻塞条件或代范围的等待条件（探测定时器、显式声明）已被唤醒信号满足，或处于回合边界且存在可投递事件（含跨代重投区间内待重投的事件）或以 hook 的 `defer`（[REF-claude-code-hooks](references.md)）推迟的审批决定，或回合中冻结的截止时间已到 |
| 订阅 | Subscription | 运行实例对某外部系统事件的持久登记，含过滤条件、关联键、完成条件与生命周期 |
| 回合 | Turn | Agent 从收到一条输入到停下等待下一条输入之间的工作段 |
| 回合边界 | turn boundary | 回合结束、Agent 等待下一条输入的时刻；L1 与 L2 以挂起为目的销毁沙箱的唯一时点（L0 不以挂起为目的销毁） |
| 安全点 | safe point | 回合边界上不变量 I9 的全部条件同时满足的时点；以挂起为目的的销毁只在此执行，条件由 `runcontroller` 预检、看守进程复核，任一条件不满足的回合边界不构成安全点 |
| 回合位置 | turn position | 运行实例在回合中的位置，由 `run` 行的回合字段表达，跨挂起保留 |
| 代 | generation | 运行实例每次预配沙箱时递增的单调计数 |
| 代范围 | generation scope | 随代递增作废的等待与定时器范围；代范围条件只对登记它的一代有效 |
| 运行实例范围 | run scope | 跨代保留的等待与定时器范围；运行实例范围的等待与定时器不携带代 |
| 失效判定 | fencing | 以代为依据拒绝过期沙箱的消息、令牌、工作区写入、游标推进与出口请求 |
| 挂起 | suspend | 释放沙箱计算资源的统称，涵盖冻结、快照与销毁重建 |
| 冻结 | freeze | 停止 Agent 进程树的 CPU 调度并保留内存；反向操作为解冻 |
| 快照 | snapshot | 把沙箱的内存与文件系统状态持久化后释放全部资源；反向操作为快照恢复 |
| 模板快照 | golden snapshot | Substrate 的 actor 模板控制器以模板运行一个 actor、待 `wakeupProbe` 返回 200 后制作的快照；新 actor 默认从它恢复，actor 模板尚无模板快照时新 actor 冷启动（[REF-substrate](references.md)） |
| 销毁重建 | destroy and recreate | 删除沙箱、保留工作区；恢复时新建沙箱并由 Agent 自身的会话恢复机制续接 |
| 沙箱提供者 | SandboxProvider | 以能力集合自描述的沙箱生命周期实现，包括 k8s 提供者与 Substrate 提供者 |
| 节点冻结代理 | node freeze agent | k8s 提供者中的特权 DaemonSet，对 Agent 子 cgroup（[REF-linux-cgroup-v2](references.md)）执行冻结与解冻 |
| 看守进程 | warden | 沙箱内的守护进程（PID 1）：驱动 Agent、执行 `Freeze{signal}` 降级路径的冻结与解冻（默认的 `Freeze{cgroup}` 由节点冻结代理执行）、采集活动度、提供本地代理、本地 DNS 与透明拦截器、平台 MCP 服务器、维持与控制面的连接 |
| Agent 驱动器 | Agent Driver | 看守进程中把某种 Agent 的原生信号翻译为协议事件、把投递翻译为该 Agent 原生输入的组件，即进程驱动器 |
| Agent 驱动器接口 | Agent Driver interface | 两种循环共用的唯一翻译层，规定协议事件、投递、挂起与恢复语义，进程驱动器与控制面循环驱动器各自实现 |
| 进程驱动器 | process driver | Agent 驱动器接口在沙箱内的实现，以无头协议（stream-json）驱动完整第三方 Agent（Claude Code），在同一代沙箱内保持 Agent 进程跨回合存活，并以恢复句柄续接会话 |
| 控制面循环驱动器 | control-plane loop driver | Agent 驱动器接口在控制面内的实现，运行平台原生 Agent 的模型循环，经看守进程的 `ToolExec` 接口使用沙箱，不假定沙箱内存在 Agent 进程 |
| 本地代理 | local proxy | 看守进程内不被冻结的 HTTP 代理，中继 Agent 进程按 LLM 基地址与代理环境变量发给它的出站 HTTP 流量：LLM 请求转发到 LLM 网关，外部系统请求以 CONNECT 或 HTTP 前置转发到出口代理；本地 DNS 指向的 `mitm` 与 `passthrough` 主机连接由透明拦截器接收，不经本地代理 |
| 本地 DNS | local DNS | 看守进程在沙箱内提供的域名解析，依据出口策略中各主机的网络通道，把 `mitm` 与 `passthrough` 主机解析到回环地址上的透明拦截器 |
| 透明拦截器 | transparent interceptor | 看守进程内与本地代理并列、不被冻结的回环组件，接收本地 DNS 指向它的连接，按 SNI 识别目标，以附运行实例身份的 CONNECT 隧道把原始 TLS 字节转给出口代理，不终止 TLS |
| 网络通道 | network channel | 外部系统定义按主机选择的沙箱出站方式，取值闭集为 `proxy`、`mitm`、`passthrough`：`proxy` 只依赖代理环境变量进入本地代理，`mitm` 与 `passthrough` 由本地 DNS 把主机指向透明拦截器 |
| LLM 网关 | LLM gateway | 控制面中代理 LLM 调用的服务：凭据换取、代校验、在途跟踪、回放存储、限流计费 |
| 直通模式 | passthrough | 本地代理在 LLM 请求开始时一次性选定的转发模式之一：逐条向 Agent 转发上游 SSE 事件；LLM 网关按同一事件顺序转发，模式不改变 LLM 网关的转发行为 |
| 缓存模式 | hold | 本地代理在 LLM 请求开始时一次性选定的转发模式之一：缓冲内容事件、只向 Agent 转发 `ping` 保活事件，解冻后一次性交付；LLM 网关仍按同一事件顺序转发并保存回放 |
| 连接器 | Connector | 按外部系统定义接入一个外部系统的组件：出口策略、事件源、订阅匹配、完成条件与状态式轮询 |
| 外部系统定义 | ExternalSystem | 描述一个外部系统的认证方式、出口策略、事件源、完成条件目录、关联键与关联键身份键的声明；不声明具体操作 |
| 协作层级 | cooperation level | Agent 向托管环境提供信号的程度：L0 纯观测、L1 驱动器中介、L2 协作式 |
| 投递意图日志 | delivery intent log | 看守进程写在工作区内的投递记录，使事件去重在沙箱重建后仍然有效 |
| 恢复准入 | resume admission | 对等待恢复的运行实例按机制限速与优先级放行的队列 |
| 预测性恢复 | predictive resume | 等待尚未确认解除时，依据 `wake_hint` 事实预测依赖即将解除而提前把运行实例转入恢复准入的唤醒；由 `suspendpolicy` 批准，对同一来源标识至多一次，唤醒原因为 `predictive` |
| 实时接入 | live attachment | 人在平台界面的会话中活跃、已连接沙箱终端，或界面正在经沙箱读取工作区文件的状态 |
| 人类通道 | human channel | 把聊天界面、即时通讯、Issue 评论等人类输入与审批决定写入收件箱的模块 |
| 平台 MCP 服务器 | platform MCP server | 看守进程提供给 Agent 的 MCP（[REF-mcp](references.md)）工具集合：列出可订阅事件、登记订阅、阻塞等待、定时等待、附件取回；不执行第三方操作 |
| 事件目录 | event catalogue | 平台 MCP 服务器向 Agent 投影的可订阅事件集合：事件类型、可匹配字段、关联键身份键、终态取值与订阅参数；由 `connectorhub` 按运行实例可用的外部系统生成 |
| 工具接入声明 | tool profile | 由 `connectorhub` 按项目装载与校验、随沙箱侧配置推送给看守进程的第三方工具与 MCP 服务器声明：凭据占位符环境变量、非秘密配置、stdio MCP 服务器与允许主机；位于工作区之外，工作区 `.mcp.json` 不生效 |
| 占位符环境变量 | placeholder environment variable | 值为 `{{<system>.<name>}}` 形式占位符的环境变量，由看守进程注入第三方工具；真实凭据仍只在出口代理内存中出现 |
| 完成条件目录 | completion catalogue | 外部系统定义中按事件类型声明的 `verify` 集合；订阅按事件类型查找状态核对方式，不按操作名查找 |
| 关联键身份键 | identity key | `events.identity_keys` 声明的关联键子集；一次性订阅的关联键必须至少包含其中一个，用于限制宽泛匹配 |
| 连接器事件日志 | connector event log | 外部系统事件的追加写日志，以 `delivery_id` 唯一标识一次投递；订阅的实时匹配与回溯匹配都以它为事实源 |
| 终态取值 | terminal outcome | 外部对象在订阅关注维度上的结束取值，取自外部系统定义 `verify.terminal` 所列的闭集；订阅完成事件的 `event_id` 以它与 `sub_id` 派生 |
| 出口策略 | egress policy | 连接器为每个运行实例生成的允许出口主机集合与请求头凭据占位符替换规则，由沙箱提供者落地；未列入的主机默认拒绝 |
| 出口代理 | egress proxy | 落地出口策略的托管侧代理，接收本地代理与透明拦截器转来的 Agent 流量，在发往允许主机时把凭据占位符替换为真实凭据；k8s 路径为 connectorhub 的平台出口代理，Substrate 路径为 Substrate 出口网关 |
| 沙箱侧配置 | SandboxConfig | 由 `connectorhub` 生成、经 `runcontroller` 在出站流上推送给看守进程、由看守进程只读的配置，内容为各主机的网络通道与工具接入声明，不含密钥，不经过 `sandboxprovider`；字段与语义由 `connectorhub` 定义，`runcontroller` 按运行实例保存并转发，不解释其字段 |
| 工作区租约 | workspace lease | workspace 模块持有的 {run_id, generation, lease_id, holder, expires_at} 记录；看守进程以首次续约绑定持有者并周期续约，续约失败即冻结或杀死 Agent 进程树；新代只在旧租约到期，或沙箱提供者确认租约所属代的沙箱已删除后接管工作区 |
| 持久化确认 | persistence confirmation | 在销毁沙箱前确认工作区写入已持久化的处理；`sync` 为 fsync 完成，`on_suspend` 为同步挂起上传完成 |
| 运行实例所有者租约 | run owner lease | `run` 行上的 `owner_id`、`owner_epoch` 与 `owner_lease_expires_at`，决定该运行实例状态机的写者 |
| 运行实例令牌 | Run token | 短期 JWT {run_id, generation, exp}，只由看守进程持有，首个令牌经引导接口领取，此后按控制面的 TokenRotate 轮换；LLM 网关与连接器以它换取真实凭据并校验其中的代，k8s 路径的平台出口代理以它逐请求校验代；令牌在签名、过期时间与 run_id 校验通过，令牌中的代等于运行实例当前代，运行实例未进入终态，本代沙箱未被判定作废时有效 |
| 引导接口 | bootstrap endpoint | `sandboxprovider` 在控制面服务中以 HTTPS 暴露的 `POST /v1/bootstrap/claim`，看守进程以沙箱实例的身份凭据调用它，领取运行实例令牌与工作区租约的 `lease_id`；领取指这一次性取得令牌与 `lease_id` 的请求，同一沙箱实例至多领取成功一次，看守进程在启动 Agent 主进程之前完成领取；k8s 路径以绑定到 Pod 的投射 ServiceAccount 令牌认证，Substrate 路径以出口网关注入的 actor JWT 认证，领取语义相同 |
| 出站流 | outbound stream | 看守进程运行时始终维持的一条主动外连控制面的 gRPC 双向流，承载全部协议消息与投递；重连后发送方重放未确认消息，Hello 与 HelloAck 做状态对账 |
| 汇入契约 | ingestion contract | runcontroller 向事件源模块提供的唯一写入接口，用于写入收件箱事件、唤醒信号、运行实例事实、事件源实例租约与沙箱侧配置；runcontroller 不反向调用事件源模块 |
| 效果记录 | effect record | 由 runcontroller 与产生它的状态转移或汇入写入在同一数据库事务内写入、提交后按幂等键执行的副作用记录；副作用例如调用沙箱提供者、workspace 与 secrets 模块，或经出站流发送消息；滞留超过截止时间由对账器重驱动 |
| 对账器 | reconciler | runcontroller 的纠正性后台组件，按 `T_reconcile` 周期扫描持久化状态，对因过渡态超时、唤醒遗漏、沙箱丢失等而停住的运行实例以同一标识幂等重驱动；以持久化状态为准，不以事件历史为准（I10） |
| 恢复句柄 | resume handle | 销毁重建后，续接 Agent 会话所需的标识，随 ReadyToSuspend 上报并在销毁执行前落库；Claude Code 驱动器下为 session_id（[REF-claude-code-headless](references.md)） |
| 状态式轮询 | state reconcile poll | 连接器按活动订阅周期查询外部对象当前状态、只产生订阅完成事件的轮询，兜底丢失或乱序的 webhook |
