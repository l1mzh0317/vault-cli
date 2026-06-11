# Vault — remote context store for Claude

A remote document & context store for Claude — **sessions, docs, and distilled
context that persist across sessions and machines** instead of vanishing when a
conversation ends. There are three ways to use it:

- **`vault` CLI** ⭐ *recommended* — one static binary (no runtime). Reads, writes,
  and wires Claude Code for you. Works on the CLI, the **Desktop app**, cron, or
  any harness.
- **MCP server** — the vault's tools inside any MCP-capable client (auto-discovered
  by the model).
- **Claude Code plugin** — bundles the MCP server + skills + session-sync hook
  (Claude Code **CLI only**).

> The vault **server** (the thing the client talks to) is a separate self-hosted
> service. Everything here is the **client** side — bring your own vault URL + token.

## Install — pick one

| | Best for | Install | Needs |
|---|---|---|---|
| **CLI** ⭐ | everything; Desktop; token-free writes; self-update | `curl … cli/install.sh \| sh` | nothing (static binary) |
| **MCP** | reads inside a model; any MCP client | `claude mcp add …` | an MCP client |
| **Plugin** | CLI users who want it bundled | `/plugin marketplace add …` | Claude Code **CLI** (not Desktop) |

**We recommend the CLI.** It's the only one that works everywhere — including the
**Desktop app, where the plugin can't load** ([anthropics/claude-code#39897](https://github.com/anthropics/claude-code/issues/39897),
closed not-planned) — needs no python, keeps large uploads out of the model's
context (token-free writes), and self-updates.

### 1. CLI  ⭐ recommended

```sh
curl -fsSL https://raw.githubusercontent.com/l1mzh0317/vault-plugin/main/cli/install.sh | sh
vault config add myvault https://your-vault.example.com <token>
vault setup            # wire Claude Code: MCP server + auto-sync hooks
# then restart Claude Code
```

Installs a single static binary **plus** a `vault` skill (so Claude knows the CLI
exists) — no python, no plugin system, works on **Desktop** too. Later:
`vault update` to upgrade, `vault setup --uninstall` to undo. Full command set in
[`cli/README.md`](cli/README.md).

One-step desktop install (binary + register + wire, all in one):

```sh
curl -fsSL https://raw.githubusercontent.com/l1mzh0317/vault-plugin/main/desktop-setup.sh \
  | sh -s -- https://your-vault.example.com <token>
```

### 2. MCP server only

Just want the vault's tools inside a model — no CLI, no plugin:

```sh
claude mcp add vault --scope user --transport http \
  https://your-vault.example.com/mcp \
  --header "Authorization: Bearer <token>"
```

Works in any MCP client (Claude Code CLI **and** Desktop). Best for **reads** —
for bulk **writes/sync** prefer the CLI, since MCP passes content *by value*
(it goes through the model's context = token cost).

### 3. Claude Code plugin (CLI only)

```
/plugin marketplace add l1mzh0317/vault-plugin
/plugin install vault@vault-plugin
```

Bundles the MCP server + skills + an opt-in session-sync hook. On enable you're
prompted for your **Vault URL** + **token** — fill them and **restart**. For
multiple vaults, leave the token blank and use the `vault-manager` skill (its
user-scope config overrides the plugin layer).

> **The Desktop app can't install plugins from a custom marketplace** — the
> marketplace step is CLI-only and the desktop skill loader is broken for them.
> Use the **CLI** on Desktop.

## Configuring & switching vaults

```sh
# CLI
vault config                       # dashboard: active vault, token, flags, state
vault config add <name> <url> <token>
vault config use <name>            # switch active vault

# plugin
vault-manager add <name> <url> <token>
vault-manager use <name>           # switch (hook follows instantly; restart CC for MCP)
vault-manager config               # one-screen dashboard
```

Both share the registry at `~/.vault/servers.json`. URLs are stored **base** (no
`/mcp`); each consumer appends the right suffix.

## Automatic session sync

Every conversation transcript can be auto-archived to your vault on session
start/stop — content is read **locally** and pushed straight to the vault, so it
never passes through the model's context.

- **CLI:** `vault setup` wires `SessionStart`/`Stop` hooks → `vault sync`. Add
  `touch ~/.vault-session-meta-on` to also index each session in `list_sessions`.
- **Plugin:** ships the hooks; gate them with `vault-manager config logging on`
  (or `touch ~/.vault-logging-on`).

Health-check anytime: `vault doctor`.

## Components

| Path | What it is |
|---|---|
| `cli/` | The **`vault` CLI** (Go, single static binary) — recommended front-end |
| `cli/install.sh` | One-line installer (binary + skill), used by the `curl … \| sh` flow |
| `cli/skill/SKILL.md` | The `vault` skill, installed to `~/.claude/skills/vault/` |
| `desktop-setup.sh` | Python-free desktop install: binary + `vault setup` |
| `.claude-plugin/plugin.json` | Plugin manifest (+ `userConfig` for Vault URL/token) |
| `.claude-plugin/marketplace.json` | Marketplace catalog (self-hosted in this repo) |
| `.mcp.json` | The plugin's `vault` MCP server, wired to `userConfig` |
| `skills/vault-mcp/`, `skills/vault-manager/`, `skills/plugin-update/` | Plugin-bundled skills |
| `hooks/` | Plugin session-sync hook (`session-log.sh` + `vault_sync.py`, pure stdlib) |

## Updating

```sh
vault update                 # CLI: self-update to the latest release (--check to peek)
/vault:plugin-update         # plugin: update vault@vault-plugin to the latest version
```

CLI releases are published from `cli-v*` tags (cross-compiled for macOS / Linux /
Windows); `install.sh` and `vault update` always fetch the latest.

## License

MIT — see [LICENSE](LICENSE).
