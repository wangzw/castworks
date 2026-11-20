# 开发环境清单

本目录建立 castworks v0.2.0 在本机开发集群中需要的基础对象：actor 落脚的 gVisor WorkerPool、控制面与 extsim 对 actor 暴露的 Service 与 EndpointSlice，以及控制面调用 ate-api 使用的身份与授权。控制面与 extsim 都是开发机上的普通进程，不部署进集群。

## 前提

- 集群上下文为 `wangzw@kubernetes`。用其它上下文时设置环境变量 `KUBE_CONTEXT`。
- Substrate v1.0.0 已安装，`ate-system` 中的 ate-api 可用。
- `kubectl-ate` 插件在 `PATH` 中，`kubectl ate` 能执行。
- 已安装 `jq`，setup.sh 用它读取访问策略的 uid 与 version。
- 已安装 `ip`，setup.sh 用它确认开发机确实持有 `10.12.72.11`。
- 开发机地址为 `10.12.72.11`。控制面的端口为 9090（gRPC）、9091（connector）与 9092（webhook 接收端）；extsim 的端口为 9100（作业接口）与 9101（测试控制接口）。EndpointSlice 与探针服务端都写死了这个地址，地址变了要同时改清单与脚本；setup.sh 在本机没有该地址时报错退出。
- 9091、9092、9100 与 9101 在开发机上没有被其他进程占用。setup.sh 不检查端口占用，冲突要到控制面或 extsim 监听时才会暴露。
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

setup.sh 还确保 webhook 签名密钥文件存在：`~/.config/castworks/extsim-signing-key`。文件不存在时生成 32 字节随机数的十六进制加一个换行，权限 0600；已存在时不改动内容，重复执行不会换掉正在使用的密钥。控制面与 extsim 读取同一个文件，内容按字节使用并去掉末尾的换行。文件只在开发机上，不进仓库；teardown.sh 不删除它。

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

## 运行 extsim

extsim 也是开发机上的普通进程，用 serve 子命令启动：

```sh
go run ./cmd/extsim serve \
  --signing-key-file ~/.config/castworks/extsim-signing-key
```

`--listen` 默认 `0.0.0.0:9100`，集群中的 actor 经 Service `extsim` 访问它；`--control-listen` 默认 `127.0.0.1:9101`，只接受回环地址；`--callback-url` 默认 `http://127.0.0.1:9092/hooks/extsim`，即启用 connectorhub 时控制面的 webhook 接收端；`--job-duration` 默认 `60s`。作业、计划与待重试的回调只保存在内存里，进程重启后全部丢失。

`--signing-key-file` 必填，与控制面配置中的 `signing_key_file` 指向同一个文件。actor 镜像里的 `/etc/extsim.json` 把 `extsim` 命令行指向 `http://extsim.castworks-system.svc.cluster.local:9100`；开发机上的本地模式没有这个文件，取默认的 `http://127.0.0.1:9100`。

## 运行控制面

控制面是开发机上的普通进程。先起数据库，把连接串指向持久化的 `postgres`（55432）：

```sh
make db-up
export CASTWORKS_DATABASE_URL='postgres://castworks:castworks@127.0.0.1:55432/castworks_test?sslmode=disable'
```

开发集群上的演示与挂起测试用 Substrate 提供者：

```sh
go run ./cmd/castworks-controlplane \
  --provider substrate \
  --grpc-addr 10.12.72.11:9090 \
  --connectorhub-config deploy/dev/connectorhub/config.yaml \
  --connector-addr 10.12.72.11:9091 \
  --webhook-addr 127.0.0.1:9092 \
  --substrate-image registry.zhanwei.wang/castworks/actor@sha256:<digest> \
  --substrate-connector castworks-dev.castworks-system.svc.cluster.local:9091 \
  --substrate-egress-http extsim.castworks-system.svc.cluster.local:9100 \
  --llm-credential-kind oauth_token \
  --llm-credential-file /path/to/credential \
  --suspend
```

`--grpc-addr` 取开发机的网卡地址，actor 经 Service 与 EndpointSlice 拨回这里。`--connector-addr` 与它规则相同，也必须是网卡 IP 字面量，开发环境取 `10.12.72.11:9091`；给出 `--connectorhub-config` 时这一项必填。`--webhook-addr` 只接受回环地址，默认 `127.0.0.1:9092`，extsim 的回调发到这里。不给出 `--connectorhub-config` 时不启动 connectorhub，也不监听 connector 与 webhook 两个地址，行为与 v0.1.0 相同。

`--substrate-image` 取 v0.2.0 的开发镜像，参数要求写成 `name@sha256:<digest>` 的形式，不能用标签。镜像要在 main 上的提交上构建：在干净的工作树上执行 `make actor-image-push`，它按该提交的短 SHA 打标签并推送，标准输出最后一行为 `<仓库>@sha256:<arm64 清单 digest>`，把这一行原样用作 `--substrate-image` 与验收的 `ACTOR_IMAGE`。构建用的提交要包含已合入 main 的看守进程；镜像里含 `extsim` 命令行与 `/etc/extsim.json`。

控制面启动时装载 `deploy/dev/connectorhub/config.yaml`。示例中的 `definition: extsim.yaml` 按配置文件所在目录解析为 `deploy/dev/connectorhub/extsim.yaml`，`signing_key_file: ~/.config/castworks/extsim-signing-key` 展开到当前用户的主目录，这两项不依赖启动时的工作目录；`--connectorhub-config` 自身的路径仍按启动目录解析。示例的 `api_base_url` 为 `http://127.0.0.1:9100`，`project` 为 `default`，`bindings` 为 `extsim: [demo]`，actor 的作业要以项目 `demo` 提交才能满足项目归属。`--substrate-connector` 写进 actor 模板的环境变量 `CASTWORKS_CONNECTOR`，默认 `castworks-dev.castworks-system.svc.cluster.local:9091`，与 Service 的端口对应。`--substrate-egress-http` 给 actor 的出口策略追加一项，开发环境取 `extsim.castworks-system.svc.cluster.local:9100`，可以重复给出。

`--suspend` 打开挂起扫描，默认回合边界空闲 30 s 后挂起。凭据文件的种类由 `--llm-credential-kind` 指明，取 `api_key` 或 `oauth_token`，文件内容只有令牌本身。

本地模式不碰集群，也不需要 Substrate，用本地提供者启动控制面：

```sh
go run ./cmd/castworks-controlplane \
  --provider local \
  --connectorhub-config deploy/dev/connectorhub/config.yaml \
  --connector-addr 127.0.0.1:9091 \
  --llm-credential-kind oauth_token \
  --llm-credential-file /path/to/credential
```

给出 `--connectorhub-config` 时 `--connector-addr` 必填，本地模式取 `127.0.0.1:9091`；webhook 接收端仍取默认的 `127.0.0.1:9092`，extsim 的回调默认发往那里。不给出 `--connectorhub-config` 时不启动 connectorhub，行为与 v0.1.0 相同。示例配置的 `api_base_url` 为 `http://127.0.0.1:9100`，`project` 为 `default`，`bindings` 为 `extsim: [demo]`。

`--provider local` 会强制关闭挂起，`Snapshot` 与 `RestoreSnapshot` 返回不支持。先创建运行实例，再在 `T_hello`（默认 60 s）内用返回的 `run_id` 启动看守进程：

```sh
go run ./cmd/castworks --server http://127.0.0.1:8080 run create '<prompt>'
go run ./cmd/warden \
  --run-id <run_id> \
  --control-plane 127.0.0.1:9090 \
  --connector 127.0.0.1:9091 \
  --home /tmp/castworks-home \
  --claude-bin "$(command -v claude)"
```

`--connector` 取控制面 `--connector-addr` 的地址，看守进程在它上面启动平台 MCP 服务器；不给出时不启动，参数与 v0.1.0 相同。本地模式下 Claude Code 沿用看守进程的 `PATH`，开发机上的 `extsim` 命令行要在这个 `PATH` 中。actor 镜像里的 `/etc/extsim.json` 在本地不存在，Agent 用的 `extsim` 取默认地址 `127.0.0.1:9100`，与 `extsim` 默认监听的 `0.0.0.0:9100` 对应。

`--home` 指定的目录存放 Claude Code 的工作目录与 stream-json 记录，本地模式不使用开发者自己的主目录。看守进程连上控制面之前，控制面从本地提供者的 `Provision` 返回起算 `T_hello`。

`make acceptance-*` 把上述步骤封装成端到端测试，运行记录写到 `/tmp/castworks-acceptance/`，除 `acceptance-clean` 外都需要 `CASTWORKS_TEST_DATABASE_URL` 指向 `postgres-test`（55433）：

| 目标 | 用途 |
| --- | --- |
| `make acceptance-dryrun JOBS=<n>` | 本地提供者加假的 Claude Code，不花 LLM 额度，验证验收 harness |
| `make acceptance-local JOBS=<n> CREDENTIAL_FILE=<path>` | 本地提供者加真实的 Claude Code，跑两个回合，并录制 stream-json 样本 |
| `make acceptance-substrate JOBS=<n> CREDENTIAL_FILE=<path> ACTOR_IMAGE=<ref>` | Substrate 提供者，不启用挂起，两个回合后结束 |
| `make acceptance-suspend JOBS=<n> CREDENTIAL_FILE=<path> ACTOR_IMAGE=<ref> ACTOR_COMMIT=<sha>` | Substrate 提供者加 `--suspend`，跑演示场景与通过标准 |
| `make acceptance-ballast JOBS=<n> BALLAST_IMAGE=<ref>` | 用压载镜像做快照恢复循环，不花 LLM 额度 |
| `make acceptance-clean JOBS=<n>` | 清理验收留下的 actor 与模板 |

Substrate 与挂起两项的 `ACTOR_IMAGE` 取与 `--substrate-image` 相同的引用；`ACTOR_COMMIT` 是构建该镜像的提交，即执行 `make actor-image-push` 时 main 上被构建的那个提交。

## 资源

| 文件 | 对象 | 用途 |
| --- | --- | --- |
| `k8s/namespace.yaml` | 命名空间 `castworks-system` | 存放 castworks 的集群对象 |
| `k8s/serviceaccount.yaml` | ServiceAccount `castworks-system/sandboxprovider` | 控制面以 TokenRequest 为它签发令牌，经端口转发调用 ate-api |
| `k8s/service.yaml` | Service `castworks-system/castworks-dev` | 不带 selector，给 actor 一个访问开发机的稳定名字；端口 9090（gRPC）与 9091（connector） |
| `k8s/endpointslice.yaml` | EndpointSlice `castworks-system/castworks-dev` | 把上面的 Service 指向 `10.12.72.11:9090` 与 `10.12.72.11:9091` |
| `k8s/extsim-service.yaml` | Service `castworks-system/extsim` | 不带 selector，给 actor 一个访问开发机上 extsim 的稳定名字；端口 9100 |
| `k8s/extsim-endpointslice.yaml` | EndpointSlice `castworks-system/extsim` | 把上面的 Service 指向 `10.12.72.11:9100` |
| `k8s/workerpool.yaml` | WorkerPool `castworks-system/castworks` | 运行 actor 的 gVisor 池，2 个 worker，单个 worker 上限 2 CPU、3 GiB |
| `access-policy.yaml` | atespace `castworks-dev` 的访问策略 | 授予 `sandboxprovider` 在 `castworks-dev` 中的 `editor` 角色 |
| `connectorhub/config.yaml` | connectorhub 配置示例 | 控制面 `--connectorhub-config` 指向它；其中相对路径按它所在目录解析 |
| `connectorhub/extsim.yaml` | extsim 的外部系统定义 | webhook 路径、签名头、事件类型与完成条件 |

setup.sh 还用 `kubectl ate create atespace` 建立 atespace `castworks-dev`，actor 都创建在这里。签名密钥文件不在本目录，路径与生成方式见“执行”一节。

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

actor 的出站只放行出口策略列出的主机。控制面在集群里的名字固定为 `castworks-dev.castworks-system.svc.cluster.local`：端口 9090 是看守进程拨号用的 gRPC 端口，9091 是 connector 端点。extsim 的名字固定为 `extsim.castworks-system.svc.cluster.local`，端口 9100。这些名字、端口与开发机地址都不能随意改；改了要同步改 sandboxprovider 的出口策略与 actor 模板，extsim 的名字或端口变了还要重建镜像中的 `/etc/extsim.json`。

## 验证

```sh
kubectl -n castworks-system get sa,svc,endpointslice
kubectl -n castworks-system get svc castworks-dev extsim
kubectl -n castworks-system get endpointslice castworks-dev extsim
kubectl -n castworks-system get workerpool castworks
kubectl ate get atespace
kubectl ate get access-policy -a castworks-dev
```

WorkerPool 的 READY 列应与 DESIRED 列相等，表示 worker 已就绪。
