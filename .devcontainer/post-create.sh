#!/bin/bash
set -euo pipefail

# Persist ~/.local/bin on PATH for future shells (the home directory is a volume).
mkdir -p "$HOME/.local/bin"
if ! grep -qF '.local/bin' "$HOME/.bashrc" 2>/dev/null; then
  echo 'export PATH="$HOME/.local/bin:$PATH"' >> "$HOME/.bashrc"
fi

# GoReleaser, for local `goreleaser check` and snapshot builds.
if command -v goreleaser >/dev/null 2>&1; then
  echo "GoReleaser already installed"
else
  echo "Installing GoReleaser..."
  GOTOOLCHAIN=auto go install github.com/goreleaser/goreleaser/v2@latest
fi

# actionlint, for validating GitHub Actions workflows.
if ! command -v actionlint >/dev/null 2>&1; then
  echo "Installing actionlint..."
  GOTOOLCHAIN=auto go install github.com/rhysd/actionlint/cmd/actionlint@latest
fi

go mod download
