#!/usr/bin/env bash
set -euo pipefail

# dokploy-tunnel — doktunnel installer for Linux and macOS.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/alebak/dokploy-tunnel/main/scripts/install.sh | bash
#
# Environment variables:
#   DOKTUNNEL_VERSION      Version to install (e.g. 0.1.0 or v0.1.0). Default: latest release.
#   DOKTUNNEL_INSTALL_DIR  Target directory. Default: ~/.local/bin (no sudo required).

GITHUB_OWNER="alebak"
GITHUB_REPO="dokploy-tunnel"
BINARY_NAME="doktunnel"

if [ -t 1 ]; then
    GREEN='\033[0;32m'
    RED='\033[0;31m'
    CYAN='\033[0;36m'
    BOLD='\033[1m'
    NC='\033[0m'
else
    GREEN='' RED='' CYAN='' BOLD='' NC=''
fi

info()    { printf '%b\n' "$*"; }
success() { printf '%b[ok]%b      %s\n' "$GREEN" "$NC" "$*"; }
error()   { printf '%b[error]%b   %s\n' "$RED" "$NC" "$*" >&2; }
die()     { error "$*"; exit 1; }

require() {
    command -v "$1" >/dev/null 2>&1 || die "Required command not found: $1"
}

detect_platform() {
    local os arch

    case "$(uname -s)" in
        Linux)  os="linux" ;;
        Darwin) os="darwin" ;;
        *)      die "Unsupported OS: $(uname -s). On Windows, use scripts/install.ps1." ;;
    esac

    case "$(uname -m)" in
        x86_64|amd64)  arch="amd64" ;;
        arm64|aarch64) arch="arm64" ;;
        *)             die "Unsupported architecture: $(uname -m)" ;;
    esac

    printf '%s_%s\n' "$os" "$arch"
}

# Resolves the latest release through the github.com redirect, which avoids the
# unauthenticated REST API rate limit.
get_latest_version() {
    local url tag
    url="$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
        "https://github.com/${GITHUB_OWNER}/${GITHUB_REPO}/releases/latest")" \
        || die "Failed to resolve the latest release from GitHub"
    tag="${url##*/}"

    case "$tag" in
        v[0-9]*) printf '%s\n' "${tag#v}" ;;
        *)       die "Could not determine the latest version (no published release yet?)" ;;
    esac
}

sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{print $1}'
    else
        die "Neither sha256sum nor shasum is available to verify the download"
    fi
}

download() {
    curl -fsSL --retry 3 -o "$2" "$1" || die "Failed to download $1"
}

# Global so the EXIT trap can clean it up.
WORK_DIR=""
cleanup() {
    if [ -n "$WORK_DIR" ]; then
        rm -rf "$WORK_DIR"
    fi
}
trap cleanup EXIT

main() {
    require curl
    require tar
    require awk
    require uname

    info "\n${CYAN}${BOLD}dokploy-tunnel — doktunnel installer${NC}\n"

    local platform version archive base_url install_dir expected actual
    platform="$(detect_platform)"

    if [ -n "${DOKTUNNEL_VERSION:-}" ]; then
        version="${DOKTUNNEL_VERSION#v}"
    else
        version="$(get_latest_version)"
    fi

    archive="${BINARY_NAME}_${version}_${platform}.tar.gz"
    base_url="https://github.com/${GITHUB_OWNER}/${GITHUB_REPO}/releases/download/v${version}"
    install_dir="${DOKTUNNEL_INSTALL_DIR:-${HOME}/.local/bin}"
    WORK_DIR="$(mktemp -d)"

    success "Platform: ${platform}"
    success "Version:  ${version}"

    info "\nDownloading ${archive}..."
    download "${base_url}/${archive}" "${WORK_DIR}/${archive}"
    download "${base_url}/checksums.txt" "${WORK_DIR}/checksums.txt"

    info "Verifying checksum..."
    expected="$(awk -v f="$archive" '$2 == f || $2 == "*"f {print $1}' "${WORK_DIR}/checksums.txt")"
    [ -n "$expected" ] || die "No checksum found for ${archive} in checksums.txt"
    actual="$(sha256_of "${WORK_DIR}/${archive}")"
    [ "$expected" = "$actual" ] || die "Checksum mismatch for ${archive} (expected ${expected}, got ${actual})"
    success "Checksum verified"

    info "Extracting..."
    tar -xzf "${WORK_DIR}/${archive}" -C "$WORK_DIR" "$BINARY_NAME" \
        || die "Archive does not contain ${BINARY_NAME}"

    mkdir -p "$install_dir" || die "Cannot create ${install_dir}"
    install -m 0755 "${WORK_DIR}/${BINARY_NAME}" "${install_dir}/${BINARY_NAME}" \
        || die "Cannot write to ${install_dir}; set DOKTUNNEL_INSTALL_DIR to a writable directory"

    info ""
    success "Installed ${BINARY_NAME} to ${install_dir}/${BINARY_NAME}"

    case ":${PATH}:" in
        *":${install_dir}:"*) ;;
        *)
            info "\n${CYAN}${install_dir} is not on your PATH. Add this to your shell profile:${NC}"
            info "  export PATH=\"${install_dir}:\$PATH\""
            ;;
    esac

    info "\nRun ${BOLD}${BINARY_NAME} --version${NC} to check the installation.\n"
}

main "$@"
