# 引用

项目文档引用的公开外部来源。

| 标识 | 名称 | 说明 | 来源 | 版本 |
| --- | --- | --- | --- | --- |
| REF-agent-sandbox | Agent Sandbox（kubernetes-sigs/agent-sandbox） | Kubernetes SIG Apps 的沙箱控制器与 CRD（Sandbox、SandboxClaim、SandboxWarmPool），覆盖沙箱生命周期、挂起与预热池 | <https://github.com/kubernetes-sigs/agent-sandbox> | v1.0.9 |
| REF-substrate | Substrate（agent-substrate/substrate） | CNCF Sandbox 项目，在 Kubernetes 上以 gVisor 或 microVM 运行可挂起的 actor，覆盖内存快照挂起与恢复、预热 worker、路由器唤醒、出口网关凭据占位符替换 | <https://github.com/agent-substrate/substrate> | v1.0.0-rc.1 |
| REF-google-ax | AX（Agent Executor） | Google 开源的声明式 Agent 编排运行时，以 Task、Workspace、Model 资源与 Substrate actor 生命周期覆盖 Agent 的沙箱化执行、环境物化与挂起恢复 | <https://github.com/google/ax> | main@f0228f1 |
| REF-claude-code-headless | Claude Code 无头模式 | 以 `-p` 编程运行 Claude Code 的官方文档，含 stream-json 输入输出与会话续接 | <https://code.claude.com/docs/en/headless> | 2.1.318 |
| REF-claude-code-hooks | Claude Code hooks 参考 | hooks 事件、输入输出与决策控制的官方参考，含 PreToolUse 的 defer、timeout 与 onFailure | <https://code.claude.com/docs/en/hooks> | 2.1.318 |
| REF-claude-code-settings | Claude Code 设置 | 设置文件作用域与优先级的官方文档，含托管策略设置 | <https://code.claude.com/docs/en/settings> | 2.1.318 |
| REF-claude-code-env-vars | Claude Code 环境变量 | Claude Code 读取的环境变量的官方参考 | <https://code.claude.com/docs/en/env-vars> | 2.1.318 |
| REF-claude-code-network-config | Claude Code 网络配置 | Claude Code 的网络行为与流式空闲看门狗：首字节期限、事件级与字节级看门狗、看门狗中止后的处理 | <https://code.claude.com/docs/en/network-config> | 2.1.318 |
| REF-claude-agent-sdk-typescript | Agent SDK TypeScript 参考 | Agent SDK 与 stream-json 消息类型的官方参考，含 session_state_changed 与 result 的 queued_turn_count | <https://code.claude.com/docs/en/agent-sdk/typescript> | 2.1.318 |
| REF-mcp | Model Context Protocol 规范 | Agent 与工具服务器之间的开放协议规范：服务器、工具与传输 | <https://modelcontextprotocol.io/specification/2026-07-28> | 2026-07-28 |
| REF-linux-cgroup-v2 | Control Group v2（Linux 内核文档） | cgroup v2 的层级、接口文件与控制器，含 cgroup.freeze 冻结接口 | <https://docs.kernel.org/admin-guide/cgroup-v2.html> | Linux 5.2 |
| REF-linux-psi | PSI - Pressure Stall Information（Linux 内核文档） | 以任务在 CPU、内存与 IO 上的停滞时间度量资源压力的内核接口 | <https://docs.kernel.org/accounting/psi.html> | Linux 4.20 |
| REF-criu | CRIU（Checkpoint/Restore In Userspace） | Linux 上的进程检查点与恢复工具，把运行中进程的状态保存到磁盘并在之后恢复 | <https://criu.org/> | — |
| REF-k8s-in-place-pod-resize | Resize CPU and Memory Resources assigned to Containers（Kubernetes 文档） | In-Place Pod Resize：无需重建 Pod 即可修改容器资源，含 resize 子资源、resizePolicy 与 Deferred 状态 | <https://kubernetes.io/docs/tasks/configure-pod-container/resize-container-resources/> | Kubernetes 1.35 |
| REF-k8s-service-accounts | Managing Service Accounts（Kubernetes 文档） | ServiceAccount 的管理文档，含绑定到 Pod 的投射 ServiceAccount 令牌（serviceAccountToken 投射源，kubelet 在到期前轮换，Pod 删除后失效），以及 TokenReview 在 status.user.extra 中返回的 Pod 名与 Pod UID | <https://kubernetes.io/docs/reference/access-authn-authz/service-accounts-admin/> | Kubernetes 1.35 |
| REF-cluster-autoscaler-faq | Cluster Autoscaler FAQ | Kubernetes Cluster Autoscaler 的常见问题文档，含 Pod 注解 cluster-autoscaler.kubernetes.io/safe-to-evict | <https://github.com/kubernetes/autoscaler/blob/master/cluster-autoscaler/FAQ.md> | — |
| REF-otel-genai-semconv | OpenTelemetry GenAI semantic conventions | 生成式 AI 调用的追踪、指标与事件属性约定（gen_ai.*） | <https://github.com/open-telemetry/semantic-conventions-genai> | — |
| REF-anthropic-streaming | Streaming Messages（Claude API 文档） | Claude Messages API 的服务器发送事件流格式，含内容事件与 ping 保活事件 | <https://platform.claude.com/docs/en/build-with-claude/streaming> | anthropic-version 2023-06-01 |
| REF-spiffe | SPIFFE | 工作负载身份标准：SPIFFE ID 与可验证身份文档（SVID） | <https://spiffe.io/docs/latest/spiffe-about/overview/> | — |
| REF-envoy | Envoy Proxy 文档 | 云原生服务代理，提供 CONNECT 终止、内部监听器、`ext_authz`、`ext_proc` 与 dynamic forward proxy 过滤器 | <https://www.envoyproxy.io/docs/envoy/latest/> | — |
