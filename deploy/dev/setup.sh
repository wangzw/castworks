#!/usr/bin/env bash
# 建立 castworks 开发环境在本机集群中的对象。重复执行安全：已存在的对象要么跳过，
# 要么按清单中的期望状态更新。
set -euo pipefail

script_dir="$(dirname -- "$(readlink -f -- "${BASH_SOURCE[0]}")")"
kube_context="${KUBE_CONTEXT:-wangzw@kubernetes}"
namespace="castworks-system"
atespace="castworks-dev"
workerpool="castworks"
worker_ready_timeout="${WORKER_READY_TIMEOUT:-300}"
# EndpointSlice 与探针服务端都写死了这个地址；开发机换了地址时清单也要改。
dev_host="10.12.72.11"

kubectl_ctx() {
  kubectl --context "$kube_context" "$@"
}

ate_ctx() {
  kubectl ate "$@" --context "$kube_context"
}

for tool in ip jq; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "错误：需要 $tool" >&2
    exit 1
  fi
done

if ! ip -o -4 addr show 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | grep -qx "$dev_host"; then
  echo "错误：本机没有地址 $dev_host，EndpointSlice 与探针服务端都写死了它" >&2
  exit 1
fi

# 控制面用开发机的 kubeconfig 经端口转发调用 ate-api，并按 TokenRequest 为
# castworks-system/sandboxprovider 签发令牌。这些权限不属于本目录创建的对象，
# 缺了要到运行时才暴露，所以先预检。
require_can_i() {
  local description="$1"
  shift
  local answer
  answer="$(kubectl_ctx auth can-i "$@" 2>/dev/null)"
  if [[ "$answer" != "yes" ]]; then
    echo "错误：当前 kubeconfig 用户缺少权限：$description" >&2
    echo "      kubectl auth can-i $* 返回 ${answer:-空}" >&2
    exit 1
  fi
  echo "    可以：$description"
}

echo "==> 预检 kubeconfig 权限"
require_can_i "ate-system 中 Pod 的端口转发" create pods --subresource=portforward -n ate-system
require_can_i "读取 ClusterTrustBundle" get clustertrustbundles
require_can_i "为 sandboxprovider 签发 ServiceAccount 令牌" create serviceaccounts/token -n "$namespace"

# 返回 0 表示 atespace 存在，1 表示不存在，其它值表示 ate-api 调用失败。
# 只按 gRPC 状态码匹配，context 写错时报的 "context ... was not found" 不能
# 被当成 atespace 不存在。
atespace_exists() {
  local output
  if output="$(ate_ctx get atespace "$atespace" 2>&1)"; then
    return 0
  fi
  if [[ "$output" == *"code = NotFound"* ]]; then
    return 1
  fi
  printf '%s\n' "$output" >&2
  return 2
}

echo "==> 应用 Kubernetes 对象"
kubectl_ctx apply -f "$script_dir/k8s/namespace.yaml"
kubectl_ctx apply -f "$script_dir/k8s/serviceaccount.yaml"
kubectl_ctx apply -f "$script_dir/k8s/service.yaml"
kubectl_ctx apply -f "$script_dir/k8s/endpointslice.yaml"
kubectl_ctx apply -f "$script_dir/k8s/workerpool.yaml"

echo "==> 确保 atespace $atespace 存在"
status=0
atespace_exists || status=$?
case "$status" in
0)
  echo "atespace $atespace 已存在，跳过创建"
  ;;
1)
  ate_ctx create atespace "$atespace"
  ;;
*)
  exit 1
  ;;
esac

echo "==> 确保 atespace $atespace 的访问策略为期望内容"
policy_json="$(ate_ctx get access-policy -a "$atespace" -o json 2>/dev/null)" || {
  echo "错误：读取 atespace $atespace 的访问策略失败" >&2
  exit 1
}
if printf '%s' "$policy_json" | jq -e '.metadata.uid' >/dev/null 2>&1; then
  uid="$(printf '%s' "$policy_json" | jq -r '.metadata.uid')"
  version="$(printf '%s' "$policy_json" | jq -r '.metadata.version')"
  {
    printf 'metadata:\n  name: default\n  uid: %s\n  version: "%s"\n' "$uid" "$version"
    cat "$script_dir/access-policy.yaml"
  } | ate_ctx update access-policy -a "$atespace" -f -
else
  ate_ctx create access-policy -a "$atespace" -f "$script_dir/access-policy.yaml"
fi

echo "==> 等待 WorkerPool $workerpool 的 worker 就绪（最多 ${worker_ready_timeout}s）"
deadline=$((SECONDS + worker_ready_timeout))
while true; do
  desired="$(kubectl_ctx -n "$namespace" get workerpool "$workerpool" -o jsonpath='{.spec.replicas}' 2>/dev/null || true)"
  ready="$(kubectl_ctx -n "$namespace" get workerpool "$workerpool" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)"
  if [[ -n "$desired" && "$desired" != "0" && "${ready:-0}" == "$desired" ]]; then
    echo "WorkerPool $workerpool 就绪：$ready/$desired"
    break
  fi
  if ((SECONDS >= deadline)); then
    echo "错误：WorkerPool $workerpool 在 ${worker_ready_timeout}s 内未就绪（ready=${ready:-0}, desired=${desired:-未知}）" >&2
    exit 1
  fi
  sleep 3
done

echo "==> 开发环境已就绪"
