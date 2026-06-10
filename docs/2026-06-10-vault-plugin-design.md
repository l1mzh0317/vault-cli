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

## Known limitation

`vault-manager config` reads `~/.claude/settings.json` to report "CC hooks
✅/❌". Plugin hooks live outside settings.json, so plugin users may see a false
`CC hooks ❌`. Cosmetic only — sync still works. Fix in a future vault-manager
release (detect plugin-provided hooks).

## Versioning

`plugin.json` pins `version: 1.0.0`. Must be bumped on each release for users to
receive updates (commit SHA is not used when version is set).
