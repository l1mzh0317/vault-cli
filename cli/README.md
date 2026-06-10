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

```
curl -fsSL https://raw.githubusercontent.com/l1mzh0317/vault-plugin/main/cli/install.sh | sh
```

Downloads the right static binary from the latest release into `~/.local/bin`
(override with `INSTALL_DIR=…`). Windows: grab `vault-windows-amd64.exe` from the
[Releases](https://github.com/l1mzh0317/vault-plugin/releases) page.

## Build from source

```
cd cli
CGO_ENABLED=0 go build -ldflags="-s -w" -o vault .
# cross-compile, e.g. macOS arm64:
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="-s -w" -o vault-darwin-arm64 .
```

Or, with Go installed:

```
go install github.com/l1mzh0317/vault-plugin/cli@latest   # installs as `cli`
```

(Rename the binary to `vault` and put it on your `PATH`.)

## Usage

```
vault sync [--resync] [--meta]   scan ~/.claude/projects/**/*.jsonl → vault
vault push <file> [--path P]     upload a local file as a doc (write_doc)
vault doctor                     config + reachability health check
vault version
```

- `sync` mirrors the python engine exactly (same `~/.vault-sync-state.json`,
  same `## role` rendering, same `MAX_BODY`), so the two are interchangeable.
  `--meta` (or `~/.vault-session-meta-on`) also registers each session in
  `list_sessions`.
- `push` uploads any local file as a doc. Doc paths **can't carry a file
  extension** — the default path strips it (`notes.md` → `docs/notes`).
- Set `VAULT_DEBUG=1` to dump raw vault responses.

## Config

Resolved in this order (matches the python engine):

```
env VAULT_URL / VAULT_TOKEN  >  ~/.vault/servers.json (active vault-manager server)  >  ~/.vault-token
```

Base URL is stored **without** `/mcp` — the CLI appends the right suffix per
endpoint (`/source/…`, `/mcp`, `/healthz`).
