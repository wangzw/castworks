# sandboxprovider

sandboxprovider 把控制面选定的沙箱生命周期操作翻译为 k8s 与 Substrate 提供者层的调用，并以能力集合、按代墓碑与幂等标识保证重复调用与迟到调用不产生第二次副作用。k8s 提供者与 Substrate 提供者共用同一套控制面、看守进程与能力接口，差异留在各自的子模块。

## 索引

| 文档 | 内容 |
| --- | --- |
| [sandboxprovider](design-sandboxprovider.md) | 模块主文档：能力自描述、生命周期接口、恢复触发与恢复时延 |
| [k8s 子模块](design-sandboxprovider-k8s.md) | Sandbox CR、节点冻结代理与 Agent 子 cgroup、原地缩放、预热池、NetworkPolicy 与平台出口代理配置、入站路由 wake-router |
| [substrate 子模块](design-sandboxprovider-substrate.md) | actor 模板、Suspend 与 Resume、路由器唤醒、EgressPolicy、外部 CSI 卷与 `durableDir`、预热 worker |

## 实现状态

| 子模块 | 状态 |
| --- | --- |
| [k8s 子模块](design-sandboxprovider-k8s.md) | 未实现 |
| [substrate 子模块](design-sandboxprovider-substrate.md) | 未实现 |
