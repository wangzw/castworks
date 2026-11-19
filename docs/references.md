# 引用

项目文档引用的公开外部来源。

| 标识 | 名称 | 说明 | 来源 | 版本 |
| --- | --- | --- | --- | --- |
| REF-agent-sandbox | Agent Sandbox（kubernetes-sigs/agent-sandbox） | Kubernetes SIG Apps 的沙箱控制器与 CRD（Sandbox、SandboxClaim、SandboxWarmPool），覆盖沙箱生命周期、挂起与预热池 | <https://github.com/kubernetes-sigs/agent-sandbox> | v1.0.9 |
| REF-substrate | Substrate（agent-substrate/substrate） | 开源项目（Apache-2.0；仓库文档说明 CNCF 捐赠尚未完成），在 Kubernetes 上以 gVisor 或 microVM 运行可挂起的 actor，覆盖内存快照挂起与恢复、预热 worker、路由器唤醒、出口网关凭据占位符替换 | <https://github.com/agent-substrate/substrate> | v1.0.0 |
| REF-google-ax | AX（Agent Executor） | Google 开源的声明式 Agent 编排运行时，用 Task、Workspace、Model 资源与 Substrate actor 生命周期覆盖 Agent 的沙箱化执行、环境物化与挂起恢复 | <https://github.com/google/ax> | main@f0228f1 |
| REF-claude-code-headless | Claude Code 无头模式 | 用 `-p` 参数以编程方式运行 Claude Code 的官方文档，含 stream-json 输入输出与会话续接 | <https://code.claude.com/docs/en/headless> | 2.1.318 |
| REF-claude-code-hooks | Claude Code hooks 参考 | hooks 事件、输入输出与决策控制的官方参考，含 PreToolUse 的 defer、timeout 与 onFailure | <https://code.claude.com/docs/en/hooks> | 2.1.318 |
| REF-claude-code-settings | Claude Code 设置 | 设置文件作用域与优先级的官方文档，含托管策略设置 | <https://code.claude.com/docs/en/settings> | 2.1.318 |
| REF-claude-code-env-vars | Claude Code 环境变量 | Claude Code 读取的环境变量官方参考 | <https://code.claude.com/docs/en/env-vars> | 2.1.318 |
| REF-claude-code-network-config | Claude Code 网络配置 | Claude Code 的网络行为与流式空闲看门狗：首字节期限、事件级与字节级看门狗、看门狗中止后的处理 | <https://code.claude.com/docs/en/network-config> | 2.1.318 |
| REF-claude-agent-sdk-typescript | Agent SDK TypeScript 参考 | Agent SDK 与 stream-json 消息类型的官方参考，含 session_state_changed 与 result 的 queued_turn_count | <https://code.claude.com/docs/en/agent-sdk/typescript> | 2.1.318 |
| REF-mcp | Model Context Protocol 规范 | Agent 与工具服务器之间的开放协议规范：服务器、工具与传输 | <https://modelcontextprotocol.io/specification/2026-07-28> | 2026-07-28 |
| REF-linux-cgroup-v2 | Control Group v2（Linux 内核文档） | cgroup v2 的层级、接口文件与控制器，含 cgroup.freeze 冻结接口 | <https://docs.kernel.org/admin-guide/cgroup-v2.html> | Linux 5.2 |
| REF-linux-psi | PSI - Pressure Stall Information（Linux 内核文档） | 以任务在 CPU、内存与 IO 上的停滞时间度量资源压力的内核接口 | <https://docs.kernel.org/accounting/psi.html> | Linux 4.20 |
| REF-criu | CRIU（Checkpoint/Restore In Userspace） | Linux 上的进程检查点与恢复工具，把运行中进程的状态保存到磁盘，并在之后恢复 | <https://criu.org/> | — |
| REF-k8s-in-place-pod-resize | Resize CPU and Memory Resources assigned to Containers（Kubernetes 文档） | In-Place Pod Resize：无需重建 Pod 即可修改容器资源，含 resize 子资源、resizePolicy 与 Deferred 状态 | <https://kubernetes.io/docs/tasks/configure-pod-container/resize-container-resources/> | Kubernetes 1.35 |
| REF-k8s-service-accounts | Managing Service Accounts（Kubernetes 文档） | ServiceAccount 的管理文档，含绑定到 Pod 的投射 ServiceAccount 令牌（serviceAccountToken 投射源，kubelet 在到期前轮换，Pod 删除后失效）。文档另含 TokenReview 在 status.user.extra 中返回的 Pod 名与 Pod UID | <https://kubernetes.io/docs/reference/access-authn-authz/service-accounts-admin/> | Kubernetes 1.35 |
| REF-cluster-autoscaler-faq | Cluster Autoscaler FAQ | Kubernetes Cluster Autoscaler 的常见问题文档，含 Pod 注解 cluster-autoscaler.kubernetes.io/safe-to-evict | <https://github.com/kubernetes/autoscaler/blob/master/cluster-autoscaler/FAQ.md> | — |
| REF-otel-genai-semconv | OpenTelemetry GenAI semantic conventions | 生成式 AI 调用的追踪、指标与事件属性约定（gen_ai.*） | <https://github.com/open-telemetry/semantic-conventions-genai> | — |
| REF-anthropic-streaming | Streaming Messages（Claude API 文档） | Claude Messages API 的服务器发送事件流格式，含内容事件与 ping 保活事件 | <https://platform.claude.com/docs/en/build-with-claude/streaming> | anthropic-version 2023-06-01 |
| REF-spiffe | SPIFFE | 工作负载身份标准：SPIFFE ID 与可验证身份文档（SVID） | <https://spiffe.io/docs/latest/spiffe-about/overview/> | — |
| REF-rfc8785 | RFC 8785: JSON Canonicalization Scheme (JCS) | IETF 发布的 JSON 规范化方案：字符串转义、按 UTF-16 码元排序对象成员、去除空白与基于 ECMAScript 的数字序列化，供对 JSON 数据做哈希与签名 | <https://www.rfc-editor.org/rfc/rfc8785> | — |
| REF-envoy | Envoy Proxy 文档 | 云原生服务代理，提供 CONNECT 终止、内部监听器、`ext_authz`、`ext_proc` 与 dynamic forward proxy 过滤器 | <https://www.envoyproxy.io/docs/envoy/latest/> | — |
| REF-kagent | kagent（kagent-dev/kagent） | 开源的 Kubernetes Agent 平台，以 Harness 资源声明 Agent，可运行在 Agent Substrate 之上，覆盖会话、回合边界的挂起与恢复、出口凭据替换 | <https://github.com/kagent-dev/kagent> | v1.0.1 |
| REF-claude-managed-agents | Claude Managed Agents | Anthropic 的托管 Agent 服务文档，覆盖 Agent、环境、会话与事件，含会话状态、沙箱检查点、工具确认与自托管沙箱 | <https://platform.claude.com/docs/en/managed-agents/overview> | managed-agents-2026-04-01 |
| REF-bedrock-agentcore-runtime | Amazon Bedrock AgentCore Runtime | AWS 的 Agent 托管运行时文档，覆盖每会话 microVM、会话状态、空闲超时、`/ping` 健康状态与会话存储 | <https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/runtime-sessions.html> | — |
| REF-foundry-hosted-agents | Hosted agents in Foundry Agent Service | Microsoft Foundry 的托管 Agent 文档，覆盖每会话隔离的沙箱、会话与计算实例的生命周期、`$HOME` 持久化与空闲超时 | <https://learn.microsoft.com/en-us/azure/foundry/agents/concepts/hosted-agents> | — |
| REF-open-inspect | Open-Inspect（open-inspect/background-agents） | 开源的后台编码 Agent 系统，控制面运行在 Cloudflare，沙箱由可替换的后端提供 | <https://github.com/open-inspect/background-agents> | main |
| REF-cullenmcdermott-sandbox | sandbox（cullenmcdermott/sandbox） | 开源的命令行工具，基于 kubernetes-sigs/agent-sandbox 在远端集群运行编码 Agent，覆盖会话的挂起与恢复 | <https://github.com/cullenmcdermott/sandbox> | main |
| REF-e2b | E2B 文档 | 沙箱服务的官方文档，覆盖沙箱的暂停、自动暂停与自动恢复 | <https://docs.e2b.dev/sandbox/persistence> | — |
| REF-daytona | Daytona 文档 | 沙箱服务的官方文档，覆盖沙箱状态、自动停止与归档 | <https://www.daytona.io/docs/en/sandboxes> | — |
| REF-blaxel | Blaxel Sandboxes 文档 | 沙箱服务的官方文档，覆盖待机模式及其保留的状态 | <https://docs.blaxel.ai/Sandboxes/Overview> | — |
| REF-sprites | The Design & Implementation of Sprites | Fly.io 对 Sprites 的设计说明，覆盖虚拟机隔离、以对象存储为后端的持久存储、自动休眠与检查点 | <https://fly.io/blog/design-and-implementation/> | — |
| REF-aliyun-agent-sandbox | 阿里云 Agent Sandbox 概述 | 阿里云容器计算服务的 Agent Sandbox 产品文档，覆盖 MicroVM 隔离、内存状态的休眠与唤醒、Sandbox 自定义资源 | <https://help.aliyun.com/zh/cs/user-guide/agent-sandbox/> | — |
| REF-cube-sandbox | Cube Sandbox（TencentCloud/CubeSandbox） | 开源的沙箱服务，基于 RustVMM 与 KVM，覆盖 MicroVM 隔离、空闲暂停、请求唤醒与跨节点恢复 | <https://github.com/TencentCloud/CubeSandbox> | v0.8.0 |
| REF-opensandbox | OpenSandbox（opensandbox-group/OpenSandbox） | 开源的沙箱平台，以统一的沙箱 API 对接 Docker 与 Kubernetes 运行时，覆盖按后端区分的暂停与恢复 | <https://github.com/opensandbox-group/OpenSandbox> | release-1.2.1 |
| REF-cloudflare-sandbox | Cloudflare Sandbox SDK 文档 | 沙箱 SDK 的官方文档，覆盖沙箱的生命周期与休眠后的状态 | <https://developers.cloudflare.com/sandbox/concepts/sandboxes/> | 0.x |
| REF-sandbox-agent | Sandbox Agent（rivet-dev） | 开源的沙箱内服务进程与 SDK，以统一接口驱动多种编码 Agent，覆盖会话、权限请求、会话持久化与会话恢复 | <https://rivet.dev/sandbox-agent/docs/> | v0.5.6 |
