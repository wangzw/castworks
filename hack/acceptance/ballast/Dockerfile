# Memory-ballast actor image for the suspend/resume acceptance test.
#
# Image tag: registry.zhanwei.wang/castworks/ballast:<UTC timestamp>
# (BALLAST_REPO and BALLAST_TAG override the default; see build.sh).
#
# Build and push, from the repository root:
#
#   hack/acceptance/ballast/build.sh
#
# build.sh cross-compiles the static binary on the host, then runs:
#
#   CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go -C hack/acceptance/ballast build \
#     -trimpath -buildvcs=false -ldflags='-s -w' \
#     -o <context>/ballast .
#   docker buildx build --platform linux/arm64 \
#     -f hack/acceptance/ballast/Dockerfile \
#     -t <repo>:<tag> --push <context>
#
# The build context holds only the binary named "ballast". distroless static
# supplies an unprivileged user (65532) and a minimal filesystem. The binary
# needs no CA roots: it answers the wakeup probe as plain HTTP on 8081 and
# reports to the development host over cleartext HTTP/2 through the actor's
# egress policy, with no TLS anywhere.
FROM gcr.io/distroless/static-debian12:nonroot
COPY ballast /ballast
EXPOSE 8081
ENTRYPOINT ["/ballast"]
