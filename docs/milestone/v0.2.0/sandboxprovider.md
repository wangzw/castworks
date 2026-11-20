# sandboxprovider（v0.2.0）

v0.2.0 的 Substrate 提供者在 [v0.1.0](../v0.1.0/sandboxprovider.md) 的基础上做三处修改：actor 模板增加 `connector` 端点的地址；模板名改由模板内容派生；EgressPolicy 的规则改为按配置生成。接口、调用 ate-api 的方式与错误处理都不变，下文只写变化。版本范围见 [v0.2.0](README.md)。

## actor 模板

`containers[0].env` 增加一项：

| 变量 | 取值 | 来源 |
| --- | --- | --- |
| `CASTWORKS_CONNECTOR` | `castworks-dev.castworks-system.svc.cluster.local:9091` | 控制面启动参数 `--substrate-connector`，默认即为该值 |

模板名由 `cw-<镜像 digest 前 12 位>` 改为 `cw-` 加模板内容摘要的前 12 位十六进制字符；模板内容摘要是模板定义以确定性编码序列化后的 SHA-256，镜像 digest 包含在内。模板创建后不能修改，而模板内容除镜像外还取决于控制面的启动参数，例如 `--substrate-control-plane` 与 `--substrate-connector`。按镜像命名时，同一镜像换了参数，控制面启动时对已有模板的逐项核对不通过，启动失败，要手工删除旧模板；按内容命名后，内容不同的模板名字一定不同，换参数不再需要手工删除。控制面启动时按名字查找、不存在就创建、已存在就逐项核对的做法不变，旧模板仍不自动删除。

## 网络

EgressPolicy `default` 的规则改为按配置生成，同一主机的端口合并为一条规则：

| 规则 | `hostnames` | `ports` | 来源 |
| --- | --- | --- | --- |
| `tls_passthrough` | `api.anthropic.com` | 443 | 固定 |
| `http` | 控制面主机，即 `castworks-dev.castworks-system.svc.cluster.local` | 9090、9091 | `--substrate-control-plane` 与 `--substrate-connector` 的主机与端口 |
| `http` | `extsim.castworks-system.svc.cluster.local` | 9100 | 新增的控制面启动参数 `--substrate-egress-http`，取 `host:port`，可以重复给出 |

`--substrate-egress-http` 给出的主机必须是全限定名，理由同 v0.1.0 的[网络](../v0.1.0/sandboxprovider.md#网络)一节。v0.2.0 的开发环境给出一项 `extsim.castworks-system.svc.cluster.local:9100`。

`deploy/dev` 中，Service `castworks-dev` 与它的 EndpointSlice 增加名为 `connector` 的 9091 端口；新增不带 selector 的 Service `extsim` 及其 EndpointSlice，指向开发机地址的 9100 端口。`setup.sh` 与 `teardown.sh` 一并建立与删除它们。

## 与设计的差异

在 v0.1.0 所列差异之外：出口规则仍由提供者按控制面配置生成，不由 `connectorhub` 按外部系统定义生成与推送；`extsim` 定义中的 `access` 不起作用。

## 测试

- 模板名：同一镜像、同一配置得到同一名字，重复启动控制面不重复创建；只改 `--substrate-connector` 时名字不同。
- 出口规则：按配置生成的规则与上表一致；控制面主机的两个端口合并为一条 `http` 规则。
- 集成：在 `castworks-dev` 中创建的 actor 能以全限定名经出口网关访问 `extsim` 的 9100 端口与控制面的 9091 端口。
