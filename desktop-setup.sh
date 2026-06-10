#!/usr/bin/env bash
# Vault — non-plugin installer ("desktop" path).
#
# Claude Code's DESKTOP app cannot load plugins from a custom GitHub
# marketplace (the marketplace-add step is CLI-only and the desktop skill
# loader is broken for custom marketplaces — Anthropic issue #39897, closed
# as not-planned). So instead of installing the *plugin*, this wires up the
# same three capabilities the plugin ships, each via a mechanism the desktop
# app DOES support. Works on the CLI too.
#
#   1. Vault MCP server  -> user-scope mcpServers in ~/.claude.json
#   2. Skills            -> ~/.claude/skills/ (personal skills load on desktop)
#   3. Session-sync hook -> "hooks" block in ~/.claude/settings.json (local)
#
# Usage:
#   ./desktop-setup.sh                  # url+token from ~/.vault/servers.json
#   ./desktop-setup.sh <base-url> <token>   # explicit; url WITHOUT /mcp
#   ./desktop-setup.sh --uninstall      # remove everything this installed
#
# Requires: python3 (the sync engine needs it too). Restart Claude Code after.
set -uo pipefail
# Prefer system python over Anaconda (whose libs have broken TLS), keep $PATH.
export PATH="/usr/bin:/bin:$PATH"

SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HOOKS_SRC="$SRC_DIR/hooks"
SKILLS_SRC="$SRC_DIR/skills"

CLAUDE_DIR="$HOME/.claude"
SKILLS_DST="$CLAUDE_DIR/skills"
SETTINGS="$CLAUDE_DIR/settings.json"
CLAUDE_JSON="$HOME/.claude.json"
HOOKS_DST="$HOME/.vault/hooks"
REGISTRY="$HOME/.vault/servers.json"
LOG_FLAG="$HOME/.vault-logging-on"
SKILLS=(vault-mcp vault-manager)   # plugin-update is plugin-only; skip it
HOOK_CMD_TAG="/.vault/hooks/session-log.sh"   # how we recognize our hook entry

command -v python3 >/dev/null 2>&1 || { echo "✗ python3 not found (required)"; exit 1; }

# ── uninstall ────────────────────────────────────────────────────────────────
if [ "${1:-}" = "--uninstall" ]; then
  echo "Vault desktop install — uninstalling…"
  python3 - "$CLAUDE_JSON" "$SETTINGS" "$HOOK_CMD_TAG" <<'PY'
import json, os, sys
cj, settings, tag = sys.argv[1], sys.argv[2], sys.argv[3]
def load(p):
    try: return json.load(open(p, encoding="utf-8"))
    except Exception: return None
def save(p, d):
    tmp = p + ".tmp"; json.dump(d, open(tmp, "w", encoding="utf-8"),
                                ensure_ascii=False, indent=2); os.replace(tmp, p)
d = load(cj)
if d and "vault" in (d.get("mcpServers") or {}):
    del d["mcpServers"]["vault"]; save(cj, d); print("  - removed MCP server 'vault'")
s = load(settings)
if s and isinstance(s.get("hooks"), dict):
    changed = False
    for ev in ("SessionStart", "Stop"):
        blocks = s["hooks"].get(ev, [])
        kept = [b for b in blocks
                if not any(tag in (h.get("command", "") if isinstance(h, dict) else "")
                           for h in (b.get("hooks", []) if isinstance(b, dict) else []))]
        if len(kept) != len(blocks):
            s["hooks"][ev] = kept; changed = True
    if changed: save(settings, s); print("  - removed session-sync hooks")
PY
  for sk in "${SKILLS[@]}"; do
    [ -d "$SKILLS_DST/$sk" ] && rm -rf "$SKILLS_DST/$sk" && echo "  - removed skill $sk"
  done
  [ -d "$HOOKS_DST" ] && rm -rf "$HOOKS_DST" && echo "  - removed $HOOKS_DST"
  echo "Done. Restart Claude Code. (kept ~/.vault/servers.json and the log flag)"
  exit 0
fi

# ── resolve url + token ──────────────────────────────────────────────────────
BASE=""; TOKEN=""
if [ $# -ge 2 ] && [ "${1#--}" = "$1" ]; then
  BASE="$1"; TOKEN="$2"
else
  read -r BASE TOKEN < <(python3 - "$REGISTRY" <<'PY'
import json, os, sys
try:
    d = json.load(open(sys.argv[1], encoding="utf-8"))
    s = (d.get("servers") or {}).get(d.get("active")) or {}
    print(s.get("url", ""), s.get("token", ""))
except Exception:
    print("", "")
PY
  )
fi
# normalize: strip trailing slash and a trailing /mcp -> base URL
BASE="$(printf '%s' "$BASE" | sed -e 's#/*$##' -e 's#/mcp$##')"
if [ -z "$BASE" ] || [ -z "$TOKEN" ]; then
  echo "✗ no vault url/token. Pass them: ./desktop-setup.sh <base-url> <token>"
  echo "  (or register one first via the vault-manager skill)"
  exit 1
fi
echo "Vault desktop install → $BASE"

# ── 1. MCP server (user scope, read by all surfaces incl. desktop) ───────────
python3 - "$CLAUDE_JSON" "$BASE/mcp" "$TOKEN" <<'PY'
import json, os, sys
p, url, token = sys.argv[1], sys.argv[2], sys.argv[3]
try: d = json.load(open(p, encoding="utf-8"))
except Exception: d = {}
d.setdefault("mcpServers", {})
d["mcpServers"]["vault"] = {"type": "http", "url": url,
                            "headers": {"Authorization": "Bearer " + token}}
tmp = p + ".tmp"; json.dump(d, open(tmp, "w", encoding="utf-8"),
                            ensure_ascii=False, indent=2); os.replace(tmp, p)
print("  ✓ MCP server 'vault' →", url)
PY

# ── 2. skills (personal skills load on desktop) ──────────────────────────────
mkdir -p "$SKILLS_DST"
for sk in "${SKILLS[@]}"; do
  if [ -d "$SKILLS_SRC/$sk" ]; then
    rm -rf "$SKILLS_DST/$sk"
    cp -R "$SKILLS_SRC/$sk" "$SKILLS_DST/$sk"
    [ -f "$SKILLS_DST/$sk/$sk" ] && chmod +x "$SKILLS_DST/$sk/$sk"
    echo "  ✓ skill → ~/.claude/skills/$sk  (use /$sk)"
  fi
done

# ── 3. session-sync hook (settings.json hooks; runs locally on desktop) ──────
mkdir -p "$HOOKS_DST"
cp "$HOOKS_SRC/session-log.sh" "$HOOKS_SRC/vault_sync.py" "$HOOKS_DST/"
chmod +x "$HOOKS_DST/session-log.sh"
touch "$LOG_FLAG"
python3 - "$SETTINGS" "$HOOKS_DST/session-log.sh" <<'PY'
import json, os, sys
settings, script = sys.argv[1], sys.argv[2]
try: s = json.load(open(settings, encoding="utf-8"))
except Exception: s = {}
hooks = s.setdefault("hooks", {})
def ensure(event, arg):
    cmd = f'"{script}" {arg}'
    blocks = hooks.setdefault(event, [])
    for b in blocks:
        for h in (b.get("hooks", []) if isinstance(b, dict) else []):
            if isinstance(h, dict) and script in h.get("command", ""):
                h["command"] = cmd  # refresh path, stay idempotent
                return
    blocks.append({"matcher": "*",
                   "hooks": [{"type": "command", "command": cmd}]})
ensure("SessionStart", "start")
ensure("Stop", "stop")
tmp = settings + ".tmp"; json.dump(s, open(tmp, "w", encoding="utf-8"),
                                   ensure_ascii=False, indent=2)
os.replace(tmp, settings)
print("  ✓ hooks → SessionStart + Stop in settings.json")
PY

echo ""
echo "Done. Restart Claude Code (desktop or CLI) to load everything."
echo "  • Vault tools (sync_session, list_sessions, …) via the 'vault' MCP server"
echo "  • Skills: /vault-mcp, /vault-manager"
echo "  • Auto session-sync on start/stop (toggle metadata: touch ~/.vault-session-meta-on)"
echo "Undo with: ./desktop-setup.sh --uninstall"
