#!/bin/sh
# Install the `vault` CLI from GitHub Releases — no Go, no runtime.
#
#   curl -fsSL https://raw.githubusercontent.com/l1mzh0317/vault-cli/main/cli/install.sh | sh
#
# Env overrides:
#   INSTALL_DIR=/usr/local/bin   where to put the binary (default ~/.local/bin)
#   VERSION=cli-v0.1.0           a specific release tag (default: latest)
set -eu

REPO="l1mzh0317/vault-cli"
DIR="${INSTALL_DIR:-$HOME/.local/bin}"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) echo "✗ unsupported arch: $arch" >&2; exit 1 ;;
esac
case "$os" in
  linux | darwin) ;;
  *) echo "✗ this script is for Linux/macOS. On Windows use PowerShell:" >&2
     echo "    irm https://raw.githubusercontent.com/${REPO}/main/cli/install.ps1 | iex" >&2
     exit 1 ;;
esac

asset="vault-${os}-${arch}"
if [ "${VERSION:-latest}" = "latest" ]; then
  url="https://github.com/${REPO}/releases/latest/download/${asset}"
else
  url="https://github.com/${REPO}/releases/download/${VERSION}/${asset}"
fi

echo "Downloading ${asset} → ${DIR}/vault"
mkdir -p "$DIR"
tmp="$(mktemp)"
if ! curl -fsSL "$url" -o "$tmp"; then
  echo "✗ download failed: $url" >&2
  echo "  (no release yet? check https://github.com/${REPO}/releases)" >&2
  rm -f "$tmp"; exit 1
fi
chmod +x "$tmp"
mv "$tmp" "$DIR/vault"

echo "✓ installed: ${DIR}/vault"
case ":$PATH:" in
  *":$DIR:"*) ;;
  *) echo "  note: ${DIR} is not on your PATH — add it to use \`vault\` directly" ;;
esac

# install the bundled `vault` skill from the binary (offline, version-matched).
# Set NO_SKILL=1 to skip.
if [ "${NO_SKILL:-}" != "1" ]; then
  "$DIR/vault" skill || echo "  (skill install skipped — binary still works)"
fi

"$DIR/vault" version || true
