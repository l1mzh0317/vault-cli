---
name: vault-mcp
description: >-
  Use when the user wants to persist session context, create context sets
  (上下文集), save/retrieve reference documents, or manage long-term project
  memory in the remote vault MCP server. Triggers: "save to vault", "create
  context set", "mount context", "summarize session", "context集", "设定集",
  "vault", "挂载上下文". The vault is an HTTP MCP server at
  longku-vault.zeabur.app — all interactions go through its tools, never direct
  file writes.
---

# Vault MCP — Remote Document & Context Server

**URL:** your vault's MCP endpoint (default `https://longku-vault.zeabur.app/mcp`)
**Auth:** Bearer token, supplied via the `vault-manager` registry (not hardcoded)
**Protocol:** HTTP MCP (JSON-RPC over POST)

## Installation

This skill ships inside the **`vault` plugin** — install it once and both skills
(`vault-mcp`, `vault-manager`) plus the session-sync hook come together:

```
/plugin marketplace add l1mzh0317/vault-plugin
/plugin install vault@vault-plugin
```

Then point it at your vault. The plugin owns **no** `.mcp.json`; the `vault-manager`
skill registers the MCP server into `~/.claude/mcp.json` and the registry, so the
vault is switchable at runtime:

```
vault-manager add myvault https://your-vault.example.com <your-token>
```

(Or `vault-manager import myvault` to adopt a `vault` entry already in `mcp.json`.)

### 验证

重启 Claude Code 后，输入 `/mcp` 应能看到 vault server；或直接说 "list sessions" 测试。
切换/查看配置用 `vault-manager use <name>` / `vault-manager config`。

## Overview

The vault is a **remote persistent document store** for Claude sessions.
Instead of context drifting across sessions or living only in local memory files,
the vault holds three distinct kinds of data:

- **Sessions（会话）** — `sync_session` / `list_sessions` 管理，带元数据（project, date, tags, label），每条 session 是一个独立实体。Sessions ≠ docs，不要混用 `list_docs` 去查 session。
- **Docs（文档）** — `write_doc` / `list_docs` 管理的 markdown 文件，纯内容，无元数据结构。Session 的 *转录内容* 可能存在 docs 里，但 doc 本身不是 session。
- **Context sets（上下文集）** — named collections of docs that can be mounted into a session at once
- **Context packages（上下文包）** — `build_context` 调用 LLM 将 context set 的成员合成为结构化摘要（项目概述 / 硬性约束 / 最近决策 / 已知坑 / 深潜索引），去噪、精炼、缓存

All vault tools are called as MCP `tools/call` with `{ name, arguments }`.
The server is stateless HTTP — each request is a JSON-RPC POST.

## When to Use

- User asks to save/summarize a session to the vault
- User wants to create or update a context set (上下文集)
- User asks to mount/load context ("挂载上下文")
- User wants to search or browse past session knowledge
- After finishing a significant piece of work — persist the learnings
- When starting a new task that references past work stored in vault

## When NOT to Use

- For local-only notes that don't need cross-session persistence → use local memory files
- For project-specific conventions → put in CLAUDE.md
- When the vault is unreachable → report the error, don't silently skip

## Tool Reference

### Reading & Discovery

| Tool | Args | Returns |
|------|------|---------|
| `list_folders` | `{}` | All folder paths |
| `list_docs` | `{ folder?: string }` | Doc paths in folder (omit for root) |
| `find_docs` | `{ query: string }` | Docs matching name search |
| `read_doc` | `{ path: string, face: "source"\|"refined" }` | Document content |
| `list_contexts` | `{}` | All context sets with member count + token estimate |
| `get_context` | `{ name: string, face?: "source"\|"refined"\|"structured" }` | 挂载上下文集；face="structured" 返回 LLM 合成的结构化上下文包（需先 build_context） |

### Session tools (sessions ≠ docs)

| Tool | Args | Returns |
|------|------|---------|
| `list_sessions` | `{ project?, tag?, since?, limit? }` | Synced sessions with metadata (id, project, date, tags, label, path) |
| `sync_session` | `{ content, project, session_id, tags?, label?, date? }` | Sync a single session transcript |
| `sync_sessions` | `{ sessions: [{content, project, session_id, tags?, label?, date?}] }` | Batch-sync multiple sessions |
| `delete_session` | `{ project, session_id }` | Delete a synced session (soft-delete doc + remove metadata) |
| `build_context` | `{ name, force?, prompt? }` | Enqueue LLM synthesis of context set into structured package (async) |
| `build_status` | `{ name }` | Query build_context progress (queued/collecting/building/fresh/error) |
| `rebuild_session` | `{ project, session_id }` | 重新生成一条已同步 session 的 refined 面（按需 re-summarize）。源随对话增长时调它刷新摘要 |

**关键区别：** `list_sessions` 查的是带元数据的 session 记录（用 `sync_session` 同步进来的）；`list_docs` 查的是纯文档文件（用 `write_doc` 写入的）。两者完全不同——要看 session 列表必须用 `list_sessions`，不要用 `list_docs("sessions")` 去凑。

### Writing & Mutating

| Tool | Args | Returns |
|------|------|---------|
| `write_doc` | `{ path: string, content: string }` | `{ ok, path }` |
| `delete_doc` | `{ path: string }` | Soft-delete (recycle bin) |
| `move_doc` | `{ from: string, to: string }` | Rename or move |
| `copy_doc` | `{ from: string, to: string }` | Duplicate doc or folder |
| `create_context` | `{ name: string, face: "source"\|"refined", build_prompt?: string, members: [{path, kind, enabled}] }` | `{ ok, name }` |

### `read_doc` face parameter

- `"source"` — Readable transcript format (for user review)
- `"refined"` — LLM-refined format optimized for loading into context. May return "not found" if only source-face content exists.

### `create_context` members

Each member is `{ path: "sessions/...", kind: "doc"|"folder", enabled: bool }`.
When `get_context` is called, all **enabled** members are concatenated into one markdown
string. Use folders to include all docs within them.

## Folder Conventions

**⚠️ 注意：** `list_docs` 下的 `sessions/` 文件夹存的是文档转录内容（纯 markdown 文件），和 `list_sessions` 返回的 session 实体（带元数据）是两套独立数据系统。

```
sessions/                        # Docs 里的转录文件夹（纯内容）
  <project>/                     # Per-project grouping (e.g. alpha137, cryoACE)
    <session-id>                 # One doc per session (8-char hex id)
  YYYY-MM-DD/                    # Date-based folder for daily summaries
    <topic>                      # Named reference docs
```

- **Session transcripts:** `sessions/<project>/<session-id>` — auto-saved by the vault system
- **Daily summaries:** `sessions/YYYY-MM-DD/summary` — manually created
- **Topic docs:** `sessions/YYYY-MM-DD/<topic>` — specific feature/bug/decision writeups
- **Context sets** live in their own namespace — not under `sessions/`

## Core Workflows

### Workflow 1: Save today's session as context

After a productive session, persist the key learnings:

1. Summarize what was done (commits, decisions, bugs, lessons)
2. `write_doc` to `sessions/YYYY-MM-DD/<topic>` for each distinct piece
3. `write_doc` to `sessions/YYYY-MM-DD/summary` for the overall day
4. `create_context` to bundle these docs + relevant past session transcripts
5. Report what was saved and the context set name

### Workflow 2: Mount a context set into current session

When the user or task references past work:
1. `list_contexts` to see available sets
2. `get_context` with the relevant name
3. The returned markdown becomes working context — cite it when using the knowledge

### Workflow 3: Search for past knowledge

1. `find_docs` with keywords related to the topic
2. `read_doc` on the most relevant hits (use `face: "source"` for browsing)
3. Synthesize findings for the current task

### Workflow 3b: List or browse session history

When the user asks to list sessions（"列出所有 session"、"查一下之前的 session"等）：

1. **只用 `list_sessions`**，不要用 `list_docs`。`list_sessions` 返回带元数据的 session 实体（id, project, date, tags, label），`list_docs` 查的是文档文件，两者完全独立。
2. 可按 `project` / `tag` / `since` 过滤：
   - 只看某个项目 → `{ project: "alpha137" }`
   - 按标签筛选 → `{ tag: "css" }`
   - 近一周的 → `{ since: "2026-06-03" }`
3. 如果用户要看某个 session 的完整转录内容，再根据 `path` 字段去 `read_doc`

### Workflow 3c: Build a structured context package（构建上下文包）

当用户需要挂载上下文上下文（"build context"、"合成上下文"、"生成上下文包"）：

1. **先确认 prompt**：`list_contexts` 查看 context set 信息；`build_context` 返回 `prompt_preview`（前 200 字符），展示给用户确认。用户可以选择：
   - 用默认 prompt 直接构建
   - 传入自定义 prompt（`build_context { name, prompt: "..." }`）
   - 先修改全局默认 prompt（编辑 `_vault/build-prompt` 文档）

2. **触发异步构建**：`build_context { name }` → 返回 `{ status: "queued" }`

3. **轮询进度**：`build_status { name }` →
   - `queued` → 排队中…
   - `collecting` → 收集成员文档…
   - `building` → 调用 LLM 合成…（含已运行秒数）
   - `fresh` → 完成 ✓
   - `error` → 失败，含错误信息

   每 2-3 秒轮询一次，向用户展示进度

4. **挂载结果**：构建完成后，`get_context { name, face: "structured" }` 返回结构化上下文包

5. **强制重建**：`build_context { name, force: true }`

6. **自定义 per-context-set prompt**：`create_context` 时传入 `build_prompt` 字段

### Workflow 3d: Re-summarize a session's agent context（刷新 refined 面）

会话转录（source 面）随对话持续增长，但 refined（agent）面是某个时刻生成的一份快照，**不会自动跟着更新**。当用户说"刷新 agent 上下文 / 重新 summarize / 更新这个 session 的摘要"时：

1. **确定是哪条 session**：用 `list_sessions` 找到 `project` + `session_id`（或用户指明当前会话）
2. **触发重生成**：`rebuild_session { project, session_id }` → 返回 `{ ok: true, queued: true }`（异步，服务端用 LLM 从**当前** source 重新生成 refined）
3. **等几秒后读取**：`read_doc { path: "sessions/<project>/<id>", face: "refined" }` 确认已刷新

**前提**：source 面必须是最新的。source 由**本地 hook** 维护（CC 退出/启动时自动同步本地 transcript → Vault）。远程 MCP server 读不到用户本地文件，所以"把最新对话推上去"这步只能靠 hook，`rebuild_session` 只负责"从已上传的 source 重新 summarize"。

**本地一步到位**：如果用户装了 session-logger hook，可在终端跑 `session-log.sh sync-rebuild` —— 先把最新 transcript 推到 Vault，再触发 refined 重生成。等价于"同步 + rebuild_session"合一。

### Workflow 4: Update an existing context set

1. `list_contexts` to confirm it exists
2. `create_context` with the same name + updated members list — it overwrites
3. New sessions mounting this context get the updated set

## Common Patterns

### Browsing session transcripts efficiently

Sessions can be thousands of lines. To find the key parts:
- `read_doc` returns full content — pipe through `tail -N` or `grep` locally
- Look for "## assistant" headers for agent responses (actual work output)
- The tail of a session transcript usually contains the verification/deploy section

### Interacting via Python when shell quoting is tricky

For docs with special characters (backticks, quotes, `$`), use a Python heredoc:

```bash
python3 << 'PYEOF'
import json, urllib.request

def vault(method, args):
    data = json.dumps({
        "jsonrpc": "2.0", "id": 1, "method": "tools/call",
        "params": {"name": method, "arguments": args}
    }).encode()
    req = urllib.request.Request(
        "https://longku-vault.zeabur.app/mcp",
        data=data,
        headers={
            "Authorization": "Bearer <token>",
            "Content-Type": "application/json"
        }
    )
    return json.loads(urllib.request.urlopen(req))

result = vault("write_doc", {"path": "...", "content": "..."})
print(result)
PYEOF
```

### Reading the auth token

```bash
cat ~/.claude/mcp.json | python3 -c "import sys,json; print(json.load(sys.stdin)['mcpServers']['vault']['headers']['Authorization'].split()[-1])"
```

## Gotchas

| Symptom | Cause / Fix |
|---|---|
| `read_doc` with `face: "refined"` returns "not found" | Only source-face content exists for that doc. Use `face: "source"`. |
| `write_doc` to a new subfolder | Folder is auto-created. No need to `mkdir` first. |
| Context set `tokens: 0` after creation | Token count is computed lazily — mount it once to trigger calculation. |
| `create_context` with same name | Overwrites the existing context set. This is the update path. |
| Shell escaping fails on markdown content | Use Python heredoc (`python3 << 'PYEOF'`) instead of inline curl. |
| Vault unreachable / timeout | The server is on Zeabur. If down, write content locally and retry later. Report to user. |
| User asks "list sessions" → you call `list_docs("sessions")` | **错误。** Sessions 和 docs 是两套独立数据。列 session 必须用 `list_sessions`，它返回带元数据的 session 记录。`list_docs("sessions")` 返回的是文档路径，不是 session。 |

## Context Set Design

A good context set bundles related knowledge so future sessions can mount it in one call:

- **Include:** session summaries, design decisions, bug fixes with root causes, gotchas
- **Exclude:** raw full-length transcripts (too large), trivial/exit sessions, duplicate starts
- **Naming:** kebab-case, descriptive (`mirrorsea-dev`, `cryoACE-modeling`)
- **Reuse:** a context set is living — update it as the project evolves
- **Face:** use `"refined"` for sets intended for Claude consumption, `"source"` for review
