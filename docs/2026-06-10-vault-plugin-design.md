# Vault Plugin — Design

**Date:** 2026-06-10
**Goal:** Package the existing vault client stack (skills + session-sync hook) as a
native Claude Code plugin, distributed from a public GitHub repo so anyone can
`/plugin install` it.

## Decisions

| # | Decision | Choice |
|---|---|---|
| 1 | Packaging granularity | **Single bundled plugin** (`vault`) containing both skills + the hook |
| 2 | Where it lives | **New public repo `l1mzh0317/vault-plugin`** (the `vault` server repo is private; marketplace must be public) |
| 3 | Config source of truth | **`vault-manager` registry** (`~/.vault/servers.json`) — changeable at runtime, NOT plugin `userConfig` or env vars |
| 4 | Session logging default | **Off** (opt-in via flag file) |

## Why vault-manager owns config (not userConfig)

`userConfig` (plugin.json) is secure but its values can only be changed by
disabling/re-enabling the plugin — not "anytime". The stack already ships
`vault-manager`, purpose-built to register/switch vaults at runtime. Crucially,
`vault_sync.py` already resolves URL+token with this precedence:

```
VAULT_URL/VAULT_TOKEN env  >  ~/.vault/servers.json active server  >  ~/.vault-token  >  default longku
```

So the hook already follows the vault-manager registry automatically. Making the
plugin ship its own `.mcp.json` would create a second `vault` MCP server that
collides with the global one vault-manager writes. Therefore the plugin ships
**no `.mcp.json` and no `userConfig`**; vault-manager registers the MCP server
into `~/.claude/mcp.json` and is the single source of truth.

The hook command is a **plain** invocation (no `VAULT_URL=`/`VAULT_TOKEN=`
injection) — injecting env would pin the values and break runtime switching.

## Structure

```
vault-plugin/
├── .claude-plugin/
│   ├── plugin.json            # manifest (name: vault) — no userConfig, no mcpServers
│   └── marketplace.json       # self-hosted catalog, plugin source "./"
├── skills/
│   ├── vault-mcp/SKILL.md
│   └── vault-manager/{SKILL.md, vault-manager}
├── hooks/
│   ├── hooks.json             # SessionStart + Stop → session-log.sh (plain)
│   ├── session-log.sh         # wrapper (install.sh self-heal removed for plugin)
│   └── vault_sync.py          # engine, flag-gated, reads registry for URL/token
├── docs/2026-06-10-vault-plugin-design.md
├── README.md
└── LICENSE
```

Deliberately excluded: `server.py` (server-side, stays in private repo) and
`hooks/install.sh` (native plugins auto-wire hooks; the self-heal call that
referenced it was stripped from `session-log.sh`).

## Install / use flow

```
/plugin marketplace add l1mzh0317/vault-plugin
/plugin install vault@vault-plugin
vault-manager add myvault https://your-vault.example.com <token>   # writes registry + mcp.json + .vault-token
# restart Claude Code → /mcp shows vault connected
vault-manager config logging on                                    # opt into session sync
```

## Plugin-managed hook detection (resolved in 1.0.1)

`vault-manager config` and `vault_sync.py doctor` originally reported "CC hooks
✅/❌" by inspecting `~/.claude/settings.json`. Plugin hooks live outside that
file, so plugin users saw a false `CC hooks ❌` / "总体 ❌". Fixed in 1.0.1: both
now detect a native plugin install (a `hooks.json` under `~/.claude/plugins/`
that wires `session-log.sh`, or `CLAUDE_PLUGIN_ROOT` in env) and report
`✅ 插件接管 (plugin)`.

## Two-layer config — `.mcp.json` re-added via userConfig (1.0.3)

The original "no `.mcp.json`" decision was driven by fear of a name collision with
the user-scope `vault` that `vault-manager` writes. That fear was unfounded:
Claude Code's documented MCP precedence is `local > project > **user** > **plugin**
> claude.ai`, so a user-scope `vault` cleanly **overrides** a plugin-bundled one
(one definition wins, no merge, no error). The downside of shipping no `.mcp.json`
was real — a bare install showed no MCP server until the user ran `vault-manager`.

1.0.3 ships **both layers**:
- **Plugin layer:** `.mcp.json` defines `vault` from `${user_config.vault_url}` /
  `${user_config.vault_token}`; `plugin.json.userConfig` declares those (url has a
  default; token is `sensitive` → keychain, not `required` so it can be skipped).
  → MCP server works right after install/enable for single-vault users.
- **User layer:** `vault-manager` keeps writing a user-scope `vault` entry +
  registry. User scope overrides the plugin layer → runtime vault switching still
  works for power users (who leave the install token blank).

No `${VAR:-default}` is supported in plugin configs, but `userConfig.default`
covers the URL default. userConfig values can only be re-edited via plugin
disable/re-enable (undocumented otherwise) — multi-vault users sidestep this by
using vault-manager instead.

## plugin-update skill (1.0.3)

Added `skills/plugin-update/` — a consumer self-update skill: refresh the
marketplace cache + `claude plugin update vault@vault-plugin` + remind to restart.
Pure instructions, no script; touches only the plugin version, never config/data.

## Versioning

`plugin.json` pins an explicit `version` (currently 1.0.3). Must be bumped on each
release for users to receive updates (commit SHA is not used when version is set).
