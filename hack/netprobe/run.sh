#!/usr/bin/env bash
# Network probe for the castworks v0.1.0 development environment.
#
# It builds the arm64 probe image, starts the development-host h2c gRPC
# server on 10.12.72.11:9090, creates a temporary actor in atespace
# castworks-dev with the probe EgressPolicy, collects the five answers from
# the actor's logs, and removes the actor and, if this run created it, the
# template again.
#
# Everything it writes goes under /tmp/castworks-scratch/dev-env-netprobe/.
# The script is safe to run repeatedly. Set SKIP_IMAGE_BUILD=1 and
# IMAGE_DIGEST=sha256:... to reuse an image that was already pushed;
# NETPROBE_SERVER_ADDR, NETPROBE_REPO and NETPROBE_TIMEOUT override the
# defaults.
set -euo pipefail

script_dir="$(dirname -- "$(readlink -f -- "${BASH_SOURCE[0]}")")"
kube_context="${KUBE_CONTEXT:-wangzw@kubernetes}"
atespace="${ATESPACE:-castworks-dev}"
repo="${NETPROBE_REPO:-registry.zhanwei.wang/castworks/netprobe}"
server_addr="${NETPROBE_SERVER_ADDR:-10.12.72.11:9090}"
probe_timeout="${NETPROBE_TIMEOUT:-1800}"
scratch_root="${NETPROBE_SCRATCH:-/tmp/castworks-scratch/dev-env-netprobe}"
run_id="${NETPROBE_RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ | tr '[:upper:]' '[:lower:]')}"
work="$scratch_root/$run_id"

mkdir -p "$work"

kubectl_ctx() { kubectl --context "$kube_context" "$@"; }
ate_ctx() { kubectl ate "$@" --context "$kube_context"; }

server_pid=""
actor_created=0
template_created=0
actor="netprobe-$run_id"
template=""
probe_failed=0

cleanup() {
  local rc=$?
  set +e
  echo "==> 清理"
  local actors_json="" actors_read=0
  if actors_json="$(ate_ctx get actor -a "$atespace" -o json 2>"$work/cleanup-list-actors.err")"; then
    actors_read=1
    printf '%s\n' "$actors_json" >"$work/actors-before-delete.json"
    echo "-- 删除前 actor 列表："
    printf '%s\n' "$actors_json"
  else
    echo "错误：读取 atespace $atespace 的 actor 列表失败，不能确认模板未被引用，保留模板：" >&2
    cat "$work/cleanup-list-actors.err" >&2
    rc=1
  fi

  if [[ "$actor_created" == 1 ]]; then
    echo "-- 删除临时 actor $actor"
    if ! ate_ctx delete actor "$actor" -a "$atespace" >"$work/cleanup-delete-actor.txt" 2>&1; then
      echo "错误：删除 actor $actor 失败：" >&2
      cat "$work/cleanup-delete-actor.txt" >&2
      rc=1
    fi
    for _ in $(seq 1 60); do
      if ! actors_json="$(ate_ctx get actor -a "$atespace" -o json 2>"$work/cleanup-list-actors.err")"; then
        echo "错误：读取 actor 列表失败，无法确认 $actor 已删除：" >&2
        cat "$work/cleanup-list-actors.err" >&2
        actors_read=0
        rc=1
        break
      fi
      actors_read=1
      if ! jq -e --arg a "$actor" 'any(.actors[]?; .metadata.name == $a)' <<<"$actors_json" >/dev/null; then
        break
      fi
      sleep 2
    done
  fi

  if [[ -n "$template" && "$template_created" == 1 ]]; then
    if [[ "$actors_read" != 1 ]]; then
      echo "错误：无法读取 actor 列表，不能确认模板 $template 未被引用，保留模板" >&2
      rc=1
    elif [[ "$(jq -r '.actors // [] | length' <<<"$actors_json")" != "0" ]]; then
      echo "错误：atespace $atespace 中仍有 actor，不能确认模板 $template 未被引用，保留模板：" >&2
      jq -r '.actors[]? | .metadata.name' <<<"$actors_json" >&2
      rc=1
    else
      echo "-- 没有 actor 引用模板 $template，删除模板"
      if ! ate_ctx delete actor-template "$template" -a "$atespace" >"$work/cleanup-delete-template.txt" 2>&1; then
        echo "错误：删除模板 $template 失败：" >&2
        cat "$work/cleanup-delete-template.txt" >&2
        rc=1
      fi
    fi
  elif [[ -n "$template" ]]; then
    echo "-- 模板 $template 是复用的，保留"
  fi

  if ate_ctx get actor-template -a "$atespace" >"$work/templates-after-delete.txt" 2>&1; then
    echo "-- 模板列表："
    cat "$work/templates-after-delete.txt"
  else
    echo "错误：读取模板列表失败：" >&2
    cat "$work/templates-after-delete.txt" >&2
    rc=1
  fi

  if [[ -n "$server_pid" ]]; then
    echo "-- 停止服务端 pid=$server_pid"
    if kill -0 "$server_pid" 2>/dev/null; then
      kill "$server_pid"
      for _ in $(seq 1 20); do
        kill -0 "$server_pid" 2>/dev/null || break
        sleep 0.5
      done
      if kill -0 "$server_pid" 2>/dev/null; then
        kill -9 "$server_pid"
      fi
    fi
    wait "$server_pid" 2>/dev/null || true
  fi

  echo "$rc" >"$work/exit-code"
  echo "==> 退出码 $rc，记录在 $work/exit-code"
  exit "$rc"
}
trap cleanup EXIT

for tool in docker go jq kubectl ss; do
  command -v "$tool" >/dev/null 2>&1 || { echo "错误：缺少 $tool" >&2; exit 1; }
done
kubectl ate --help >/dev/null 2>&1 || { echo "错误：kubectl-ate 不可用" >&2; exit 1; }

echo "==> 运行 $run_id"
echo "-- 检查开发环境"
ate_ctx get atespace "$atespace" >/dev/null 2>&1 || {
  echo "错误：atespace $atespace 不存在，先运行 deploy/dev/setup.sh" >&2
  exit 1
}
desired="$(kubectl_ctx -n castworks-system get workerpool castworks -o jsonpath='{.spec.replicas}')"
ready="$(kubectl_ctx -n castworks-system get workerpool castworks -o jsonpath='{.status.readyReplicas}')"
if [[ -z "$desired" || "$ready" != "$desired" ]]; then
  echo "错误：WorkerPool castworks 未就绪（ready=${ready:-0}, desired=${desired:-未知}）" >&2
  exit 1
fi
server_port="${server_addr##*:}"
if [[ ! "$server_port" =~ ^[0-9]+$ ]]; then
  echo "错误：无法从 NETPROBE_SERVER_ADDR=$server_addr 解析端口" >&2
  exit 1
fi
if ss -ltnH "sport = :$server_port" | grep -q .; then
  echo "错误：$server_addr 的端口 $server_port 已被占用" >&2
  exit 1
fi

echo "==> 构建并启动开发机侧服务端"
server_bin="$work/netprobe-server"
CGO_ENABLED=0 go -C "$script_dir" build -o "$server_bin" ./cmd/server
"$server_bin" -addr "$server_addr" >"$work/server.log" 2>&1 &
server_pid=$!
echo "$server_pid" >"$work/server.pid"
for _ in $(seq 1 50); do
  grep -q 'SERVER_READY' "$work/server.log" 2>/dev/null && break
  kill -0 "$server_pid" 2>/dev/null || { echo "错误：服务端启动失败" >&2; cat "$work/server.log" >&2; exit 1; }
  sleep 0.2
done
grep -q 'SERVER_READY' "$work/server.log" || { echo "错误：服务端未就绪" >&2; cat "$work/server.log" >&2; exit 1; }
cat "$work/server.log"

if [[ "${SKIP_IMAGE_BUILD:-0}" == 1 ]]; then
  : "${IMAGE_DIGEST:?SKIP_IMAGE_BUILD=1 时需要 IMAGE_DIGEST}"
  digest="$IMAGE_DIGEST"
  echo "==> 复用镜像 digest $digest"
else
  echo "==> 构建 linux/arm64 探针镜像并推送"
  build_ctx="$work/buildctx"
  mkdir -p "$build_ctx"
  CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go -C "$script_dir" build -o "$build_ctx/netprobe" ./cmd/client
  tag="$repo:$run_id"
  docker buildx build --platform linux/arm64 -f "$script_dir/Dockerfile" -t "$tag" --push "$build_ctx"
  # The push creates an OCI index with the arm64 image and an attestation
  # manifest. Reference the arm64 image manifest itself, not the index.
  manifest_json="$(docker buildx imagetools inspect "$tag" --raw)"
  digest="$(printf '%s' "$manifest_json" | jq -r \
    'if .manifests then (.manifests[] | select(.platform.architecture == "arm64" and .platform.os == "linux") | .digest) else .digest end' \
    | head -n1)"
  echo "-- 镜像 ${tag}"
fi
if [[ ! "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  echo "错误：无法解析 arm64 镜像 digest（得到 ${digest:-空}）" >&2
  exit 1
fi
image_ref="$repo@$digest"
short="${digest#sha256:}"
short="${short:0:12}"
template="cw-$short"
echo "-- 镜像引用 $image_ref"
echo "-- 模板名 $template"

echo "==> 创建临时 ActorTemplate（关闭模板快照）"
rendered="$work/actor-template.yaml"
sed -e "s|__TEMPLATE__|$template|g" -e "s|__IMAGE__|$image_ref|g" \
  -e "s|__ATESPACE__|$atespace|g" \
  "$script_dir/actor-template.yaml" >"$rendered"
if ate_ctx get actor-template "$template" -a "$atespace" -o json >"$work/template-get.json" 2>"$work/template-get.err"; then
  echo "-- 模板已存在，复用（本次运行不删除）"
elif grep -q 'code = NotFound' "$work/template-get.err"; then
  ate_ctx create actor-template -f "$rendered"
  template_created=1
else
  echo "错误：查询模板 $template 失败：" >&2
  cat "$work/template-get.err" >&2
  exit 1
fi

echo "==> 创建临时 actor $actor"
ate_ctx create actor "$actor" -a "$atespace" --template "$template"
actor_created=1
echo "==> 先建 EgressPolicy，再冷启动 actor"
ate_ctx create egress-policy "$actor" -a "$atespace" -f "$script_dir/egress-policy.yaml"
ate_ctx get egress-policy "$actor" -a "$atespace" -o yaml >"$work/egress-policy-created.yaml" 2>&1 || true
ate_ctx resume actor "$actor" -a "$atespace"

echo "==> 等待 actor 输出 NETPROBE_DONE（最多 ${probe_timeout}s）"
: >"$work/client.log"
deadline=$((SECONDS + probe_timeout))
while ((SECONDS < deadline)); do
  # kubectl ate logs wraps every line as {"time":...,"message":"..."}. Keep
  # the last successful read; a transient failure must not erase the marker.
  if ate_ctx logs actor "$actor" -a "$atespace" --source containers \
    >"$work/client.log.tmp" 2>"$work/client-logs.err"; then
    mv "$work/client.log.tmp" "$work/client.log"
  fi
  if grep -q 'NETPROBE_DONE' "$work/client.log" 2>/dev/null; then
    break
  fi
  state="$(ate_ctx get actor "$actor" -a "$atespace" -o json 2>/dev/null | jq -r '.status.state // empty')"
  case "$state" in
  *CRASHED* | *FAILED* | *STOPPED*)
    echo "错误：actor 状态为 $state" >&2
    break
    ;;
  esac
  sleep 10
done

echo "==> actor 输出"
cat "$work/client.log"
jq -Rr 'fromjson? | .message? // empty' "$work/client.log" | grep '^NETPROBE_' >"$work/summary.txt" || true
cat "$work/summary.txt"
if ! grep -q 'NETPROBE_DONE' "$work/client.log"; then
  echo "错误：未等到 NETPROBE_DONE" >&2
  exit 1
fi
missing=0
for q in Q1 Q2 Q3 Q4 Q5; do
  if ! grep -Eq "^NETPROBE_$q (PASS|FAIL) " "$work/summary.txt"; then
    echo "错误：日志中缺少 $q 的 PASS/FAIL 结果" >&2
    missing=1
  fi
done
if [[ "$missing" == 1 ]]; then
  exit 1
fi
if grep -q '^NETPROBE_Q[0-9] FAIL' "$work/summary.txt"; then
  probe_failed=1
fi

echo "==> 服务端日志"
cat "$work/server.log"

if [[ "$probe_failed" == 1 ]]; then
  echo "==> 至少一个问题未通过，见 $work/summary.txt" >&2
  exit 1
fi
echo "==> 五个问题均得到结果且未报 FAIL，见 $work/summary.txt"
