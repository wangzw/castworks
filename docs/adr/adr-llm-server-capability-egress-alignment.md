# LLM 上游服务端代发的外联与出口策略同级约束

## 背景

[LLM 网关](../glossary.md)以平台的 LLM 凭据把 Agent 的请求转发给 Anthropic API，Agent 的 Anthropic 请求头经[本地代理](../glossary.md)透传。[ADR-two-tier-llm-proxy](adr-two-tier-llm-proxy.md) 让本地代理只做转发、模式选择与回放取回，不解析请求体。

Anthropic 的服务端工具（`web_fetch`、`web_search`）与远程 MCP 服务器接入（请求体中的 `mcp_servers`）由上游以平台账户代为发起外联。这些外联不经[出口代理](../glossary.md)，也不受[运行实例](../glossary.md)[出口策略](../glossary.md)中允许主机的约束。受提示注入控制的 Agent 完全控制请求体与请求头，可以在请求中启用 `web_fetch` 或指定任意主机的 `mcp_servers`，把[工作区](../glossary.md)内容编码进 URL 后外泄，或抓取出口策略未放行的资源。

Claude Code 的 WebFetch 在客户端执行，其流量经本地代理或[透明拦截器](../glossary.md)到出口代理；WebSearch 依赖服务端的 `web_search`。托管环境承载完整的第三方 Agent，不以禁用 Agent 的功能换取可挂起性（[ADR-host-owns-external-wake-sources](adr-host-owns-external-wake-sources.md)）；对上游代发外联施加约束时，同样须保留 WebSearch 等依赖服务端工具的功能。

## 考虑过的选项

### 原样转发

采用它意味着网关只替换认证头，请求体与请求头原样发往上游。**被拒**：Agent 可以经上游代发的外联绕过出口策略，允许主机只约束沙箱到出口代理这一跳。

### 禁用全部服务端工具与 beta 能力

采用它意味着网关剥离请求中的全部服务端工具、远程 MCP 服务器接入与 `anthropic-beta` 取值。**被拒**：降低 Agent 能力，WebSearch 等依赖服务端工具的功能不可用。

### 本地代理解析并改写请求体

采用它意味着沙箱内的本地代理按出口策略改写请求体中的服务端工具参数。**被拒**：本地代理位于沙箱内，处于 Agent 可影响的一侧；解析请求体与两级代理的分工相悖，出口策略的允许主机也不在沙箱内维护。

### 网关解析有限的顶层字段，按允许主机约束外联类能力

采用它意味着网关只解析请求体顶层的 `tools`、`mcp_servers`、`container` 与 `anthropic-beta` 请求头。外联类服务端工具的域限制改写为与运行实例允许主机的交集，`mcp_servers` 的主机须在允许主机内，服务端工具类型与 beta 取值走允许清单，`web_search` 审计放行。**采用**——见决策。

## 决策

采用 LLM 网关按出口策略约束上游服务端代发外联的方案：

- 网关是唯一的解析点：对 Agent 请求与分类请求只解析请求体顶层的 `tools`、`mcp_servers`、`container` 字段与 `anthropic-beta` 请求头，其余字段原样转发；本地代理原样转发，不解析请求体。
- 外联类服务端工具（`web_fetch` 的各版本）的域限制改写为请求原值与该运行实例当前允许主机的交集，请求未给出时取允许主机，交集为空时拒绝。
- `mcp_servers` 中每个 URL 的主机必须属于允许主机，否则拒绝。
- 服务端工具类型与 `anthropic-beta` 取值各设允许清单，清单外的取值拒绝。
- `web_search` 放行并审计，部署可配置其域过滤。
- `container` 只接受本运行实例此前响应中出现过的容器标识。
- 允许主机由[连接器](../glossary.md)的出口策略查询接口 `EgressPolicyQuery.AllowedHosts` 提供。
- 网关只暴露 `POST /v1/messages` 与 `GET /v1/requests/{request_id}`。

## 后果

本决策使项目承担以下义务，穷举如下：

- `llmgateway` 依赖 `connectorhub` 的出口策略查询接口；这是事件源模块之间唯一的依赖边。
- 允许清单随所固定的 Claude Code 版本维护，缺省包含该版本实际使用的取值；上游出现的新服务端能力须评估后加入清单。
- 越界请求返回 400 或 403 并审计，不访问上游，不产生 LLM 失败唤醒信号；本地代理按客户端错误交还 Agent，不重发。
- 网关按运行实例记录响应中出现过的容器标识。

承担的代价穷举如下：

- `web_search` 的检索来源不受允许主机约束，这一残余风险已被接受，由审计与可选的域过滤覆盖。
- 网关增加请求体解析开销。
- 出口策略查询不可用时，含外联类服务端工具或 `mcp_servers` 的请求失败，其余请求不受影响。
- 允许清单落后于 Claude Code 版本时，新版本使用的服务端工具或 beta 取值被拒绝，须随版本升级同步更新清单。

## 失效条件

- Anthropic API 提供平台可强制执行的按请求或组织级出站域策略时，网关侧的改写可由该策略替代。
