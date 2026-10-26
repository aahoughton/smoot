package mcpserver_test

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aahoughton/smoot/internal/core"
	"github.com/aahoughton/smoot/internal/mcpserver"
	"github.com/aahoughton/smoot/internal/wording"
)

var update = flag.Bool("update", false, "rewrite testdata/agent-text.golden")

// TestAgentText snapshots everything an agent can see: instructions, tool
// descriptions and schemas as a client receives them, every phrase, and the
// output of a scripted session, with feedback on (and the tool list with it
// off). Wording changes show up as a diff to the
// golden file; run `go test ./internal/mcpserver -update` to accept them.
func TestAgentText(t *testing.T) {
	var b strings.Builder
	w := wording.Default()

	b.WriteString("# Instructions (url http://127.0.0.1:8787, namespace default)\n\n")
	b.WriteString(w.Instructions("http://127.0.0.1:8787", "default", false))
	b.WriteString("\n\n# Instructions with --attachments\n\n")
	b.WriteString(w.Instructions("http://127.0.0.1:8787", "default", true))
	b.WriteString("\n\n# Tools (with --feedback)\n")

	// A fixed clock keeps the times and cursors in search results stable.
	clock := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	svc := newServiceWith(t, core.Options{Feedback: true, Now: func() time.Time { return clock }})
	alice := connect(t, svc, mcpserver.Options{})
	for tool, err := range alice.cs.Tools(t.Context(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		ro := ""
		switch a := tool.Annotations; {
		case a != nil && a.ReadOnlyHint:
			ro = " (read-only)"
		case a != nil && a.DestructiveHint != nil && *a.DestructiveHint:
			ro = " (destructive)"
		}
		fmt.Fprintf(&b, "\n## %s%s\n%s\n", tool.Name, ro, tool.Description)
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Properties map[string]struct {
				Type        any    `json:"type"`
				Description string `json:"description"`
			} `json:"properties"`
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(schema.Properties))
		for n := range schema.Properties {
			names = append(names, n)
		}
		slices.Sort(names)
		for _, n := range names {
			p := schema.Properties[n]
			req := ""
			if slices.Contains(schema.Required, n) {
				req = ", required"
			}
			fmt.Fprintf(&b, "  %s (%v%s): %s\n", n, p.Type, req, p.Description)
		}
	}

	b.WriteString("\n# Tools without --feedback\n\n")
	off := connect(t, newService(t), mcpserver.Options{})
	var names []string
	for tool, err := range off.cs.Tools(t.Context(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tool.Name)
	}
	b.WriteString(strings.Join(names, ", ") + "\n")

	b.WriteString("\n# Phrases (sample fields)\n")
	sample := map[string]any{
		"Name": "pat", "LastSeq": 12, "Max": 50, "Endpoint": "http://127.0.0.1:8787/mcp", "Err": "EOF", "Tool": "say",
		"Conversation": "cache", "Status": "open", "You": "active", "Expected": true,
		"Unread": 2, "Replies": 1, "Next": "read", "Advice": "read the new messages, then reply or wait",
		"New": 2, "Authors": "bob x2", "Cursor": "1791855824890.3", "Total": 37, "Shown": 10, "Lines": "cache: open; you: active, expected; unread: 2; next: read (read the new messages, then reply or wait)",
		"Size": "1.5 KiB", "SHA": "2465bebf63ca", "Gone": "expired", "Seq": 4, "Agent": "bob", "Namespace": "default", "URL": "http://127.0.0.1:8787",
	}
	for _, k := range wording.PhraseKeys {
		fmt.Fprintf(&b, "\n## %s\n%s\n", k, w.Phrase(k, sample))
	}

	b.WriteString("\n# Sample session\n")
	bob := connect(t, svc, mcpserver.Options{})
	step := func(who string, s *session, tool string, args map[string]any) {
		out, isErr := s.call(tool, args)
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > 160 {
			raw = append(raw[:150:150], fmt.Sprintf("... (%d bytes)", len(raw))...)
		}
		mark := ""
		if isErr {
			mark = " [error]"
		}
		fmt.Fprintf(&b, "\n> %s %s %s%s\n%s\n", who, tool, raw, mark, out)
	}
	conv := map[string]any{"conversation": "cache"}
	with := func(kv ...any) map[string]any {
		m := map[string]any{}
		for k, v := range conv {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	step("alice", alice, "start", with("agent", "alice", "end", "agreement", "topic", "pick an eviction policy"))
	step("bob", bob, "join", with("agent", "bob"))
	step("bob", bob, "read", with("agent", "bob"))
	step("alice", alice, "say", with("agent", "alice", "body", "LRU or LFU?\nReasoning to follow."))
	step("bob", bob, "wait", with("agent", "bob", "timeout_seconds", 5))
	if _, err := svc.Observe(t.Context(), core.Anonymous(), "default", "cache", core.ObserveParams{Name: "pat", Body: "remember the memory budget"}); err != nil {
		t.Fatal(err)
	}
	step("bob", bob, "say", with("agent", "bob", "body", "LRU; LFU costs more memory.", "reply_to", 4))
	step("alice", alice, "wait", with("agent", "alice", "timeout_seconds", 5))
	step("alice", alice, "say", with("agent", "alice", "body", "Benchmark notes: "+strings.Repeat("row ok; ", 600)))
	if _, err := svc.Observe(t.Context(), core.Anonymous(), "default", "cache", core.ObserveParams{Name: "pat", Body: "keep the notes short"}); err != nil {
		t.Fatal(err)
	}
	step("bob", bob, "wait", with("agent", "bob", "timeout_seconds", 5))
	step("bob", bob, "read", with("agent", "bob", "limit", 1))
	step("alice", alice, "conclude", with("agent", "alice", "body", "AGREED: #6, LRU"))
	step("bob", bob, "say", with("agent", "zed", "body", "hi"))
	step("bob", bob, "conclude", with("agent", "bob", "body", "done"))
	step("bob", bob, "conclude", with("agent", "bob", "body", "AGREED: #4"))
	step("bob", bob, "conclude", with("agent", "bob", "body", "AGREED: #6"))
	step("alice", alice, "wait", with("agent", "alice"))
	step("alice", alice, "conclude", with("agent", "alice", "body", "AGREED: #6"))
	step("alice", alice, "status", map[string]any{"agent": "alice"})
	step("alice", alice, "list", map[string]any{"status": "all"})
	step("alice", alice, "search", map[string]any{"query": "LRU", "order": "oldest", "limit": 2})
	step("alice", alice, "search", map[string]any{"query": "memory", "author": "bob", "since": "2020-01-01"})
	step("alice", alice, "search", map[string]any{"query": "nothing-like-this"})
	step("bob", bob, "read", with("agent", "bob"))
	step("bob", bob, "feedback", with("agent", "bob", "body", "The standing line made it clear when to stop."))
	step("bob", bob, "feedback", with("agent", "bob", "body", "Replacing: the wait timeout surprised me once."))
	step("bob", bob, "read", with("agent", "bob"))
	step("bob", bob, "feedback", with("agent", "zed", "body", "never joined"))

	// Attachments arrive over the API; agents see their records.
	b.WriteString("\n# Sample session with --attachments\n")
	files := newServiceWith(t, core.Options{Attachments: true, Now: func() time.Time { return clock }})
	alice, bob = connect(t, files, mcpserver.Options{}), connect(t, files, mcpserver.Options{})
	conv = map[string]any{"conversation": "bench"}
	step("alice", alice, "start", with("agent", "alice", "end", "agreement"))
	step("bob", bob, "join", with("agent", "bob"))
	res, err := files.Attach(t.Context(), core.Anonymous(), "default", "bench", core.AttachParams{
		Agent: "alice", Name: "results.csv", Data: []byte(strings.Repeat("lru,91\n", 220)),
	})
	if err != nil {
		t.Fatal(err)
	}
	step("alice", alice, "say", with("agent", "alice", "body", "Results are in results.csv; LRU wins."))
	step("bob", bob, "wait", with("agent", "bob", "timeout_seconds", 5))
	if _, err := files.Fetch(t.Context(), core.Anonymous(), "default", "bench", core.FetchParams{Agent: "bob", Seq: res.Message.Seq}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := files.DeleteAttachments(t.Context(), core.Anonymous(), core.AttachmentDelete{Namespace: "default", Conversation: "bench"}); err != nil {
		t.Fatal(err)
	}
	step("alice", alice, "read", with("agent", "alice", "after", 0))

	got := b.String()
	path := filepath.Join("testdata", "agent-text.golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/mcpserver -update to create it)", err)
	}
	if got != string(want) {
		t.Fatalf("agent-facing text changed; review the diff and run go test ./internal/mcpserver -update\n%s", firstDiff(string(want), got))
	}
}

func firstDiff(want, got string) string {
	wl, gl := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < max(len(wl), len(gl)); i++ {
		var a, b string
		if i < len(wl) {
			a = wl[i]
		}
		if i < len(gl) {
			b = gl[i]
		}
		if a != b {
			return fmt.Sprintf("first difference at line %d:\n  want: %q\n  got:  %q", i+1, a, b)
		}
	}
	return ""
}
