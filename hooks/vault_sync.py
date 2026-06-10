#!/usr/bin/env python3
"""Vault session-sync engine (stdlib only).

Scans Claude Code transcript files (~/.claude/projects/**/*.jsonl) and syncs
new/changed conversations to a Vault instance. Replaces the per-file bash+python
fan-out with a single robust process: atomic state, one pass, real logging.

Modes:
  scan    incremental sync (default) — quiet, for hooks
  once    incremental sync — prints a one-line summary
  resync  forget sync state, re-upload everything
  doctor  full health check (config, token, vault reachability, hooks, state)

Config (env overrides registry overrides legacy default):
  VAULT_URL / VAULT_TOKEN   env override (highest priority)
  ~/.vault/servers.json     active server from vault-manager (multi-vault)
  ~/.vault-token            legacy single-token file
  default URL               https://longku-vault.zeabur.app
  CLAUDE_PROJECTS_DIR       default ~/.claude/projects
Flag file ~/.vault-logging-on must exist or scan/once are no-ops.
"""
import json
import os
import sys
import time
import urllib.parse
import urllib.request

HOME = os.path.expanduser("~")
FLAG = os.path.join(HOME, ".vault-logging-on")
TOKEN_FILE = os.path.join(HOME, ".vault-token")
STATE_FILE = os.path.join(HOME, ".vault-sync-state.json")
LOG_FILE = os.path.join(HOME, ".vault-sync.log")
SETTINGS = os.path.join(HOME, ".claude", "settings.json")
LOG_CAP = 800  # keep last N log lines

REGISTRY = os.path.join(HOME, ".vault", "servers.json")
PROJECTS_DIR = os.environ.get(
    "CLAUDE_PROJECTS_DIR", os.path.join(HOME, ".claude", "projects"))
MAX_BODY = 100_000


def _registry_active():
    """The active vault from ~/.vault/servers.json (vault-manager), or None."""
    try:
        with open(REGISTRY, encoding="utf-8") as f:
            d = json.load(f)
        srv = (d.get("servers") or {}).get(d.get("active"))
        if srv and srv.get("url"):
            return srv
    except Exception:
        pass
    return None


# URL precedence: env override > registry active server > legacy default.
VAULT_URL = (os.environ.get("VAULT_URL")
             or (_registry_active() or {}).get("url")
             or "https://longku-vault.zeabur.app").rstrip("/")


# ── small utils ──────────────────────────────────────────────────────────────
def log(msg):
    line = f"[{time.strftime('%Y-%m-%d %H:%M:%S')}] {msg}"
    try:
        old = []
        if os.path.exists(LOG_FILE):
            with open(LOG_FILE, encoding="utf-8") as f:
                old = f.read().splitlines()
        old.append(line)
        with open(LOG_FILE, "w", encoding="utf-8") as f:
            f.write("\n".join(old[-LOG_CAP:]) + "\n")
    except Exception:
        pass


def get_token():
    # precedence mirrors VAULT_URL: env > registry active > legacy ~/.vault-token
    t = os.environ.get("VAULT_TOKEN", "")
    if t:
        return t.strip()
    a = _registry_active()
    if a and a.get("token"):
        return a["token"].strip()
    try:
        with open(TOKEN_FILE, encoding="utf-8") as f:
            return f.read().strip()
    except Exception:
        return ""


def load_state():
    try:
        with open(STATE_FILE, encoding="utf-8") as f:
            d = json.load(f)
            return d if isinstance(d, dict) else {}
    except Exception:
        return {}  # missing or corrupt → treat as empty (self-healing)


def save_state(state):
    # atomic: write tmp then rename
    tmp = STATE_FILE + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(state, f, ensure_ascii=False)
    os.replace(tmp, STATE_FILE)


# ── vault http ───────────────────────────────────────────────────────────────
def vault_put(path, body, token, timeout=20):
    url = f"{VAULT_URL}/source/" + urllib.parse.quote(path)
    req = urllib.request.Request(
        url, data=body.encode("utf-8"), method="PUT",
        headers={"Authorization": "Bearer " + token})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return r.status


def vault_ping(token, timeout=10):
    req = urllib.request.Request(
        VAULT_URL + "/healthz",
        headers={"Authorization": "Bearer " + token})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status == 200
    except Exception:
        return False


def vault_list_docs(token, timeout=20):
    req = urllib.request.Request(
        VAULT_URL + "/d", headers={"Authorization": "Bearer " + token})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return [d["name"] for d in json.load(r)]


def vault_rebuild(path, token, timeout=20):
    """Force the refined (agent) face to regenerate for one doc."""
    url = f"{VAULT_URL}/source/" + urllib.parse.quote(path) + "?action=rebuild"
    req = urllib.request.Request(
        url, data=b"", method="POST",
        headers={"Authorization": "Bearer " + token})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return r.status


# ── transcript parsing ───────────────────────────────────────────────────────
def extract_body(jsonl_path):
    lines = []
    try:
        with open(jsonl_path, encoding="utf-8") as f:
            for raw in f:
                try:
                    d = json.loads(raw)
                except Exception:
                    continue
                if d.get("type") not in ("user", "assistant"):
                    continue
                m = d.get("message", {})
                role = m.get("role", d.get("type"))
                content = m.get("content", "")
                if isinstance(content, list):
                    content = " ".join(
                        c.get("text", "") if isinstance(c, dict) else str(c)
                        for c in content)
                if isinstance(content, str) and content.strip():
                    lines.append(f"## {role}\n{content}")
    except Exception:
        return ""
    return "\n\n".join(lines)[:MAX_BODY]


def pretty_project(raw):
    """Turn a CC project-dir slug (or any path) into a readable Vault segment."""
    proj = raw
    # CC slugifies the cwd path into the dir name (/ → -). Strip the slugified
    # $HOME prefix (and a leading Projects-) so the name is readable for ANY user.
    home_slug = HOME.replace("/", "-")          # e.g. -home-alice
    if proj.startswith(home_slug):
        proj = proj[len(home_slug):]
    proj = proj.lstrip("-")
    for pfx in ("Projects-", "projects-", "Documents-", "code-", "src-"):
        if proj.startswith(pfx):
            proj = proj[len(pfx):]
            break
    proj = proj.replace("/", "-").replace(" ", "-").strip("-") or "misc"
    return proj[:48].rstrip("-")  # Vault path segment cap is 64 chars


def doc_name_for(jsonl_path):
    session_id = os.path.basename(jsonl_path)[:-6]  # strip .jsonl
    proj = pretty_project(os.path.basename(os.path.dirname(jsonl_path)))
    return f"sessions/{proj}/{session_id[:8]}"


def cwd_project():
    """Project segment for the current working directory (no jsonl needed)."""
    return pretty_project(os.getcwd().replace("/", "-"))


def iter_transcripts():
    if not os.path.isdir(PROJECTS_DIR):
        return
    for root, _dirs, files in os.walk(PROJECTS_DIR):
        if os.sep + "subagents" + os.sep in (root + os.sep):
            continue
        for name in files:
            if name.endswith(".jsonl"):
                yield os.path.join(root, name)


# ── core scan ────────────────────────────────────────────────────────────────
def do_scan(token, force=False):
    state = {} if force else load_state()
    synced = skipped = empty = failed = 0

    for jsonl in iter_transcripts():
        try:
            st = os.stat(jsonl)
        except OSError:
            continue
        if st.st_size == 0:
            continue
        key = jsonl
        prev = state.get(key, {})
        if not force and prev.get("last_modified") == int(st.st_mtime):
            skipped += 1
            continue

        body = extract_body(jsonl)
        if not body:
            state[key] = {"last_modified": int(st.st_mtime)}
            empty += 1
            continue

        try:
            vault_put(doc_name_for(jsonl), body, token)
            state[key] = {"last_modified": int(st.st_mtime),
                          "doc": doc_name_for(jsonl), "bytes": len(body)}
            synced += 1
        except Exception as e:
            failed += 1
            log(f"PUT failed for {os.path.basename(jsonl)}: {e}")

    save_state(state)
    return {"synced": synced, "skipped": skipped, "empty": empty,
            "failed": failed}


# ── rebuild (re-summarize refined face on demand) ────────────────────────────
def select_session_docs(token, selector):
    """Pick which session docs to rebuild.
      selector=None  → all sessions under the current project
      selector=<id>  → sessions whose id segment startswith <id>
      selector=path  → that exact doc (contains '/')
    """
    if selector and "/" in selector:
        return [selector if selector.startswith("sessions/")
                else "sessions/" + selector]
    docs = [d for d in vault_list_docs(token) if d.startswith("sessions/")]
    if selector:
        return [d for d in docs if d.rsplit("/", 1)[-1].startswith(selector)]
    # no selector: prefer the CURRENT conversation (CC sets this env var),
    # otherwise fall back to every session under the current project.
    cur = os.environ.get("CLAUDE_CODE_SESSION_ID", "")[:8]
    if cur:
        hit = [d for d in docs if d.rsplit("/", 1)[-1].startswith(cur)]
        if hit:
            return hit
    proj = cwd_project()
    return [d for d in docs if d.split("/", 2)[1] == proj]


def do_rebuild(token, selector=None):
    targets = select_session_docs(token, selector)
    ok = failed = 0
    for path in targets:
        try:
            vault_rebuild(path, token)
            ok += 1
        except Exception as e:
            failed += 1
            log(f"rebuild failed for {path}: {e}")
    return {"rebuilt": ok, "failed": failed,
            "targets": [t.split("/", 1)[1] for t in targets]}


# ── doctor ───────────────────────────────────────────────────────────────────
PLUGINS_DIR = os.path.join(HOME, ".claude", "plugins")


def _plugin_managed():
    """True when SessionStart/Stop are wired by a native plugin install (whose
    hooks live in the plugin's hooks.json, NOT in settings.json)."""
    # Running from inside a plugin install, or invoked by CC as a plugin hook.
    try:
        here = os.path.realpath(__file__)
    except Exception:
        here = ""
    root = os.path.realpath(PLUGINS_DIR)
    if here.startswith(root + os.sep) or os.environ.get("CLAUDE_PLUGIN_ROOT"):
        return True
    # Any installed plugin whose hooks.json wires our session-log.sh?
    import glob
    for hj in glob.glob(os.path.join(PLUGINS_DIR, "cache", "*", "*", "*",
                                     "hooks", "hooks.json")):
        try:
            with open(hj, encoding="utf-8") as f:
                if "session-log.sh" in f.read():
                    return True
        except Exception:
            pass
    return False


def hooks_registered():
    # Native plugin wires SessionStart/Stop from its own hooks.json — Claude Code
    # auto-loads them, so they never appear in settings.json. Treat as registered.
    if _plugin_managed():
        return {"source": "plugin", "settings": True, "start": True, "stop": True}
    try:
        with open(SETTINGS, encoding="utf-8") as f:
            cfg = json.load(f)
    except Exception:
        return {"source": None, "settings": False, "start": False, "stop": False}
    hooks = cfg.get("hooks", {}) or {}

    def has(event):
        for block in hooks.get(event, []):
            for h in (block.get("hooks", []) if isinstance(block, dict) else []):
                if "session-log.sh" in (h.get("command", "")
                                        if isinstance(h, dict) else ""):
                    return True
        return False

    return {"source": "settings", "settings": True,
            "start": has("SessionStart"), "stop": has("Stop")}


def doctor():
    token = get_token()
    state = load_state()
    reg = hooks_registered()
    ok = "✅"
    no = "❌"

    print("龙库 · 会话同步体检")
    print("─" * 40)
    print(f"  日志开关 (~/.vault-logging-on) : {ok if os.path.exists(FLAG) else no + ' 未开启 (touch 它)'}")
    print(f"  Vault token                   : {ok if token else no + ' 缺失'}")
    print(f"  transcript 目录               : {ok if os.path.isdir(PROJECTS_DIR) else no} {PROJECTS_DIR}")
    print(f"  Vault 可达 ({VAULT_URL})")
    print(f"                                : {ok if token and vault_ping(token) else no}")
    print("  CC hooks 注册:")
    if reg.get("source") == "plugin":
        print(f"    插件接管 (plugin-managed)   : {ok}")
    else:
        hint = " 缺失 → 安装插件 vault@vault-plugin"
        print(f"    settings.json 存在          : {ok if reg['settings'] else no}")
        print(f"    SessionStart hook           : {ok if reg['start'] else no + hint}")
        print(f"    Stop hook                   : {ok if reg['stop'] else no + hint}")
    print(f"  已记录 transcript             : {len(state)} 个")
    # transcript count on disk
    disk = sum(1 for _ in iter_transcripts())
    print(f"  磁盘上 transcript             : {disk} 个")
    print("─" * 40)
    healthy = (os.path.exists(FLAG) and token and reg["start"] and reg["stop"])
    print("总体: " + (ok + " 健康，会话会自动同步" if healthy
                     else no + " 有问题，见上面 ❌ 项"))
    return 0 if healthy else 1


# ── main ─────────────────────────────────────────────────────────────────────
def main():
    mode = sys.argv[1] if len(sys.argv) > 1 else "scan"
    selector = sys.argv[2] if len(sys.argv) > 2 else None

    if mode == "doctor":
        sys.exit(doctor())

    # everything below needs the flag + token
    if not os.path.exists(FLAG):
        return
    token = get_token()
    if not token:
        log("no token; skipping")
        return

    if mode == "rebuild":
        res = do_rebuild(token, selector)
        log("rebuild: " + json.dumps(res))
        print(json.dumps(res, ensure_ascii=False))
        return

    if mode == "sync-rebuild":
        s = do_scan(token)
        r = do_rebuild(token, selector)
        log("sync-rebuild: " + json.dumps({"scan": s, "rebuild": r}))
        print(json.dumps({"scan": s, "rebuild": r}, ensure_ascii=False))
        return

    force = (mode == "resync")
    res = do_scan(token, force=force)
    log(f"{mode}: " + json.dumps(res))
    if mode in ("once", "resync"):
        print(json.dumps(res, ensure_ascii=False))


if __name__ == "__main__":
    main()
