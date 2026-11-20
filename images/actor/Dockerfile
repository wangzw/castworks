# actor 镜像：看守进程与 Claude Code 的运行环境。
#
# 只构建 linux/arm64。构建上下文由 build.sh 准备，其中只有名为 warden 与
# extsim 的两个静态链接 arm64 二进制。基础镜像固定到 Node 24 的 Debian
# bookworm slim 官方索引（index）的 digest，node 与 npm 由它提供。
FROM node:24-bookworm-slim@sha256:dc26ae92c9fb15cf6b1d63e20f0944b2efe0f115b20e00be48fb23fceb061c54

# 镜像里的固定 PATH。看守进程在 actor 模式把它注入 Claude Code 的环境，
# 驱动的常量必须与这一行一致。
ENV PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates procps \
 && rm -rf /var/lib/apt/lists/*

# Claude Code 的版本写死在这里，构建期校验；升级时改版本号与校验两处。
# 显式允许这个包的 postinstall：它把平台原生二进制链接到 bin/claude.exe。
RUN npm_config_update_notifier=false npm install -g --allow-scripts=@anthropic-ai/claude-code @anthropic-ai/claude-code@2.1.328 \
 && version="$(claude --version)" \
 && echo "claude --version: ${version}" \
 && case "${version}" in \
        2.1.328|2.1.328\ *) ;; \
        *) echo "错误：claude --version 不是 2.1.328：${version}" >&2; exit 1 ;; \
    esac

# 非 root 用户 agent，uid 与 gid 都是 10001。
RUN groupadd --gid 10001 agent \
 && useradd --uid 10001 --gid 10001 --create-home --home-dir /home/agent --shell /bin/bash agent

COPY --chmod=0755 warden /usr/local/bin/warden
COPY --chmod=0755 extsim /usr/local/bin/extsim

# extsim 命令行的服务地址与项目。看守进程只把固定的几个环境变量交给
# Claude Code，actor 里靠这个文件确定地址；开发机上的本地模式没有这个文件，
# 取默认的回环地址。
RUN printf '%s\n' '{"url": "http://extsim.castworks-system.svc.cluster.local:9100", "project": "demo"}' > /etc/extsim.json \
 && chmod 0644 /etc/extsim.json

# 不在镜像中设置任何凭据或 ANTHROPIC_*、CLAUDE_* 变量。
USER agent
WORKDIR /home/agent

ENTRYPOINT ["/usr/local/bin/warden"]
