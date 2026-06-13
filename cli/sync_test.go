package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func init() { time.Local = time.UTC } // make date bucketing deterministic in tests

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "abcd1234ef.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSplitByDay(t *testing.T) {
	p := writeTranscript(t,
		`{"type":"ai-title","aiTitle":"Build the thing"}`,
		`{"type":"user","timestamp":"2026-06-11T03:30:00.000Z","message":{"role":"user","content":"day one hello"}}`,
		`{"type":"assistant","timestamp":"2026-06-11T03:31:00.000Z","message":{"role":"assistant","content":"day one reply"}}`,
		`{"type":"user","timestamp":"2026-06-12T09:15:00.000Z","message":{"role":"user","content":"day two hello"}}`,
		`{"type":"system","timestamp":"2026-06-12T09:16:00.000Z","content":"ignored system line"}`,
	)
	frags := splitByDay(p, "2026-06-11")
	if len(frags) != 2 {
		t.Fatalf("want 2 day fragments, got %d: %+v", len(frags), frags)
	}
	if frags[0].Date != "2026-06-11" || frags[1].Date != "2026-06-12" {
		t.Fatalf("dates wrong / out of order: %s, %s", frags[0].Date, frags[1].Date)
	}
	if frags[0].HHMM != "0330" || frags[1].HHMM != "0915" {
		t.Fatalf("HHMM wrong: %s, %s", frags[0].HHMM, frags[1].HHMM)
	}
	if !strings.Contains(frags[0].Body, "day one hello") || !strings.Contains(frags[0].Body, "## assistant") {
		t.Fatalf("day-one body missing content: %q", frags[0].Body)
	}
	if strings.Contains(frags[0].Body, "day two") {
		t.Fatalf("day-one fragment leaked day-two content")
	}
	if strings.Contains(frags[1].Body, "ignored system line") {
		t.Fatalf("system line should not appear in body")
	}
}

func TestReadTitle(t *testing.T) {
	// custom (user rename) wins over ai-title
	p := writeTranscript(t,
		`{"type":"ai-title","aiTitle":"auto name"}`,
		`{"type":"custom-title","customTitle":"my rename"}`,
		`{"type":"ai-title","aiTitle":"newer auto name"}`,
	)
	if got := readTitle(p); got != "my rename" {
		t.Fatalf("custom rename should win, got %q", got)
	}
	// no custom → last ai-title
	p2 := writeTranscript(t,
		`{"type":"ai-title","aiTitle":"first"}`,
		`{"type":"ai-title","aiTitle":"last"}`,
	)
	if got := readTitle(p2); got != "last" {
		t.Fatalf("want last ai-title, got %q", got)
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Troubleshoot MCP tool integration": "troubleshoot-mcp-tool-integration",
		"  Hello, World!!  ":                "hello-world",
		"":                                  "",
		"vault-server-dev":                  "vault-server-dev",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
	// CJK is preserved
	if got := slugify("会话归档设计"); got != "会话归档设计" {
		t.Errorf("CJK slug = %q", got)
	}
}

func TestScrubSecrets(t *testing.T) {
	redactedCases := []string{
		"Authorization: Bearer 0052c321e3489af4911fb418b60d6732d9dcf4ad",
		"export OPENAI_KEY=sk-abcdefghijklmnopqrstuvwxyz0123",
		"token: supersecretvalue123",
		"the vault token is 0052c321e3489af4911fb418b60d6732d9dcf4ad5a8eccde",
		"ghp_0123456789abcdef0123456789abcdef0123",
	}
	for _, in := range redactedCases {
		out := scrubSecrets(in)
		if !strings.Contains(out, redacted) {
			t.Errorf("scrubSecrets(%q) = %q — expected a redaction", in, out)
		}
	}
	// the label is kept for keyed forms
	if got := scrubSecrets("token: supersecretvalue123"); !strings.HasPrefix(got, "token:") {
		t.Errorf("keyed scrub should keep label: %q", got)
	}
	// ordinary prose is untouched
	for _, in := range []string{"the password reset flow works fine", "a 40-hex git sha 0123456789abcdef0123456789abcdef01234567"} {
		if scrubSecrets(in) != in {
			t.Errorf("scrubSecrets over-redacted benign text: %q -> %q", in, scrubSecrets(in))
		}
	}
}

func TestSyncMarks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sid := "24202849-25ee-4b45-93d7-38a2ceb5c6e0"
	if sessionMark(sid) != "" {
		t.Fatal("unmarked session should resolve to empty")
	}
	if err := setMark(sid, "off"); err != nil {
		t.Fatal(err)
	}
	if got := sessionMark(sid); got != "off" {
		t.Fatalf("mark = %q, want off", got)
	}
	if err := setMark(sid, "on"); err != nil {
		t.Fatal(err)
	}
	if got := sessionMark(sid); got != "on" {
		t.Fatalf("mark = %q, want on (overwrite)", got)
	}
	// sparse: a different, unmarked sid stays empty
	if sessionMark("deadbeef-0000") != "" {
		t.Fatal("other sid should be unmarked")
	}
}

func TestThreadAndTranscriptSID(t *testing.T) {
	sid := "24202849-25ee-4b45-93d7-38a2ceb5c6e0"
	if got := threadOf(sid); got != "24202849" {
		t.Fatalf("threadOf = %q, want 24202849", got)
	}
	if got := transcriptSID("/x/y/" + sid + ".jsonl"); got != sid {
		t.Fatalf("transcriptSID = %q, want %q", got, sid)
	}
	if got := threadOf("short"); got != "short" {
		t.Fatalf("threadOf(short) = %q", got)
	}
}

func TestSessionFrontmatter(t *testing.T) {
	paths := []string{"sessions/2026-06-11/x-0330-abcd1234", "sessions/2026-06-12/x-0915-abcd1234"}
	fr := dayFrag{Date: "2026-06-12", HHMM: "0915"}
	fm := sessionFrontmatter("abcd1234", fr, 1, 2, paths, "x")
	for _, want := range []string{
		"thread: abcd1234", "day: 2026-06-12", "part: 2/2", "start: 0915",
		"title: x", "prev: sessions/2026-06-11/x-0330-abcd1234",
	} {
		if !strings.Contains(fm, want) {
			t.Errorf("frontmatter missing %q:\n%s", want, fm)
		}
	}
	if strings.Contains(fm, "next:") {
		t.Errorf("last fragment should have no next: link:\n%s", fm)
	}
}
