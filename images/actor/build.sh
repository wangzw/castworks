#!/usr/bin/env bash
# 构建 actor 镜像，只构建 linux/arm64（开发机本身是 arm64）。
#
#   build.sh                          只构建，打本地标签 castworks/actor:dev
#   build.sh --push --tag <tag>       构建并推送 <tag>，最后一行输出
#                                     <仓库>@sha256:<arm64 镜像清单 digest>
#
# 推送后不再引用 buildx 产生的多平台索引：digest 取索引里 linux/arm64
# 一项的清单 digest，做法与 hack/netprobe/run.sh 相同。
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"

local_tag="castworks/actor:dev"
push=0
tag=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --push)
      push=1
      shift
      ;;
    --tag)
      [[ $# -ge 2 ]] || { echo "错误：--tag 缺少参数" >&2; exit 1; }
      tag="$2"
      shift 2
      ;;
    *)
      echo "错误：未知参数 $1" >&2
      exit 1
      ;;
  esac
done

if [[ "${push}" -eq 1 ]]; then
  [[ -n "${tag}" ]] || { echo "错误：--push 需要 --tag" >&2; exit 1; }
else
  [[ -n "${tag}" ]] || tag="${local_tag}"
fi

build_ctx="$(mktemp -d)"
trap 'rm -rf "${build_ctx}"' EXIT INT TERM HUP

echo "==> 编译 linux/arm64 的 warden 与 extsim"
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go -C "${repo_root}" build -trimpath -o "${build_ctx}/warden" ./cmd/warden
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go -C "${repo_root}" build -trimpath -o "${build_ctx}/extsim" ./cmd/extsim

echo "==> 构建 linux/arm64 的 actor 镜像 ${tag}"
build_args=(
  --platform linux/arm64
  --progress=plain
  -f "${script_dir}/Dockerfile"
  -t "${tag}"
)
if [[ "${push}" -eq 1 ]]; then
  docker buildx build "${build_args[@]}" --push "${build_ctx}"
else
  docker buildx build "${build_args[@]}" --load "${build_ctx}"
fi

if [[ "${push}" -eq 1 ]]; then
  repo="${tag%:*}"
  manifest_json="$(docker buildx imagetools inspect "${tag}" --raw)"
  digest="$(printf '%s' "${manifest_json}" | jq -r \
    'if .manifests then (.manifests[] | select(.platform.architecture == "arm64" and .platform.os == "linux") | .digest) else .digest end' \
    | head -n1)"
  if [[ ! "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    echo "错误：无法解析 linux/arm64 镜像清单的 digest（得到 ${digest:-空}）" >&2
    exit 1
  fi
  echo "${repo}@${digest}"
fi
