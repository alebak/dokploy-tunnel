# Image for doktunnel-companion and doktunnel-socket-proxy.
#
# This Dockerfile is used by GoReleaser (dockers_v2 in .goreleaser.yaml), not
# by a plain "docker build": GoReleaser builds the binaries first and puts
# them in a temporary build context as <os>/<arch>/<binary>, so nothing is
# compiled here. It lives under build/ rather than at the repository root so
# that "docker build ." is not mistaken for a supported way to build it.
#
# Both binaries run from this one image:
#   - doktunnel-companion is the default entrypoint.
#   - doktunnel-socket-proxy runs with --entrypoint doktunnel-socket-proxy
#     (Compose: entrypoint: ["doktunnel-socket-proxy"]).
#
# Everything runs as the distroless "nonroot" user (65532). The socket proxy
# needs read/write access to the Docker socket, which is usually owned by
# root:docker; give the proxy container the socket's group with Compose
# "group_add" (a numeric GID, since the image has no "docker" group) instead
# of running it as root.

# gcr.io/distroless/static-debian13:nonroot (multi-arch index).
FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3 AS base

FROM base

ARG TARGETPLATFORM

COPY $TARGETPLATFORM/doktunnel-companion $TARGETPLATFORM/doktunnel-socket-proxy /usr/local/bin/

# The repeater key proves which repeater containers this companion created,
# so it must outlive the container: keep it on a volume. The directory is
# created owned by nonroot (copying the empty home directory of the base
# image), and Docker copies that ownership into a new volume mounted there.
COPY --from=base --chown=65532:65532 /home/nonroot/ /var/lib/doktunnel/
VOLUME ["/var/lib/doktunnel"]
ENV DOKTUNNEL_COMPANION_REPEATER_KEY_FILE=/var/lib/doktunnel/repeater.key

USER 65532:65532

# doktunnel-companion's default --listen; doktunnel-socket-proxy listens on
# 2375, which must stay on a network only the companion joins and is
# deliberately not exposed.
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/doktunnel-companion"]
