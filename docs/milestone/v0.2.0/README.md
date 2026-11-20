# v0.2.0 MVP 范围

v0.2.0 仍是概念验证，不上生产。它在 [v0.1.0](../v0.1.0/README.md) 的基础上证明：Agent 等外部系统时，castworks 同样能把沙箱挂起，外部事件到达后由控制面恢复运行实例，Agent 带着原有上下文处理结果。外部唤醒源由托管侧持有：Agent 登记[订阅](../../glossary.md)后结束回合，外部系统的回调由控制面接收，经收件箱唤醒运行实例（[ADR-host-owns-external-wake-sources](../../adr/adr-host-owns-external-wake-sources.md)）。

外部系统不接 GitHub，改用自建的[模拟外部系统](../../glossary.md) `extsim`。直接对接第三方系统，开发、测试与部署都要依赖公网 webhook 入口、CI runner 与第三方凭据；`extsim` 运行在开发机上，回调的时机与作业的结论都可以由测试控制。`extsim` 按[外部系统定义](../../glossary.md)接入 `connectorhub`。`connectorhub` 的通用部分不含针对它的代码，针对它的只有一个连接器，负责签名头、事件类型、结论字段与状态查询；以后接入 GitHub，是多装载一份定义并实现 GitHub 的连接器。

v0.1.0 的范围、组成、偏离与安全让步在 v0.2.0 中继续有效，下文只写变化。[设计文档](../../design/README.md)仍是长期目标，v0.2.0 的代码同样不声称满足[架构不变量](../../design/architecture.md#架构不变量)。

各模块在 v0.2.0 中的细化：[extsim](extsim.md)、[connectorhub](connectorhub.md)，以及在 v0.1.0 基础上的增量 [runcontroller](runcontroller.md)、[agentprotocol](agentprotocol.md)、[warden](warden.md)、[sandboxprovider](sandboxprovider.md)。

## 演示场景

1. 用命令行创建运行实例。提示要求 Agent 把一份内容提交给 `extsim` 校验，不通过就按给出的原因改正后重交，直到通过，并说明用平台的订阅工具等待校验结果。提示给出的初始内容不满足 `extsim` 的校验规则。
2. Agent 用镜像内的 `extsim` 命令行提交作业，取得 `job_id`；经[平台 MCP 服务器](../../glossary.md)调用 `list_events` 查到 `job.completed`，再调用 `subscribe_event` 按 `job_id` 订阅，然后结束回合。
3. 回合边界空闲满 30 s，控制面对 actor 做快照挂起，与 v0.1.0 相同。
4. 作业结束，`extsim` 回调控制面，`connectorhub` 匹配订阅，把订阅完成事件写入收件箱。控制面恢复 actor，订阅完成事件作为下一回合的输入交给 Claude Code。第一次提交的结论是不通过，Agent 读到原因后改正、重交、重新订阅，回到第 3 步；第二次通过，Agent 报告结果。
5. 用命令行结束运行实例。

满足下列条件即视为 v0.2.0 完成：

- 场景在开发集群上完整跑通：第一次提交不通过，改正后第二次通过，两次等待都经过挂起与恢复；恢复后的回合引用了此前提交的内容与不通过的原因。
- 作业在订阅登记之前结束并回调：登记时的回溯匹配命中该事件，订阅直接完成，运行实例不会一直等下去。
- 作业已经结束而回调被丢弃：登记时按定义中的状态查询核对作业，直接生成订阅完成事件。这一条与上一条合起来，覆盖设计中 I2 的两种情形。
- 同一回调重复投递：Agent 只收到一次订阅完成事件。
- 运行实例已挂起、订阅尚未完成时重启控制面：停机期间的回调由 `extsim` 重试送达，运行实例恢复并处理该事件。
- Agent 在后台运行 `extsim job watch` 等待作业时，运行实例不挂起；后台作业结束、回合结束并空闲满 30 s 之后照常挂起。
- actor 在运行实例处于 `active` 时崩溃：运行实例在 `T_reconnect`（30 s）加一个挂起扫描周期之内转为失败，失败原因写明 actor 已崩溃，见 [runcontroller](runcontroller.md#actor-崩溃的检测)。

## 模拟外部系统

`extsim` 是开发机上的独立进程，不并入控制面，模拟“发起异步作业，作业结束后回调”这一类外部系统。

| 接口 | 监听地址 | 调用方 | 作用 |
| --- | --- | --- | --- |
| `POST /v1/jobs` | 开发机网卡地址 | Agent | 提交作业，立即返回 `job_id`，状态为 `queued` |
| `GET /v1/jobs/{job_id}` | 开发机网卡地址 | Agent、`connectorhub` | 查询作业状态；`connectorhub` 以它作为定义中 `job.completed` 的状态查询 |
| 回调 `job.completed` | 不适用，由 `extsim` 发出 | `extsim` 发往 `connectorhub` 的 webhook 接收端 | 作业结束时发出 |
| 测试控制接口 | `127.0.0.1` | 开发机上的测试 | 设定作业耗时，以及回调的延迟、重复投递与丢弃 |

- 作业的结论由提交的内容决定：`extsim` 按固定规则校验内容，符合时结论为 `succeeded`，不符合时为 `failed` 并附原因。
- 回调的格式参照 GitHub webhook：带 HMAC-SHA256 签名头、投递标识头与事件类型，负载中有作业标识、项目、结论、原因与发生时间。收不到 2xx 时按退避重试，直到收到 2xx，或者测试控制接口把这次回调设定为丢弃。
- `extsim` 的外部系统定义中，关联键为 `job.project` 与 `job.id`，[关联键身份键](../../glossary.md)为 `job.id`，[项目归属键](../../glossary.md)为 `job.project`，`job.completed` 的[终态取值](../../glossary.md)为 `succeeded` 与 `failed`。
- 镜像内的 `extsim` 命令行像 `gh` 一样供 Agent 调用，提供 `job submit`、`job get` 与阻塞到作业结束的 `job watch`。`job watch` 让 Agent 可以不登记订阅，在回合中或后台等待，用于验证后台作业规则，以后也用于验证等待签名。

## 订阅与唤醒

- 平台 MCP 服务器在看守进程中运行，只提供 `list_events` 与 `subscribe_event`，把调用转发到控制面的[面向沙箱的端点](../../glossary.md) `connector`；请求以 `run_id` 标识运行实例，不带令牌。工具描述与 MCP 初始化应答中的 `instructions` 按设计说明等待方式的选择，平台不强制 Agent 使用订阅。
- 订阅登记按设计执行：在同一事务中写入订阅，并对[连接器事件日志](../../glossary.md)回溯匹配；没有命中时，按定义的状态查询核对作业，作业已经结束就直接生成订阅完成事件。
- webhook 接收端校验签名与投递标识，按项目归属键确定事件所属项目，把事件写入连接器事件日志并提交，再匹配活动订阅，之后才应答 200；同一投递标识再次到达时不重复写入，重新匹配后应答 200，匹配是幂等的。
- 订阅命中后，`connectorhub` 在同一事务中把订阅置为已完成，并写入一条待投递的完成事件投递记录；提交后经汇入契约的 `AppendInbox` 写入收件箱，成功后把记录置为已投递。控制面启动时与运行期间周期性地重投待投递的记录。收件箱以 `(run_id, dedup_key)` 去重，重复的回调与重投只留下一行。
- 写入收件箱之后的处理沿用 v0.1.0：运行实例已挂起时转为恢复中并发起恢复，挂起中时由挂起完成后的检查接手，消息在回合边界投递。订阅完成事件与人类消息在同一收件箱中按 `inbox_seq` 排序，可以出现在同一批投递中；看守进程把订阅完成事件写成一条用户消息，内容包括订阅标识、作业标识、结论与原因。
- 运行实例结束时，它的活动订阅置为已取消。

## 后台作业

v0.1.0 中，Claude Code 的后台任务会随快照一起停住。v0.2.0 中，看守进程按 Claude Code 的 `background_tasks_changed` 维护后台作业集合，不计 `ambient` 为真的任务，集合变化时以 `bg_jobs_changed` 上报；后台作业集合非空时，控制面不挂起运行实例。设计规定后台作业存活时只允许[静止冻结](../../glossary.md)，Substrate 不提供冻结，所以在 Substrate 上的做法是不挂起。后台任务照常运行，Agent 的功能不受限制，只是这段时间不释放计算资源。后台任务结束后 Claude Code 开始的[自发回合](../../glossary.md)，按 v0.1.0 的规则处理。

## 组成

| 组成部分 | 运行位置与内容 |
| --- | --- |
| 控制面进程 | 开发机上的普通进程，在 v0.1.0 的基础上加入 `connectorhub`：webhook 接收端只监听回环地址，`connector` 端点监听开发机网卡地址 |
| 模拟外部系统 | 开发机上的独立进程 `extsim` |
| PostgreSQL | 同 v0.1.0；`connectorhub` 的表与 `runcontroller` 的表在同一数据库中 |
| 命令行 | 同 v0.1.0 |
| actor 镜像 | 在 v0.1.0 的基础上加入 `extsim` 命令行 |

## 各模块的范围

未列出的模块与 v0.1.0 相同。

| 模块 | v0.2.0 新增 | v0.2.0 不做 |
| --- | --- | --- |
| `connectorhub` | 外部系统定义与一个项目的项目绑定配置的装载；webhook 接收与连接器事件日志；`once` 订阅的登记、回溯匹配与登记时的状态核对；事件目录；完成事件投递记录与经 `AppendInbox` 的写入；运行实例结束时取消订阅 | 状态式轮询、订阅对账扫描、`until_cancelled` 订阅、订阅超时、附件、出口策略与沙箱侧配置的生成和推送、等待签名、工具接入声明、多项目、核对的合并与公平调度、令牌校验 |
| `warden` | 平台 MCP 服务器的 `list_events` 与 `subscribe_event`；后台作业集合的维护 | `wait_for`、`sleep_until`、附件取回、等待意图与情境提示、自唤醒源镜像、孤儿进程判定 |
| `agentprotocol` | `AgentEvent` 增加 `bg_jobs_changed{job_ids}`；`Hello` 携带当前的后台作业集合 | `subscription_created` 等其余订阅相关事件 |
| `runcontroller` | 收件箱区分人类消息与订阅完成事件，以 `(run_id, dedup_key)` 去重；后台作业集合非空时不挂起；`active` 状态下 actor 崩溃时转为失败 | 唤醒谓词的一般形式、等待行、订阅在唤醒集合中的登记 |
| `sandboxprovider` | EgressPolicy 放行 `extsim` 与控制面的 `connector` 端点，规则改为按配置生成；actor 模板增加 `connector` 端点的地址，模板名改由模板内容派生 | 由 `connectorhub` 生成与推送出口策略 |

## 网络

EgressPolicy `default` 在 v0.1.0 的基础上变化如下：

| 规则 | `hostnames` | `ports` | 变化 |
| --- | --- | --- | --- |
| `http` | `castworks-dev.castworks-system.svc.cluster.local` | 9090、9091 | 增加 9091，即 `connector` 端点 |
| `http` | `extsim.castworks-system.svc.cluster.local` | 9100 | 新增，`extsim` 的作业接口 |

`extsim` 在集群中以单独的不带 selector 的 Service 出现，它的全限定名就是定义中 `access.upstreams` 列出的主机。`extsim` 的回调在开发机上经回环地址到达控制面，不经过集群。

## 安全

保留 v0.1.0 的三项，增加一项：webhook 接收端只监听回环地址，校验 HMAC-SHA256 签名与投递标识；签名密钥放在开发机上的本地文件中，不进代码仓库，控制面与 `extsim` 读取同一文件。

在 v0.1.0 的让步之外增加：

- `connector` 端点不认证，只凭请求中的 `run_id` 识别运行实例。能连到这个端口的任何进程，都能以任意运行实例的身份查询事件目录、登记订阅。
- `extsim` 的作业接口不鉴权，监听开发机网卡地址。

## 已知限制

v0.1.0 的已知限制仍然成立，其中两条缩小：actor 崩溃后运行实例仍无法恢复，但 `active` 状态下的崩溃会被发现并转为失败；关于后台任务的一条缩小为会话内定时（`CronCreate`、`ScheduleWakeup`）随快照停住，到点不会唤醒 actor。另有以下限制：

- 订阅登记之后回调丢失时，没有状态式轮询兜底，运行实例一直等到有人发来消息。
- 订阅没有超时，作业一直不结束时，运行实例一直处于挂起状态。
- Agent 在回合中阻塞等待时，例如在前台运行 `extsim job watch`，运行实例不挂起，这段时间不释放计算资源。
- `extsim` 重启后，作业与待重试的回调都会丢失。
- 只有一个项目，所有运行实例共用同一份项目绑定配置。

## 不在范围内

接入 GitHub 与其他真实外部系统，状态式轮询与订阅对账扫描，`until_cancelled` 订阅与订阅超时，附件，`wait_for` 与 `sleep_until`，等待签名、等待意图与情境提示，自唤醒源镜像，凭据经出口网关注入，以及 v0.1.0 列出的其余各项。

## 实现顺序

各模块按冻结的接口并行开发，各自以替身测试；在真实环境上的验收按编号顺序进行，前一步通过之后再做下一步：

1. `extsim`：服务、命令行、测试控制接口与它的外部系统定义，测试不依赖控制面。
2. `connectorhub`、`runcontroller` 与 `agentprotocol`：控制面进程接入 `connectorhub`，以真实的 `extsim`、假的沙箱提供者与假的看守进程，测试订阅登记、回调、重复投递、控制面重启、后台作业规则与 actor 崩溃的检测。
3. 看守进程的本地模式：在开发机上连接真实的控制面与 `extsim`，驱动真实的 Claude Code 完成提交作业、登记订阅、结束回合、收到订阅完成事件的完整循环，此时不挂起；同时录制含平台 MCP 调用与后台任务的 stream-json 样本。
4. Substrate：建立新增的 Service 与端口，actor 镜像加入 `extsim` 命令行，打开挂起扫描，跑通演示场景并逐条检查通过标准；崩溃检测一条以运行超出内存上限的进程等方式让 actor 崩溃来检查。

## 开发环境需求

在 v0.1.0 的基础上增加以下条件，都由 `deploy/dev/setup.sh` 建立：

- Service `castworks-dev` 与它的 EndpointSlice 增加端口 9091，供 `connector` 端点使用。
- 命名空间 `castworks-system` 中不带 selector 的 Service `extsim` 及其 EndpointSlice，指向开发机地址的 9100 端口。
- 开发机上的 webhook 签名密钥文件。

actor 经出口网关与不带 selector 的 Service 访问开发机的路径已在 v0.1.0 中验证。
