# Vault — Claude Code Plugin

A remote document & context store for Claude Code, packaged as a native plugin. Bundles:

- **`vault-mcp` skill** — work with vault docs, sessions, and context sets (上下文集)
- **`vault-manager` skill** — register / switch between vault servers; one-screen config dashboard
- **Session-sync hook** — auto-archive each conversation transcript to your vault (opt-in)

> The vault **server** (the thing this client talks to) is a separate
> self-hosted service. This plugin is the **client** side — you bring your own
> vault URL + token.

## Configuration model

There is **one source of truth**: the vault registry at `~/.vault/servers.json`,
managed by the bundled **`vault-manager`** skill. It holds your vault servers and
which one is active. Switching the active vault repoints **both**:

- the MCP `vault` tools (via `~/.claude/mcp.json`), and
- the background session-sync hook (which reads the registry directly).

So your config is **changeable at any time** — just run a `vault-manager` command.
No environment variables to export, no plugin re-enable dance.

## Install

```
/plugin marketplace add l1mzh0317/vault-plugin
/plugin install vault@vault-plugin
```

Then register your vault (this writes the registry + `~/.claude/mcp.json` + the
hook token) and restart Claude Code:

```
vault-manager add myvault https://your-vault.example.com <your-token>
```

(Or, if you already have a `vault` entry in `~/.claude/mcp.json`,
`vault-manager import myvault` adopts it without re-entering the token.)

After restarting, run `/mcp` to confirm the `vault` server is connected.

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
| `.claude-plugin/plugin.json` | Plugin manifest |
| `.claude-plugin/marketplace.json` | Marketplace catalog (self-hosted in this repo) |
| `skills/vault-mcp/` | Skill for working with vault docs/sessions/context sets |
| `skills/vault-manager/` | Skill + script that owns vault config (the registry) |
| `hooks/hooks.json` | Declares the SessionStart/Stop sync hooks |
| `hooks/session-log.sh` + `hooks/vault_sync.py` | The sync engine (pure stdlib) |

> Note: this plugin intentionally ships **no** `.mcp.json`. The `vault` MCP server
> is registered into your global `~/.claude/mcp.json` by `vault-manager`, so that
> the vault is switchable at runtime instead of pinned inside the plugin.

## License

MIT — see [LICENSE](LICENSE).
