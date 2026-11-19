#!/usr/bin/env bash
# Builds and pushes the linux/arm64 memory-ballast image used by the
# suspend/resume acceptance test. The image answers the wakeup probe on 8081
# and reports to the development host through the Substrate egress gateway at
# castworks-dev.castworks-system.svc.cluster.local:9090.
#
# Image tag: ${BALLAST_REPO:-registry.zhanwei.wang/castworks/ballast}:${BALLAST_TAG:-<UTC timestamp>}
#
# The binary is built on the host with CGO_ENABLED=0, so the image stays
# static and distroless. This machine is aarch64, so linux/arm64 is a native
# build; the platform flag is explicit anyway so the manifest is arm64 on any
# host. The build context lives under
# /tmp/castworks-scratch/acceptance-ballast/<tag>/buildctx/.
#
# The last line printed is the arm64 image manifest reference:
#
#   registry.zhanwei.wang/castworks/ballast@sha256:<digest>
#
# That digest, not the OCI index digest, is the one to pass as the Substrate
# provider's Config.Image: the index also carries an attestation manifest.
set -euo pipefail

script_dir="$(dirname -- "$(readlink -f -- "${BASH_SOURCE[0]}")")"
repo="${BALLAST_REPO:-registry.zhanwei.wang/castworks/ballast}"
tag="${BALLAST_TAG:-$(date -u +%Y%m%dt%H%M%Sz | tr '[:upper:]' '[:lower:]')}"
scratch_root="${BALLAST_SCRATCH:-/tmp/castworks-scratch/acceptance-ballast}"
work="$scratch_root/$tag"

mkdir -p "$work/buildctx"

echo "==> 构建 linux/arm64 静态二进制"
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go -C "$script_dir" build \
	-trimpath -buildvcs=false -ldflags='-s -w' \
	-o "$work/buildctx/ballast" .

echo "==> 构建并推送镜像 $repo:$tag"
docker buildx build --platform linux/arm64 \
	-f "$script_dir/Dockerfile" \
	-t "$repo:$tag" --push "$work/buildctx"

# The push creates an OCI index with the arm64 image and an attestation
# manifest. Resolve the arm64 image manifest itself, not the index.
manifest_json="$(docker buildx imagetools inspect "$repo:$tag" --raw)"
digest="$(printf '%s' "$manifest_json" | jq -r \
	'if .manifests then (.manifests[] | select(.platform.architecture == "arm64" and .platform.os == "linux") | .digest) else .digest end' \
	| head -n1)"

if [[ ! "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
	echo "错误：无法解析 arm64 镜像 digest（得到 ${digest:-空}）" >&2
	exit 1
fi

echo "-- 镜像 $repo:$tag"
echo "-- 构建上下文 $work/buildctx"
echo "$repo@$digest"
