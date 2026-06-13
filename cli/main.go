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
	_ "embed"
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
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// version is overridden at build time via -ldflags "-X main.version=…".
var version = "0.1.0-dev"

// skillMD is the `vault` skill, embedded so `vault setup` / `vault skill` can
// install it offline — no install.sh required, always matching this binary.
//
//go:embed skill/SKILL.md
var skillMD string

const (
	maxBody = 100_000 // chars; matches python MAX_BODY
	// ghRepo is THIS tool's own source repo, used for self-update. It is NOT a
	// vault — no vault URL/token is baked into the binary.
	ghRepo = "l1mzh0317/vault-cli"
)

var errNoVault = errors.New("no vault configured — run `vault config add <name> <url> <token>` (or set VAULT_URL / VAULT_TOKEN)")

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
	return strings.TrimRight(u, "/") // "" if nothing configured — no baked-in default
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
	if baseURL() == "" {
		return errNoVault
	}
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
	if baseURL() == "" {
		return nil, errNoVault
	}
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

// ── transcript parsing ───────────────────────────────────────────────────────
type tLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   struct {
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

func sessionID(path string) string {
	sid := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if len(sid) > 8 {
		sid = sid[:8]
	}
	return sid
}

// titleLine carries the two title sidecar entries Claude Code writes into a
// transcript: customTitle (user rename) and aiTitle (auto-generated summary).
type titleLine struct {
	Type        string `json:"type"`
	CustomTitle string `json:"customTitle"`
	AiTitle     string `json:"aiTitle"`
}

// readTitle returns the human name for a session: the user's manual rename if
// present, else Claude Code's auto-generated title, else "" (caller falls back
// to the thread id). Both are sidecar entries; the last one of each type wins.
func readTitle(path string) string {
	sc, f, err := scanLines(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var custom, ai string
	for sc.Scan() {
		var tl titleLine
		if json.Unmarshal(sc.Bytes(), &tl) != nil {
			continue
		}
		switch tl.Type {
		case "custom-title":
			if tl.CustomTitle != "" {
				custom = tl.CustomTitle
			}
		case "ai-title":
			if tl.AiTitle != "" {
				ai = tl.AiTitle
			}
		}
	}
	if custom != "" {
		return custom // user rename wins
	}
	return ai
}

// dayFrag is one calendar-day slice of a transcript (local time).
type dayFrag struct {
	Date string // YYYY-MM-DD, local
	HHMM string // start time of this day's first message, local
	Body string // "## role\ncontent" blocks for this day
}

// splitByDay partitions a transcript's user/assistant turns into per-local-day
// fragments. Timestamps are UTC in the file; we convert to local before taking
// the date, so a 23:30-local message lands on the right day. Lines without a
// parseable timestamp inherit the previous line's day (else the file's mtime).
func splitByDay(path string, fallbackDate string) []dayFrag {
	sc, f, err := scanLines(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	parts := map[string][]string{}
	firstTS := map[string]time.Time{}
	lastDate := ""
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
		if strings.TrimSpace(content) == "" {
			continue
		}
		var t time.Time
		haveT := false
		if ln.Timestamp != "" {
			if pt, perr := time.Parse(time.RFC3339, ln.Timestamp); perr == nil {
				t = pt.Local()
				haveT = true
			}
		}
		date := lastDate
		if haveT {
			date = t.Format("2006-01-02")
		} else if date == "" {
			date = fallbackDate
		}
		parts[date] = append(parts[date], "## "+role+"\n"+content)
		if haveT {
			if cur, ok := firstTS[date]; !ok || t.Before(cur) {
				firstTS[date] = t
			}
		}
		lastDate = date
	}
	dates := keysSorted(parts) // lexical sort == chronological for YYYY-MM-DD
	out := make([]dayFrag, 0, len(dates))
	for _, d := range dates {
		hhmm := "0000"
		if t, ok := firstTS[d]; ok {
			hhmm = t.Format("1504")
		}
		out = append(out, dayFrag{
			Date: d,
			HHMM: hhmm,
			Body: truncRunes(strings.Join(parts[d], "\n\n"), maxBody),
		})
	}
	return out
}

// slugify turns a title into a path-safe, readable slug. Keeps ASCII
// alphanumerics and CJK (titles may be Chinese); everything else collapses to
// a single dash. Empty in → empty out (caller then names by time+id only).
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		case r >= 0x4e00 && r <= 0x9fff: // CJK ideographs
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	// trim again after truncation: a 40-rune cut can land on a dash boundary.
	return strings.Trim(truncRunes(strings.Trim(b.String(), "-"), 40), "-")
}

// ── secret scrubbing — redact credentials before any byte leaves the machine ──
// This is a hard requirement: transcripts routinely contain live tokens/keys,
// and the server's refine pipeline ships content to an LLM provider, so an
// un-scrubbed secret would leak twice (at rest + into refined summaries).
const redacted = "[REDACTED]"

var (
	// keyedSecretRes: group 1 is a label we keep; the value after it is redacted.
	keyedSecretRes = []*regexp.Regexp{
		regexp.MustCompile(`(?i)(authorization:\s*bearer\s+)\S+`),
		regexp.MustCompile(`(?i)((?:api[_-]?key|secret|token|password|passwd|access[_-]?token)\s*[:=]\s*["']?)[^\s"']{6,}`),
	}
	// tokenSecretRes: vendor-shaped credentials / high-entropy blobs, redacted whole.
	tokenSecretRes = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._\-]{16,}`),
		regexp.MustCompile(`sk-[A-Za-z0-9_\-]{20,}`),        // openai / anthropic
		regexp.MustCompile(`ghp_[A-Za-z0-9]{36}`),           // github PAT (classic)
		regexp.MustCompile(`gho_[A-Za-z0-9]{36}`),           // github oauth
		regexp.MustCompile(`github_pat_[A-Za-z0-9_]{50,}`),  // github PAT (fine-grained)
		regexp.MustCompile(`glpat-[A-Za-z0-9_\-]{20,}`),     // gitlab
		regexp.MustCompile(`xox[baprs]-[A-Za-z0-9\-]{10,}`), // slack
		regexp.MustCompile(`AKIA[0-9A-Z]{16}`),              // aws access key id
		regexp.MustCompile(`AIza[0-9A-Za-z_\-]{35}`),        // google api key
		regexp.MustCompile(`\b[0-9a-f]{41,}\b`),             // vault 48-hex token & longer (>40 skips git SHAs)
		regexp.MustCompile(`-----BEGIN[A-Z ]+PRIVATE KEY-----[\s\S]*?-----END[A-Z ]+PRIVATE KEY-----`),
	}
)

func scrubSecrets(s string) string {
	for _, re := range keyedSecretRes {
		s = re.ReplaceAllString(s, "${1}"+redacted)
	}
	for _, re := range tokenSecretRes {
		s = re.ReplaceAllString(s, redacted)
	}
	return s
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
	LastModified int64    `json:"last_modified"`
	Docs         []string `json:"docs,omitempty"` // per-day fragment paths from the last sync
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
// cmdSync scans local transcripts and ships them to the vault, splitting each
// conversation into one doc per calendar day under sessions/<date>/. The
// metaFlag is accepted for backward compat but inert: the old project-keyed
// sync_session index is retired in favour of frontmatter-linked day fragments.
func cmdSync(force, metaFlag bool) {
	_ = metaFlag
	if token() == "" {
		die(errors.New("no vault token (set one via vault-manager, env VAULT_TOKEN, or ~/.vault-token)"))
	}
	state := map[string]stEntry{}
	if !force {
		state = loadState()
	}
	var synced, skipped, empty, failed, pruned int

	for _, jl := range iterTranscripts() {
		fi, err := os.Stat(jl)
		if err != nil || fi.Size() == 0 {
			continue
		}
		mt := fi.ModTime().Unix()
		prev, hadPrev := state[jl]
		if !force && hadPrev && prev.LastModified == mt {
			skipped++
			continue
		}

		thread := sessionID(jl)
		slug := slugify(readTitle(jl))
		frags := splitByDay(jl, fi.ModTime().Format("2006-01-02"))
		if len(frags) == 0 {
			state[jl] = stEntry{LastModified: mt}
			empty++
			continue
		}

		// Resolve every fragment's path up front — we need them all to write
		// the prev/next links and part k/n into each fragment's frontmatter.
		paths := make([]string, len(frags))
		for i, fr := range frags {
			name := fr.HHMM + "-" + thread
			if slug != "" {
				name = slug + "-" + fr.HHMM + "-" + thread
			}
			paths[i] = "sessions/" + fr.Date + "/" + name
		}

		ok := true
		for i, fr := range frags {
			body := sessionFrontmatter(thread, fr, i, len(frags), paths, slug) +
				scrubSecrets(fr.Body) + "\n"
			if err := vaultPut(paths[i], body); err != nil {
				ok = false
				break
			}
		}
		if !ok {
			failed++
			continue
		}
		if hadPrev {
			pruned += pruneOrphans(prev.Docs, paths)
		}
		state[jl] = stEntry{LastModified: mt, Docs: paths}
		synced++
	}
	saveState(state)
	res := map[string]any{
		"synced": synced, "skipped": skipped, "empty": empty,
		"failed": failed, "pruned": pruned,
	}
	b, _ := json.Marshal(res)
	fmt.Println(string(b))
}

// sessionFrontmatter builds the YAML header that ties a conversation's per-day
// fragments together: a stable thread id, the day, part k/n, the day's start
// time, and prev/next links to the adjacent fragments.
func sessionFrontmatter(thread string, fr dayFrag, idx, total int, paths []string, slug string) string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("thread: " + thread + "\n")
	b.WriteString("day: " + fr.Date + "\n")
	b.WriteString(fmt.Sprintf("part: %d/%d\n", idx+1, total))
	b.WriteString("start: " + fr.HHMM + "\n")
	if slug != "" {
		b.WriteString("title: " + slug + "\n")
	}
	if idx > 0 {
		b.WriteString("prev: " + paths[idx-1] + "\n")
	}
	if idx < total-1 {
		b.WriteString("next: " + paths[idx+1] + "\n")
	}
	b.WriteString("---\n\n")
	return b.String()
}

// pruneOrphans soft-deletes docs written by a previous sync that the current
// sync no longer produces — i.e. fragments whose path changed because the
// title resolved (id → topic) or a day re-bucketed. Keeps the vault tidy.
func pruneOrphans(old, current []string) int {
	keep := make(map[string]bool, len(current))
	for _, p := range current {
		keep[p] = true
	}
	n := 0
	for _, p := range old {
		if keep[p] {
			continue
		}
		if _, err := vaultCallTool("delete_doc", map[string]any{"path": p}); err == nil {
			n++
		}
	}
	return n
}

// ── sessions ─────────────────────────────────────────────────────────────────
// sessionFrag is one per-day session doc, reconstructed purely from its path
// sessions/<date>/<slug>-<hhmm>-<thread> — no server-side session index needed.
type sessionFrag struct {
	Path, Date, HHMM, Thread, Slug string
}

func looksLikeDate(s string) bool {
	return len(s) == 10 && s[4] == '-' && s[7] == '-' &&
		isDigits(s[:4], 4) && isDigits(s[5:7], 2) && isDigits(s[8:], 2)
}

func isDigits(s string, n int) bool {
	if n >= 0 && len(s) != n {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

func isThreadID(s string) bool {
	if len(s) < 4 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// parseFragPath splits a session doc path back into its parts. The trailing
// -<hhmm>-<thread> is stripped only when it actually looks like one, so a slug
// that happens to contain dashes (or a doc with no slug) still parses sanely.
func parseFragPath(p string) sessionFrag {
	f := sessionFrag{Path: p}
	segs := strings.Split(p, "/")
	if len(segs) >= 2 {
		f.Date = segs[len(segs)-2]
	}
	name := segs[len(segs)-1]
	parts := strings.Split(name, "-")
	if n := len(parts); n >= 2 && isDigits(parts[n-2], 4) && isThreadID(parts[n-1]) {
		f.Thread = parts[n-1]
		f.HHMM = parts[n-2]
		f.Slug = strings.Join(parts[:n-2], "-")
	} else {
		f.Slug = name
	}
	return f
}

// listSessionFragments enumerates every session doc by walking the date folders
// under sessions/ and parsing the paths — the client reconstructs the model
// from the layout itself, so no server-side session table is required.
func listSessionFragments(since string) []sessionFrag {
	foldersTxt, err := callText("list_folders", map[string]any{})
	if err != nil {
		die(err)
	}
	var folders []string
	_ = json.Unmarshal([]byte(foldersTxt), &folders)
	var frags []sessionFrag
	for _, fld := range folders {
		date, ok := strings.CutPrefix(fld, "sessions/")
		if !ok || strings.Contains(date, "/") || !looksLikeDate(date) {
			continue
		}
		if since != "" && date < since {
			continue
		}
		docsTxt, err := callText("list_docs", map[string]any{"folder": fld})
		if err != nil {
			continue
		}
		var docs []string
		_ = json.Unmarshal([]byte(docsTxt), &docs)
		for _, d := range docs {
			// list_docs returns names relative to the folder; rebuild the
			// full path so parseFragPath (and delete_doc) see the date too.
			full := d
			if !strings.HasPrefix(full, "sessions/") {
				full = fld + "/" + d
			}
			f := parseFragPath(full)
			if f.Date == "" {
				f.Date = date
			}
			frags = append(frags, f)
		}
	}
	return frags
}

// cmdSessions lists conversations, grouping a thread's per-day fragments so a
// multi-day conversation shows as one entry spanning its days.
func cmdSessions(since, thread string, limit int) {
	frags := listSessionFragments(since)
	if thread != "" {
		kept := frags[:0]
		for _, f := range frags {
			if f.Thread == thread {
				kept = append(kept, f)
			}
		}
		frags = kept
	}
	if jsonOut {
		b, _ := json.Marshal(frags)
		fmt.Println(string(b))
		return
	}
	if len(frags) == 0 {
		fmt.Println("(no sessions)")
		return
	}

	byThread := map[string][]sessionFrag{}
	for _, f := range frags {
		byThread[f.Thread] = append(byThread[f.Thread], f)
	}
	type conv struct {
		id, latest, title string
		frags             []sessionFrag
	}
	var convs []conv
	for _, t := range keysSorted(byThread) {
		fs := byThread[t]
		sort.Slice(fs, func(i, j int) bool { return fs[i].Date < fs[j].Date })
		title := ""
		for _, f := range fs {
			if f.Slug != "" {
				title = f.Slug
				break
			}
		}
		convs = append(convs, conv{id: t, latest: fs[len(fs)-1].Date, title: title, frags: fs})
	}
	sort.Slice(convs, func(i, j int) bool {
		if convs[i].latest != convs[j].latest {
			return convs[i].latest > convs[j].latest // most recent first
		}
		return convs[i].id < convs[j].id
	})
	if limit > 0 && len(convs) > limit {
		convs = convs[:limit]
	}

	fmt.Printf("%d conversation(s):\n", len(convs))
	for _, c := range convs {
		title := c.title
		if title == "" {
			title = "(untitled)"
		}
		span := c.frags[0].Date
		if last := c.frags[len(c.frags)-1].Date; last != span {
			span += " → " + last
		}
		fmt.Printf("\n▸ %s  [%s]  %s\n", title, c.id, span)
		for _, f := range c.frags {
			fmt.Printf("    %s %s  %s\n", f.Date, f.HHMM, f.Path)
		}
	}
}

// cmdRmSession deletes every per-day fragment sharing a thread id.
func cmdRmSession(thread string) {
	var victims []string
	for _, f := range listSessionFragments("") {
		if f.Thread == thread {
			victims = append(victims, f.Path)
		}
	}
	if len(victims) == 0 {
		fmt.Printf("(no session with thread %s)\n", thread)
		return
	}
	n := 0
	for _, p := range victims {
		if _, err := vaultCallTool("delete_doc", map[string]any{"path": p}); err == nil {
			n++
		} else {
			fmt.Fprintln(os.Stderr, "  ✗ "+p)
		}
	}
	fmt.Printf("deleted %d/%d fragment(s) of thread %s\n", n, len(victims), thread)
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
	fmt.Printf("  base url      : %s\n", or(baseURL(), "(not configured)"))
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
	warnStaleRefs()
}

// warnStaleRefs flags (does NOT edit) references to the old python hook in the
// user's CLAUDE.md files, so they can clean them up by hand. Editing a user's
// CLAUDE.md automatically would be invasive, so this only warns.
func warnStaleRefs() {
	stale := []string{"session-log.sh", "vault_sync.py"}
	files := []string{
		filepath.Join(home(), ".claude", "CLAUDE.md"),
		filepath.Join(home(), "CLAUDE.md"),
	}
	var hits []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, s := range stale {
			if strings.Contains(string(b), s) {
				hits = append(hits, fmt.Sprintf("%s mentions %q", f, s))
			}
		}
	}
	if len(hits) > 0 {
		fmt.Println("  stale notes   : ⚠ old python-hook references found — consider removing:")
		for _, h := range hits {
			fmt.Println("                    - " + h)
		}
	}
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

var (
	claudeJSON   = func() string { return filepath.Join(home(), ".claude.json") }
	settingsJSON = func() string { return filepath.Join(home(), ".claude", "settings.json") }
	skillPath    = func() string { return filepath.Join(home(), ".claude", "skills", "vault", "SKILL.md") }
)

// ── individual setup steps (each idempotent) ─────────────────────────────────
func setupMCP() error {
	if token() == "" {
		return errors.New("no vault token — run `vault config add <name> <url> <token>` first")
	}
	d := loadJSONFile(claudeJSON())
	ms, _ := d["mcpServers"].(map[string]any)
	if ms == nil {
		ms = map[string]any{}
	}
	ms["vault"] = map[string]any{"type": "http", "url": baseURL() + "/mcp",
		"headers": map[string]any{"Authorization": "Bearer " + token()}}
	d["mcpServers"] = ms
	return saveJSONFile(claudeJSON(), d)
}

func setupHooks() error {
	self, err := os.Executable()
	if err != nil {
		self = "vault"
	}
	d := loadJSONFile(settingsJSON())
	hooks, _ := d["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	cmd := quoteCmd(self) + " sync"
	setHook(hooks, "SessionStart", cmd)
	setHook(hooks, "Stop", cmd)
	d["hooks"] = hooks
	return saveJSONFile(settingsJSON(), d)
}

func installSkill() error {
	if err := os.MkdirAll(filepath.Dir(skillPath()), 0o755); err != nil {
		return err
	}
	return os.WriteFile(skillPath(), []byte(skillMD), 0o644)
}

func uninstallSkill() error {
	return os.RemoveAll(filepath.Dir(skillPath()))
}

// ── interactive checklist (stdlib only, line-based) ──────────────────────────
type setupStep struct {
	label, detail string
	on            bool
	apply         func() error
}

func isInteractive() bool {
	fi, _ := os.Stdin.Stat()
	return fi != nil && fi.Mode()&os.ModeCharDevice != 0
}

// chooseSteps shows the checklist and lets the user toggle by number. Returns
// false if the user aborts.
func chooseSteps(steps []*setupStep) bool {
	r := bufio.NewReader(os.Stdin)
	for {
		fmt.Println("\nvault setup will apply:")
		for i, s := range steps {
			mark := " "
			if s.on {
				mark = "x"
			}
			fmt.Printf("  [%s] %d. %-22s %s\n", mark, i+1, s.label, s.detail)
		}
		fmt.Print("Proceed? [Y/n], or numbers to toggle (e.g. \"1 3\"): ")
		line, _ := r.ReadString('\n')
		line = strings.TrimSpace(line)
		switch strings.ToLower(line) {
		case "", "y", "yes":
			return true
		case "n", "no", "q":
			return false
		}
		for _, tok := range strings.Fields(line) {
			var n int
			if _, err := fmt.Sscanf(tok, "%d", &n); err == nil && n >= 1 && n <= len(steps) {
				steps[n-1].on = !steps[n-1].on
			}
		}
	}
}

func cmdSetup(uninstall, noHook, noMCP, noSkill, yes bool) {
	if uninstall {
		d := loadJSONFile(claudeJSON())
		if ms, ok := d["mcpServers"].(map[string]any); ok {
			delete(ms, "vault")
			_ = saveJSONFile(claudeJSON(), d)
		}
		s := loadJSONFile(settingsJSON())
		if hooks, ok := s["hooks"].(map[string]any); ok {
			dropVaultHooks(hooks, "SessionStart")
			dropVaultHooks(hooks, "Stop")
			_ = saveJSONFile(settingsJSON(), s)
		}
		_ = uninstallSkill()
		fmt.Println("✓ removed vault MCP server + sync hooks + skill (binary + registry kept)")
		return
	}

	steps := []*setupStep{
		{"MCP server", "→ ~/.claude.json (reads inside a model)", !noMCP, setupMCP},
		{"auto-sync hooks", "→ settings.json (SessionStart/Stop: vault sync)", !noHook, setupHooks},
		{"vault skill", "→ ~/.claude/skills/vault (so Claude knows the CLI)", !noSkill, installSkill},
	}

	// Interactive checklist for humans; agents / pipes / --yes skip it.
	if isInteractive() && !yes {
		if !chooseSteps(steps) {
			fmt.Println("aborted — nothing changed")
			return
		}
	}

	applied := 0
	for _, s := range steps {
		if !s.on {
			continue
		}
		if err := s.apply(); err != nil {
			die(err)
		}
		fmt.Printf("  ✓ %s %s\n", s.label, s.detail)
		applied++
	}
	if applied == 0 {
		fmt.Println("nothing selected")
		return
	}
	fmt.Println("Restart Claude Code to apply.")
}

// ── self-update ──────────────────────────────────────────────────────────────
func httpGet(u string) ([]byte, error) {
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s -> HTTP %d", u, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func latestTag() (string, error) {
	b, err := httpGet("https://api.github.com/repos/" + ghRepo + "/releases/latest")
	if err != nil {
		return "", err
	}
	var d struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return "", err
	}
	if d.TagName == "" {
		return "", errors.New("no release found")
	}
	return d.TagName, nil
}

func refreshSkill() {
	b, err := httpGet("https://raw.githubusercontent.com/" + ghRepo + "/main/cli/skill/SKILL.md")
	if err != nil {
		return
	}
	dir := filepath.Join(home(), ".claude", "skills", "vault")
	if os.MkdirAll(dir, 0o755) == nil && os.WriteFile(filepath.Join(dir, "SKILL.md"), b, 0o644) == nil {
		fmt.Println("  ✓ skill refreshed")
	}
}

func cmdUpdate(checkOnly bool) {
	tag, err := latestTag()
	if err != nil {
		die(err)
	}
	latest := strings.TrimPrefix(tag, "cli-v")
	fmt.Printf("current %s · latest %s\n", version, latest)
	if checkOnly {
		if latest == version {
			fmt.Println("already up to date ✓")
		} else {
			fmt.Println("update available → run `vault update`")
		}
		return
	}
	if latest == version {
		fmt.Println("binary already up to date ✓")
		refreshSkill() // skill is docs — may have changed independently
		return
	}
	asset := "vault-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		asset += ".exe"
	}
	bin, err := httpGet("https://github.com/" + ghRepo + "/releases/download/" + tag + "/" + asset)
	if err != nil {
		die(fmt.Errorf("download %s: %w", asset, err))
	}
	self, err := os.Executable()
	if err != nil {
		die(err)
	}
	if rp, err := filepath.EvalSymlinks(self); err == nil {
		self = rp
	}
	tmp := self + ".new"
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		die(err)
	}
	if err := os.Rename(tmp, self); err != nil { // replaces a running binary on Unix
		os.Remove(tmp)
		die(fmt.Errorf("replace %s: %w (on Windows, close vault and rename %s by hand)", self, err, tmp))
	}
	fmt.Printf("✓ updated %s → %s\n", version, latest)
	refreshSkill()
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

sessions  (stored as per-day docs under sessions/<date>/, linked by thread id)
  vault sessions [--since YYYY-MM-DD] [--thread ID] [--limit N]   list conversations
  vault rm-session <thread> [--yes]     delete every day-fragment of a thread

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
  vault setup [--uninstall] [-y] [--no-hook] [--no-mcp] [--no-skill]   wire Claude Code (interactive)
  vault skill [--uninstall]             install the bundled /vault skill
  vault config                          show resolved config + registered vaults
  vault config use <name>               switch active vault
  vault config add <name> <url> <token> register a vault
  vault update [--check]                self-update to the latest release
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
		since := fs.String("since", "", "only days on/after YYYY-MM-DD")
		thread := fs.String("thread", "", "only this thread id")
		limit := fs.Int("limit", 0, "max conversations")
		parseArgs(fs, rest)
		cmdSessions(*since, *thread, *limit)

	case "rm-session":
		fs := flag.NewFlagSet("rm-session", flag.ExitOnError)
		yes := fs.Bool("yes", false, "skip confirmation")
		fs.BoolVar(yes, "y", false, "skip confirmation")
		pos := parseArgs(fs, rest)
		need(pos, 1, "usage: vault rm-session <thread> [--yes]")
		confirmOrDie(*yes, fmt.Sprintf("delete all fragments of thread %s", pos[0]))
		cmdRmSession(pos[0])

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
		un := fs.Bool("uninstall", false, "remove the MCP server + sync hooks + skill")
		noHook := fs.Bool("no-hook", false, "skip the auto-sync hooks")
		noMCP := fs.Bool("no-mcp", false, "skip the MCP server")
		noSkill := fs.Bool("no-skill", false, "skip installing the skill")
		yes := fs.Bool("yes", false, "non-interactive: apply without the checklist")
		fs.BoolVar(yes, "y", false, "")
		parseArgs(fs, rest)
		cmdSetup(*un, *noHook, *noMCP, *noSkill, *yes)

	case "skill":
		fs := flag.NewFlagSet("skill", flag.ExitOnError)
		un := fs.Bool("uninstall", false, "remove the installed skill")
		parseArgs(fs, rest)
		if *un {
			if err := uninstallSkill(); err != nil {
				die(err)
			}
			fmt.Println("✓ removed " + filepath.Dir(skillPath()))
		} else {
			if err := installSkill(); err != nil {
				die(err)
			}
			fmt.Println("✓ skill → " + skillPath() + "  (restart Claude Code to load /vault)")
		}

	case "update":
		fs := flag.NewFlagSet("update", flag.ExitOnError)
		check := fs.Bool("check", false, "only check for a newer version, don't install")
		parseArgs(fs, rest)
		cmdUpdate(*check)

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
