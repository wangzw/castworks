# Sandbox-side probe image. Build it for linux/arm64 only:
#
#   docker buildx build --platform linux/arm64 \
#     -f hack/netprobe/Dockerfile -t <repo>:<tag> --push <build-context>
#
# The build context is prepared by run.sh: it holds the statically linked
# linux/arm64 binary named "netprobe". The binary is built on the host with
# CGO_ENABLED=0, so it runs in a scratch-like image; distroless static
# supplies the CA roots used by Q3 and an unprivileged user (65532).
FROM gcr.io/distroless/static-debian12:nonroot
COPY netprobe /netprobe
ENTRYPOINT ["/netprobe"]
