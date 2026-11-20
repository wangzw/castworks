#!/usr/bin/env bash
# actor 镜像冒烟：在本地镜像上检查镜像本身，不连接控制面。
#
#   smoke.sh [--image <tag>]
#
# 检查项：
#   - 镜像是 linux/arm64，装有 git；
#   - claude --version 为 2.1.328；
#   - extsim job --help 可执行，/etc/extsim.json 内容正确；
#   - actor 模式下 /readyz 在 10 s 内返回 200；
#   - 容器里的 PID 1 是看守进程；
#   - 看守进程以非 root 用户 agent（uid 10001）运行，其主目录与工作目录为 /home/agent；
#   - docker stop 在 15 s 内结束，容器退出码为 0；
#   - actor-name 不合法时看守进程以非零状态退出。
#
# 看守进程连不上控制面时按退避一直重连，因此冒烟只需要本机 docker，
# 控制面地址指向一个不可达的本地端口。
set -euo pipefail

image="${ACTOR_IMAGE:-castworks/actor:dev}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --image)
      [[ $# -ge 2 ]] || { echo "错误：--image 缺少参数" >&2; exit 1; }
      image="$2"
      shift 2
      ;;
    *)
      echo "错误：未知参数 $1" >&2
      exit 1
      ;;
  esac
done

for tool in docker curl awk; do
  command -v "${tool}" >/dev/null 2>&1 || { echo "错误：缺少 ${tool}" >&2; exit 1; }
done
docker image inspect "${image}" >/dev/null 2>&1 || {
  echo "错误：本地没有镜像 ${image}，先运行 make actor-image" >&2
  exit 1
}

work="$(mktemp -d)"
container="castworks-actor-smoke-$$"
container_started=0

cleanup() {
  local rc=$?
  set +e
  if [[ "${container_started}" -eq 1 ]]; then
    docker rm -f "${container}" >/dev/null 2>&1
  fi
  rm -rf "${work}"
  exit "${rc}"
}
trap cleanup EXIT INT TERM HUP

echo "==> 检查镜像架构与 git"
arch="$(docker image inspect -f '{{.Architecture}}' "${image}")"
[[ "${arch}" == "arm64" ]] || {
  echo "错误：镜像架构是 ${arch}，不是 arm64" >&2
  exit 1
}
echo "-- 镜像架构: ${arch}"
git_version="$(docker run --rm --entrypoint git "${image}" --version)"
[[ "${git_version}" == git\ version\ * ]] || {
  echo "错误：git --version 输出异常：${git_version}" >&2
  exit 1
}
echo "-- ${git_version}"

echo "==> 检查 Claude Code 版本"
version="$(docker run --rm --entrypoint claude "${image}" --version)"
case "${version}" in
  2.1.328|2.1.328\ *) ;;
  *)
    echo "错误：claude --version 不是 2.1.328：${version}" >&2
    exit 1
    ;;
esac
echo "-- claude --version: ${version}"

echo "==> 检查 extsim 命令行与配置"
expected_config='{"url": "http://extsim.castworks-system.svc.cluster.local:9100", "project": "demo"}'
actual_config="$(docker run --rm --entrypoint cat "${image}" /etc/extsim.json)"
[[ "${actual_config}" == "${expected_config}" ]] || {
  echo "错误：/etc/extsim.json 内容不符：${actual_config}" >&2
  exit 1
}
echo "-- /etc/extsim.json: ${actual_config}"

# 当前的命令行没有 --help 开关，job --help 被当作未知子命令拒绝并打印用法行；
# 输出里有这一行就说明二进制在镜像里可以执行，并分派到了 job 子命令。
set +e
job_help="$(docker run --rm --entrypoint extsim "${image}" job --help 2>&1)"
job_help_rc=$?
set -e
if ! grep -q 'want submit, get or watch' <<<"${job_help}"; then
  echo "错误：extsim job --help 不可执行或输出异常（退出码 ${job_help_rc}）：${job_help}" >&2
  exit 1
fi
echo "-- extsim job --help 可执行（退出码 ${job_help_rc}）"

echo "==> 启动 actor 模式容器"
printf 'cw-smoke-%s-%s' "$(date +%s)" "$$" >"${work}/actor-name"
chmod 0444 "${work}/actor-name"
docker run -d --name "${container}" \
  -e CASTWORKS_CONTROL_PLANE=127.0.0.1:1 \
  -p 127.0.0.1::8081 \
  -v "${work}/actor-name:/run/ate/actor-name:ro" \
  "${image}" >/dev/null
container_started=1

port="$(docker port "${container}" 8081/tcp | head -n1 | sed 's/.*://')"
[[ -n "${port}" ]] || { echo "错误：拿不到 8081 的映射端口" >&2; exit 1; }

echo "==> 等待 /readyz"
deadline=$((SECONDS + 10))
while ! curl -fsS "http://127.0.0.1:${port}/readyz" >/dev/null 2>&1; do
  if [[ "$(docker inspect -f '{{.State.Running}}' "${container}")" != "true" ]]; then
    echo "错误：容器在 /readyz 就绪前退出，日志：" >&2
    docker logs "${container}" >&2 || true
    exit 1
  fi
  if ((SECONDS >= deadline)); then
    echo "错误：10 s 内 /readyz 未返回 200，日志：" >&2
    docker logs "${container}" >&2 || true
    exit 1
  fi
  sleep 0.2
done
echo "-- /readyz 返回 200（127.0.0.1:${port}）"

echo "==> 检查 PID 1 与运行用户"
pid1_exe="$(docker exec "${container}" readlink /proc/1/exe)"
[[ "${pid1_exe}" == "/usr/local/bin/warden" ]] || {
  echo "错误：PID 1 不是看守进程：${pid1_exe}" >&2
  exit 1
}
echo "-- PID 1: ${pid1_exe}"
user_name="$(docker exec "${container}" id -un)"
user_id="$(docker exec "${container}" id -u)"
pid1_uid="$(docker exec "${container}" cat /proc/1/status | awk '/^Uid:/{print $2}')"
[[ "${user_name}" == "agent" ]] || {
  echo "错误：运行用户不是 agent：${user_name}" >&2
  exit 1
}
[[ "${user_id}" == "10001" && "${pid1_uid}" == "10001" ]] || {
  echo "错误：运行用户的 uid 不是 10001（exec=${user_id}, PID 1=${pid1_uid}）" >&2
  exit 1
}
echo "-- 运行用户: ${user_name}（uid ${user_id}，PID 1 uid ${pid1_uid}）"

echo "==> 检查用户主目录与工作目录"
agent_home="$(docker exec "${container}" getent passwd agent | cut -d: -f6)"
[[ "${agent_home}" == "/home/agent" ]] || {
  echo "错误：用户 agent 的主目录是 ${agent_home}，不是 /home/agent" >&2
  exit 1
}
home_env="$(docker exec "${container}" printenv HOME)"
[[ "${home_env}" == "/home/agent" ]] || {
  echo "错误：容器里的 HOME 是 ${home_env}，不是 /home/agent" >&2
  exit 1
}
workdir="$(docker exec "${container}" pwd)"
[[ "${workdir}" == "/home/agent" ]] || {
  echo "错误：容器的工作目录是 ${workdir}，不是 /home/agent" >&2
  exit 1
}
echo "-- 主目录 ${agent_home}，HOME ${home_env}，工作目录 ${workdir}"

echo "==> 检查 docker stop"
started_ms="$(date +%s%3N)"
docker stop -t 15 "${container}" >/dev/null
stopped_ms="$(date +%s%3N)"
elapsed_ms=$((stopped_ms - started_ms))
exit_code="$(docker inspect -f '{{.State.ExitCode}}' "${container}")"
[[ "${exit_code}" == "0" ]] || {
  echo "错误：docker stop 后容器退出码为 ${exit_code}，日志：" >&2
  docker logs "${container}" >&2 || true
  exit 1
}
((elapsed_ms <= 15000)) || {
  echo "错误：docker stop 用了 ${elapsed_ms} ms，超过 15 s" >&2
  exit 1
}
echo "-- docker stop 用了 ${elapsed_ms} ms，容器退出码 0"

echo "==> 检查非法 actor-name"
printf 'not-an-actor' >"${work}/bad-actor-name"
chmod 0444 "${work}/bad-actor-name"
set +e
output="$(docker run --rm --name "${container}-bad" \
  -e CASTWORKS_CONTROL_PLANE=127.0.0.1:1 \
  -v "${work}/bad-actor-name:/run/ate/actor-name:ro" \
  "${image}" 2>&1)"
rc=$?
set -e
if [[ "${rc}" -eq 0 ]]; then
  echo "错误：非法 actor-name 时看守进程以 0 退出，日志：" >&2
  printf '%s\n' "${output}" >&2
  exit 1
fi
if ! grep -q 'read actor identity' <<<"${output}"; then
  echo "错误：非零退出不是由 actor-name 解析失败引起，日志：" >&2
  printf '%s\n' "${output}" >&2
  exit 1
fi
echo "-- 非法 actor-name 时以退出码 ${rc} 退出"

echo "==> 冒烟通过：${image}"
