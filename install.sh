#!/bin/sh
# odoo-cli installer. Detects OS + arch, downloads the matching
# release tarball from GitHub, verifies its SHA256 against the
# release's checksums.txt, and installs the `odoo` binary into
# PREFIX/bin (default: $HOME/.local/bin).
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/xdamman/odoo-cli/main/install.sh | sh
#
# Env vars:
#   VERSION   release tag to install (default: latest)
#   PREFIX    install prefix; binary goes to $PREFIX/bin (default: $HOME/.local)
#
# Re-running this script is the supported first-time install path.
# After install, `odoo update` is the supported upgrade path.

set -eu

REPO="xdamman/odoo-cli"
PREFIX="${PREFIX:-$HOME/.local}"
VERSION="${VERSION:-latest}"

# ── stdout helpers ────────────────────────────────────────────────
if [ -t 1 ]; then
  BOLD="$(printf '\033[1m')"
  DIM="$(printf '\033[2m')"
  GREEN="$(printf '\033[32m')"
  YELLOW="$(printf '\033[33m')"
  RED="$(printf '\033[31m')"
  RESET="$(printf '\033[0m')"
else
  BOLD=""; DIM=""; GREEN=""; YELLOW=""; RED=""; RESET=""
fi

info() { printf '%s● %s%s\n' "$DIM" "$1" "$RESET"; }
ok()   { printf '%s✓ %s%s\n' "$GREEN" "$1" "$RESET"; }
warn() { printf '%s! %s%s\n' "$YELLOW" "$1" "$RESET" >&2; }
fail() { printf '%s✗ %s%s\n' "$RED" "$1" "$RESET" >&2; exit 1; }

# ── prerequisite check ────────────────────────────────────────────
need() { command -v "$1" >/dev/null 2>&1 || fail "$1 is required but not installed"; }
need uname
need tar
need install
need mktemp
if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL "$1" -o "$2"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q -O "$2" "$1"; }
else
  fail "either curl or wget is required"
fi

# Pick the first available SHA256 tool — coreutils on Linux, openssl
# everywhere, shasum on macOS. The output format differs but all
# three put the digest as the first whitespace-delimited token.
if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
elif command -v openssl >/dev/null 2>&1; then
  sha256() { openssl dgst -sha256 "$1" | awk '{print $NF}'; }
else
  warn "no sha256 tool found (sha256sum / shasum / openssl) — skipping verification"
  sha256() { echo skip; }
fi

# ── platform detection ────────────────────────────────────────────
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$OS" in
  linux|darwin) ;;
  *) fail "unsupported OS: $OS (build from source: https://github.com/$REPO)" ;;
esac

ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) fail "unsupported arch: $ARCH (build from source)" ;;
esac

# Two naming schemes exist on the release page: the current workflow
# emits version-agnostic "odoo-<os>-<arch>.tar.gz"; earlier releases
# included the version in the filename. Try both so the installer
# works across the transition.
ASSET="odoo-${OS}-${ARCH}.tar.gz"

# ── resolve VERSION ───────────────────────────────────────────────
# Resolve "latest" to an explicit tag so the legacy-asset fallback
# has a concrete tag to substitute. One extra API call on the
# unauthenticated tier (60/hr/IP) — fine for a one-shot installer.
if [ "$VERSION" = "latest" ]; then
  TMP_TAG=$(mktemp)
  if fetch "https://api.github.com/repos/${REPO}/releases/latest" "$TMP_TAG" 2>/dev/null; then
    VERSION=$(awk -F'"' '/"tag_name":/ {print $4; exit}' "$TMP_TAG")
  fi
  rm -f "$TMP_TAG"
  [ -n "$VERSION" ] || fail "could not resolve latest release tag from GitHub API"
fi
# Normalise "0.0.1" → "v0.0.1" so users can pass either form.
case "$VERSION" in
  v*) ;;
  *) VERSION="v${VERSION}" ;;
esac
BASE="https://github.com/${REPO}/releases/download/${VERSION}"
LEGACY_ASSET="odoo-${VERSION}-${OS}-${ARCH}.tar.gz"
info "Installing ${VERSION} of ${REPO} for ${OS}/${ARCH}"

# ── download + verify ─────────────────────────────────────────────
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

info "Downloading ${ASSET}"
if ! fetch "${BASE}/${ASSET}" "${TMP}/${ASSET}" 2>/dev/null; then
  if [ -n "$LEGACY_ASSET" ] && fetch "${BASE}/${LEGACY_ASSET}" "${TMP}/${LEGACY_ASSET}" 2>/dev/null; then
    ASSET="$LEGACY_ASSET"
    info "(using legacy asset name ${LEGACY_ASSET})"
  else
    fail "download failed: ${BASE}/${ASSET}"
  fi
fi

info "Downloading checksums.txt"
if fetch "${BASE}/checksums.txt" "${TMP}/checksums.txt"; then
  EXPECTED=$(awk -v a="$ASSET" '$2==a || $2=="*"a {print $1; exit}' "${TMP}/checksums.txt")
  if [ -z "$EXPECTED" ]; then
    warn "checksum for ${ASSET} not listed — skipping verification"
  else
    GOT=$(sha256 "${TMP}/${ASSET}")
    if [ "$GOT" != "skip" ] && [ "$GOT" != "$EXPECTED" ]; then
      fail "checksum mismatch (expected ${EXPECTED}, got ${GOT})"
    fi
    ok "checksum verified"
  fi
else
  warn "checksums.txt not available — skipping verification"
fi

# ── extract + install ─────────────────────────────────────────────
info "Extracting"
tar -xzf "${TMP}/${ASSET}" -C "${TMP}" || fail "tar extraction failed"

# The tarball wraps the binary in a directory named after the asset
# (without .tar.gz). Both workflow generations follow that pattern.
BIN_SRC=$(find "$TMP" -type f -name odoo -perm -u+x 2>/dev/null | head -1)
[ -n "$BIN_SRC" ] && [ -f "$BIN_SRC" ] || fail "odoo binary not found in tarball"

BIN_DST_DIR="${PREFIX}/bin"
BIN_DST="${BIN_DST_DIR}/odoo"

info "Installing to ${BIN_DST}"
if ! install -d "$BIN_DST_DIR" 2>/dev/null; then
  # Couldn't create the bin dir as the current user; the most likely
  # cause is PREFIX pointing at a system path. Surface the sudo hint
  # rather than failing silently.
  fail "cannot create ${BIN_DST_DIR} (try: sudo PREFIX=/usr/local sh install.sh)"
fi
if ! install -m 0755 "$BIN_SRC" "$BIN_DST" 2>/dev/null; then
  fail "cannot write ${BIN_DST} (try: sudo PREFIX=/usr/local sh install.sh)"
fi

INSTALLED_VERSION=$("$BIN_DST" --version 2>/dev/null | awk '{print $2}' || echo unknown)
ok "Installed odoo ${INSTALLED_VERSION} to ${BIN_DST}"

# ── post-install checks ───────────────────────────────────────────
case ":$PATH:" in
  *:"$BIN_DST_DIR":*) ;;
  *)
    warn "${BIN_DST_DIR} is not on your \$PATH"
    printf '   Add this to your shell profile (~/.bashrc, ~/.zshrc, …):\n\n'
    printf '     %sexport PATH="%s:$PATH"%s\n\n' "$BOLD" "$BIN_DST_DIR" "$RESET"
    ;;
esac

printf '\n%sNext:%s odoo setup       (or %sodoo --help%s)\n\n' \
  "$BOLD" "$RESET" "$DIM" "$RESET"
