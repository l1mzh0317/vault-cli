---
name: vault
description: Persist and recall long-term context in the remote Vault using the `vault` CLI and its MCP server. Use when the user wants to save/sync a session, list or search past sessions/docs, read or upload a doc, mount or build a context set, or check vault config. Triggers include "save to vault", "sync session", "list vault sessions", "search the vault", "mount context", "上下文集", "同步会话", "vault".
---

# Vault — remote context store (via the `vault` CLI)

The vault is a **remote, persistent store** for Claude sessions, documents, and
distilled context — so knowledge survives across sessions and machines instead
of vanishing when a session ends. There are two access paths:

- **MCP `vault` server** — its tools are auto-discovered by the model. Best for
  **reads** you want in context (`list_sessions`, `read_doc`, `get_context`).
- **`vault` CLI** (a static binary on `PATH`) — best for **writes / bulk / sync**:
  it reads files locally and ships bytes straight to the vault, so content
  **never passes through the model's context** (no token cost). It also works
  when the MCP server isn't loaded.

**Rule of thumb:** reads → MCP tools (or a `vault` read command); writes / sync →
**always the CLI** (cheaper and works everywhere).

## CLI cheat-sheet

```
# sessions
vault sessions [--project P] [--tag T] [--since YYYY-MM-DD]
vault rebuild <project> <id>          # refresh a session's refined summary

# docs
vault ls [folder] / folders / find <query>
vault cat <path> [--as source|refined]
vault push <file> [--path P]          # upload a doc (token-free)
vault write <path>                    # write a doc from stdin
vault rm/mv/cp …                       # (rm/mv take --yes; refuse non-interactively)

# context sets
vault contexts
vault context <name> [--as source|refined|structured]
vault context-create <name> --member path[:kind[:on|off]] …
vault build <name>                    # distill a set → structured package
vault build-status <name>

# local / config / self
vault sync [--meta]                   # scan ~/.claude transcripts → vault
vault setup [--uninstall]             # wire Claude Code (MCP server + auto-sync hooks)
vault config [use <name> | add <name> <url> <token>]
vault update [--check]                # self-update to the latest release
vault doctor / version / help
```

Run `vault` with no args for the full list. Add `--json` for raw output.

## Conventions

- **Sessions** = metadata-rich conversation records (`vault sessions`,
  not `vault ls`). **Docs** = plain markdown (`ls` / `cat` / `push`).
- **Context sets** = named bundles of docs; `vault build <name>` distills one
  into a structured package you mount with `vault context <name> --as structured`.
- Never push large content through MCP tools (token cost) — use `vault push` /
  `vault sync`, which read locally.

## When to use

- After substantial work worth keeping → `vault sync` (add `--meta` to index the
  session in `list_sessions`).
- Recalling past work → `vault sessions` / `vault find <q>` / `vault cat <path>`.
- Starting a task that builds on prior context → `vault context <name> --as structured`.
- Multiple vaults / switching → `vault config` / `vault config use <name>`.
