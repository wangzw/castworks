#!/usr/bin/env bash
# Build and push the integration-test image for the sandboxprovider tests.
#
# It cross-compiles the statically linked linux/arm64 binary, builds the image
# for linux/arm64 only, pushes it to
# registry.zhanwei.wang/castworks/providertest, and prints the arm64 image
# manifest reference as name@sha256:<digest>. The reference is the arm64
# manifest, not the multi-platform index, because the actor template must
# reference a single image.
#
# Everything it writes goes under /tmp/castworks-scratch/substrate-testimage/.
# PROVIDERTEST_REPO, PROVIDERTEST_TAG and PROVIDERTEST_SCRATCH override the
# repo, the tag and the scratch root.
set -euo pipefail

script_dir="$(dirname -- "$(readlink -f -- "${BASH_SOURCE[0]}")")"
repo="${PROVIDERTEST_REPO:-registry.zhanwei.wang/castworks/providertest}"
scratch_root="${PROVIDERTEST_SCRATCH:-/tmp/castworks-scratch/substrate-testimage}"
tag="${PROVIDERTEST_TAG:-$(date -u +%Y%m%dT%H%M%SZ | tr '[:upper:]' '[:lower:]')}"
work="$scratch_root/$tag"

for tool in docker go jq; do
  command -v "$tool" >/dev/null 2>&1 || { echo "错误：缺少 $tool" >&2; exit 1; }
done
docker buildx version >/dev/null 2>&1 || { echo "错误：docker buildx 不可用" >&2; exit 1; }

build_ctx="$work/buildctx"
mkdir -p "$build_ctx/state"
: >"$build_ctx/state/.keep"

echo "==> 交叉编译 linux/arm64 静态二进制"
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go -C "$script_dir" build -trimpath -o "$build_ctx/providertest" .

echo "==> 构建并推送 linux/arm64 镜像 $repo:$tag"
docker buildx build --platform linux/arm64 -f "$script_dir/Dockerfile" -t "$repo:$tag" --push "$build_ctx"

# The push creates an OCI index with the arm64 image and an attestation
# manifest. Reference the arm64 image manifest itself, not the index.
manifest_json="$(docker buildx imagetools inspect "$repo:$tag" --raw)"
digest="$(printf '%s' "$manifest_json" | jq -r \
  'if .manifests then (.manifests[] | select(.platform.architecture == "arm64" and .platform.os == "linux") | .digest) else .digest end' \
  | head -n1)"
if [[ ! "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  echo "错误：无法解析 arm64 镜像 digest（得到 ${digest:-空}）" >&2
  exit 1
fi

image_ref="$repo@$digest"
echo "$image_ref" >"$work/image-ref"
echo "$image_ref"
