// vault — a tiny, dependency-free CLI for the Vault context store.
//
// Why a CLI (vs the MCP tools or the python hook):
//   - MCP tools pass content BY VALUE, so uploading a transcript drags it
//     through the model's context (token cost). The CLI reads files locally
//     and ships bytes straight to the vault — content never enters context.
//   - The python hook does the same but needs a python runtime and is wired to
//     Claude Code's hook lifecycle. This compiles to ONE static binary (no
//     runtime) and any agent/harness can call it via a shell.
//
// Commands: see usage() or `vault` with no args. Global flag --json prints raw
// JSON for read commands (handy for scripts/agents).
//
// Config precedence (mirrors the python engine):
//
//	env VAULT_URL / VAULT_TOKEN  >  ~/.vault/servers.json (active)  >
//	default URL / ~/.vault-token
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// version is overridden at build time via -ldflags "-X main.version=…".
var version = "0.1.0-dev"

const (
	maxBody    = 100_000 // chars; matches python MAX_BODY
	defaultURL = "https://longku-vault.zeabur.app"
)

var (
	httpClient = &http.Client{Timeout: 30 * time.Second}
	jsonOut    bool // --json: print raw JSON instead of formatted output
)

func home() string { h, _ := os.UserHomeDir(); return h }

// ── config ───────────────────────────────────────────────────────────────────
type server struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

func registryActive() server {
	var d struct {
		Active  string            `json:"active"`
		Servers map[string]server `json:"servers"`
	}
	b, err := os.ReadFile(filepath.Join(home(), ".vault", "servers.json"))
	if err != nil {
		return server{}
	}
	if json.Unmarshal(b, &d) != nil {
		return server{}
	}
	return d.Servers[d.Active]
}

func baseURL() string {
	u := os.Getenv("VAULT_URL")
	if u == "" {
		u = registryActive().URL
	}
	if u == "" {
		u = defaultURL
	}
	return strings.TrimRight(u, "/")
}

func token() string {
	if t := strings.TrimSpace(os.Getenv("VAULT_TOKEN")); t != "" {
		return t
	}
	if t := strings.TrimSpace(registryActive().Token); t != "" {
		return t
	}
	if b, err := os.ReadFile(filepath.Join(home(), ".vault-token")); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}

// ── vault http ───────────────────────────────────────────────────────────────
// quotePath escapes each path segment but keeps "/" separators, matching
// python's urllib.parse.quote(path) (default safe="/").
func quotePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

func vaultPut(path, body string) error {
	req, _ := http.NewRequest(http.MethodPut,
		baseURL()+"/source/"+quotePath(path), strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token())
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("PUT %s -> HTTP %d", path, resp.StatusCode)
	}
	return nil
}

func vaultPing() bool {
	req, _ := http.NewRequest(http.MethodGet, baseURL()+"/healthz", nil)
	req.Header.Set("Authorization", "Bearer "+token())
	resp, err := httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}

// vaultCallTool invokes an MCP tool over JSON-RPC. Content stays in this
// process (read from disk) — it never goes through the model.
func vaultCallTool(name string, args map[string]any) (map[string]any, error) {
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	})
	req, _ := http.NewRequest(http.MethodPost, baseURL()+"/mcp", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+token())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if os.Getenv("VAULT_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[debug] %s HTTP %d body=%s\n", name, resp.StatusCode, string(raw))
	}
	var data map[string]any
	if json.Unmarshal(raw, &data) != nil {
		for _, line := range strings.Split(string(raw), "\n") { // tolerate SSE
			line = strings.TrimSpace(line)
			if after, ok := strings.CutPrefix(line, "data:"); ok {
				_ = json.Unmarshal([]byte(strings.TrimSpace(after)), &data)
			}
		}
	}
	if data["error"] != nil {
		return nil, fmt.Errorf("vault error: %v", data["error"])
	}
	if res, ok := data["result"].(map[string]any); ok {
		if isErr, _ := res["isError"].(bool); isErr {
			return res, fmt.Errorf("%s: %s", name, toolText(res))
		}
		return res, nil
	}
	return data, nil
}

// toolText pulls the first content text block out of an MCP tool result.
func toolText(res map[string]any) string {
	if c, ok := res["content"].([]any); ok && len(c) > 0 {
		if m, ok := c[0].(map[string]any); ok {
			if t, ok := m["text"].(string); ok {
				return t
			}
		}
	}
	return ""
}

// callText calls a tool and returns its text payload.
func callText(name string, args map[string]any) (string, error) {
	res, err := vaultCallTool(name, args)
	if err != nil {
		return "", err
	}
	return toolText(res), nil
}

// ── transcript parsing (ports python extract_body / pretty_project) ──────────
type tLine struct {
	Type    string `json:"type"`
	Message struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

func contentToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		parts := make([]string, 0, len(arr))
		for _, el := range arr {
			var m map[string]any
			if json.Unmarshal(el, &m) == nil {
				if t, ok := m["text"].(string); ok {
					parts = append(parts, t)
				} else {
					parts = append(parts, "")
				}
			} else {
				parts = append(parts, string(el))
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func scanLines(path string) (*bufio.Scanner, *os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	return sc, f, nil
}

func extractBody(path string) string {
	sc, f, err := scanLines(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var parts []string
	for sc.Scan() {
		var ln tLine
		if json.Unmarshal(sc.Bytes(), &ln) != nil {
			continue
		}
		if ln.Type != "user" && ln.Type != "assistant" {
			continue
		}
		role := ln.Message.Role
		if role == "" {
			role = ln.Type
		}
		content := contentToString(ln.Message.Content)
		if strings.TrimSpace(content) != "" {
			parts = append(parts, "## "+role+"\n"+content)
		}
	}
	return truncRunes(strings.Join(parts, "\n\n"), maxBody)
}

func firstUserText(path string) string {
	sc, f, err := scanLines(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	for sc.Scan() {
		var ln tLine
		if json.Unmarshal(sc.Bytes(), &ln) != nil {
			continue
		}
		if ln.Type != "user" {
			continue
		}
		s := strings.TrimSpace(contentToString(ln.Message.Content))
		if s != "" && !strings.HasPrefix(s, "<") {
			if i := strings.IndexByte(s, '\n'); i >= 0 {
				s = s[:i]
			}
			return truncRunes(s, 60)
		}
	}
	return ""
}

func prettyProject(raw string) string {
	proj := raw
	homeSlug := strings.ReplaceAll(home(), "/", "-")
	proj = strings.TrimPrefix(proj, homeSlug)
	proj = strings.TrimLeft(proj, "-")
	for _, pfx := range []string{"Projects-", "projects-", "Documents-", "code-", "src-"} {
		if strings.HasPrefix(proj, pfx) {
			proj = proj[len(pfx):]
			break
		}
	}
	proj = strings.ReplaceAll(proj, "/", "-")
	proj = strings.ReplaceAll(proj, " ", "-")
	proj = strings.Trim(proj, "-")
	if proj == "" {
		proj = "misc"
	}
	if len(proj) > 48 {
		proj = proj[:48]
	}
	return strings.TrimRight(proj, "-")
}

func sessionID(path string) string {
	sid := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if len(sid) > 8 {
		sid = sid[:8]
	}
	return sid
}

func docNameFor(path string) string {
	return "sessions/" + prettyProject(filepath.Base(filepath.Dir(path))) + "/" + sessionID(path)
}

func iterTranscripts() []string {
	dir := os.Getenv("CLAUDE_PROJECTS_DIR")
	if dir == "" {
		dir = filepath.Join(home(), ".claude", "projects")
	}
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == "subagents" {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".jsonl") {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// ── state ────────────────────────────────────────────────────────────────────
type stEntry struct {
	LastModified int64  `json:"last_modified"`
	Doc          string `json:"doc,omitempty"`
	Bytes        int    `json:"bytes,omitempty"`
}

func statePath() string { return filepath.Join(home(), ".vault-sync-state.json") }

func loadState() map[string]stEntry {
	m := map[string]stEntry{}
	if b, err := os.ReadFile(statePath()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func saveState(m map[string]stEntry) {
	b, _ := json.Marshal(m)
	tmp := statePath() + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, statePath())
	}
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// ── local sync ───────────────────────────────────────────────────────────────
func cmdSync(force, metaFlag bool) {
	if token() == "" {
		die(errors.New("no vault token (set one via vault-manager, env VAULT_TOKEN, or ~/.vault-token)"))
	}
	state := map[string]stEntry{}
	if !force {
		state = loadState()
	}
	metaOn := metaFlag || fileExists(filepath.Join(home(), ".vault-session-meta-on"))
	var synced, skipped, empty, failed, metaOK, metaFail int

	for _, jl := range iterTranscripts() {
		fi, err := os.Stat(jl)
		if err != nil || fi.Size() == 0 {
			continue
		}
		mt := fi.ModTime().Unix()
		if prev, ok := state[jl]; !force && ok && prev.LastModified == mt {
			skipped++
			continue
		}
		body := extractBody(jl)
		if body == "" {
			state[jl] = stEntry{LastModified: mt}
			empty++
			continue
		}
		if err := vaultPut(docNameFor(jl), body); err != nil {
			failed++
			continue
		}
		state[jl] = stEntry{LastModified: mt, Doc: docNameFor(jl), Bytes: len(body)}
		synced++
		if metaOn {
			if _, err := vaultCallTool("sync_session", sessionMeta(jl, body)); err == nil {
				metaOK++
			} else {
				metaFail++
			}
		}
	}
	saveState(state)
	res := map[string]any{"synced": synced, "skipped": skipped, "empty": empty, "failed": failed}
	if metaOn {
		res["meta_synced"] = metaOK
		res["meta_failed"] = metaFail
	}
	b, _ := json.Marshal(res)
	fmt.Println(string(b))
}

func sessionMeta(jl, body string) map[string]any {
	date := time.Now().Format("2006-01-02")
	if fi, err := os.Stat(jl); err == nil {
		date = fi.ModTime().Format("2006-01-02")
	}
	return map[string]any{
		"content": body, "project": prettyProject(filepath.Base(filepath.Dir(jl))),
		"session_id": sessionID(jl), "date": date,
		"tags": []string{"auto"}, "label": firstUserText(jl),
	}
}

// ── sessions ─────────────────────────────────────────────────────────────────
func cmdSessions(project, tag, since string, limit int) {
	args := map[string]any{}
	if project != "" {
		args["project"] = project
	}
	if tag != "" {
		args["tag"] = tag
	}
	if since != "" {
		args["since"] = since
	}
	if limit > 0 {
		args["limit"] = limit
	}
	txt, err := callText("list_sessions", args)
	if err != nil {
		die(err)
	}
	if jsonOut {
		fmt.Println(txt)
		return
	}
	var ss []map[string]any
	_ = json.Unmarshal([]byte(txt), &ss)
	if len(ss) == 0 {
		fmt.Println("(no sessions)")
		return
	}
	byProj := map[string][]map[string]any{}
	for _, s := range ss {
		byProj[str(s["project"])] = append(byProj[str(s["project"])], s)
	}
	projs := keysSorted(byProj)
	fmt.Printf("%d sessions:\n", len(ss))
	for _, p := range projs {
		fmt.Printf("\n▸ %s\n", p)
		for _, s := range byProj[p] {
			fmt.Printf("    %-11s %-28s [%s]\n", str(s["date"]), str(s["session_id"]), tagsOf(s))
			if lbl := str(s["label"]); lbl != "" {
				fmt.Printf("               %s\n", lbl)
			}
		}
	}
}

func cmdRebuild(project, id string) {
	txt, err := callText("rebuild_session", map[string]any{"project": project, "session_id": id})
	if err != nil {
		die(err)
	}
	fmt.Println(txt)
}

func cmdRmSession(project, id string) {
	txt, err := callText("delete_session", map[string]any{"project": project, "session_id": id})
	if err != nil {
		die(err)
	}
	fmt.Println(txt)
}

// ── docs: read ───────────────────────────────────────────────────────────────
func printList(name string, args map[string]any) {
	txt, err := callText(name, args)
	if err != nil {
		die(err)
	}
	if jsonOut {
		fmt.Println(txt)
		return
	}
	var items []string
	if json.Unmarshal([]byte(txt), &items) != nil {
		fmt.Println(txt)
		return
	}
	if len(items) == 0 {
		fmt.Println("(none)")
		return
	}
	for _, it := range items {
		fmt.Println(it)
	}
}

func cmdCat(path, as string) {
	txt, err := callText("read_doc", map[string]any{"path": path, "face": as})
	if err != nil {
		die(err)
	}
	fmt.Println(txt)
}

// ── docs: write ──────────────────────────────────────────────────────────────
func cmdPush(file, path string) {
	b, err := os.ReadFile(file)
	if err != nil {
		die(err)
	}
	if path == "" {
		base := filepath.Base(file)
		path = "docs/" + strings.TrimSuffix(base, filepath.Ext(base))
	}
	writeDoc(path, string(b), fmt.Sprintf("pushed %s → %s (%d bytes)", file, path, len(b)))
}

func cmdWrite(path string) {
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		die(err)
	}
	writeDoc(path, string(b), fmt.Sprintf("wrote %s (%d bytes from stdin)", path, len(b)))
}

func writeDoc(path, content, okMsg string) {
	if _, err := vaultCallTool("write_doc", map[string]any{"path": path, "content": content}); err != nil {
		die(err)
	}
	fmt.Println(okMsg)
}

func cmdMutate(tool, from, to string) { // delete/move/copy
	args := map[string]any{}
	switch tool {
	case "delete_doc":
		args["path"] = from
	default:
		args["from"], args["to"] = from, to
	}
	txt, err := callText(tool, args)
	if err != nil {
		die(err)
	}
	fmt.Println(txt)
}

// ── contexts ─────────────────────────────────────────────────────────────────
func cmdContexts() {
	txt, err := callText("list_contexts", map[string]any{})
	if err != nil {
		die(err)
	}
	if jsonOut {
		fmt.Println(txt)
		return
	}
	var cs []map[string]any
	_ = json.Unmarshal([]byte(txt), &cs)
	if len(cs) == 0 {
		fmt.Println("(no context sets)")
		return
	}
	fmt.Printf("%-28s %-9s %8s %8s\n", "NAME", "FACE", "MEMBERS", "TOKENS")
	for _, c := range cs {
		fmt.Printf("%-28s %-9s %8s %8s\n", str(c["name"]), str(c["face"]),
			numStr(c["member_count"]), numStr(c["tokens"]))
	}
}

func cmdContext(name, as string) {
	args := map[string]any{"name": name}
	if as != "" {
		args["face"] = as
	}
	txt, err := callText("get_context", args)
	if err != nil {
		die(err)
	}
	fmt.Println(txt)
}

func cmdContextCreate(name, face, prompt string, memberSpecs []string) {
	members := make([]map[string]any, 0, len(memberSpecs))
	for _, m := range memberSpecs {
		// path[:kind[:on|off]]   kind defaults to doc, enabled defaults to on
		parts := strings.Split(m, ":")
		path, kind, enabled := parts[0], "doc", true
		if len(parts) >= 2 && parts[1] != "" {
			kind = parts[1]
		}
		if len(parts) >= 3 {
			enabled = parseEnabled(parts[2])
		}
		members = append(members, map[string]any{"path": path, "kind": kind, "enabled": enabled})
	}
	args := map[string]any{"name": name, "face": face, "members": members}
	if prompt != "" {
		args["build_prompt"] = prompt
	}
	txt, err := callText("create_context", args)
	if err != nil {
		die(err)
	}
	fmt.Println(txt)
}

func cmdBuild(name string, force bool, prompt string) {
	args := map[string]any{"name": name}
	if force {
		args["force"] = true
	}
	if prompt != "" {
		args["prompt"] = prompt
	}
	txt, err := callText("build_context", args)
	if err != nil {
		die(err)
	}
	fmt.Println(txt)
}

func cmdBuildStatus(name string) {
	txt, err := callText("build_status", map[string]any{"name": name})
	if err != nil {
		die(err)
	}
	fmt.Println(txt)
}

// ── doctor ───────────────────────────────────────────────────────────────────
func cmdDoctor() {
	ok, no := "OK", "MISSING"
	tok := token()
	fmt.Println("vault CLI · health check")
	fmt.Println(strings.Repeat("-", 32))
	fmt.Printf("  base url      : %s\n", baseURL())
	tokStat := no
	if tok != "" {
		tokStat = ok
	}
	fmt.Printf("  token         : %s\n", tokStat)
	reach := no
	if tok != "" && vaultPing() {
		reach = ok
	}
	fmt.Printf("  reachable     : %s\n", reach)
	dir := os.Getenv("CLAUDE_PROJECTS_DIR")
	if dir == "" {
		dir = filepath.Join(home(), ".claude", "projects")
	}
	fmt.Printf("  transcripts   : %d on disk (%s)\n", len(iterTranscripts()), dir)
	fmt.Printf("  synced state  : %d tracked\n", len(loadState()))
}

// ── helpers ──────────────────────────────────────────────────────────────────
func str(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func numStr(v any) string {
	if f, ok := v.(float64); ok {
		return fmt.Sprintf("%d", int(f))
	}
	return str(v)
}

func tagsOf(s map[string]any) string {
	if arr, ok := s["tags"].([]any); ok {
		ts := make([]string, 0, len(arr))
		for _, t := range arr {
			ts = append(ts, str(t))
		}
		return strings.Join(ts, ",")
	}
	return ""
}

func keysSorted[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func die(err error) { fmt.Fprintln(os.Stderr, "✗ "+err.Error()); os.Exit(1) }

func parseEnabled(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "off", "false", "0", "no", "disabled", "disable":
		return false
	}
	return true
}

// confirmOrDie guards destructive ops. --yes skips; otherwise prompt on a TTY,
// or refuse outright when non-interactive (safe for scripts/agents).
func confirmOrDie(yes bool, action string) {
	if yes {
		return
	}
	if fi, _ := os.Stdin.Stat(); fi == nil || fi.Mode()&os.ModeCharDevice == 0 {
		die(fmt.Errorf("refusing to %s without --yes (non-interactive)", action))
	}
	fmt.Fprintf(os.Stderr, "%s? [y/N] ", action)
	var ans string
	_, _ = fmt.Scanln(&ans)
	if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
		die(errors.New("aborted"))
	}
}

// ── registry / config ────────────────────────────────────────────────────────
type registry struct {
	Active  string            `json:"active"`
	Servers map[string]server `json:"servers"`
}

func registryPath() string { return filepath.Join(home(), ".vault", "servers.json") }

func loadRegistry() registry {
	r := registry{Servers: map[string]server{}}
	if b, err := os.ReadFile(registryPath()); err == nil {
		_ = json.Unmarshal(b, &r)
	}
	if r.Servers == nil {
		r.Servers = map[string]server{}
	}
	return r
}

func saveRegistry(r registry) error {
	if err := os.MkdirAll(filepath.Dir(registryPath()), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	tmp := registryPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, registryPath())
}

func mask(t string) string {
	if t == "" {
		return "(none)"
	}
	if len(t) > 12 {
		return t[:6] + "…" + t[len(t)-4:]
	}
	return "…"
}

func tokenSource() string {
	if os.Getenv("VAULT_TOKEN") != "" {
		return "env VAULT_TOKEN"
	}
	if registryActive().Token != "" {
		return "registry: " + loadRegistry().Active
	}
	if fileExists(filepath.Join(home(), ".vault-token")) {
		return "~/.vault-token"
	}
	return "none"
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func cmdConfig() {
	r := loadRegistry()
	fmt.Println("vault CLI · config")
	fmt.Println(strings.Repeat("-", 44))
	fmt.Printf("  resolved base url : %s\n", baseURL())
	fmt.Printf("  token             : %s  (from %s)\n", mask(token()), tokenSource())
	fmt.Printf("  registry          : %s\n", registryPath())
	fmt.Printf("  active vault      : %s\n", or(r.Active, "(none)"))
	if len(r.Servers) > 0 {
		fmt.Println("  registered vaults :")
		for _, name := range keysSorted(r.Servers) {
			mark := " "
			if name == r.Active {
				mark = "*"
			}
			s := r.Servers[name]
			fmt.Printf("    %s %-14s %-38s %s\n", mark, name, s.URL, mask(s.Token))
		}
	}
	fmt.Printf("  logging flag      : %s  (~/.vault-logging-on)\n",
		onOff(fileExists(filepath.Join(home(), ".vault-logging-on"))))
	fmt.Printf("  session-meta flag : %s  (~/.vault-session-meta-on)\n",
		onOff(fileExists(filepath.Join(home(), ".vault-session-meta-on"))))
	fmt.Printf("  sync state        : %d tracked (%s)\n", len(loadState()), statePath())
}

func or(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

func cmdConfigUse(name string) {
	r := loadRegistry()
	if _, ok := r.Servers[name]; !ok {
		die(fmt.Errorf("unknown vault %q (vault config to list)", name))
	}
	r.Active = name
	if err := saveRegistry(r); err != nil {
		die(err)
	}
	fmt.Printf("✓ active vault → %s (%s)\n", name, r.Servers[name].URL)
}

func cmdConfigAdd(name, urlArg, tok string) {
	u := strings.TrimSuffix(strings.TrimRight(urlArg, "/"), "/mcp") // store base, no /mcp
	r := loadRegistry()
	r.Servers[name] = server{URL: u, Token: strings.TrimSpace(tok)}
	if r.Active == "" {
		r.Active = name
	}
	if err := saveRegistry(r); err != nil {
		die(err)
	}
	star := ""
	if r.Active == name {
		star = "  (active)"
	}
	fmt.Printf("✓ added %s → %s%s\n", name, u, star)
}

// ── Claude Code integration (setup) ──────────────────────────────────────────
// All JSON edits happen here in Go, so the desktop install needs no python.
const hookMarker = "vault sync" // identifies hooks this CLI installed

func loadJSONFile(path string) map[string]any {
	m := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m
}

func saveJSONFile(path string, m map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// isVaultHook reports whether a hook block was installed by us (vault CLI) or by
// the legacy python session-log hook — either way, ours to manage.
func isVaultHook(b any) bool {
	bm, ok := b.(map[string]any)
	if !ok {
		return false
	}
	hs, _ := bm["hooks"].([]any)
	for _, h := range hs {
		if hm, ok := h.(map[string]any); ok {
			c, _ := hm["command"].(string)
			if strings.Contains(c, hookMarker) || strings.Contains(c, "session-log.sh") {
				return true
			}
		}
	}
	return false
}

func dropVaultHooks(hooks map[string]any, event string) bool {
	arr, _ := hooks[event].([]any)
	out := make([]any, 0, len(arr))
	for _, b := range arr {
		if !isVaultHook(b) {
			out = append(out, b)
		}
	}
	hooks[event] = out
	return len(out) != len(arr)
}

func setHook(hooks map[string]any, event, cmd string) {
	dropVaultHooks(hooks, event) // replace any prior vault/legacy entry
	arr, _ := hooks[event].([]any)
	arr = append(arr, map[string]any{"matcher": "*",
		"hooks": []any{map[string]any{"type": "command", "command": cmd}}})
	hooks[event] = arr
}

func quoteCmd(p string) string {
	if strings.ContainsAny(p, " \t") {
		return `"` + p + `"`
	}
	return p
}

func cmdSetup(uninstall, noHook, noMCP bool) {
	cj := filepath.Join(home(), ".claude.json")
	settings := filepath.Join(home(), ".claude", "settings.json")

	if uninstall {
		d := loadJSONFile(cj)
		if ms, ok := d["mcpServers"].(map[string]any); ok {
			delete(ms, "vault")
			_ = saveJSONFile(cj, d)
		}
		s := loadJSONFile(settings)
		if hooks, ok := s["hooks"].(map[string]any); ok {
			dropVaultHooks(hooks, "SessionStart")
			dropVaultHooks(hooks, "Stop")
			_ = saveJSONFile(settings, s)
		}
		fmt.Println("✓ removed vault MCP server + sync hooks (binary + registry kept)")
		return
	}

	if token() == "" {
		die(errors.New("no vault token — run `vault config add <name> <url> <token>` first"))
	}
	self, err := os.Executable()
	if err != nil {
		self = "vault"
	}

	if !noMCP {
		d := loadJSONFile(cj)
		ms, _ := d["mcpServers"].(map[string]any)
		if ms == nil {
			ms = map[string]any{}
		}
		ms["vault"] = map[string]any{"type": "http", "url": baseURL() + "/mcp",
			"headers": map[string]any{"Authorization": "Bearer " + token()}}
		d["mcpServers"] = ms
		if err := saveJSONFile(cj, d); err != nil {
			die(err)
		}
		fmt.Printf("  ✓ MCP server 'vault' → %s\n", cj)
	}
	if !noHook {
		s := loadJSONFile(settings)
		hooks, _ := s["hooks"].(map[string]any)
		if hooks == nil {
			hooks = map[string]any{}
		}
		cmd := quoteCmd(self) + " sync"
		setHook(hooks, "SessionStart", cmd)
		setHook(hooks, "Stop", cmd)
		s["hooks"] = hooks
		if err := saveJSONFile(settings, s); err != nil {
			die(err)
		}
		fmt.Printf("  ✓ sync hooks (SessionStart+Stop → %s) → %s\n", cmd, settings)
	}
	fmt.Println("Restart Claude Code to apply.")
}

// parseArgs parses a flagset while tolerating flags placed AFTER positionals
// (Go's flag pkg stops at the first positional). Returns the positional args.
func parseArgs(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for len(args) > 0 {
		_ = fs.Parse(args)
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	return pos
}

func need(pos []string, n int, msg string) {
	if len(pos) < n {
		die(errors.New(msg))
	}
}

func usage() { usageTo(os.Stderr) }

func usageTo(w io.Writer) {
	fmt.Fprintln(w, `vault — context store CLI   (global: --json for raw output)

sessions
  vault sessions [--project P] [--tag T] [--since YYYY-MM-DD] [--limit N]
  vault rebuild <project> <id>          re-summarize a session's refined face
  vault rm-session <project> <id> [--yes]   delete a session

docs (read)
  vault ls [folder]                     list docs
  vault folders                         list folders
  vault find <query>                    search docs by name
  vault cat <path> [--as source|refined]   read a doc

docs (write)
  vault push <file> [--path P]          upload a local file as a doc
  vault write <path>                    write a doc from stdin
  vault rm <path> [--yes]               delete a doc
  vault mv <from> <to> [--yes]          move/rename a doc
  vault cp <from> <to>                  copy a doc

context sets
  vault contexts                        list context sets
  vault context <name> [--as source|refined|structured]   get/mount a context
  vault context-create <name> [--face F] [--prompt P] --member path[:kind[:on|off]] ...
  vault build <name> [--force] [--prompt P]   distill a context set (async)
  vault build-status <name>             check build progress

local / meta
  vault sync [--resync] [--meta]        scan transcripts → vault
  vault setup [--uninstall] [--no-hook] [--no-mcp]   wire Claude Code (MCP + auto-sync hooks)
  vault config                          show resolved config + registered vaults
  vault config use <name>               switch active vault
  vault config add <name> <url> <token> register a vault
  vault doctor                          config + reachability check
  vault version
  vault help                            show this help`)
}

func main() {
	// strip a global --json from anywhere in the args
	args := make([]string, 0, len(os.Args))
	for _, a := range os.Args {
		if a == "--json" || a == "-json" {
			jsonOut = true
			continue
		}
		args = append(args, a)
	}
	os.Args = args

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	rest := os.Args[2:]
	switch os.Args[1] {

	case "sessions":
		fs := flag.NewFlagSet("sessions", flag.ExitOnError)
		project := fs.String("project", "", "filter by project")
		tag := fs.String("tag", "", "filter by tag")
		since := fs.String("since", "", "only since YYYY-MM-DD")
		limit := fs.Int("limit", 0, "max results")
		parseArgs(fs, rest)
		cmdSessions(*project, *tag, *since, *limit)

	case "rebuild":
		pos := parseArgs(flag.NewFlagSet("rebuild", flag.ExitOnError), rest)
		need(pos, 2, "usage: vault rebuild <project> <id>")
		cmdRebuild(pos[0], pos[1])

	case "rm-session":
		fs := flag.NewFlagSet("rm-session", flag.ExitOnError)
		yes := fs.Bool("yes", false, "skip confirmation")
		fs.BoolVar(yes, "y", false, "skip confirmation")
		pos := parseArgs(fs, rest)
		need(pos, 2, "usage: vault rm-session <project> <id> [--yes]")
		confirmOrDie(*yes, fmt.Sprintf("delete session %s/%s", pos[0], pos[1]))
		cmdRmSession(pos[0], pos[1])

	case "ls":
		pos := parseArgs(flag.NewFlagSet("ls", flag.ExitOnError), rest)
		a := map[string]any{}
		if len(pos) > 0 {
			a["folder"] = pos[0]
		}
		printList("list_docs", a)

	case "folders":
		printList("list_folders", map[string]any{})

	case "find":
		pos := parseArgs(flag.NewFlagSet("find", flag.ExitOnError), rest)
		need(pos, 1, "usage: vault find <query>")
		printList("find_docs", map[string]any{"query": pos[0]})

	case "cat":
		fs := flag.NewFlagSet("cat", flag.ExitOnError)
		as := fs.String("as", "source", "view: source|refined")
		pos := parseArgs(fs, rest)
		need(pos, 1, "usage: vault cat <path> [--as source|refined]")
		cmdCat(pos[0], *as)

	case "push":
		fs := flag.NewFlagSet("push", flag.ExitOnError)
		path := fs.String("path", "", "vault doc path (default docs/<filename>)")
		pos := parseArgs(fs, rest)
		need(pos, 1, "usage: vault push <file> [--path P]")
		cmdPush(pos[0], *path)

	case "write":
		pos := parseArgs(flag.NewFlagSet("write", flag.ExitOnError), rest)
		need(pos, 1, "usage: vault write <path>   (content from stdin)")
		cmdWrite(pos[0])

	case "rm":
		fs := flag.NewFlagSet("rm", flag.ExitOnError)
		yes := fs.Bool("yes", false, "skip confirmation")
		fs.BoolVar(yes, "y", false, "skip confirmation")
		pos := parseArgs(fs, rest)
		need(pos, 1, "usage: vault rm <path> [--yes]")
		confirmOrDie(*yes, "delete doc "+pos[0])
		cmdMutate("delete_doc", pos[0], "")

	case "mv":
		fs := flag.NewFlagSet("mv", flag.ExitOnError)
		yes := fs.Bool("yes", false, "skip confirmation")
		fs.BoolVar(yes, "y", false, "skip confirmation")
		pos := parseArgs(fs, rest)
		need(pos, 2, "usage: vault mv <from> <to> [--yes]")
		confirmOrDie(*yes, fmt.Sprintf("move %s → %s", pos[0], pos[1]))
		cmdMutate("move_doc", pos[0], pos[1])

	case "cp":
		pos := parseArgs(flag.NewFlagSet("cp", flag.ExitOnError), rest)
		need(pos, 2, "usage: vault cp <from> <to>")
		cmdMutate("copy_doc", pos[0], pos[1])

	case "contexts":
		cmdContexts()

	case "context":
		fs := flag.NewFlagSet("context", flag.ExitOnError)
		as := fs.String("as", "", "view: source|refined|structured")
		pos := parseArgs(fs, rest)
		need(pos, 1, "usage: vault context <name> [--as source|refined|structured]")
		cmdContext(pos[0], *as)

	case "context-create":
		fs := flag.NewFlagSet("context-create", flag.ExitOnError)
		face := fs.String("face", "refined", "source|refined")
		prompt := fs.String("prompt", "", "custom build prompt")
		var members []string
		fs.Func("member", "path[:kind] (repeatable; kind defaults to doc)", func(s string) error {
			members = append(members, s)
			return nil
		})
		pos := parseArgs(fs, rest)
		need(pos, 1, "usage: vault context-create <name> [--face F] [--prompt P] --member path[:kind] ...")
		cmdContextCreate(pos[0], *face, *prompt, members)

	case "build":
		fs := flag.NewFlagSet("build", flag.ExitOnError)
		force := fs.Bool("force", false, "force rebuild")
		prompt := fs.String("prompt", "", "custom prompt")
		pos := parseArgs(fs, rest)
		need(pos, 1, "usage: vault build <name> [--force] [--prompt P]")
		cmdBuild(pos[0], *force, *prompt)

	case "build-status":
		pos := parseArgs(flag.NewFlagSet("build-status", flag.ExitOnError), rest)
		need(pos, 1, "usage: vault build-status <name>")
		cmdBuildStatus(pos[0])

	case "sync":
		fs := flag.NewFlagSet("sync", flag.ExitOnError)
		force := fs.Bool("resync", false, "forget state and re-upload everything")
		meta := fs.Bool("meta", false, "also register session metadata (list_sessions)")
		parseArgs(fs, rest)
		cmdSync(*force, *meta)

	case "config":
		if len(rest) == 0 {
			cmdConfig()
			return
		}
		sub, subArgs := rest[0], rest[1:]
		switch sub {
		case "use":
			pos := parseArgs(flag.NewFlagSet("config use", flag.ExitOnError), subArgs)
			need(pos, 1, "usage: vault config use <name>")
			cmdConfigUse(pos[0])
		case "add":
			pos := parseArgs(flag.NewFlagSet("config add", flag.ExitOnError), subArgs)
			need(pos, 3, "usage: vault config add <name> <base-url> <token>")
			cmdConfigAdd(pos[0], pos[1], pos[2])
		default:
			die(errors.New("usage: vault config [use <name> | add <name> <url> <token>]"))
		}

	case "setup":
		fs := flag.NewFlagSet("setup", flag.ExitOnError)
		un := fs.Bool("uninstall", false, "remove the MCP server + sync hooks")
		noHook := fs.Bool("no-hook", false, "skip the auto-sync hooks")
		noMCP := fs.Bool("no-mcp", false, "skip the MCP server")
		parseArgs(fs, rest)
		cmdSetup(*un, *noHook, *noMCP)

	case "doctor":
		cmdDoctor()

	case "version", "-v", "--version":
		fmt.Println("vault " + version)

	case "help", "-h", "--help":
		usageTo(os.Stdout) // explicit help → stdout, exit 0

	default:
		usage()
		os.Exit(2)
	}
}
