#!/usr/bin/env bash
#
# Builds the companion image from this checkout, for the runner's
# architecture only, and loads it into the local Docker daemon as
# ghcr.io/alebak/doktunnel-companion:0.0.0-e2e. Dokploy's "docker compose up"
# then uses it without pulling.
#
# build/companion.Dockerfile expects GoReleaser's context layout, with the
# binaries already built under <os>/<arch>/, so the same layout is made here.

set -euo pipefail
# shellcheck source=test/e2e/lib.sh
source "$(dirname "$0")/lib.sh"

arch="$(go env GOARCH)"
context="$E2E_DIR/image-context"
rm -rf "$context"
mkdir -p "$context/linux/$arch"

ldflags="-s -w -X github.com/alebak/dokploy-tunnel/internal/version.Version=$COMPANION_VERSION"
for binary in doktunnel-companion doktunnel-socket-proxy; do
	log "building $binary for linux/$arch"
	CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags "$ldflags" \
		-o "$context/linux/$arch/$binary" "./cmd/$binary"
done

image="$COMPANION_IMAGE:$COMPANION_VERSION"
docker build --platform "linux/$arch" -f build/companion.Dockerfile -t "$image" "$context"

version="$(docker run --rm "$image" --version)"
[[ $version == *"$COMPANION_VERSION"* ]] || fail "the image's companion reports '$version'"
pass "companion image $image built from this checkout ($version)"
