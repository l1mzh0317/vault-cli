# Vault — Claude Code Plugin

A remote document & context store for Claude Code, packaged as a native plugin. Bundles:

- **`vault-mcp` skill** — work with vault docs, sessions, and context sets (上下文集)
- **`vault-manager` skill** — register / switch between vault servers; one-screen config dashboard
- **Session-sync hook** — auto-archive each conversation transcript to your vault (opt-in)

> The vault **server** (the thing this client talks to) is a separate
> self-hosted service. This plugin is the **client** side — you bring your own
> vault URL + token.

## Configuration model — two layers

The plugin works for both casual and multi-vault users via Claude Code's MCP
precedence (`local > project > **user** > **plugin** > claude.ai`):

1. **Plugin layer (`userConfig`)** — the plugin ships its own `.mcp.json` whose
   `vault` server reads `userConfig` values. On enable, Claude Code prompts for
   your **Vault URL** (defaults to the public instance) and **token** (stored in
   the system keychain). This makes the MCP server work **right after install** —
   no extra setup.
2. **User layer (`vault-manager`)** — for multiple vaults, the bundled
   `vault-manager` skill writes a **user-scope** `vault` entry into
   `~/.claude/mcp.json` plus a registry at `~/.vault/servers.json`. User scope
   **overrides** the plugin layer, so `vault-manager use <name>` switches the
   active vault at runtime (the session-sync hook follows instantly; MCP tools
   reconnect on the next restart).

Single vault → just fill the install prompt. Many vaults → use `vault-manager`
and leave the install token blank (the user-scope entry wins).

## Install

```
/plugin marketplace add l1mzh0317/vault-plugin
/plugin install vault@vault-plugin
```

On enable you'll be prompted for your **Vault URL** + **token** — fill them and
**restart Claude Code**, then `/mcp` shows the `vault` server connected. That's the
whole setup for a single vault.

**Multiple vaults instead?** Leave the token prompt blank and register vaults with
the manager (its user-scope config takes precedence):

```
vault-manager add myvault https://your-vault.example.com <your-token>
```

(Or `vault-manager import myvault` to adopt a `vault` entry already in `mcp.json`.)

### Changing config later (anytime)

```
vault-manager add <name> <url> <token>   # register another vault
vault-manager use <name>                  # switch active vault (hook follows instantly; restart CC for MCP tools)
vault-manager current                     # show active vault
vault-manager ping all                    # health-check all
vault-manager config                      # one-screen dashboard of every local setting
```

## Automatic session sync (opt-in)

The plugin registers `SessionStart` + `Stop` hooks, but they upload **only** when
a flag file exists — so **nothing syncs until you turn it on**:

```
vault-manager config logging on     # or: touch ~/.vault-logging-on
vault-manager config logging off    # or: rm ~/.vault-logging-on
```

The hook reads the active vault's URL + token straight from the registry, so it
always syncs to whichever vault is active.

Health-check the sync at any time:

```bash
"${CLAUDE_PLUGIN_ROOT}"/hooks/session-log.sh doctor
```

## Components

| Path | What it is |
|---|---|
| `.claude-plugin/plugin.json` | Plugin manifest (+ `userConfig` for Vault URL/token) |
| `.claude-plugin/marketplace.json` | Marketplace catalog (self-hosted in this repo) |
| `.mcp.json` | The `vault` MCP server, wired to `userConfig` values |
| `skills/vault-mcp/` | Skill for working with vault docs/sessions/context sets |
| `skills/vault-manager/` | Skill + script for multi-vault config (the registry; overrides the plugin layer) |
| `skills/plugin-update/` | Skill to manually update the plugin to the latest published version |
| `hooks/hooks.json` | Declares the SessionStart/Stop sync hooks |
| `hooks/session-log.sh` + `hooks/vault_sync.py` | The sync engine (pure stdlib) |

## Updating the plugin

```
/vault:plugin-update     # or just ask: "update the vault plugin"
```

Refreshes the marketplace and updates `vault@vault-plugin` to the latest published
version (then restart Claude Code). Updates land only when the author bumps the
version in `plugin.json`.

## License

MIT — see [LICENSE](LICENSE).
