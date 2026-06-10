#!/usr/bin/env bash
# Vault session logger — thin wrapper around vault_sync.py (the real engine).
# CC calls this on SessionStart ("start") and Stop ("stop").
#
#   start   self-heal hooks if missing, then incremental scan
#   stop    incremental scan (final flush for the exiting session)
#   scan    incremental scan (manual/cron)
#   once          incremental scan + print summary
#   resync        forget state, re-upload everything + print summary
#   rebuild [id]  re-summarize refined face: current project's sessions, or one id
#   sync-rebuild  scan then rebuild (one step: push latest + refresh summary)
#   doctor        health check (no flag/token required to run)
#
# Enable logging:  touch ~/.vault-logging-on
# Disable:         rm ~/.vault-logging-on
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENGINE="$SCRIPT_DIR/vault_sync.py"
SETTINGS="$HOME/.claude/settings.json"

# Prefer system python/curl over Anaconda (whose libs broke TLS earlier).
export PATH="/usr/bin:/bin:$PATH"

mode="${1:-scan}"

# Native plugin: Claude Code auto-wires the SessionStart/Stop hooks from
# hooks/hooks.json — no install.sh self-heal needed (that's the manual-install path).

exec python3 "$ENGINE" "$@"
