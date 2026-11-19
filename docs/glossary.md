# 术语表

项目级术语，在全部文档类型中含义一致。文档正文使用中文术语，“英文对照”列是源语言或代码中的写法；模块 slug、文件路径、ADR 标识与协议字段名保留代码中的英文写法。

| 术语 | 英文对照 | 定义 |
| --- | --- | --- |
| 运行实例 | Run | 一次 Agent 任务的执行实体；拥有工作区、收件箱、等待状态与当前代，生命周期跨越沙箱的多次创建与销毁 |
| 运行实例创建者 | run creator | 发起创建某运行实例的平台用户，记录于 `run.created_by`；与运行实例所有者租约中的所有者（`runcontroller` 副本）无关 |
| 沙箱 | Sandbox | 承载 Agent 进程的隔离计算实例（容器或 actor），生命周期短于运行实例 |
| 工作区 | Workspace | 运行实例的持久化文件系统目录，在沙箱销毁与重建之间保留 |
| 收件箱 | Inbox | 运行实例的追加写事件队列，只存放可投递给 Agent 的事件 |
| 唤醒信号 | wake signal | 不进入收件箱的内部信号，用于满足阻塞条件与代范围的等待条件，例如 LLM 响应完成或失败、定时器触发；审批决定与订阅完成写入收件箱时，在同一事务内派生对应的唤醒信号 |
| 等待 | Wait | 运行实例当前等待的条件集合；由控制面从持久化事实推导，协作式 Agent 可追加显式条件 |
| 唤醒谓词 | wake predicate | 判定运行实例是否应恢复的条件：阻塞条件或代范围的等待条件（探测定时器、显式声明、静止冻结登记的等待）已被唤醒信号满足，或冻结期间 Agent 持有的外部系统连接收到入站数据，或处于回合边界，且存在可投递事件（含跨代重投区间内待重投的事件）或以 hook 的 `defer`（[REF-claude-code-hooks](references.md)）推迟的审批决定，或冻结的截止时间已到。自唤醒源的到期不属于唤醒谓词，由所有者侧截止时间驱动 |
| 自唤醒源 | self-wake source | Agent 自行安排、到点由 Agent 进程自行触发而不经任何托管侧组件的唤醒来源，例如 Claude Code 的会话内定时（`CronCreate`、`ScheduleWakeup`）、`sleep` 进程、工具超时与 `Monitor` 截止；与之相对，LLM 完成、外部系统事件、人类消息与托管侧定时器经托管侧组件到达。托管侧观测其触发时刻并镜像进唤醒集合。每个已镜像的自唤醒安排称为自唤醒条目，触发时刻无法确定的条目同样镜像，其触发时刻为空 |
| 有效自唤醒条目 | valid self-wake entry | `fire_at` 非空且不早于 `now − (L_self_wake(substate) + P_quiesce)` 的自唤醒条目；`substate` 是界限所针对的挂起形态，`L_self_wake(substate)` 是该形态的自唤醒提前量，`P_quiesce` 是静止冻结的探测间隔上限；`fire_at` 早于该界限的自唤醒条目称为过期自唤醒条目，视为已触发、待 Agent 驱动器对账；`fire_at` 为空的自唤醒条目既不是有效自唤醒条目，也不是过期自唤醒条目 |
| 等待意图 | wait intent | Agent 驱动器对某个前台工具调用、后台作业或回合推断出的等待性质：在等什么、预计多久，以及该调用或作业是在工作、等待还是提供服务，该回合是以提问结束还是以任务完成结束；每条意图带来源，置信度由 `suspendpolicy` 按来源确定 |
| 订阅 | Subscription | 运行实例对某外部系统事件的持久登记，含所属项目、过滤条件、关联键、完成条件与生命周期；只匹配 `project_ids` 包含其 `project_id` 的事件 |
| 回合 | Turn | Agent 从收到一条输入到停下等待下一条输入之间的工作段 |
| 回合边界 | turn boundary | 回合结束、Agent 等待下一条输入的时刻；L1 与 L2 以挂起为目的销毁沙箱的唯一时点（L0 不以挂起为目的销毁） |
| 安全点 | safe point | 回合边界上不变量 I9 的全部条件同时满足的时点；以挂起为目的的销毁只在此执行，条件由 `runcontroller` 预检、看守进程复核，任一条件不满足的回合边界不构成安全点 |
| 回合位置 | turn position | 运行实例在回合中的位置，由 `run` 行的回合字段表达，跨挂起保留 |
| 代 | generation | 运行实例每次预配沙箱时递增的单调计数 |
| 代范围 | generation scope | 随代递增作废的等待与定时器范围；代范围条件只对登记它的一代有效 |
| 运行实例范围 | run scope | 跨代保留的等待与定时器范围；运行实例范围的等待与定时器不携带代 |
| 失效判定 | fencing | 以代为依据拒绝过期沙箱的消息、令牌、工作区写入、游标推进与出口请求；出站流上，同一代内工作区租约或 `sandbox_id` 与登记不符的连接同样按失效处理 |
| 挂起 | suspend | 释放沙箱计算资源的统称，涵盖冻结、快照与销毁重建 |
| 冻结 | freeze | 停止 Agent 进程树的 CPU 调度并保留内存；反向操作为解冻 |
| 静止冻结 | quiescent freeze | 没有单一等待条件可依（后台作业存活，或回合中多个 Agent 循环并发）时，依据静止证据执行的冻结。静止证据指没有任何 Agent 循环能在不依赖托管侧唤醒的情况下推进。冻结附带探测定时器，由本代 LLM 完成或失败、冻结期间的入站数据、探测定时器、自唤醒到期或冻结上限唤醒，不升级为快照或销毁 |
| 快照 | snapshot | 把沙箱的内存与文件系统状态持久化后释放全部资源；反向操作为快照恢复 |
| 模板快照 | golden snapshot | Substrate 的 actor 模板控制器以模板运行一个临时 actor，待 `wakeupProbe` 返回 200 后按模板的 `preferredFidelity` 制作的快照（每个容器都声明探针时跳过约 20 s 暖机）；新 actor 默认从它恢复，actor 模板尚无模板快照，或快照与 worker 运行时不兼容时，新 actor 冷启动或降级为 `ROOTFS` 冷启动（[REF-substrate](references.md)） |
| 销毁重建 | destroy and recreate | 删除沙箱、保留工作区；恢复时新建沙箱并由 Agent 自身的会话恢复机制续接 |
| 沙箱提供者 | SandboxProvider | 以能力集合自描述的沙箱生命周期实现，包括 k8s 提供者与 Substrate 提供者 |
| 节点冻结代理 | node freeze agent | k8s 提供者中的特权 DaemonSet，对 Agent 子 cgroup（[REF-linux-cgroup-v2](references.md)）执行冻结与解冻 |
| 节点挂载代理 | node mount agent | k8s 提供者中的特权 DaemonSet（CSI 节点插件），在节点上挂载工作区挂载根，为沙箱 Pod 与预热 Pod 提供工作区挂载点，创建运行实例子目录并设置属主与配额，再把该运行实例的子目录绑定挂载进 Pod；挂载根不进入任何 Pod |
| 看守进程 | warden | 沙箱内的守护进程（PID 1）：驱动 Agent；执行 `Freeze{signal}` 降级路径的冻结与解冻（默认的 `Freeze{cgroup}` 由节点冻结代理执行）；采集活动度；提供本地代理、本地 DNS、透明拦截器与平台 MCP 服务器；维持与控制面的连接 |
| Agent 驱动器 | Agent Driver | 看守进程中把某种 Agent 的原生信号翻译为协议事件，并把投递翻译为该 Agent 原生输入的组件，即进程驱动器 |
| Agent 驱动器接口 | Agent Driver interface | 两种循环共用的唯一翻译层，规定协议事件、投递、挂起与恢复语义，进程驱动器与控制面循环驱动器各自实现 |
| 进程驱动器 | process driver | Agent 驱动器接口在沙箱内的实现，以无头协议（stream-json）驱动完整第三方 Agent（Claude Code），在同一代沙箱内保持 Agent 进程跨回合存活，并以恢复句柄续接会话 |
| 控制面循环驱动器 | control-plane loop driver | Agent 驱动器接口在控制面内的实现，运行平台原生 Agent 的模型循环，经看守进程的 `ToolExec` 接口使用沙箱，不假定沙箱内存在 Agent 进程 |
| 本地代理 | local proxy | 看守进程内不被冻结的 HTTP 代理：处理 Agent 进程的 LLM 请求并转发到 LLM 网关，把按代理环境变量到达的外部系统请求以 CONNECT 或 HTTP 前置转发到出口代理。LLM 请求默认由透明拦截器终止 TLS 后交入，启用 LLM 基地址或代理环境变量时由 Agent 进程直接发给本地代理；本地 DNS 指向的 `mitm` 与 `passthrough` 主机连接由透明拦截器接收，不经本地代理 |
| LLM 主机 | LLM host | Agent 驱动器声明的、其 Agent 在默认网络配置下发送 LLM 请求的目的主机 |
| 拦截 CA | interception CA | 看守进程为每个沙箱实例生成的证书签发者，只为 LLM 主机签发证书，用于在沙箱内终止 Agent 发往 LLM 主机的 TLS；私钥只在看守进程的内存中，证书并入沙箱的合并信任库 |
| 本地 DNS | local DNS | 看守进程在沙箱内提供的域名解析，把 LLM 主机以及出口策略中网络通道为 `mitm` 与 `passthrough` 的主机解析到回环地址上的透明拦截器 |
| 透明拦截器 | transparent interceptor | 看守进程内与本地代理并列、不被冻结的回环组件，接收本地 DNS 指向它的连接并按 SNI 识别目标：LLM 主机的连接以拦截 CA 签发的证书终止 TLS，明文请求交给本地代理；外部系统主机的连接以附运行实例身份的 CONNECT 隧道把原始 TLS 字节转给出口代理，不终止 TLS |
| 网络通道 | network channel | 外部系统定义中按主机选择的沙箱出站方式，取值闭集为 `proxy`、`mitm`、`passthrough`，未声明时取 `mitm`：`proxy` 只依赖代理环境变量进入本地代理，`mitm` 与 `passthrough` 由本地 DNS 把主机指向透明拦截器 |
| LLM 网关 | LLM gateway | 控制面中代理 LLM 调用的服务：凭据换取、代校验、在途跟踪、回放存储、限流计费 |
| 直通模式 | passthrough | 本地代理在 LLM 请求开始时一次性选定的转发模式之一：逐条向 Agent 转发上游 SSE 事件；LLM 网关按同一事件顺序转发，模式不改变 LLM 网关的转发行为 |
| 缓存模式 | hold | 本地代理在 LLM 请求开始时一次性选定的转发模式之一：缓冲内容事件、只向 Agent 转发 `ping` 保活事件，解冻后一次性交付；LLM 网关仍按同一事件顺序转发并保存回放 |
| 连接器 | Connector | 按外部系统定义接入一个外部系统的组件：出口策略、事件源、订阅匹配、完成条件与状态式轮询 |
| 外部系统定义 | ExternalSystem | 描述一个外部系统的认证方式、出口策略、事件源、完成条件目录、关联键、关联键身份键与等待签名的声明；不声明具体操作 |
| 协作层级 | cooperation level | Agent 向托管环境提供信号的程度：L0 纯观测、L1 驱动器中介、L2 协作式 |
| 投递意图日志 | delivery intent log | 看守进程写在工作区内的投递记录，使事件去重在沙箱重建后仍然有效 |
| 恢复准入 | resume admission | 对等待恢复的运行实例按机制限速与优先级放行的队列 |
| 预测性恢复 | predictive resume | 等待尚未确认解除时，依据 `wake_hint` 事实预测依赖即将解除而提前把运行实例转入恢复准入的唤醒；由 `suspendpolicy` 批准，对同一来源标识至多一次，唤醒原因为 `predictive` |
| 实时接入 | live attachment | 人在平台界面的会话中活跃、已连接沙箱终端，或界面正在经沙箱读取工作区文件的状态 |
| 人类通道 | human channel | 把聊天界面、即时通讯、Issue 评论等人类输入与审批决定写入收件箱的模块 |
| 项目角色 | project role | 平台用户在项目中的授权角色，取值闭集为 `member`、`approver`、`admin`，由部署对接的身份提供方的组声明映射得到；`humanchannel` 据此判定用户对运行实例与工作区的操作权限 |
| 审批人 | approver | 对某运行实例的待决审批有权提交决定的平台用户：该运行实例的创建者，或在其项目中持有 `approver` 角色的用户 |
| 审批内容摘要 | approval content digest | 待决工具调用的工具输入按规范 JSON 编码后的 SHA-256 小写十六进制值（协议字段 `input_hash`）；审批决定与定向投递都携带它，看守进程在交付决定前重算比对，不一致即拒绝该次调用 |
| 外部身份关联 | identity link | 即时通讯账号或 Issue 评论账号等外部身份与平台用户之间经 OAuth 账号绑定或带签名回调显式建立的对应关系；未关联的外部身份发出的消息与审批决定不产生事件 |
| 平台 MCP 服务器 | platform MCP server | 看守进程提供给 Agent 的 MCP（[REF-mcp](references.md)）工具集合：列出可订阅事件、登记订阅、阻塞等待、定时等待、附件取回；不执行第三方操作 |
| 事件目录 | event catalogue | 平台 MCP 服务器向 Agent 投影的可订阅事件集合：事件类型、可匹配字段、关联键身份键、终态取值与订阅参数；由 `connectorhub` 按运行实例可用的外部系统生成 |
| 等待签名 | wait signature | 外部系统定义中声明的等待识别规则：把匹配某一模式的命令或请求（例如 `gh run watch <id>`）识别为对该外部系统事件目录中某一事件类型的等待，并给出从匹配结果提取关联键的规则与该类等待的时长先验 |
| 工具接入声明 | tool profile | 由 `connectorhub` 按项目装载与校验，并随沙箱侧配置推送给看守进程的第三方工具与 MCP 服务器声明：凭据占位符环境变量、非秘密配置、stdio MCP 服务器与允许主机；位于工作区之外，工作区 `.mcp.json` 不生效 |
| 占位符环境变量 | placeholder environment variable | 值为 `{{<system>.<name>}}` 形式的占位符的环境变量，由看守进程注入第三方工具；真实凭据仍只在出口代理内存中出现 |
| 凭据主体 | credential principal | 项目凭据对象在外部系统中代表的账号，例如 GitHub App 机器人账号、个人访问令牌持有人或即时通讯机器人；人类通道拒绝凭据主体发出的消息与审批决定 |
| 完成条件目录 | completion catalogue | 外部系统定义中按事件类型声明的 `verify` 集合；订阅按事件类型查找状态核对方式，不按操作名查找 |
| 关联键身份键 | identity key | `events.identity_keys` 声明的关联键子集；一次性订阅的关联键必须至少包含其中一个，用于限制宽泛匹配 |
| 项目归属键 | project binding key | 外部系统定义 `events.project_binding` 声明的关联键字段路径；连接器事件按其规范化取值与项目配置的绑定值确定所属项目，订阅只匹配本项目的事件 |
| 连接器事件日志 | connector event log | 外部系统事件的追加写日志，以 `delivery_id` 唯一标识一次投递，每条事件带所属项目集合 `project_ids`；订阅的实时匹配与回溯匹配都以它为事实源 |
| 终态取值 | terminal outcome | 外部对象在订阅关注维度上的结束取值，取自外部系统定义 `verify.terminal` 所列的闭集；订阅完成事件的 `event_id` 以它与 `sub_id` 派生 |
| 出口策略 | egress policy | 连接器为每个运行实例生成的允许出口主机集合与请求头凭据占位符替换规则，由沙箱提供者落地；未列入的主机默认拒绝 |
| 出口代理 | egress proxy | 落地出口策略的托管侧代理，接收本地代理与透明拦截器转来的 Agent 流量，在发往允许主机时把凭据占位符替换为真实凭据；k8s 路径为 connectorhub 的平台出口代理，Substrate 路径为 Substrate 出口网关 |
| 沙箱侧配置 | SandboxConfig | 由 `connectorhub` 生成、经 `runcontroller` 在出站流上推送给看守进程、由看守进程只读的配置，内容为各主机的网络通道、工具接入声明、该运行实例可用外部系统的等待签名以及情境提示与宿主侧分类的开关。该配置不含密钥，不经过 `sandboxprovider`；字段与语义由 `connectorhub` 定义，`runcontroller` 按运行实例保存并转发，不解释其字段 |
| 工作区租约 | workspace lease | workspace 模块持有的 {run_id, generation, lease_id, holder, holder_jkt, expires_at} 记录；`holder_jkt` 是领取时登记的实例公钥指纹，看守进程以首次续约绑定持有者并周期续约，续约须附持有证明，续约失败即冻结或杀死 Agent 进程树；新代只在旧租约到期或沙箱提供者确认租约所属代的沙箱已删除后接管工作区 |
| 持久化确认 | persistence confirmation | 在销毁沙箱前确认工作区写入已持久化的处理；`sync` 为 fsync 完成，`on_suspend` 为同步挂起上传完成 |
| 运行实例所有者租约 | run owner lease | `run` 行上的 `owner_id`、`owner_epoch` 与 `owner_lease_expires_at`，决定该运行实例状态机的写者 |
| 实例密钥 | instance key | 看守进程在领取之前生成、私钥只保存在进程内存中的签名密钥对；公钥随领取请求提交，其指纹写入运行实例令牌的 `cnf.jkt` 与工作区租约的 `holder_jkt` |
| 持有证明 | holder proof | 看守进程以实例密钥的私钥对用途、运行实例或租约标识、代、沙箱标识，以及 `Hello` 时控制面为该条出站流生成的一次性挑战随机数或 `RenewLease` 时的签名时刻所作的签名，随 `Hello` 与 `RenewLease` 提交；`Hello` 的持有证明不带时窗，`RenewLease` 的持有证明按时窗判定签名时刻。接收方据此确认调用方持有领取时登记的实例密钥 |
| 运行实例令牌 | Run token | 按受众划分的短期 JWT 组，一次签发同 `run_id`、同代、同 `exp`、同一实例公钥指纹（`cnf.jkt`）的一组令牌，接收方只接受本身受众的令牌；受众闭集为 `runcontroller`、`llmgateway`、`connectorhub`、`humanchannel`、`egress`。首个令牌组经引导接口领取，此后按控制面的 `TokenRotate` 轮换，只由看守进程持有；令牌须同时满足以下条件才有效：签名、过期时间、`run_id` 与受众校验通过；令牌中的代等于运行实例当前代；运行实例未进入终态；本代沙箱未被判定作废 |
| 引导接口 | bootstrap endpoint | `sandboxprovider` 在控制面服务中以 HTTPS 暴露的 `POST /v1/bootstrap/claim`，看守进程以沙箱实例的身份凭据调用它，请求体携带实例公钥，领取按受众划分的运行实例令牌组、工作区租约的 `lease_id` 与接入票据公钥集合；领取指一次性取得令牌组、`lease_id` 与公钥集合的这一次请求，同一沙箱实例至多领取成功一次，看守进程在启动 Agent 主进程之前完成领取。k8s 路径以绑定到 Pod 的投射 ServiceAccount 令牌认证，Substrate 路径以出口网关注入的 actor JWT 认证，领取语义相同 |
| 接入票据 | access ticket | 由 `secrets` 按 `humanchannel` 的申请签发的短期 JWT，绑定平台用户、项目、运行实例、代与接入通道（`terminal` 或 `file`）；入站路由原样转发，看守进程在 `/terminal` 与 `/files` 上校验后才建立终端或读取文件。`terminal` 通道的票据只接受一次，`file` 通道的票据在有效期内可用于同一用户、同一运行实例、同一代的多次只读请求 |
| 面向沙箱的端点 | sandbox-facing endpoint | 控制面中供看守进程调用、与只接受 SPIFFE（[REF-spiffe](references.md)）对端的内部接口分开监听的端点，部署在独立的主机名与端口上：出站流、引导接口、工作区租约续约、LLM 网关的请求与回放取回、`connectorhub` 的事件目录、订阅登记与附件取回、`humanchannel` 的附件取回，以及 k8s 路径的平台出口代理；每个端点都要求 Agent 进程不持有的凭据 |
| 出站流 | outbound stream | 看守进程运行时始终维持的一条主动外连控制面的 gRPC 双向流，承载全部协议消息与投递；重连后看守进程重放控制面尚未提交的消息，控制面发往看守进程的消息不跨连接重放，由 `runcontroller` 按持久化状态重新产生，Hello 与 HelloAck 做状态对账 |
| 汇入契约 | ingestion contract | runcontroller 向事件源模块提供的唯一写入接口，用于写入收件箱事件、唤醒信号、运行实例事实、事件源实例租约与沙箱侧配置；runcontroller 不反向调用事件源模块 |
| 效果记录 | effect record | 由 runcontroller 与产生它的状态转移或汇入写入在同一数据库事务内写入，并在提交后按幂等键执行的副作用记录；副作用例如调用沙箱提供者、workspace 与 secrets 模块，或经出站流发送消息；滞留超过截止时间的记录由对账器重驱动 |
| 对账器 | reconciler | runcontroller 的纠正性后台组件，按 `T_reconcile` 周期扫描持久化状态，对因过渡态超时、唤醒遗漏、沙箱丢失等而停住的运行实例以同一标识幂等重驱动；以持久化状态为准，不以事件历史为准（I10） |
| 恢复句柄 | resume handle | 销毁重建后，续接 Agent 会话所需的标识，随 ReadyToSuspend 上报并在销毁执行前落库；Claude Code 驱动器下为 session_id（[REF-claude-code-headless](references.md)） |
| 状态式轮询 | state reconcile poll | 连接器按活动订阅周期查询外部对象当前状态且只产生订阅完成事件的轮询，兜底丢失或乱序的 webhook |
