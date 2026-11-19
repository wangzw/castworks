# castworks 设计文档

castworks 是托管 AI Agent 环境。完整的第三方 Agent 以进程形式运行在挂载持久化工作区的沙箱内，托管的 Agent 为 Claude Code；托管环境在 Agent 等待 LLM、外部系统或人类时挂起沙箱以释放计算资源，并在依赖解除后恢复，以同时达到计算资源利用率与响应延迟目标。

## 索引

- [架构](architecture.md)：核心概念与设计主线、系统级结构、跨切面设计与架构不变量
- [术语表](../glossary.md)：项目级术语，所有文档类型共用
- [引用](../references.md)：文档引用的公开外部来源

## 模块

| 模块 | 文档 |
| --- | --- |
| `runcontroller` | [runcontroller](modules/design-runcontroller.md) |
| `llmgateway` | [llmgateway](modules/design-llmgateway.md) |
| `connectorhub` | [connectorhub](modules/connectorhub/README.md) |
| `sandboxprovider` | [sandboxprovider](modules/sandboxprovider/README.md) |
| `agentprotocol` | [agentprotocol](modules/design-agentprotocol.md) |
| `secrets` | [secrets](modules/design-secrets.md) |
| `humanchannel` | [humanchannel](modules/design-humanchannel.md) |
| `warden` | [看守进程](modules/warden/README.md) |
| `suspendpolicy` | [suspendpolicy](modules/design-suspendpolicy.md) |
| `workspace` | [workspace](modules/design-workspace.md) |

## 对照

- [与 Google AX 的对照](../faq/castworks-vs-google-ax.md)：需求与设计上的重合度分析；AX 见 [REF-google-ax](../references.md)。
- [与同类项目的对照](../faq/castworks-vs-similar-projects.md)：按托管完整 Agent、可挂起沙箱与沙箱内的 Agent 驱动层分组，对照挂起触发、挂起后保留的状态与恢复触发。对照的项目：
  - 托管完整 Agent：kagent（[REF-kagent](../references.md)）、Claude Managed Agents（[REF-claude-managed-agents](../references.md)）、Amazon Bedrock AgentCore Runtime（[REF-bedrock-agentcore-runtime](../references.md)）、Foundry hosted agents（[REF-foundry-hosted-agents](../references.md)）、Open-Inspect（[REF-open-inspect](../references.md)）、cullenmcdermott/sandbox（[REF-cullenmcdermott-sandbox](../references.md)）。
  - 可挂起沙箱：E2B（[REF-e2b](../references.md)）、Daytona（[REF-daytona](../references.md)）、Blaxel（[REF-blaxel](../references.md)）、Sprites（[REF-sprites](../references.md)）、阿里云 Agent Sandbox（[REF-aliyun-agent-sandbox](../references.md)）、Cube Sandbox（[REF-cube-sandbox](../references.md)）、OpenSandbox（[REF-opensandbox](../references.md)），以及作为反例的 Cloudflare Sandbox SDK（[REF-cloudflare-sandbox](../references.md)）。
  - 沙箱内的 Agent 驱动层：Sandbox Agent（[REF-sandbox-agent](../references.md)）。
