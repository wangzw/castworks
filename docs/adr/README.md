# 架构决策记录

本目录记录在用的架构决策：一条记录对应一个难以逆转、或影响模块边界、公共接口、数据模型、部署形态、外部依赖与安全态势的选择，陈述约束、权衡过的选项、采用的决策与接受的代价。

## 等待与唤醒

| 决策 |
| --- |
| [在持久化收件箱上等待](adr-wait-on-durable-inbox.md) |

## 一致性与围栏

| 决策 |
| --- |
| [以代围栏沙箱消息与副作用](adr-generation-fencing.md) |
| [投递幂等状态随工作区持久化](adr-delivery-intent-log.md) |

## 租户

| 决策 |
| --- |
| [保留租户维度而不实现跨租户隔离](adr-tenant-dimension-without-enforcement.md) |
