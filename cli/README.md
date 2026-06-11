# `vault` — context-store CLI

A tiny, **dependency-free** Go CLI for the Vault context store. One static
binary, no runtime.

## Why it exists

The vault has two access paths, each wrong for half the job:

- **MCP tools** pass arguments *by value*, so uploading a transcript drags its
  full content through the model's context — real token cost. Great for
  **reads** (small results the model needs anyway); bad for **bulk writes**.
- The **python hook** avoids that (reads files locally) but needs a python
  runtime and is wired to Claude Code's hook lifecycle — not portable.

This CLI is the **write side**: it reads files locally and ships bytes straight
to the vault, so **content never enters the model's context**, and it's a single
static binary any agent/harness can call over a shell (CC, Codex, desktop, cron).

> Rule of thumb: **MCP for reads/queries, this CLI for writes/bulk.**

## Install (no Go needed)

**Linux / macOS:**

```sh
curl -fsSL https://raw.githubusercontent.com/l1mzh0317/vault-cli/main/cli/install.sh | sh
```

**Windows (PowerShell):**

```powershell
irm https://raw.githubusercontent.com/l1mzh0317/vault-cli/main/cli/install.ps1 | iex
```

Both download the right static binary for your OS/arch from the latest release
(into `~/.local/bin` on Unix, `%LOCALAPPDATA%\Programs\vault` on Windows; override
with `INSTALL_DIR`) **and** install a markdown `vault` skill to
`~/.claude/skills/vault/` so Claude knows the CLI exists (`NO_SKILL=1` to skip;
restart Claude Code to load `/vault`). Prebuilt binaries: macOS / Linux / Windows
× amd64 / arm64 on the [Releases](https://github.com/l1mzh0317/vault-cli/releases)
page.

## Build from source

```
cd cli
CGO_ENABLED=0 go build -ldflags="-s -w" -o vault .
# cross-compile, e.g. macOS arm64:
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="-s -w" -o vault-darwin-arm64 .
```

Or, with Go installed:

```
go install github.com/l1mzh0317/vault-cli/cli@latest   # installs as `cli`
```

(Rename the binary to `vault` and put it on your `PATH`.)

## Usage

Global flag `--json` prints raw JSON for read commands (for scripts/agents).

```
# sessions
vault sessions [--project P] [--tag T] [--since YYYY-MM-DD] [--limit N]
vault rebuild <project> <id>          re-summarize a session's refined face
vault rm-session <project> <id>       delete a session

# docs — read
vault ls [folder]                     list docs
vault folders                         list folders
vault find <query>                    search docs by name
vault cat <path> [--as source|refined]   read a doc

# docs — write
vault push <file> [--path P]          upload a local file as a doc
vault write <path>                    write a doc from stdin
vault rm <path> [--yes]               delete a doc
vault mv <from> <to> [--yes]          move/rename a doc
vault cp <from> <to>                  copy a doc

# context sets
vault contexts                        list context sets
vault context <name> [--as source|refined|structured]   get/mount a context
vault context-create <name> [--face F] [--prompt P] --member path[:kind[:on|off]] ...
vault build <name> [--force] [--prompt P]   distill a context set (async)
vault build-status <name>             check build progress

# local / meta
vault sync [--resync] [--meta]        scan transcripts → vault
vault setup [--uninstall]             wire Claude Code: MCP server + auto-sync hooks
vault config                          show resolved config + registered vaults
vault config use <name>               switch active vault
vault config add <name> <url> <token> register a vault
vault doctor                          config + reachability check
vault version
```

Notes:

- **Reads** (`sessions`, `ls`, `find`, `cat`, `contexts`, `context`) go through
  the vault's MCP endpoint; results are small and land in your terminal. Use
  the remote MCP server instead when you want these inside a model.
- **Writes** (`push`, `write`, `sync`) read content locally and ship it straight
  to the vault — it never passes through a model's context.
- `--as` picks which face of a doc/context to read: `source` (raw), `refined`
  (LLM summary), or `structured` (a built context package).
- `sync` mirrors the python engine exactly (same `~/.vault-sync-state.json`,
  `## role` rendering, `MAX_BODY`) — the two are interchangeable. `--meta`
  (or `~/.vault-session-meta-on`) also registers each session in `list_sessions`.
- Doc paths **can't carry a file extension**; `push`'s default path strips it
  (`notes.md` → `docs/notes`).
- **Destructive** commands (`rm`, `mv`, `rm-session`) prompt for confirmation on
  a TTY and **refuse outright when non-interactive** (scripts/agents) unless you
  pass `--yes`.
- `context-create --member path[:kind[:on|off]]`: `kind` defaults to `doc`,
  enabled defaults to `on`. `off` keeps a member in the set but excludes it when
  the context is mounted.
- `config` reads/writes the vault-manager registry (`~/.vault/servers.json`);
  URLs are stored **base** (no `/mcp`).
- Set `VAULT_DEBUG=1` to dump raw vault responses.

## Config

Resolved in this order (matches the python engine):

```
env VAULT_URL / VAULT_TOKEN  >  ~/.vault/servers.json (active vault-manager server)  >  ~/.vault-token
```

Base URL is stored **without** `/mcp` — the CLI appends the right suffix per
endpoint (`/source/…`, `/mcp`, `/healthz`).
