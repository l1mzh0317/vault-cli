#!/bin/sh
# Python-free desktop install for the vault.
#
# Claude Code's DESKTOP app can't load plugins from a custom marketplace
# (marketplace-add is CLI-only; the desktop skill loader is broken for custom
# marketplaces — anthropics/claude-code#39897, closed not-planned). So instead
# of installing the plugin, this installs the `vault` CLI (one static binary,
# no runtime) and wires Claude Code to it. Everything below is Go + sh — no
# python, no plugin system.
#
#   curl -fsSL https://raw.githubusercontent.com/l1mzh0317/vault-plugin/main/desktop-setup.sh | sh -s -- <base-url> <token>
#   ./desktop-setup.sh                 # use the vault already in ~/.vault registry
#   ./desktop-setup.sh --uninstall     # remove the CC integration (keeps binary)
#
# It installs:
#   1. the `vault` binary          → ~/.local/bin (override with INSTALL_DIR)
#   2. the vault MCP server        → ~/.claude.json   (reads in a model)
#   3. auto-sync hooks             → ~/.claude/settings.json (`vault sync`)
set -eu

REPO="l1mzh0317/vault-plugin"
DIR="${INSTALL_DIR:-$HOME/.local/bin}"
VAULT="$DIR/vault"

if [ "${1:-}" = "--uninstall" ]; then
  [ -x "$VAULT" ] || { echo "✗ vault CLI not found at $VAULT" >&2; exit 1; }
  "$VAULT" setup --uninstall
  echo "Done. Restart Claude Code. (binary kept — remove with: rm $VAULT)"
  exit 0
fi

# 1. install the binary if missing
if [ ! -x "$VAULT" ]; then
  echo "Installing vault CLI…"
  curl -fsSL "https://raw.githubusercontent.com/${REPO}/main/cli/install.sh" | INSTALL_DIR="$DIR" sh
fi

# 2. register the vault if a url + token were provided (else use the registry)
if [ "$#" -ge 2 ] && [ "${1#--}" = "$1" ]; then
  "$VAULT" config add default "$1" "$2"
fi

# 3. wire Claude Code: MCP server + auto-sync hooks (all in Go, no python)
"$VAULT" setup

echo ""
echo "Done. Restart Claude Code."
echo "  • Reads (sessions/ls/find/cat…) via the 'vault' MCP server, or the CLI"
echo "  • Writes/sync via the vault CLI (token-free, never through the model)"
echo "Undo: ./desktop-setup.sh --uninstall"
