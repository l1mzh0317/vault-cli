---
name: vault-manager
description: >-
  Use when the user has (or wants) MORE THAN ONE vault server and needs to
  register, list, switch, or health-check them. Triggers: "切换 vault", "切到
  X 库", "换一个 vault", "add a vault server", "switch vault", "list my vaults",
  "which vault am I on", "ping all vaults", "我有很多 vault", "vault-manager".
  Backed by a registry at ~/.vault/servers.json; switching repoints BOTH the
  MCP `vault` tools and the background session-sync hook. For working WITH a
  vault's documents/sessions (not managing servers), use the vault-mcp skill.
---

# Vault Manager — switch between multiple Vault servers

A profile manager for people who run several vaults (work / personal / per-client).
One registry holds many named vaults; exactly **one is active**. Switching the
active vault rewrites two things so everything follows along:

- `~/.claude/mcp.json` → the MCP `vault` entry (the in-conversation tools)
- `~/.vault-token` + the registry → what the **session-sync hook** pushes to

So `use <name>` repoints **both** the tools you call in a conversation **and**
the background transcript sync. (MCP tools reconnect on the next Claude Code
restart; the hook picks up the change immediately.)

## The tool

A zero-dependency script lives next to this skill:

```
${SKILL_DIR}/vault-manager        # python3, stdlib only
```

Run it with `python3 "${SKILL_DIR}/vault-manager" <cmd>` (or chmod +x and call directly).
Resolve `${SKILL_DIR}` to this skill's base directory (printed when the skill loads).

| Command | What it does |
|---|---|
| `add <name> <url> <token>` | Register/update a vault. First one added becomes active. `/mcp` suffix on the URL is stripped automatically. |
| `ls` | List vaults — active marked `*`, tokens masked. |
| `use <name>` | Make `<name>` active → rewrite `mcp.json` + `.vault-token`, ping it. |
| `current` | Print the active vault (name, url, masked token). |
| `ping [name\|all]` | Health-check (`GET /healthz`) the active / one / all vaults. |
| `rm <name>` | Remove a vault; active falls back to another if needed. |
| `import [name]` | Adopt the vault already in `mcp.json` as `<name>` (one-time migration). |
| `config` | One-screen dashboard of **every** local vault setting (see below). |
| `config logging on\|off` | Toggle background auto-sync (the `~/.vault-logging-on` flag). |

## Unified config (`vault-manager config`)

One place to see/set all the **local** vault settings that otherwise live in five
scattered files (registry, logging flag, token, mcp.json, CC settings):

```
龙库 · Vault 配置
────────────────────────────────────────────────
  活跃 vault    longku  https://longku-vault.zeabur.app  ✓ 可达
  token         ✅ 0052c3…ccde
  已注册        2 个: longku*, home
  自动同步      ✅ 开   (~/.vault-logging-on)
  CC hooks      ✅ 插件接管 (plugin)
  同步状态      3 条 transcript 已记录
────────────────────────────────────────────────
  改: use <name> | config logging on|off | add <name> <url> <token>
```

- **活跃 vault / token / 已注册** — from the registry (`use` / `add` / `rm` to change).
- **自动同步** — `config logging on|off` (background sync on session start/stop).
- **CC hooks** — whether the session-sync hook is active. Installed as a native plugin,
  it shows `✅ 插件接管 (plugin)` (the hook lives in the plugin's `hooks.json`, not
  `settings.json`). If ❌, install/enable the plugin: `/plugin install vault@vault-plugin`.
- **同步状态** — how many transcripts the sync engine is tracking.

When the user asks "看一下 vault 配置 / 现在都是什么设置 / 关掉自动同步", run `config`
(or `config logging off`). It's the single entry point — no hunting through dotfiles.

## Registry format

`~/.vault/servers.json` (chmod 600 — it holds tokens):

```json
{
  "active": "work",
  "servers": {
    "work": { "url": "https://work-vault.zeabur.app",  "token": "..." },
    "home": { "url": "https://home-vault.zeabur.app",  "token": "..." }
  }
}
```

URLs are stored **base only** (no `/mcp`). The MCP entry gets `<url>/mcp`; the
hook appends `/source/…`, `/healthz`, etc.

## When the user says…

| User intent | Do |
|---|---|
| "我有很多 vault / 帮我管理 vault 服务器" | Explain the model, run `ls`. If empty, offer `import` for the current one, then `add` for others. |
| "加一个 vault / register a new server" | `add <name> <url> <token>` — ask for the three values if missing. |
| "切到 work 库 / switch to X" | `use <name>`, then **remind the user to restart Claude Code** for the MCP tools to reconnect. |
| "我现在在哪个库 / which vault" | `current` |
| "哪些库还活着 / ping all" | `ping all` |
| "删掉那个库" | confirm, then `rm <name>` |

## Workflow: first-time setup with multiple vaults

1. `import longku` — adopt the vault already configured (no token re-entry).
2. `add <name> <url> <token>` for each additional vault.
3. `ls` to confirm; `ping all` to verify reachability.
4. `use <name>` to pick the active one.
5. Tell the user: **restart Claude Code** so the MCP `vault__*` tools point at the new server. The hook needs no restart.

## Gotchas

| Symptom | Cause / Fix |
|---|---|
| Switched vault but MCP tools still hit the old one | MCP connections are established at Claude Code startup. **Restart Claude Code** after `use`. The hook (sync) switches instantly. |
| A project's `.mcp.json` overrides the switch | `use` rewrites the **global** `~/.claude/mcp.json`. A project-local `.mcp.json` with its own `vault` entry wins for that project — edit or remove it if you want the global switch to apply there. |
| `use` says "⚠ unreachable" | The target's `/healthz` didn't answer. The switch still happened; check the URL/server. |
| Token visible in registry | `~/.vault/servers.json` is chmod 600. Tokens are masked in `ls`/`current` output, but the file itself holds them in clear — keep it local, never commit it. |
| Env var won't switch | `VAULT_URL` / `VAULT_TOKEN` env vars override the registry for the hook. Unset them if a switch "doesn't take". |

## Relation to vault-mcp

- **vault-manager** (this skill) — *which* vault you're talking to (server management).
- **vault-mcp** — *what* you do with a vault (docs, sessions, context sets).

After switching with vault-manager, use vault-mcp's tools against the now-active vault.
