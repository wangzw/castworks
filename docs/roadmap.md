# 实施路线与技术验证

路线图位于设计规范管辖的目录之外，不受设计文档章节结构的约束。设计结论以 [架构](design/architecture.md) 与 [ADR](adr/README.md) 为准；路线图只记录实施前必须实测的外部行为、每项结果决定的参数或路径，以及阶段之间的技术门槛。验证结果改变参数或机制选择时，修改对应设计文档与 ADR，路线图不复制状态机、协议与参数定义。

技术验证清单中的「通过」表示可观察条件成立，「不通过」表示按对应回退路径处理。分阶段实施路线以技术门槛划分，不以时间划分；每个阶段的门槛由可观察结果表达，不以完成度或工作量表达。

## 技术验证清单

下表逐项列出实施前需要实测的事项。「影响」列给出该项结果所决定的参数或实现路径，「判定标准」列给出可通过观察区分通过与不通过的条件。

| 事项 | 影响 | 判定标准 |
| --- | --- | --- |
| Claude Code 被 `SIGSTOP` 30～600 s 后流式看门狗是否按墙钟计时；自定义 `ANTHROPIC_BASE_URL` 下字节级看门狗的取值（[REF-claude-code-network-config](references.md)） | `F_stream` 与 `F_cap(llm)`，决定直通模式与缓存模式下的冻结上限 | 通过——看门狗按墙钟推进，恢复后客户端行为与未冻结时一致；字节级取值可重复测量且不低于 `F_stream` 所需的安全余量。不通过——看门狗在冻结期间停止推进，或在恢复瞬间误判中止；此时按实测值下调 `F_stream`，直通模式在预测等待超过该值时不做冻结。 |
| `--permission-prompt-tool` 的入参与返回格式；`claude -p --resume <session_id>` 不带 prompt 时的 `defer` 续接行为（[REF-claude-code-hooks](references.md)） | 审批路径：`PreToolUse` 的 `defer`、定向投递与回合边界续接 | 通过——不带 prompt 的续接重新触发 `PreToolUse`，hook 能返回 `allow` 或 `deny` 并改变该次调用结果；入参与返回格式与文档一致。不通过——续接不重新触发 hook，或决定无法生效；此时审批改为 hook 阻塞加冻结，冻结上限取 `F_cap(approval)`。 |
| `--resume` 面对末尾悬空 `tool_use` 的行为；`CLAUDE_CODE_RESUME_INTERRUPTED_TURN=1` 的实际效果；`session_id` 在续接后是否变化（[REF-claude-code-headless](references.md)） | 降级路径：回合被中断后的重投与恢复句柄 | 通过——悬空 `tool_use` 的续接行为可区分，表现为拒绝或自动续接；被中断回合在该变量开启后自动续接；`session_id` 保持不变，若变化则能从通道观察并以新值更新恢复句柄。不通过——续接静默丢失回合或无法建立会话；此时被中断回合改走新会话加合成摘要的兜底路径。 |
| 关闭 stdin 后进程退出与会话记录刷盘的先后；`CLAUDE_CONFIG_DIR` 与 cwd 变化对续接的影响（无头模式文档） | 安全点中的工作区持久化确认 | 通过——进程退出时可观察到本回合记录已写入会话记录文件，或存在可等待的刷盘完成判据；cwd 逐字一致且 `CLAUDE_CONFIG_DIR` 位于工作区内时续接成功。不通过——退出后会话记录文件仍缺本回合记录且无判据；此时销毁前改为同步快照上传确认，或改由新会话加合成摘要承接会话状态。 |
| MCP 工具调用超时的默认值与可配置项；`SIGCONT` 后超时与已就绪响应的处理顺序；`wait_for` 阻塞调用在冻结 30、120、600 s 后的行为（[REF-mcp](references.md)） | `F_cap(approval/wait_for)` 与 `MCP_TOOL_TIMEOUT` | 通过——默认值与可配置项可确定；`SIGCONT` 后客户端先收到已就绪响应；冻结期间 `wait_for` 返回已转为订阅而非超时。不通过——冻结跨越超时导致调用被判失败；此时把 `F_cap` 下调到实测超时减 60 s，并让 `wait_for` 在冻结临近上限时返回已转为订阅的结果。 |
| 托管策略设置文件在 `-p` 无头模式下对 hooks 与权限设置的生效；`allowManagedHooksOnly` 对工作区内设置与 `CLAUDE_CONFIG_DIR` 用户设置的屏蔽（[REF-claude-code-settings](references.md)） | hooks 注入位置与安全边界 | 通过——托管设置中的 hooks 与权限设置在 `-p` 下生效，工作区 `.claude/settings.json` 与 `CLAUDE_CONFIG_DIR` 用户设置中的 hooks 被屏蔽且不可覆盖。不通过——托管设置不生效，或可被工作区覆盖；此时审批 hook 不得放在工作区内，需重新确定注入位置并复核安全边界。 |
| `-p` 下 `PostToolUse` 与 `PostToolBatch` hook 返回的 `additionalContext` 是否进入下一次模型请求，以及在请求中的位置（[REF-claude-code-hooks](references.md)） | 情境提示的载体；[ADR-wait-intent-as-soft-input](adr/adr-wait-intent-as-soft-input.md) 的失效条件 | 通过——本地代理观察到的下一次模型请求中含该 `additionalContext`，位于对应工具结果旁，且 hook 不引发额外的模型请求。不通过——该内容不进入模型请求，或只在回合结束之后出现；此时 `intent.hints` 保持关闭，等待意图停在只观测与自报，等待方式的选择规则仍由平台 MCP 服务器的 `instructions` 与工具描述承载。 |
| 平台 MCP 服务器初始化时提供的 `instructions` 在 `-p` 下是否进入系统提示（[REF-mcp](references.md)） | 等待方式选择规则的常驻载体 | 通过——本地代理观察到的模型请求的系统提示中含该 `instructions` 原文。不通过——不含；此时同一规则由托管 `SessionStart` hook 的 `additionalContext` 注入，工具描述照常承载等待方式的选择规则。 |
| `SubagentStart` hook 返回的 `additionalContext` 对子代理是否可见（[REF-claude-code-hooks](references.md)） | 子代理的等待方式契约 | 通过——本地代理观察到的子代理首个模型请求中含该 `additionalContext`。不通过——不含；此时不注册 `SubagentStart` hook，子代理只经平台 MCP 工具描述获得契约，子代理内的等待由机械层兜住。 |
| `-p` 下 `Stop` hook 输入的 `last_assistant_message` 是否可用（[REF-claude-code-hooks](references.md)） | 回合意图的规则分类，以及回合完成时的宽限期 `G_turn_grace_done` 与「回合完成」先验 | 通过——每个回合结束时，`Stop` hook 输入含该回合最后一条助手消息的完整文本。不通过——字段缺失或为空；此时驱动器从 stream-json 输出中该回合最后一条 assistant 消息取文本，仍取不到时回合意图恒为 `unknown`，宽限期与先验保持默认。 |
| 托管 hook 二进制同步返回的时延：从 Claude Code 启动 hook 到收到输出，含经本地 IPC 向驱动器查询、IPC 端点以 `SO_PEERCRED` 取得对端 pid 后对其可执行文件与父进程的校验，以及依据 stream-json 输出的一次性核对；Claude Code 执行 command hook 的方式：是否经 shell 执行，经 shell 时以 `exec <托管 hook 二进制路径>` 注册、直接执行时以二进制路径注册，能否使 hook 二进制的父进程为 Agent 主进程；hook 执行与对应事件写出 stdout 的先后：hook 启动时对应的 `tool_use`、批次结束、响应结束或子代理开始是否已经写出，或在 `T_hook_sync`（200 ms）之内写出；子代理的 `tool_use` 与响应是否出现在主会话的 stream-json 中（[REF-claude-code-hooks](references.md)） | 情境提示能否同步返回而不拉长工具结果的交付；托管 hook IPC 端点对端校验中的父进程条件；一次性核对能否接受全部真实查询，审批是否会因核对误拒而以「平台审批暂不可用」拒绝 | 通过——目标镜像中 p95 不超过 10 ms，并行工具批次下同样成立，情境提示所用 hook 失败时工具结果不变；按 hook 的执行方式选定的注册方式下，hook 二进制在 `/proc/<pid>/stat` 中的父进程号等于 Agent 主进程的 pid；全部托管 hook 事件的真实查询在 `T_hook_sync` 之内都能观察到对应事件，子代理事件出现在主会话的 stream-json 中，核对误拒为零。不通过——p95 超过 10 ms；此时驱动器在 `tool_started` 时预先算好该调用的提示并缓存，hook 只读缓存；仍超过时 `intent.hints` 保持关闭。hook 二进制的父进程不是 Agent 主进程时，父进程校验放宽为允许父进程是 Agent 主进程的直接子进程，一次性核对作为主要认证依据。对应事件在 `T_hook_sync` 之内仍未写出时，按实测的最大时差放大 `T_hook_sync`；子代理事件不出现在主会话 stream-json 中时，子代理上下文中的查询（`SubagentStart`、`SubagentStop` 与子代理内的工具调用）改以已观察到的子代理开始与结束窗口为依据，不按单个调用核对。 |
| CLI 以 stream-json 输出时 `background_tasks_changed` 与 `task_started`、`task_updated`、`task_notification` 消息的输出；workflow 内各 agent 是否作为独立任务出现；`ambient` 的取值（[REF-claude-agent-sdk-typescript](references.md)） | 后台作业登记，即 `bg_jobs_changed` 的来源 | 通过——CLI 的 stream-json 输出包含这些消息，`background_tasks_changed` 推送完整的存活任务集合；workflow 内的 agent 各自作为任务出现，或其存活期被 workflow 任务覆盖；`ambient` 的取值与任务种类的对应关系可重复观察。不通过——CLI 不输出这些消息，或 workflow 内 agent 的存活不反映在任务集合中；此时以 Stop 与 SubagentStop hook 输入的 `background_tasks` 在每个回合结束时对账，仍无法覆盖时不设置 `CLAUDE_CODE_BG_TASKS_REPORT_RUNNING=0`，改由其默认行为判定后台子代理与 workflow 的存活（二者存活时回合结束后仍报 running）；`ambient` 的取值无法判定时 `ambient` 任务一律计入后台作业。 |
| `--resume` 对 `CronCreate` 与 `ScheduleWakeup` 作业的恢复；错过触发时刻后是否补跑；Stop hook 输入的 `session_crons` 是否包含 `ScheduleWakeup` 安排的唤醒（[REF-claude-agent-sdk-typescript](references.md)） | 自唤醒条目的 `survives_resume` 与 `L_self_wake` | 通过——`--resume` 后未过期的 `CronCreate` 作业按原计划触发；错过触发时刻时补跑一次或跳过，行为可重复观察；`session_crons` 是否包含 `ScheduleWakeup` 可确定。不通过——`--resume` 不恢复 `CronCreate` 作业，或恢复行为不可重复；此时 `cron` 条目的 `survives_resume` 取假，存在自唤醒条目时不以挂起为目的销毁，`L_self_wake` 只取冻结与快照两档；`session_crons` 不包含 `ScheduleWakeup` 时，`wakeup` 条目只按工具调用维护，不参与 `session_crons` 对账。 |
| `--resume` 之后此前的后台任务（后台 shell、后台子代理、workflow）与 `Monitor` 的去留，以及续接后收到的 `task_notification`（[REF-claude-agent-sdk-typescript](references.md)） | I9 中「无后台作业」与「自唤醒条目都随续接恢复」的判定，以及「回合被中断」通知的内容 | 通过——续接后此前的后台任务与 `Monitor` 都已终止，续接后的后台任务集合从空集起算；此前任务的结局以可识别的方式呈现（带终止状态的 `task_notification`，或会话记录中的中断标记），且不重复通知已在进程退出前结束的任务。不通过——续接后旧任务仍出现在任务集合中而没有对应的执行体，或旧任务的结局不可识别；此时驱动器把续接前已存在的任务标识排除出后台作业，并在「回合被中断」通知中列出中断时存活的后台任务。 |
| 非交互模式（`-p`）下长 MCP 调用自动转后台的消息序列：占位 `tool_result` 的内容、`task_notification.tool_use_id` 与真实结果的到达方式；超时的 shell 命令自动转后台的同类序列（[REF-claude-agent-sdk-typescript](references.md)、[REF-claude-code-env-vars](references.md)） | 平台 MCP 阻塞型 `wait_for` 与 `sleep_until` 被转后台时的转换；第三方 MCP 调用转后台后的后台作业登记 | 通过——非交互模式下会自动转后台，占位 `tool_result` 可识别，`task_notification` 携带原调用的 `tool_use_id`，并在真实结果到达时发出。不通过——非交互模式下不自动转后台，或占位与通知无法关联到原调用；不自动转后台时平台 MCP 阻塞调用沿用临近冻结上限时的转换语义，不需另行处理；无法关联时驱动器把未关联的 MCP 后台任务按 `task_id` 计入后台作业，平台 MCP 服务器在调用时长达到自动转后台阈值之前主动返回转换结果。 |
| 非特权 Pod 的 `/sys/fs/cgroup` 挂载属性与 PSI 文件可读性；节点冻结代理对 50 个进程的进程树的冻结与解冻时延；`SIGSTOP` 降级路径的收敛轮数（[REF-linux-cgroup-v2](references.md)、[REF-linux-psi](references.md)） | `Freeze{cgroup}` 的实现与 L0 活动度采集 | 通过——PSI 文件可读；该进程树可被冻结与解冻，解冻后满足决策完成到可接收 p95 < 100 ms 与事件到达至可接收 p95 < 200 ms；`SIGSTOP` 降级在有限轮内收敛且不残留 T 态进程。不通过——cgroup 不可写或时延超出目标；此时 k8s 提供者只保留 `Freeze{signal}` 短冻结，或把冻结路径移出该环境。 |
| 非特权沙箱中 `/proc/<pid>/wchan`、`/proc/<pid>/stat`、`/proc/net/tcp` 与 `/proc/net/tcp6` 的可读性；`wchan` 的符号名是否可见 | 静止证据中定时睡眠的识别（`timed_sleepers`）；入站数据唤醒中冻结确认时的接收队列检查，含两端都在 Agent 进程树内的回环连接的排除 | 通过——看守进程能读取 Agent 进程的 `wchan` 并得到 `nanosleep`、`hrtimer` 一类符号名，能读取 `starttime`，能读取 `rx_queue` 并按套接字 inode 关联到 Agent 进程，能按回环连接的对端地址找到对端套接字并按其 inode 判定持有者是否属于 Agent 进程树。不通过——任一文件不可读，或 `wchan` 只返回 0；此时定时睡眠一律视为忙，冻结确认时不做接收队列检查，改为解冻后短暂观察。 |
| PSI 的 `cpu.pressure` 与 `io.pressure` 中 `some` 行 `total` 累计值的更新粒度，及其与活动度采样间隔、`Q_quiet` 的匹配（[REF-linux-psi](references.md)）；Claude Code 空闲（回合边界、无后台作业）时 `agent` 子 cgroup 的 `cpu.stat` 中 `usage_usec` 的增长基线，及其与 `U_quiet` 的匹配（[REF-linux-cgroup-v2](references.md)） | 静止证据中的 PSI 静默判定与 CPU 用量判据，`Q_quiet` 与 `U_quiet` 的取值 | 通过——Agent 进程树静止期间相邻样本差为零；Agent 进程树出现 CPU 或 I/O 停滞时，差值在一个采样间隔内变为非零；`total` 的更新粒度不粗于采样间隔；Claude Code 空闲期间任一静止窗内 `usage_usec` 的增量不超过 `U_quiet` 乘以窗口时长。不通过——静止期间出现非零差值，或停滞发生后差值滞后多个采样间隔才出现；此时把采样间隔上调到实测更新粒度以上，按实测滞后上调 `Q_quiet`；静止期间的非零差值来自可识别的固定噪声时，PSI 静默判定改为样本差不超过实测噪声上限。Claude Code 空闲期间静止窗内 `usage_usec` 的增量超过 `U_quiet` 乘以窗口时长时，按实测基线上调 `U_quiet`，或静止证据改以连续多个窗口判定 CPU 用量。 |
| 原地缩小 CPU request 的生效时延；节点 CPU 分配 80% 时放大进入 `Deferred` 的比例与持续时间；agent-sandbox 控制器是否回滚 Pod 的 `resources`（[REF-k8s-in-place-pod-resize](references.md)） | `InPlaceResize` 能力，以及冻结是否真正释放调度额度 | 通过——缩小立即生效且不触发重建；节点 CPU 分配 80% 时 `Deferred` 的比例与持续时间可度量并落在设计可接受范围；控制器不回滚 `resources`。不通过——控制器回滚，或缩小不释放调度额度；此时从能力集中移除 `InPlaceResize`，冻结只释放用量、不释放 request。 |
| `claude -p --resume` 冷启动分布：会话记录文件为 1、10、50 MB，CPU request 为 250m 与 2 核，二进制在镜像内与共享文件系统上，外联黑洞与 RST，MCP 初始化耗时（无头模式文档） | 销毁重建 p95 < 10 s 与 p95 < 20 s 的目标、`T_nofreeze` 与预热池容量 | 通过——预热池认领 p95 < 10 s，共享文件系统冷启动 p95 < 20 s，分布可按会话记录文件大小与 CPU 规格解释。不通过——启动期外联或 MCP 初始化使 p95 超出目标；此时把对应耗时计入 `T_nofreeze` 与抖动预算，并按实测补充前置时间重算预热池容量。 |
| agent-sandbox 认领时延（单个与 100 并发）与池补充前置时间 L；共享文件系统的 `mkdir`、`chown`、`fsync` 时延与 `root_squash` 行为；块存储 `attach` 在 50 与 200 并发下的时延（[REF-agent-sandbox](references.md)） | 预热池容量公式、冷启动目标与工作区隔离实现 | 通过——100 并发认领满足 p95 < 10 s，池补充前置时间 L 可测；共享文件系统的 `mkdir`、`chown`、`fsync` 支持 I9 的持久化确认，`root_squash` 行为与工作区设计一致；块存储 `attach` 时延与目标一致。不通过——并发认领显著劣化，或 `root_squash` 阻止目录创建；此时池按分片扩容，或改由看守进程 `seteuid` 后创建目录。 |
| Substrate 试点：看守进程作 PID 1 在 gVisor 下驱动 Claude Code；约 500 MB actor 的 Suspend 与 Resume 时延与快照大小；经路由器 `/wake` 唤醒；EgressPolicy 占位符注入；S3 插件；`parked-request-budget`；编译测试类负载；MEMORY 快照恢复后的时钟与旧连接；外部 CSI 卷；`durableDir` 退化配置；经出口网关注入 actor JWT 的[引导接口](glossary.md)领取，以及从[模板快照](glossary.md)恢复的 actor 以 `actor-name` 变化触发领取（[REF-substrate](references.md)） | Substrate 提供者的 go 与 no-go | 通过——阶段 0 的 go 判定全部满足。不通过——任一阻断项无法成立且无补偿路径；此时第一版只交付 k8s 提供者，Substrate 提供者顺延。 |

## 当前验证结论

验证环境为 macOS arm64、Docker Desktop Linux 容器（cgroup v2）、Kubernetes 1.36.4 与 Claude Code 2.1.318；没有在目标 Linux/k8s 集群或 Substrate 上完成端到端实测。下表的「通过」表示机制、官方文档与本机实验相互印证，不是目标环境验收。

| 事项 | 结论 | 已确立 | 仍需目标环境实测 |
| --- | --- | --- | --- |
| 看门狗 | 通过（机制） | 看门狗按单调时钟计时；30 s、120 s 冻结后正常续流；320 s 冻结后在解冻后约 0.35 s 触发中止并重试；自定义 `ANTHROPIC_BASE_URL` 下事件级与字节级默认 300 s。`F_stream=120 s` 安全 | 600 s 端点、cgroup freezer、Substrate VM 挂起、真实网关保活 |
| `--permission-prompt-tool` 与 defer | 通过 | 参数仍存在但被 `--help` 隐藏；入参 `{tool_name, input, tool_use_id}`，返回单文本块内的 `PermissionResult`；`allow`、`deny`、`updatedInput` 均生效；`defer` 只在 `-p` 单工具回合生效；不带 prompt 的 `--resume` 以同一 `tool_use_id` 再次触发 `PreToolUse` | 并行批次忽略 defer、`tool_deferred_unavailable`、Linux 镜像 |
| `--resume` 与悬空工具调用 | 部分证实 | 空提示续接对非 defer 悬空 `tool_use` 直接拒绝；`session_id` 默认不变，`--fork-session` 才换新 ID；带提示续接保留被切断调用并合成 `is_error` 的 interrupted `tool_result`，模型可能重试 | `CLAUDE_CODE_RESUME_INTERRUPTED_TURN=1` 的运行级自动续接、`MAX_AGE`、工具是否重跑 |
| 关闭 stdin 与 cwd | 部分证实 | JSONL 在进程退出前已写入，退出后立即可读；`--resume` 跨 cwd 可用，`--continue` 以 cwd 为作用域 | 非 `--bare` 命令、`tool_use`/`tool_result` 回合、Linux 共享文件系统 fsync |
| MCP 工具超时 | **不通过** | `MCP_TOOL_TIMEOUT` 默认约 28 小时，但 HTTP 首字节 60 s、HTTP idle 300 s、stdio idle 1800 s 是更紧的约束；冻结跨越任一窗口时，恢复后超时先于已就绪响应生效；实测 stdio 冻结 12 s、超时 5 s 时 3/3 判超时 | cgroup 同时冻结、审批路径、`wait_for` 实现后复测、HTTP idle 跨 300 s |
| 托管策略设置 | 通过（限定） | Linux 容器内 `-p` 下 managed settings 生效且优先级最高；`allowManagedHooksOnly` 只允许托管来源，屏蔽用户、项目、本地 hooks，更低层级不能覆盖 | PreToolUse 用例、`settings.local.json`、`CLAUDE_CONFIG_DIR` 用户 hooks、目标镜像只读根 |
| cgroup、PSI 与 50 进程冻结 | 部分证实 | PSI 可读；`agent` 子 cgroup 可冻结/解冻；树形实测冻结标志 1.805 ms、解冻标志 2.923 ms（单次采样）；SIGSTOP 3～4 轮收敛；冻结完成须读 `cgroup.events`，进程态仍为 S；`throttled_usec` 须读容器父 cgroup | 目标节点端到端 p95、非特权 Pod 挂载、真实多线程 Agent |
| 原地缩放与 Deferred | 部分证实 | 缩小 CPU request 约 0.1～0.27 s 生效，不重启、不重建，并释放调度额度；agent-sandbox 主线控制器不回滚 resources；容量边界处进入 Deferred | 恰好 80% 分配的统计、Deferred 持续 10 min 以上、缩放不跨 Pod 重建保留 |
| 冷启动 | 部分证实 | 2 核 + 50 MB 约 8.0 s，250m + 50 MB 约 14.4 s；外联黑洞与 RST 都约 5.15 s；MCP 初始化最长可等满 30 s | 目标集群 p95、真实会话夹具、共享文件系统、amd64 |
| 预热池与工作区时延 | 部分证实 | agent-sandbox 文档给出 burst 约 300 claims/s、p90 ≤ 200 ms、持续每池 70～85/s、Pod 冷启动约 42～50 s p50；`root_squash` 将 uid 0 映射为 65534 | 单次与 100 并发认领 p95、池补充 L、RWX `mkdir/chown/fsync`、50/200 并发 attach |
| Substrate 试点 | 阻断 | PID 1、MEMORY 快照、`/wake`、`parked-request-budget`、EgressPolicy、S3 插件与 `durableDir` 在文档与源码中存在 | 500 MB Suspend/Resume、外部 CSI、gVisor 下无 Docker、旧 TCP 连接、actor JWT 注入的引导领取、go/no-go |

结论对设计的修改已写入 [suspendpolicy](design/modules/design-suspendpolicy.md) 的 MCP 传输上限与 `F_cap`、[Claude Code 驱动器](design/modules/warden/design-warden-driver-claude-code.md) 的续接与 MCP 超时、[warden](design/modules/warden/design-warden.md) 的 per-cgroup PSI 与 `cgroup.events`、[sandboxprovider k8s](design/modules/sandboxprovider/design-sandboxprovider-k8s.md) 的缩放不跨 Pod 重建保留与 [sandboxprovider substrate](design/modules/sandboxprovider/design-sandboxprovider-substrate.md) 的试点前提。

## 分阶段实施路线

阶段以技术门槛划分。每阶段的「交付物」是该阶段结束时必须存在且可观察的能力，「进入下一阶段的条件」是下一阶段开始前必须成立的观察结果。

### 阶段 0：底座能力验证

交付物：

- k8s 冻结路径：节点冻结代理与 Agent 子 cgroup、PSI、CPU 用量与进程态采集、原地缩小 CPU request、agent-sandbox 认领，以及共享文件系统上的工作区操作。
- Substrate 试点：看守进程作 PID 1、Suspend 与 Resume、路由器唤醒、出口策略、引导领取、工作区持久化与恢复后状态清理。

Substrate 试点的 go 判定如下，全部满足为 go：

| 判定项 | 可观察条件 |
| --- | --- |
| 驱动 Claude Code | 看守进程以 PID 1 在 gVisor 下驱动 Claude Code 完成一个回合：启动、LLM 请求、工具调用与回合结束均可观察，进程树受看守进程监督 |
| 快照恢复 | 约 500 MB actor 的 Suspend 与 Resume 时延与快照大小可重复测量；快照恢复 p95 < 5 s，或偏差可归因于已知项并触发目标调整 |
| 入站唤醒 | 经带 `ate-target-actor` 头的路由器调用 `/wake` 成功唤醒；控制面以 SPIFFE 身份（[REF-spiffe](references.md)）通过白名单；调大 `parked-request-budget` 后停泊请求不被提前切断 |
| 出口策略 | EgressPolicy 对 `api.github.com` 与 git 的占位符注入生效，缺头不注入，未列主机默认拒绝；S3 插件可用 |
| 引导领取 | 出口网关为看守进程发往引导接口、携带 `Authorization` 占位符的请求注入 audience 为 `castworks-bootstrap` 的 actor JWT，引导接口经 Substrate 签发方的 OIDC 发现验签、核对 actor 名与 actor UID 后返回运行实例令牌与 `lease_id`；从模板快照恢复的新 actor 在 `wakeupProbe` 探测时读到新的 `actor-name` 并立即领取，在 `ResumeActor` 成功之后的首次退避重试内领取成功；同一 actor 内的再次领取返回 409 |
| 工作区持久化 | 外部 CSI 卷（NFS CSI 驱动）在 actor 内可挂载、可读写，且沙箱外可经同一导出访问；不可用时 `durableDir` 同步挂起的耗时与崩溃丢失窗口可测并落在可接受范围 |
| 恢复后状态 | MEMORY 快照恢复后客户机时钟可校正，旧 TCP 连接可被察觉并断开，本地代理按 `request_id` 取回在途请求的已完成响应 |
| 负载兼容 | 编译测试类负载在 gVisor 下的开销与无 Docker 的影响可度量，不阻断 Claude Code 驱动 |

任一判定项不成立且无补偿路径为 no-go。no-go 时第一版只交付 k8s 提供者，Substrate 提供者顺延；能力分层与策略只在能力集内选择机制的设计不变（[ADR-capability-tiered-suspension](adr/adr-capability-tiered-suspension.md)）。

进入阶段 1 的条件：

- Substrate 的 go 或 no-go 结论已确定，并反映为第一版交付的提供者范围。
- k8s 冻结路径的 cgroup 挂载属性与 PSI 可读性、原地缩放、认领与工作区时延均得到通过结论或明确的回退路径。
- Claude Code 的冻结与续接行为、MCP 超时、托管设置生效均得到通过结论或明确的回退路径：MCP 超时一项已判定不通过，回退路径为按 MCP 传输类型重算 `F_cap`，并让 `wait_for` 在冻结临近上限时返回已转为订阅的结果；`F_stream`、`F_cap` 与 `T_nofreeze` 的取值已确定或已触发设计更新。

### 阶段 1：最小挂起恢复闭环

交付物：

- 控制面的收件箱、唤醒信号、等待、代与失效判定、投递与两级确认、效果记录与纠正性对账器的最小实现。
- 两级 LLM 代理：看守进程内的本地代理与 LLM 网关，含直通与缓存两种模式、单次回放缓存、令牌轮换与代校验。
- 看守进程与 Claude Code 驱动器：PID 1 监督、活动度采集、工作区租约续约、平台 MCP 服务器与投递意图日志。
- k8s 提供者与 Substrate 提供者的 `Destroy` 与 `Resume` 路径。
- GitHub 连接器：外部系统定义、webhook 落盘与订阅回溯匹配。
- 聊天 UI 通道：人类消息与审批决定写入收件箱，在线信号可用。

进入阶段 2 的条件：

- 等 LLM、等 PR 门禁、审批 `defer`、等人并销毁重建在真实集群走通，行为与协议场景一致。
- 事件先于订阅登记不丢失；挂起与唤醒并发不丢唤醒；旧代消息被拒绝；控制面重启后过渡态由对账器收敛。
- 沙箱内无长期凭据，投递去重跨代有效，以挂起为目的的销毁只在安全点执行。

### 阶段 2：全部挂起机制与调度效率

交付物：

- Substrate 快照挂起（go 判定成立时）。
- k8s 冻结、原地缩小 CPU request 与节点冻结代理的完整路径（k8s 验证通过时）。
- 预热池。
- 连接器的状态式轮询。
- 恢复准入与优先级。

进入阶段 3 的条件：

- 解冻满足决策完成到可接收 p95 < 100 ms、事件到达至可接收 p95 < 200 ms；快照恢复满足同节点本地且无在途请求 p95 < 2 s、通用 p95 < 5 s；预热池认领 p95 < 10 s；冷启动满足共享文件系统 p95 < 20 s、块存储 p95 < 60 s。
- 冻结并原地缩小后，节点调度额度可观察地释放，表现为 Pod 的 CPU request 与节点可分配量同向变化。
- 预热池缺池率与按节点的冻结态内存比例在配置阈值内；恢复准入按优先级放行，低优先级不阻塞实时接入与异步人类回复。
- webhook 丢失或乱序时，状态式轮询产生同一订阅完成事件，订阅对账差异收敛。

### 阶段 3：预测与通用接入

交付物：

- 预测性恢复：首 token 与「正在输入」信号触发提前恢复，首 token 到达后按已等待时长重估剩余等待。
- 通用 HTTP 加轮询型外部系统接入。
- 冻结态内存压力下的升级梯。

进入阶段 4 的条件：

- 预测直方图按模型、thinking 预算、`max_tokens` 与输入 token 分桶，置信度与实测偏差可度量；预测性恢复对每个 `request_id` 只执行一次；抖动预算耗尽次数在阈值内。
- 通用连接器以声明式定义接入一个非 GitHub 系统，订阅、回溯匹配、去重与状态式轮询的行为与 GitHub 连接器一致。
- 节点冻结态内存超过 `ρ_soft` 触发升级，超过 `ρ_hard` 打污点并使该节点上新的等待按 `grace = 0` 直接销毁。

### 阶段 4：原生运行时与多租户维度

交付物：

- 控制面循环驱动器：经 `ToolExec` 使用沙箱，向runcontroller发出与进程驱动器相同的事件序列。
- 多租户配额与优先级：`tenant` 维度从保留字段变为执行约束。

阶段完成的观察结果：

- 控制面循环驱动器在不修改协议的前提下，在回合语义、挂起机制与投递确认上与进程驱动器一致。
- 多租户配额与优先级对单组织默认配置无可观察影响，跨租户抢占按配置生效。

### 等待意图的分步落地

等待意图按下列意图阶段逐步启用。前提是阶段 1 的 Claude Code 驱动器、平台 MCP 服务器与 GitHub 连接器已交付；意图阶段与阶段 2～4 并行，不改变这些阶段的门槛。进入下一意图阶段的条件以下列指标表达：

- 可转订阅的前台等待占比：分类为 `waiting` 且命中等待签名的前台工具调用时长，在全部前台工具调用时长中的占比，按外部系统与签名分组。
- 自报误差：Agent 自报的 `agent_expected_ms` 与实际等待时长的相对误差分布，以及实际等待落在自报值 ±50% 以内的命中率。
- 提示转化率：被采纳的情境提示在全部情境提示中的比例；订阅建议之后 `N_adopt` 个回合内出现同一关联对象或同一 `event_type` 的 `subscribe_event`，纯定时建议之后 `N_adopt` 个回合内出现 `sleep_until`，计为采纳。
- 挂起收益：情境提示开启组与关闭组之间 `idle_while_scheduled_ratio`、回合边界销毁占比与冻结总时长的差异。
- 误提示率：情境提示所依据的签名匹配经抽样复核并非该签名对应等待的比例。

#### 意图阶段 A：只观测

交付物：

- Claude Code 驱动器的意图解析：Bash、Agent、Workflow 与 `Monitor` 工具的输入，`Stop` hook 的 `last_assistant_message`，本地代理观察到的对同一主机的周期性连接。
- `connectorhub` 装载并校验外部系统定义中的等待签名，随沙箱侧配置下发；驱动器按命令签名匹配。
- `intent_observed` 上报与 `run.intents`；意图分类分布与签名命中的指标。
- `intent.hints` 关闭，意图不进入等待时长预测与宽限期选择，Agent 看不到任何意图相关内容。

进入意图阶段 B 的条件：

- 真实负载上，可转订阅的前台等待占比可按外部系统与签名度量；占比为零的外部系统不进入情境提示。
- 误提示率可按抽样复核度量，并低于为进入情境提示设定的阈值。
- 同一负载下，单一条件门控、静止证据与 I9 前置检查的判定结果与关闭意图解析时一致。

#### 意图阶段 B：契约与自报

交付物：

- 平台 MCP 服务器的 `instructions` 与 `SubagentStart` 契约注入，载体按技术验证清单的结论选定。
- `subscribe_event`、`wait_for` 与 `sleep_until` 的可选参数 `expected_wait` 与 `reason`，`agent_expected_ms` 的上报。
- 意图接入 `suspendpolicy`：等待时长预测纳入 Agent 自报与签名先验，`policy_state` 记录自报的历史命中情况用于置信度校准；回合意图为 `completed` 时宽限期取 `G_turn_grace_done`，等待预测取「回合完成」先验行。

进入意图阶段 C 的条件：

- 自报误差按运行实例与等待种类可度量；纳入自报与签名先验之后，实际等待落在预测区间 `[p25, p90]` 内的比例不低于只用历史直方图时。
- 回合意图为 `completed` 而取 `G_turn_grace_done` 的回合边界中，宽限期满后销毁、又在 `G_turn_grace` 以内被下一条输入唤醒的比例不高于设定阈值。
- 技术验证清单中 `PostToolUse` 与 `PostToolBatch` 的 `additionalContext` 一项通过；不通过时意图层停在本阶段。

#### 意图阶段 C：情境提示

交付物：

- `PostToolUse` 与 `PostToolBatch` 的情境提示：托管 hook 二进制经本地 IPC 向驱动器查询并同步返回；分类为 `waiting` 且命中签名的后台 `local_bash` 在下一次 `PostToolUse` 提示一次。
- 频率控制与采纳判定：同一签名在每个运行实例首次命中时提示，未采纳时按 2^k 个回合退避，连续两次未采纳后本运行实例停止提示该签名；订阅建议之后 `N_adopt` 个回合内出现同一关联对象或同一 `event_type` 的 `subscribe_event`，纯定时建议之后 `N_adopt` 个回合内出现 `sleep_until`，计为采纳；纯定时等待只在定时总长不短于 `T_hint_timer` 时提示。
- 提示开与关的 A/B 对比：按运行实例随机分组，`intent.hints` 只对实验组开启。

进入意图阶段 D 的条件：

- 提示转化率与挂起收益可度量；实验组相对对照组的 `idle_while_scheduled_ratio` 下降，回合边界销毁占比上升，误提示率不高于阈值。转化率长期过低或挂起收益没有改善时，按 [ADR-wait-intent-as-soft-input](adr/adr-wait-intent-as-soft-input.md) 的失效条件处理，`intent.hints` 保持关闭。
- 规则与签名判为 `unknown` 的前台与后台调用时长占比可度量，其中经抽样复核为等待的部分足以抵偿分类请求的成本。

#### 意图阶段 D：宿主侧分类与出口流量形态识别（可选）

交付物：

- 宿主侧分类模型，开关为 `intent.classifier`，默认关闭：规则判不了的命令、描述与最后一条消息由看守进程经本地代理发往 `llmgateway` 分类，不经 Agent 进程，不进入 Agent 上下文，按运行实例计费。
- 出口流量形态识别：等待签名的 `request{host, method, path_regex}` 匹配，依赖出口代理对请求方法与路径的可见性；出口代理只在终止 TLS 的主机上看得到路径，以 `tls_passthrough` 透传的主机不启用路径级签名。

阶段完成的观察结果：

- 分类结果经抽样复核的准确率与其置信度 0.6 相符；开启分类后可转订阅的前台等待占比上升，误提示率不高于阈值；分类请求的成本可按运行实例审计。
- 路径级签名只在出口代理可见路径的主机上生效，命令签名的匹配不受影响。

## 与设计文档的边界

路线图不定义状态机、协议与参数取值；这些内容以 [架构](design/architecture.md)、[ADR](adr/README.md) 与各模块设计文档为准。技术验证清单列出的，是设计已经依赖但尚未经实测确认的外部行为；验证结果改变设计时，以设计文档与 ADR 的更新为准，路线图只保留可观察条件与阶段门槛。
