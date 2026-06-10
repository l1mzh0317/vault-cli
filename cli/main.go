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
// Split of responsibilities: use the remote MCP server for READS/queries
// (small payloads the model needs anyway); use this CLI for WRITES/bulk
// (large payloads that must stay out of context).
//
// Commands:
//
//	vault sync [--resync] [--meta]   scan ~/.claude/projects/**/*.jsonl → vault
//	vault push <file> [--path P]     upload a local file as a doc (write_doc)
//	vault doctor                     config + reachability health check
//	vault version
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
	"strings"
	"time"
)

// version is overridden at build time via -ldflags "-X main.version=…".
var version = "0.1.0-dev"

const (
	maxBody    = 100_000 // chars; matches python MAX_BODY
	defaultURL = "https://longku-vault.zeabur.app"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

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
// quotePath escapes each path segment but keeps the "/" separators, matching
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
		// tolerate SSE 'data: {...}' framing
		for _, line := range strings.Split(string(raw), "\n") {
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

// toolText pulls the first content text block out of an MCP tool result, e.g.
// the server's "invalid path" message.
func toolText(res map[string]any) string {
	if c, ok := res["content"].([]any); ok && len(c) > 0 {
		if m, ok := c[0].(map[string]any); ok {
			if t, ok := m["text"].(string); ok {
				return t
			}
		}
	}
	return "isError"
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
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024) // tolerate long lines
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
	proj := prettyProject(filepath.Base(filepath.Dir(path)))
	return "sessions/" + proj + "/" + sessionID(path)
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

// ── commands ─────────────────────────────────────────────────────────────────
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

func cmdPush(file, path string) {
	b, err := os.ReadFile(file)
	if err != nil {
		die(err)
	}
	if path == "" {
		// vault doc paths can't carry a file extension — strip it.
		base := filepath.Base(file)
		path = "docs/" + strings.TrimSuffix(base, filepath.Ext(base))
	}
	if _, err := vaultCallTool("write_doc", map[string]any{"path": path, "content": string(b)}); err != nil {
		die(err)
	}
	fmt.Printf("pushed %s → %s (%d bytes)\n", file, path, len(b))
}

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

func die(err error) { fmt.Fprintln(os.Stderr, "✗ "+err.Error()); os.Exit(1) }

func usage() {
	fmt.Fprintln(os.Stderr, `vault — context store CLI

  vault sync [--resync] [--meta]   scan transcripts → vault (token-free upload)
  vault push <file> [--path P]     upload a local file as a doc
  vault doctor                     config + reachability check
  vault version`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "sync":
		fs := flag.NewFlagSet("sync", flag.ExitOnError)
		force := fs.Bool("resync", false, "forget state and re-upload everything")
		meta := fs.Bool("meta", false, "also register session metadata (list_sessions)")
		_ = fs.Parse(os.Args[2:])
		cmdSync(*force, *meta)
	case "push":
		fs := flag.NewFlagSet("push", flag.ExitOnError)
		path := fs.String("path", "", "vault doc path (default docs/<filename>)")
		// Go's flag pkg stops at the first positional, so loop to also accept
		// flags placed AFTER the file: `vault push <file> --path P`.
		args, file := os.Args[2:], ""
		for len(args) > 0 {
			_ = fs.Parse(args)
			if fs.NArg() == 0 {
				break
			}
			if file == "" {
				file = fs.Arg(0)
			}
			args = fs.Args()[1:]
		}
		if file == "" {
			die(errors.New("usage: vault push <file> [--path P]"))
		}
		cmdPush(file, *path)
	case "doctor":
		cmdDoctor()
	case "version", "-v", "--version":
		fmt.Println("vault " + version)
	default:
		usage()
		os.Exit(2)
	}
}
