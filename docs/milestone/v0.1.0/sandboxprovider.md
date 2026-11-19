# sandboxprovider（v0.1.0）

v0.1.0 只实现 Substrate 提供者。它在控制面进程中经 ate-api 管理 actor 模板、actor 与 actor 的 EgressPolicy。下文的 Substrate 事实依据 Substrate v1.0.0 的 API 定义与源码（[REF-substrate](../../references.md)）。版本范围见 [v0.1.0](README.md)。

## 职责

- 保证当前 actor 镜像对应的 actor 模板存在。
- 为每个运行实例创建、挂起、恢复与删除一个 actor，并为它建立 EgressPolicy。
- 返回每次挂起与恢复的耗时，供 `runcontroller` 记录。

## 接口

供 `runcontroller` 调用，方法名取自设计中的 `Provider` 接口：

| 方法 | ate-api 调用 | 说明 |
| --- | --- | --- |
| `Provision(run_id)` | `CreateActor`，`CreateActorEgressPolicy`，`ResumeActor` | 新建的 actor 处于 `ACTOR_STATE_SUSPENDED`，先建出口策略，再以 `ResumeActor` 冷启动；`ResumeActor` 等到 `wakeupProbe` 返回 200 才返回 |
| `Snapshot(actor)` | `SuspendActor`，`mode=DURABILITY_MODE_SYNCHRONOUS` | 阻塞到快照在对象存储中写完 |
| `RestoreSnapshot(actor)` | `ResumeActor` | 阻塞到 `wakeupProbe` 返回 200 |
| `Destroy(actor)` | `DeleteActor` | EgressPolicy 随 actor 一起删除；actor 不存在视为成功 |
| `GetSandbox(actor)` | `GetActor` | 返回 `status.state` |

actor 名为 `cw-<run_id>`，在 atespace `castworks-dev` 中创建。Substrate 要求 actor 名是 DNS 标签：不超过 63 个字符，只含小写字母、数字与 `-`，首尾为字母或数字。`run_id` 是 12 位小写 base32，满足这一要求。

## actor 模板

模板创建后不能修改，所以模板名由镜像 digest 派生，形如 `cw-<digest 前 12 位>`。控制面启动时按配置的镜像查找模板，不存在就创建。Substrate 删除模板时不检查是否还有 actor 引用它，被引用的模板删除后，这些 actor 恢复时会失败，所以 v0.1.0 不自动删除模板。

| 字段 | 取值 | 原因 |
| --- | --- | --- |
| `containers[0].name` | `warden` | |
| `containers[0].image` | castworks 镜像，按 `name@sha256:…` 写 digest | Substrate 要求镜像带 digest |
| `containers[0].env` | `CASTWORKS_CONTROL_PLANE=castworks-dev.castworks-system.svc.cluster.local:9090` | 看守进程的控制面地址，见[网络](#网络) |
| `containers[0].volume_mounts` | 卷 `system-info` 挂在 `/run/ate` | |
| `containers[0].wakeup_probe` | `http_get`，端口 8081，路径 `/readyz` | `ResumeActor` 以它判断 actor 就绪 |
| `volumes` | 名为 `system-info`、类型字段为 `system_info` 的卷，`actor_metadata` 把 `ACTOR_METADATA_FIELD_NAME` 写到 `actor-name` | 看守进程从 `/run/ate/actor-name` 读出 actor 名；Substrate 按 DNS 标签校验卷名，不接受下划线 |
| `resources.limits` | CPU 2000 millicores，内存 2 GiB | 初始值，按 Claude Code 的实测占用调整 |
| `snapshot_config.preferred_fidelity` | MEMORY | 快照保存进程内存与根文件系统的写入 |
| `snapshot_config.golden_snapshot_config.mode` | `GOLDEN_SNAPSHOT_MODE_DISABLED` | 见下文 |
| `worker_selector.match_labels` | `castworks.dev/pool: castworks` | 只落在 castworks 的 gVisor 池上 |

关闭[模板快照](../../glossary.md)，新 actor 冷启动。模板快照制作时的 actor 没有出口策略，也不属于任何运行实例；关闭之后，看守进程启动时读到的 actor 名在 actor 的整个生命周期内不变。冷启动只启动看守进程，Claude Code 等第一条消息到达才启动，冷启动的代价不大。

模板不设置运行用户，进程以镜像的 `USER` 运行，因为 Substrate 的模板没有 `runAsUser` 字段。

## 网络

`Provision` 在 `ResumeActor` 之前创建名为 `default` 的 EgressPolicy，规则为：

| 规则 | `hostnames` | `ports` |
| --- | --- | --- |
| `tls_passthrough` | `api.anthropic.com` | 443 |
| `http` | `castworks-dev.castworks-system.svc.cluster.local` | 9090 |

`http` 规则覆盖明文 HTTP/2（h2c），按请求的 authority 匹配主机名，所以看守进程必须以同一个名字拨号。actor 的 DNS 中继只放行与规则主机名一致的查询，并把它交给集群 DNS 解析，因此规则与拨号都使用全限定名，不使用 `castworks-dev.castworks-system.svc` 这样依赖搜索域补全的短名。

策略生效有延迟：DNS 约 5 s，出口网关的策略缓存默认 10 s。策略在 actor 冷启动之前建立，看守进程的首次连接不受影响。

## 调用 ate-api

ate-api 只在集群内提供服务，控制面使用开发机的 kubeconfig 访问它：

- 端口转发：取 `ate-system` 中 Service `api` 的一个就绪 Pod，转发它的 443 端口。转发固定在一个 Pod 上，调用返回 `Unavailable` 时重新建立转发。
- TLS：要求 TLS 1.3，服务器名为 `api.ate-system.svc`。根证书取自标签为 `podcert.ate.dev/canarying=live`、`signerName` 为 `servicedns.podcert.ate.dev/identity` 的 ClusterTrustBundle。
- 认证：以 TokenRequest 为 ServiceAccount `castworks-system/sandboxprovider` 签发 audience 为 `api.ate-system.svc`、有效期 1 小时的令牌，在过期前一半时间重新签发，每次调用携带最新令牌。该 ServiceAccount 在 `castworks-dev` 的访问策略中持有 `editor` 角色。

## 错误处理

| 情形 | 处理 |
| --- | --- |
| `CreateActor` 返回 `ALREADY_EXISTS` | 视为已创建，继续后续步骤，使 `Provision` 可以重试 |
| `ResumeActor` 返回 `RESOURCE_EXHAUSTED` 或 `ABORTED` | 退避重试，2 分钟后仍失败则返回错误 |
| `ResumeActor` 返回 `FAILED_PRECONDITION`（actor 为 `CRASHED`） | 返回“actor 已崩溃”，`runcontroller` 把运行实例转为失败；v0.1.0 不调用 `RevertActor` |
| `wakeupProbe` 超时 | Substrate 把 actor 置为 `CRASHED`，按上一行处理 |
| `SuspendActor` 失败 | 返回错误，由 `runcontroller` 稍后重试 |
| `DeleteActor` 返回 `NOT_FOUND` | 视为成功 |

## 日志

`kubectl ate logs actor cw-<run_id> -a castworks-dev` 读取看守进程与 Claude Code 的标准输出和标准错误，只在 actor 处于 `RUNNING` 时可读。日志保存在 worker Pod 的容器日志中，worker Pod 被替换后就没有了。

## 与设计的差异

- 只有 Substrate 提供者，没有能力集合；设计中 `Provider` 接口的其余方法都不实现。
- 不提供引导接口，看守进程不领取令牌与租约。
- 恢复直接调用 `ResumeActor`，不经路由器的 `/wake`。
- 出口策略不按代落地与撤销，随 actor 删除；不注入凭据，Anthropic 走 `tls_passthrough`。
- 不撤销快照尝试，不处理快照保留期，不处理 `CRASHED` 后的 `RevertActor`。
- 关闭模板快照，新 actor 冷启动；设计中新 actor 从模板快照恢复。

## 测试

在开发集群的 `castworks-dev` 上做集成测试：

- 模板查找与创建是幂等的，重复启动控制面不会重复创建模板。
- `Provision`、`Snapshot`、`RestoreSnapshot` 与 `Destroy` 的完整循环；`Snapshot` 前在 actor 中写入的文件与进程状态在恢复后仍在。
- 记录冷启动、快照与恢复的耗时，以及快照大小，写回[技术验证](../verification.md)的当前验证结论。
- `Destroy` 重复调用不报错；对已删除的 actor 调用 `GetSandbox` 返回不存在。
