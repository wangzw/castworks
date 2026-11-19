# 开发环境清单

本目录建立 castworks v0.1.0 在本机开发集群中需要的基础对象：actor 落脚的 gVisor WorkerPool、控制面对 actor 暴露的 Service 与 EndpointSlice，以及控制面调用 ate-api 使用的身份与授权。控制面本身是开发机上的普通进程，不部署进集群。

## 前提

- 集群上下文为 `wangzw@kubernetes`。用其它上下文时设置环境变量 `KUBE_CONTEXT`。
- Substrate v1.0.0 已安装，`ate-system` 中的 ate-api 可用。
- `kubectl-ate` 插件在 `PATH` 中，`kubectl ate` 能执行。
- 已安装 `jq`，setup.sh 用它读取访问策略的 uid 与 version。
- 已安装 `ip`，setup.sh 用它确认开发机确实持有 `10.12.72.11`。
- 开发机地址为 `10.12.72.11`，控制面的 gRPC 端口为 9090。EndpointSlice 与探针服务端都写死了这个地址，地址变了要同时改清单与脚本；setup.sh 在本机没有该地址时报错退出。
- 当前 kubeconfig 用户具备以下权限，它们用于控制面调用 ate-api，不属于本目录创建的对象：
  - `ate-system` 中 Pod 的 `pods/portforward`（端口转发到 ate-api）；
  - `certificates.k8s.io` 中 `clustertrustbundles` 的读权限（取 ate-api 的服务端证书根）；
  - `castworks-system` 中 `serviceaccounts/token` 的 create 权限（为 `sandboxprovider` 签发令牌）。

  setup.sh 用 `kubectl auth can-i` 逐项预检，缺任何一项都以非零状态退出，命令为：

  ```sh
  kubectl auth can-i create pods --subresource=portforward -n ate-system
  kubectl auth can-i get clustertrustbundles
  kubectl auth can-i create serviceaccounts/token -n castworks-system
  ```

## 执行

建立全部对象：

```sh
./setup.sh
```

setup.sh 可重复执行：清单用 `kubectl apply` 更新，已存在的 atespace 跳过创建，访问策略存在时按当前 uid 与 version 更新，最后等待 WorkerPool 的 worker 就绪。worker 在 `WORKER_READY_TIMEOUT` 秒内没有就绪时，脚本以非零状态退出，默认 300。

删除本目录建立的全部对象：

```sh
./teardown.sh
```

teardown.sh 先确认 atespace 中没有 actor 与模板，再删除 atespace；访问策略随 atespace 在同一事务中删除，不单独先删。随后删除命名空间内的对象与命名空间本身。atespace 中仍有 actor 或模板时脚本以非零状态退出，不会连带删除它们。

## 测试数据库

`deploy/dev/compose.yaml` 定义两个 PostgreSQL 服务，镜像版本、凭据与库名都相同，只有端口、数据位置与用途不同：

| 服务 | 端口 | 数据 | 用途 |
| --- | --- | --- | --- |
| `postgres` | `127.0.0.1:55432` | 命名卷 `postgres-data`，持久 | 开发机上运行的控制面使用；控制面在挂起状态下重启后仍要读回数据，依赖这里的持久性 |
| `postgres-test` | `127.0.0.1:55433` | tmpfs，容器停止后清空 | 单元测试专用；关闭 `fsync`、`full_page_writes`、`synchronous_commit`，让建库和删库触发的 checkpoint 不再写机械盘 |

测试从环境变量 `CASTWORKS_TEST_DATABASE_URL` 读取连接串，示例值：

```sh
export CASTWORKS_TEST_DATABASE_URL='postgres://castworks:castworks@127.0.0.1:55433/castworks_test?sslmode=disable'
```

`make db-up` 同时启动两个服务并等到健康检查通过，`make db-down` 一起停止。`postgres-test` 的数据只在内存里，测试建删库不会影响 `postgres` 中控制面使用的数据。

## 资源

| 文件 | 对象 | 用途 |
| --- | --- | --- |
| `k8s/namespace.yaml` | 命名空间 `castworks-system` | 存放 castworks 的集群对象 |
| `k8s/serviceaccount.yaml` | ServiceAccount `castworks-system/sandboxprovider` | 控制面以 TokenRequest 为它签发令牌，经端口转发调用 ate-api |
| `k8s/service.yaml` | Service `castworks-system/castworks-dev` | 不带 selector，给 actor 一个访问开发机的稳定名字 |
| `k8s/endpointslice.yaml` | EndpointSlice `castworks-system/castworks-dev` | 把上面的 Service 指向 `10.12.72.11:9090` |
| `k8s/workerpool.yaml` | WorkerPool `castworks-system/castworks` | 运行 actor 的 gVisor 池，2 个 worker，单个 worker 上限 2 CPU、3 GiB |
| `access-policy.yaml` | atespace `castworks-dev` 的访问策略 | 授予 `sandboxprovider` 在 `castworks-dev` 中的 `editor` 角色 |

setup.sh 还用 `kubectl ate create atespace` 建立 atespace `castworks-dev`，actor 都创建在这里。

WorkerPool 的副本数取 2：每个 worker 的 2 CPU 只容得下模板里的一个 actor，两个集成测试的 actor 可以同时各占一个 worker。标签 `castworks.dev/pool: castworks` 会复制到每个 worker 上，actor 模板用 `worker_selector.match_labels` 选中这个池。worker 镜像按 digest 引用 `registry.zhanwei.wang/ateworker-gvisor`，与 counter demo 使用的是同一个 Substrate v1.0.0 构建。gVisor worker 会从内存上限中保留 64 MiB 给自己，所以内存上限取 3 GiB，让 2 GiB 的 actor 仍有足够容量。

### Worker 镜像

`k8s/workerpool.yaml` 中的 `workerImage` 是 Substrate 的 `cmd/ateworker-gvisor`，由 Substrate v1.0.0 源码用 ko 构建并推送到 `registry.zhanwei.wang`，不是从上游拉取的镜像。重建步骤：

1. 检出并切到 Substrate v1.0.0：

   ```sh
   git -C /data1/wangzw/workspace/substrate checkout v1.0.0
   ```

2. 构建并推送本机集群的 worker 镜像，命令会打印每个 sandbox class 的镜像引用：

   ```sh
   KO_DOCKER_REPO=registry.zhanwei.wang ate-setup publish worker-images
   ```

3. 把输出的 `ateworker-gvisor` 引用的 digest 写进 `k8s/workerpool.yaml` 的 `spec.workerImage`，再运行 `./setup.sh`。已有的 WorkerPool 会按新镜像滚动更新。

镜像换了以后，旧的 worker Pod 会被替换，节点上运行中的 actor 也随之消失；这一步只在开发集群里做。

## 网络

actor 的出站只放行出口策略列出的主机。控制面在集群里的名字固定为 `castworks-dev.castworks-system.svc.cluster.local`，端口 9090，看守进程按这个名字以明文 gRPC 拨号。Service 与 EndpointSlice 的名称、端口和开发机地址都不能随意改，改了要同步改 sandboxprovider 的出口策略和 actor 模板。

## 验证

```sh
kubectl -n castworks-system get sa,svc,endpointslice
kubectl -n castworks-system get workerpool castworks
kubectl ate get atespace
kubectl ate get access-policy -a castworks-dev
```

WorkerPool 的 READY 列应与 DESIRED 列相等，表示 worker 已就绪。
