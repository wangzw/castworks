#!/usr/bin/env bash
# 删除本工作项创建的 castworks 开发环境对象，只动 castworks-system 命名空间与
# atespace castworks-dev。重复执行安全：不存在的对象跳过。
set -euo pipefail

script_dir="$(dirname -- "$(readlink -f -- "${BASH_SOURCE[0]}")")"
kube_context="${KUBE_CONTEXT:-wangzw@kubernetes}"
namespace="castworks-system"
atespace="castworks-dev"

kubectl_ctx() {
  kubectl --context "$kube_context" "$@"
}

ate_ctx() {
  kubectl ate "$@" --context "$kube_context"
}

if ! command -v jq >/dev/null 2>&1; then
  echo "错误：需要 jq 读取 actor 与模板列表" >&2
  exit 1
fi

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

status=0
atespace_exists || status=$?
case "$status" in
0)
  echo "==> 确认 atespace $atespace 中没有 actor 与模板"
  actor_json="$(ate_ctx get actor -a "$atespace" -o json)" || {
    echo "错误：读取 atespace $atespace 的 actor 失败" >&2
    exit 1
  }
  if ! actor_count="$(printf '%s' "$actor_json" | jq -r '.actors // [] | length')"; then
    echo "错误：解析 atespace $atespace 的 actor 列表失败" >&2
    exit 1
  fi
  if [[ "$actor_count" != "0" ]]; then
    echo "错误：atespace $atespace 中仍有 $actor_count 个 actor，先删除 actor 再运行 teardown" >&2
    exit 1
  fi
  template_json="$(ate_ctx get actor-template -a "$atespace" -o json)" || {
    echo "错误：读取 atespace $atespace 的 actor 模板失败" >&2
    exit 1
  }
  if ! template_count="$(printf '%s' "$template_json" | jq -r '.actorTemplates // [] | length')"; then
    echo "错误：解析 atespace $atespace 的模板列表失败" >&2
    exit 1
  fi
  if [[ "$template_count" != "0" ]]; then
    echo "错误：atespace $atespace 中仍有 $template_count 个 actor 模板，先删除模板再运行 teardown" >&2
    exit 1
  fi

  # 访问策略随 atespace 一起删除：ate-api 在删除 atespace 的同一事务里清掉它。
  # 先单独删策略，会在模板或 actor 挡住 atespace 删除时留下一个没有策略的
  # atespace，所以这里直接删 atespace。
  echo "==> 删除 atespace $atespace（访问策略随它一起删除）"
  ate_ctx delete atespace "$atespace"
  ;;
1)
  echo "atespace $atespace 不存在，跳过"
  ;;
*)
  exit 1
  ;;
esac

# --ignore-not-found 让“命名空间不存在”返回空输出与退出码 0；其它错误（例如
# context 写错）必须非零退出，不能当成已清理。
if ! ns_name="$(kubectl_ctx get namespace "$namespace" --ignore-not-found -o name 2>&1)"; then
  echo "错误：查询命名空间 $namespace 失败：$ns_name" >&2
  exit 1
fi
if [[ -z "$ns_name" ]]; then
  echo "命名空间 $namespace 不存在，跳过"
else
  echo "==> 删除命名空间 $namespace 中的对象"
  # 签名密钥文件 ~/.config/castworks/extsim-signing-key 留在开发机上，不随这些
  # 对象删除。
  kubectl_ctx delete -f "$script_dir/k8s/workerpool.yaml" --ignore-not-found --timeout=180s
  kubectl_ctx delete -f "$script_dir/k8s/extsim-endpointslice.yaml" --ignore-not-found
  kubectl_ctx delete -f "$script_dir/k8s/endpointslice.yaml" --ignore-not-found
  kubectl_ctx delete -f "$script_dir/k8s/extsim-service.yaml" --ignore-not-found
  kubectl_ctx delete -f "$script_dir/k8s/service.yaml" --ignore-not-found
  kubectl_ctx delete -f "$script_dir/k8s/serviceaccount.yaml" --ignore-not-found

  echo "==> 删除命名空间 $namespace"
  kubectl_ctx delete namespace "$namespace" --ignore-not-found --timeout=180s
fi

echo "==> 开发环境已清理"
