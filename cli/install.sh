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
  *) echo "✗ unsupported OS: $os — on Windows download vault-windows-amd64.exe from Releases" >&2; exit 1 ;;
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

# install the `vault` skill (markdown only) so Claude knows the CLI exists.
# Set NO_SKILL=1 to skip.
if [ "${NO_SKILL:-}" != "1" ]; then
  skill_dir="$HOME/.claude/skills/vault"
  mkdir -p "$skill_dir"
  if curl -fsSL "https://raw.githubusercontent.com/${REPO}/main/cli/skill/SKILL.md" \
       -o "$skill_dir/SKILL.md.tmp"; then
    mv "$skill_dir/SKILL.md.tmp" "$skill_dir/SKILL.md"
    echo "✓ skill → ${skill_dir}/SKILL.md  (restart Claude Code to load /vault)"
  else
    rm -f "$skill_dir/SKILL.md.tmp"
    echo "  (skill download skipped — binary still works)"
  fi
fi

"$DIR/vault" version || true
