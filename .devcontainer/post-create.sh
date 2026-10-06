#!/bin/bash
set -euo pipefail

# Pinned tool versions. Prebuilt release binaries are downloaded instead of
# `go install`, which compiles from source and keeps several cores busy for
# minutes in every new container. They go to $(go env GOPATH)/bin, which the
# image puts on PATH for every user, like `go install` did; ~/.local/bin is
# only on PATH in interactive shells, so `docker exec` would not find them.
GORELEASER_VERSION=2.18.2
ACTIONLINT_VERSION=1.7.12

# Persist ~/.local/bin on PATH for future shells (the home directory is a volume).
mkdir -p "$HOME/.local/bin"
if ! grep -qF '.local/bin' "$HOME/.bashrc" 2>/dev/null; then
  echo 'export PATH="$HOME/.local/bin:$PATH"' >> "$HOME/.bashrc"
fi
export PATH="$HOME/.local/bin:$PATH"

tool_bin="$(go env GOPATH)/bin"
mkdir -p "$tool_bin"

# install_release <binary> <archive URL> <checksums URL>
# Downloads a release archive, checks it against the release's checksums file
# and installs <binary> from it into $tool_bin.
install_release() {
  local binary=$1 archive_url=$2 checksums_url=$3
  local tmp archive
  tmp=$(mktemp -d)
  archive=$(basename "$archive_url")
  curl -fsSL -o "$tmp/$archive" "$archive_url"
  curl -fsSL -o "$tmp/checksums.txt" "$checksums_url"
  if ! grep -E "^[0-9a-f]{64}  $archive\$" "$tmp/checksums.txt" > "$tmp/expected"; then
    echo "No checksum for $archive in $checksums_url" >&2
    rm -rf "$tmp"
    return 1
  fi
  (cd "$tmp" && sha256sum --check --strict --quiet expected)
  tar -xzf "$tmp/$archive" -C "$tmp" "$binary"
  install -m 0755 "$tmp/$binary" "$tool_bin/$binary"
  rm -rf "$tmp"
}

case "$(uname -m)" in
  x86_64) goreleaser_arch=x86_64 actionlint_arch=amd64 ;;
  aarch64 | arm64) goreleaser_arch=arm64 actionlint_arch=arm64 ;;
  *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

# GoReleaser, for local `goreleaser check` and snapshot builds.
if command -v goreleaser >/dev/null 2>&1; then
  echo "GoReleaser already installed"
else
  echo "Installing GoReleaser ${GORELEASER_VERSION}..."
  base="https://github.com/goreleaser/goreleaser/releases/download/v${GORELEASER_VERSION}"
  install_release goreleaser \
    "$base/goreleaser_Linux_${goreleaser_arch}.tar.gz" \
    "$base/checksums.txt"
fi

# actionlint, for validating GitHub Actions workflows.
if command -v actionlint >/dev/null 2>&1; then
  echo "actionlint already installed"
else
  echo "Installing actionlint ${ACTIONLINT_VERSION}..."
  base="https://github.com/rhysd/actionlint/releases/download/v${ACTIONLINT_VERSION}"
  install_release actionlint \
    "$base/actionlint_${ACTIONLINT_VERSION}_linux_${actionlint_arch}.tar.gz" \
    "$base/actionlint_${ACTIONLINT_VERSION}_checksums.txt"
fi

go mod download
