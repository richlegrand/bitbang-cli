#!/bin/sh
#
# bitbang install script (Linux). Detects arch, downloads the matching
# binary from the latest GitHub release, verifies the SHA-256 checksum,
# and installs to ~/.local/bin.
#
# Common invocation:
#   curl -sSfL bitba.ng/install | sh
#
# Pinning a version or changing the install prefix:
#   curl -sSfL bitba.ng/install | sh -s -- --version v0.5.0
#   curl -sSfL bitba.ng/install | sh -s -- --prefix /usr/local/bin
#
# Audit-first variant (recommended in shared environments):
#   curl -sSfL bitba.ng/install -o install.sh
#   less install.sh
#   sh install.sh
#
# macOS and Windows builds are coming soon. Until then, check
# https://github.com/richlegrand/bitbang-cli/releases for status.
#
# Source: https://github.com/richlegrand/bitbang-cli/blob/main/install.sh

set -eu

REPO="richlegrand/bitbang-cli"
DEFAULT_PREFIX="${HOME}/.local/bin"

main() {
    version="latest"
    prefix="$DEFAULT_PREFIX"

    while [ $# -gt 0 ]; do
        case "$1" in
            --version)   version="$2"; shift 2 ;;
            --version=*) version="${1#--version=}"; shift ;;
            --prefix)    prefix="$2"; shift 2 ;;
            --prefix=*)  prefix="${1#--prefix=}"; shift ;;
            -h|--help)   usage; exit 0 ;;
            *) err "unknown option: $1"; usage; exit 2 ;;
        esac
    done

    require curl uname mkdir mv sha256sum

    check_linux_only
    arch="$(detect_arch)"
    asset="bitbang-linux-${arch}"

    if [ "$version" = "latest" ]; then
        base="https://github.com/${REPO}/releases/latest/download"
    else
        base="https://github.com/${REPO}/releases/download/${version}"
    fi

    tmp="$(mktemp -d)"
    trap 'rm -rf "$tmp"' EXIT INT TERM

    info "downloading ${asset} (${version})"
    if ! curl -fsSL "${base}/${asset}" -o "${tmp}/${asset}"; then
        err "download failed: ${base}/${asset}"
        err "check that a release exists for linux/${arch}"
        exit 1
    fi

    # Verify when checksums.txt is published alongside the assets. Format:
    #   <sha256 hex>  <asset name>
    # Once your release pipeline reliably publishes checksums.txt, change
    # the `warn` branch to `err; exit 1` to make verification mandatory.
    if curl -fsSL "${base}/checksums.txt" -o "${tmp}/checksums.txt" 2>/dev/null; then
        verify_checksum "${tmp}/${asset}" "${tmp}/checksums.txt" "$asset"
    else
        warn "no checksums.txt in release — skipping verification"
    fi

    chmod +x "${tmp}/${asset}"
    mkdir -p "$prefix"
    dest="${prefix}/bitbang"
    mv "${tmp}/${asset}" "$dest"
    info "installed: $dest"

    case ":$PATH:" in
        *:"$prefix":*) ;;
        *) warn "${prefix} is not on your PATH. Add it to your shell rc:"
           warn "  export PATH=\"${prefix}:\$PATH\"" ;;
    esac

    info "run 'bitbang --help' to get started"
}

check_linux_only() {
    case "$(uname -s)" in
        Linux) return ;;
        Darwin)
            err "macOS builds are coming soon."
            err "Check https://github.com/${REPO}/releases for updates."
            exit 1
            ;;
        MINGW*|CYGWIN*|MSYS*)
            err "Windows builds are coming soon."
            err "Check https://github.com/${REPO}/releases for updates."
            exit 1
            ;;
        *)
            err "unsupported OS: $(uname -s)"
            err "Check https://github.com/${REPO}/releases for available builds."
            exit 1
            ;;
    esac
}

detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64)  echo amd64 ;;
        aarch64|arm64) echo arm64 ;;
        armv7l|armv7)  echo armv7 ;;
        *) err "unsupported arch: $(uname -m)"; exit 1 ;;
    esac
}

verify_checksum() {
    file="$1"; sums="$2"; name="$3"
    expected="$(awk -v n="$name" '$2 == n {print $1; exit}' "$sums")"
    if [ -z "$expected" ]; then
        warn "checksums.txt has no entry for ${name} — skipping verification"
        return
    fi
    actual="$(sha256sum "$file" | awk '{print $1}')"
    if [ "$expected" != "$actual" ]; then
        err "checksum mismatch for ${name}"
        err "  expected: ${expected}"
        err "  actual:   ${actual}"
        exit 1
    fi
    info "checksum verified"
}

require() {
    for cmd; do
        command -v "$cmd" >/dev/null 2>&1 || { err "required command not found: $cmd"; exit 1; }
    done
}

info() { printf "bitbang: %s\n" "$*" >&2; }
warn() { printf "bitbang: %s\n" "$*" >&2; }
err()  { printf "bitbang: error: %s\n" "$*" >&2; }

usage() {
    cat >&2 <<EOF
bitbang installer — downloads the latest Linux release from GitHub.

macOS and Windows are coming soon; check
https://github.com/${REPO}/releases for status.

Usage:
  install.sh [--version vX.Y.Z] [--prefix DIR]

Options:
  --version VER   install a specific release tag (default: latest)
  --prefix DIR    install to DIR (default: ${DEFAULT_PREFIX})
  -h, --help      show this help
EOF
}

main "$@"
