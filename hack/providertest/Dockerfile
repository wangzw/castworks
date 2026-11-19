# Integration-test image for the sandboxprovider tests. Build it for
# linux/arm64 only:
#
#   hack/providertest/build.sh
#
# The build context is prepared by build.sh: it holds the statically linked
# linux/arm64 binary named "providertest" and a state/ directory. The image
# runs as the distroless unprivileged user (65532), the same base as
# hack/netprobe/Dockerfile. /var/lib/providertest is owned by that user
# because the program writes its random state file there, on the root
# filesystem rather than /tmp.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --chown=65532:65532 state/ /var/lib/providertest/
COPY providertest /providertest
ENTRYPOINT ["/providertest"]
