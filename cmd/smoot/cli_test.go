package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aahoughton/smoot/internal/auth"
	"github.com/aahoughton/smoot/internal/client"
	"github.com/aahoughton/smoot/internal/core"
	"github.com/aahoughton/smoot/internal/server"
	"github.com/aahoughton/smoot/internal/store"
)

func newHandler(t *testing.T) (http.Handler, *core.Service) {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "smoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	svc := core.New(db, core.Options{})
	return server.New(server.Deps{Service: svc, Authenticator: auth.None{}, Logger: slog.New(slog.DiscardHandler)}), svc
}

type cliResult struct {
	code           int
	stdout, stderr string
}

func runCLI(ctx context.Context, url string, args ...string) cliResult {
	var out, errOut bytes.Buffer
	env := func(k string) string {
		if k == "SMOOT_URL" {
			return url
		}
		switch k {
		case "HOME", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "PI_CODING_AGENT_DIR":
			return os.Getenv(k)
		}
		return ""
	}
	code := run(ctx, args, strings.NewReader(""), &out, &errOut, env)
	return cliResult{code, out.String(), errOut.String()}
}

func setup(t *testing.T) (string, *core.Service) {
	t.Helper()
	h, svc := newHandler(t)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	p := core.Anonymous()
	if _, err := svc.Start(t.Context(), p, "default", core.StartParams{Name: "c", Agent: "alice", Join: true, End: core.EndAgreement}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Join(t.Context(), p, "default", "c", core.JoinParams{Agent: "bob"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Read(t.Context(), p, "default", "c", core.ReadParams{Agent: "alice"}); err != nil {
		t.Fatal(err)
	}
	return srv.URL, svc
}

func TestHealth(t *testing.T) {
	url, _ := setup(t)
	if r := runCLI(t.Context(), url, "health"); r.code != exitOK || r.stdout != "ok\n" {
		t.Fatalf("health = %+v", r)
	}
	if r := runCLI(t.Context(), "http://127.0.0.1:1", "health"); r.code != exitError {
		t.Fatalf("health on a closed port = %+v", r)
	}
}

func TestCommandsWithoutANamespaceIgnoreIt(t *testing.T) {
	url, _ := setup(t)
	env := map[string]string{"SMOOT_NAMESPACE": "bad/name"}
	for _, args := range [][]string{{"health"}, {"whoami"}} {
		if r := runCLIInput(t.Context(), url, "", env, args...); r.code != exitOK {
			t.Errorf("%v with an invalid SMOOT_NAMESPACE = %+v", args, r)
		}
		r := runCLI(t.Context(), url, append(args, "--namespace", "default")...)
		if r.code != exitUsage || !strings.Contains(r.stderr, "flag provided but not defined: -namespace") {
			t.Errorf("%v --namespace = %+v", args, r)
		}
	}
	r := runCLI(t.Context(), url, "health", "--token", "t")
	if r.code != exitUsage {
		t.Errorf("health --token = %+v", r)
	}
}

func TestStatus(t *testing.T) {
	url, _ := setup(t)
	r := runCLI(t.Context(), url, "status", "--agent", "alice")
	if r.code != exitOK || !strings.HasPrefix(r.stdout, "c: open; you: active, expected") {
		t.Fatalf("status = %+v", r)
	}
	r = runCLI(t.Context(), url, "status", "--agent", "zed")
	if r.code != exitOK || r.stdout != "not expected in any open conversation\n" {
		t.Fatalf("status = %+v", r)
	}
}

func TestWaitWakesOnPost(t *testing.T) {
	url, svc := setup(t)
	done := make(chan cliResult, 1)
	go func() { done <- runCLI(context.Background(), url, "wait", "c", "--agent", "alice") }()

	time.Sleep(100 * time.Millisecond)
	if _, err := svc.Say(t.Context(), core.Anonymous(), "default", "c", core.SayParams{Agent: "bob", Body: "ping"}); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.code != exitOK || !strings.HasPrefix(r.stdout, "1 new in c (bob x1)\nc: open") || !strings.Contains(r.stdout, "next: read") {
			t.Fatalf("wait = %+v", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("wait did not return")
	}
}

func TestWaitReturnsOnConclusion(t *testing.T) {
	url, svc := setup(t)
	p := core.Anonymous()
	for _, a := range []string{"alice", "bob"} {
		if _, err := svc.Conclude(t.Context(), p, "default", "c", core.ConcludeParams{Agent: a, Body: "AGREED: abc1234"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.Read(t.Context(), p, "default", "c", core.ReadParams{Agent: "alice"}); err != nil {
		t.Fatal(err)
	}
	r := runCLI(t.Context(), url, "wait", "--agent", "alice", "c")
	if r.code != exitOK || !strings.HasPrefix(r.stdout, "c concluded\n") || !strings.Contains(r.stdout, "next: done") {
		t.Fatalf("wait = %+v", r)
	}
}

func TestWaitTimeout(t *testing.T) {
	url, _ := setup(t)
	r := runCLI(t.Context(), url, "wait", "c", "--agent", "alice", "--timeout", "1500ms")
	if r.code != exitTimedOut || !strings.HasPrefix(r.stdout, "no new messages before the timeout") {
		t.Fatalf("wait = %+v", r)
	}
}

func TestWaitErrorsAndUsage(t *testing.T) {
	url, _ := setup(t)
	tests := []struct {
		name string
		args []string
		code int
		msg  string
	}{
		{"not a participant", []string{"wait", "c", "--agent", "zed"}, exitError, "not_participant"},
		{"no conversation", []string{"wait", "--agent", "alice"}, exitUsage, "conversation name is required"},
		{"no agent", []string{"wait", "c"}, exitUsage, "agent name is required"},
		{"extra argument", []string{"wait", "c", "d", "--agent", "alice"}, exitUsage, "unexpected argument"},
		{"bad url", []string{"wait", "c", "--agent", "alice", "--url", "ftp://x"}, exitUsage, "invalid server URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := runCLI(t.Context(), url, tt.args...)
			if r.code != tt.code || !strings.Contains(r.stderr, tt.msg) {
				t.Fatalf("got %+v", r)
			}
		})
	}
}

func TestWaitRetriesWhileServerIsDown(t *testing.T) {
	h, svc := newHandler(t)
	p := core.Anonymous()
	if _, err := svc.Start(t.Context(), p, "default", core.StartParams{Name: "c", Agent: "alice", Join: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Join(t.Context(), p, "default", "c", core.JoinParams{Agent: "bob"}); err != nil {
		t.Fatal(err)
	}

	// Reserve an address, release it, and start the server there later.
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	done := make(chan cliResult, 1)
	go func() {
		done <- runCLI(context.Background(), "http://"+addr, "wait", "c", "--agent", "alice", "--timeout", "20s")
	}()
	time.Sleep(1500 * time.Millisecond)

	ln, err = lc.Listen(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	select {
	case r := <-done:
		// alice never read, so bob's join is already new to her.
		if r.code != exitOK || !strings.Contains(r.stderr, "retrying") || !strings.Contains(r.stdout, "new in c") {
			t.Fatalf("wait = %+v", r)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("wait never recovered")
	}
}

func TestWaitTimeoutBoundsAHungServer(t *testing.T) {
	// The server never answers; with a request body it does not read, it
	// also never sees the client go, so the test stops it.
	stop := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	t.Cleanup(hung.Close)
	t.Cleanup(func() { close(stop) })
	start := time.Now()
	r := runCLI(t.Context(), hung.URL, "wait", "c", "--agent", "alice", "--timeout", "2s")
	if r.code != exitTimedOut || time.Since(start) > 5*time.Second {
		t.Fatalf("wait = %+v after %s", r, time.Since(start))
	}
}

func TestWaitTimeoutBoundsBackoff(t *testing.T) {
	start := time.Now()
	r := runCLI(t.Context(), "http://127.0.0.1:1", "wait", "c", "--agent", "alice", "--timeout", "2500ms")
	if r.code != exitTimedOut || time.Since(start) > 3*time.Second {
		t.Fatalf("wait = %+v after %s", r, time.Since(start))
	}
}

func TestWaitShortTimeoutStillChecksOnce(t *testing.T) {
	url, svc := setup(t)
	if _, err := svc.Say(t.Context(), core.Anonymous(), "default", "c", core.SayParams{Agent: "bob", Body: "ping"}); err != nil {
		t.Fatal(err)
	}
	r := runCLI(t.Context(), url, "wait", "c", "--agent", "alice", "--timeout", "300ms")
	if r.code != exitOK || !strings.HasPrefix(r.stdout, "1 new in c") {
		t.Fatalf("wait = %+v", r)
	}
}

func TestTUIUsage(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"tui", "bad name"}, "invalid target"},
		{[]string{"tui", "default/bad name"}, "invalid target"},
		{[]string{"tui", "a", "b"}, `unexpected argument "b"`},
		{[]string{"tui", "--as", "bad name"}, "invalid observer name"},
		{[]string{"tui", "--theme-dir", "/does/not/exist"}, "theme dir"},
	} {
		r := runCLI(t.Context(), "http://127.0.0.1:1", tt.args...)
		if r.code == exitOK || !strings.Contains(r.stderr, tt.want) {
			t.Errorf("%v = %d %q, want an error containing %q", tt.args, r.code, r.stderr, tt.want)
		}
	}
}

func runCLIInput(ctx context.Context, url, stdin string, env map[string]string, args ...string) cliResult {
	var out, errOut bytes.Buffer
	getenv := func(k string) string {
		if k == "SMOOT_URL" {
			return url
		}
		return env[k]
	}
	code := run(ctx, args, strings.NewReader(stdin), &out, &errOut, getenv)
	return cliResult{code, out.String(), errOut.String()}
}

func TestHookStop(t *testing.T) {
	url, _ := setup(t)
	// In setup, alice has read and bob has not: bob is expected in c.
	tests := []struct {
		name, stdin string
		env         map[string]string
		block       bool
	}{
		{"expected", `{"stop_hook_active":false}`, map[string]string{"SMOOT_AGENT": "bob"}, true},
		{"not expected", `{}`, map[string]string{"SMOOT_AGENT": "carol"}, false},
		{"no agent", `{}`, nil, false},
		{"already continuing", `{"stop_hook_active":true}`, map[string]string{"SMOOT_AGENT": "bob"}, false},
		{"bad input", `not json`, map[string]string{"SMOOT_AGENT": "bob"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := runCLIInput(t.Context(), url, tt.stdin, tt.env, "hook", "stop")
			if r.code != exitOK {
				t.Fatalf("exit %d: %s", r.code, r.stderr)
			}
			if !tt.block {
				if r.stdout != "" {
					t.Fatalf("blocked: %s", r.stdout)
				}
				return
			}
			var out struct{ Decision, Reason string }
			if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
				t.Fatalf("stdout %q: %v", r.stdout, err)
			}
			if out.Decision != "block" || !strings.Contains(out.Reason, "c: open; you: active, expected") {
				t.Fatalf("hook output = %+v", out)
			}
		})
	}
	t.Run("server down", func(t *testing.T) {
		r := runCLIInput(t.Context(), "http://127.0.0.1:1", `{}`, map[string]string{"SMOOT_AGENT": "bob"}, "hook", "stop")
		if r.code != exitOK || r.stdout != "" || !strings.Contains(r.stderr, "smoot hook stop:") {
			t.Fatalf("server down = %+v", r)
		}
	})
}

func TestHookStopPatience(t *testing.T) {
	url, svc := setup(t)
	p := core.Anonymous()
	cache := t.TempDir()
	env := map[string]string{"SMOOT_AGENT": "bob", "XDG_CACHE_HOME": cache}
	stop := func(stdin string, args ...string) bool {
		t.Helper()
		r := runCLIInput(t.Context(), url, stdin, env, append([]string{"hook", "stop"}, args...)...)
		if r.code != exitOK {
			t.Fatalf("exit %d: %s", r.code, r.stderr)
		}
		return strings.Contains(r.stdout, `"decision":"block"`)
	}
	const first = `{"session_id":"s1","stop_hook_active":false}`
	const again = `{"session_id":"s1","stop_hook_active":true}`

	// A session that keeps stopping without calling in is held three times,
	// then let go, and stays let go until it makes progress.
	for i, want := range []bool{true, true, true, false, false} {
		in := again
		if i == 0 {
			in = first
		}
		if got := stop(in); got != want {
			t.Fatalf("stop %d: blocked = %v, want %v", i+1, got, want)
		}
	}
	// A new message is progress: the hook holds the session again.
	if _, err := svc.Say(t.Context(), p, "default", "c", core.SayParams{Agent: "alice", Body: "still there?"}); err != nil {
		t.Fatal(err)
	}
	if !stop(again) {
		t.Fatal("not blocked after a new message")
	}
	// So is calling in, such as a wait that finds nothing new.
	for range 3 {
		if _, err := svc.Wait(t.Context(), p, "default", "c", core.WaitParams{Agent: "bob", Timeout: time.Millisecond}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
		if !stop(again) {
			t.Fatal("not blocked after calling wait")
		}
	}
	// Another session is counted separately.
	if !stop(`{"session_id":"s2","stop_hook_active":true}`) {
		t.Fatal("other session not blocked")
	}
	// Once bob concludes he is not expected: the stop is allowed and the
	// session's state is gone.
	if _, err := svc.Conclude(t.Context(), p, "default", "c", core.ConcludeParams{Agent: "bob", Body: "AGREED: abc1234"}); err != nil {
		t.Fatal(err)
	}
	if stop(again) {
		t.Fatal("blocked when not expected")
	}
	files, err := os.ReadDir(filepath.Join(cache, "smoot", "stop-hook"))
	if err != nil || len(files) != 1 {
		t.Fatalf("state files = %v, %v; want only s2's", files, err)
	}

	t.Run("patience flag", func(t *testing.T) {
		env["SMOOT_AGENT"] = "alice"
		if !stop(`{"session_id":"s3"}`, "--patience", "1") || stop(`{"session_id":"s3"}`, "--patience", "1") {
			t.Fatal("--patience 1 did not block exactly once")
		}
	})
	// Exit 2 from a Stop hook keeps the session going, so a mistake in the
	// hook's command line must not exit 2.
	t.Run("usage errors exit 1", func(t *testing.T) {
		env["SMOOT_AGENT"] = "alice"
		for _, args := range [][]string{{"hook"}, {"hook", "start"}, {"hook", "stop", "--patience", "0"}, {"hook", "stop", "--bogus"}, {"hook", "stop", "extra"}, {"hook", "stop", "--namespace", "../x"}, {"hook", "stop", "--url", "ftp://x"}} {
			if r := runCLIInput(t.Context(), url, `{"session_id":"s4"}`, env, args...); r.code != exitError || r.stderr == "" {
				t.Errorf("%v = %+v, want exit 1 with the error", args, r)
			}
		}
	})
}

func TestClientConfig(t *testing.T) {
	r := runCLI(t.Context(), "http://127.0.0.1:9000/", "client", "config", "codex", "--namespace", "team-a", "--stop-hook")
	if r.code != exitOK {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	for _, want := range []string{
		`url = "http://127.0.0.1:9000/mcp?namespace=team-a"`,
		`default_tools_approval_mode = "approve"`,
		`"command": "smoot hook stop --url http://127.0.0.1:9000 --namespace team-a"`,
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("missing %q in:\n%s", want, r.stdout)
		}
	}
	// The client name may also sit between flags.
	if r := runCLI(t.Context(), "http://127.0.0.1:9000", "client", "config", "--namespace", "x", "claude", "--stop-hook"); r.code != exitOK || !strings.Contains(r.stdout, "--namespace x") {
		t.Errorf("name between flags = %+v", r)
	}
	for _, args := range [][]string{{"client", "config"}, {"client", "config", "gemini"}, {"client", "config", "claude", "codex"}, {"client", "config", "--profile", "claude"}} {
		if r := runCLI(t.Context(), "http://127.0.0.1:9000", args...); r.code != exitUsage {
			t.Errorf("%v = %d, want usage error", args, r.code)
		}
	}
}

func TestShellWord(t *testing.T) {
	for in, want := range map[string]string{
		"http://h:8790":     "http://h:8790",
		"http://h/a b":      `'http://h/a b'`,
		"http://h/?a=1&b=2": `'http://h/?a=1&b=2'`,
		"http://h/it's":     `'http://h/it'\''s'`,
		"":                  "''",
	} {
		if got := shellWord(in); got != want {
			t.Errorf("shellWord(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestClientSkills(t *testing.T) {
	dir := t.TempDir()
	install := func(args ...string) cliResult {
		return runCLI(t.Context(), "http://127.0.0.1:1", append([]string{"client", "skills", "--dir", dir}, args...)...)
	}
	r := install()
	if r.code != exitOK || !strings.Contains(r.stdout, filepath.Join(dir, "smoot", "SKILL.md")) || !strings.Contains(r.stdout, filepath.Join(dir, "convene", "SKILL.md")) {
		t.Fatalf("first install = %+v", r)
	}
	if r := install(); r.code != exitOK || !strings.Contains(r.stdout, "unchanged") {
		t.Fatalf("second install = %+v", r)
	}

	// A changed file blocks the install, and nothing else is written.
	edited := filepath.Join(dir, "smoot", "SKILL.md")
	if err := os.WriteFile(edited, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "convene", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if r := install(); r.code != exitError || !strings.Contains(r.stderr, edited) {
		t.Fatalf("conflicting install = %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "convene", "SKILL.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused install wrote files")
	}
	if r := install("--force"); r.code != exitOK {
		t.Fatalf("forced install = %+v", r)
	}
	if got, _ := os.ReadFile(edited); string(got) == "mine" {
		t.Fatal("--force did not replace the file")
	}
	if got, err := os.ReadFile(edited + ".smoot-bak"); err != nil || string(got) != "mine" {
		t.Fatalf("skills backup: %s %v", got, err)
	}

	for _, args := range [][]string{{"client", "skills"}, {"client", "skills", "claude", "--dir", dir}, {"client", "skills", "gemini"}} {
		if r := runCLI(t.Context(), "", args...); r.code != exitUsage {
			t.Fatalf("%v = %+v", args, r)
		}
	}

	// A client name installs into that client's skills directory, with
	// flags before or after the name.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	for _, args := range [][]string{{"client", "skills", "claude"}, {"client", "skills", "--force", "codex"}} {
		if r := runCLI(t.Context(), "", args...); r.code != exitOK {
			t.Fatalf("%v = %+v", args, r)
		}
	}
	for _, f := range []string{".claude/skills/smoot/SKILL.md", ".agents/skills/smoot/SKILL.md"} {
		if _, err := os.Stat(filepath.Join(home, f)); err != nil {
			t.Errorf("client skills did not write %s: %v", f, err)
		}
	}
}

// tokenServer serves smoot in auth mode token and returns its URL and the
// bootstrapped server admin's token.
func tokenServer(t *testing.T) (string, string, *core.Service) {
	t.Helper()
	return tokenServerWith(t, core.Options{})
}

func tokenServerWith(t *testing.T, opts core.Options) (string, string, *core.Service) {
	t.Helper()
	opts.Policy = core.RolePolicy()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "smoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	svc := core.New(db, opts)
	tok, err := svc.Bootstrap(t.Context(), "root")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(server.Deps{
		Service: svc, Authenticator: auth.Bearer{Service: svc}, Logger: slog.New(slog.DiscardHandler),
		AuthMode: "token", Version: "test", Activity: auth.NewActivity(nil, nil),
	}))
	t.Cleanup(srv.Close)
	return srv.URL, tok.Secret, svc
}

func TestAdminFeedback(t *testing.T) {
	url, token, svc := tokenServerWith(t, core.Options{Feedback: true})
	ctx := t.Context()
	admin := func(args ...string) cliResult {
		return runCLIInput(ctx, url, "", map[string]string{"SMOOT_TOKEN": token}, append([]string{"admin", "feedback"}, args...)...)
	}
	if r := admin("list"); r.code != exitOK || r.stdout != "No feedback.\n" {
		t.Fatalf("empty list = %+v", r)
	}
	root, err := svc.AuthenticateToken(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"c1", "c2"} {
		if _, err := svc.Start(ctx, root, "default", core.StartParams{Name: c, Agent: "a", Join: true}); err != nil {
			t.Fatal(err)
		}
	}
	long := "line one\n" + strings.Repeat("word ", 30) + "\x1b[31m"
	for _, c := range []string{"c1", "c2"} {
		if _, err := svc.SubmitFeedback(ctx, root, "default", c, core.FeedbackParams{Agent: "a", Body: c + ": " + long}); err != nil {
			t.Fatal(err)
		}
	}

	r := admin("list", "--in", "default/c1")
	if r.code != exitOK || !strings.Contains(r.stdout, "default/c1") || strings.Contains(r.stdout, "default/c2") ||
		!strings.Contains(r.stdout, "c1: line one word") || !strings.Contains(r.stdout, "...") || strings.Contains(r.stdout, "\x1b") {
		t.Fatalf("list --in = %+v", r)
	}
	if r := admin("list", "--limit", "1"); r.code != exitOK || !strings.Contains(r.stdout, "More notes are older") {
		t.Fatalf("list --limit = %+v", r)
	}
	if r := admin("list", "--since", "1h"); r.code != exitOK || !strings.Contains(r.stdout, "default/c2") {
		t.Fatalf("list --since = %+v", r)
	}
	if r := admin("list", "--in", "default/c1/x"); r.code != exitError || !strings.Contains(r.stderr, "--in") {
		t.Fatalf("bad --in = %+v", r)
	}
	if r := admin("list", "--since", "soon"); r.code != exitError || !strings.Contains(r.stderr, "--since") {
		t.Fatalf("bad --since = %+v", r)
	}
	r = admin("show", "1")
	if r.code != exitOK || !strings.Contains(r.stdout, "feedback 1 from a (account root)") ||
		!strings.Contains(r.stdout, "conversation: default/c1, read to #") ||
		!strings.Contains(r.stdout, "viewer: "+url+"/n/default/c/c1") ||
		!strings.Contains(r.stdout, "\n\nc1: line one\nword ") || strings.Contains(r.stdout, "\x1b") {
		t.Fatalf("show = %+v", r)
	}
	if r := admin("show", "9"); r.code != exitError || !strings.Contains(r.stderr, "feedback_not_found") {
		t.Fatalf("show missing = %+v", r)
	}
}

func TestClientsSendTheToken(t *testing.T) {
	url, token, _ := tokenServer(t)
	with := map[string]string{"SMOOT_TOKEN": token}

	r := runCLIInput(t.Context(), url, "", with, "whoami")
	if r.code != exitOK || !strings.Contains(r.stdout, "auth token") || !strings.Contains(r.stdout, "token:   from SMOOT_TOKEN\n") || !strings.Contains(r.stdout, "account: root (server admin)") {
		t.Fatalf("whoami = %+v", r)
	}
	if r := runCLIInput(t.Context(), url, "", nil, "whoami"); r.code != exitError || !strings.Contains(r.stderr, "unauthorized") || !strings.Contains(r.stderr, "token:   none\n") {
		t.Fatalf("whoami without a token = %+v", r)
	}
	if r := runCLIInput(t.Context(), url, "", with, "status", "--agent", "a"); r.code != exitOK {
		t.Fatalf("status = %+v", r)
	}
	// --token beats the environment.
	if r := runCLIInput(t.Context(), url, "", with, "whoami", "--token", "smoot_wrong"); r.code != exitError {
		t.Fatalf("whoami with a bad --token = %+v", r)
	}
}

func TestWhoamiWithAuthOff(t *testing.T) {
	url, _ := setup(t)
	r := runCLI(t.Context(), url, "whoami")
	if r.code != exitOK || !strings.Contains(r.stdout, "token:   none\n") || !strings.Contains(r.stdout, "account: none (auth is off") {
		t.Fatalf("whoami = %+v", r)
	}
}

func TestAdminBootstrapAndRecover(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.db")
	if r := runCLI(t.Context(), "", "db", "bootstrap", "--db", missing, "--name", "root"); r.code != exitError {
		t.Fatalf("bootstrap on a missing file = %+v", r)
	}
	path := filepath.Join(dir, "smoot.db")
	db, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	r := runCLI(t.Context(), "", "db", "bootstrap", "--db", path, "--name", "root")
	if r.code != exitOK || !strings.Contains(r.stdout, "root is a server admin") || !strings.Contains(r.stdout, core.TokenPrefix) {
		t.Fatalf("bootstrap = %+v", r)
	}
	if r := runCLI(t.Context(), "", "db", "bootstrap", "--db", path, "--name", "again"); r.code != exitError || !strings.Contains(r.stderr, "accounts already exist") {
		t.Fatalf("second bootstrap = %+v", r)
	}
	if r := runCLI(t.Context(), "", "db", "recover", "--db", path, "--name", "root"); r.code != exitOK || !strings.Contains(r.stdout, core.TokenPrefix) {
		t.Fatalf("recover = %+v", r)
	}
	if r := runCLI(t.Context(), "", "db", "recover", "--db", path); r.code != exitUsage {
		t.Fatalf("recover without --name = %+v", r)
	}
}

func TestAdminThroughTheServer(t *testing.T) {
	url, token, _ := tokenServer(t)
	admin := func(args ...string) cliResult {
		return runCLIInput(t.Context(), url, "", map[string]string{"SMOOT_TOKEN": token}, append([]string{"admin"}, args...)...)
	}
	steps := []struct {
		args []string
		want string
	}{
		{[]string{"accounts", "add", "pat"}, "pat "},
		{[]string{"accounts", "add", "boss", "--server-admin"}, "boss  yes"},
		{[]string{"accounts", "disable", "pat"}, "pat   no            yes"},
		{[]string{"accounts", "enable", "pat"}, "pat   no            no"},
		{[]string{"accounts", "list"}, "root"},
		{[]string{"namespaces", "create", "team"}, "created namespace team"},
		{[]string{"members", "set", "team", "pat", "member"}, "pat is member in team"},
		{[]string{"members", "list", "team"}, "pat      member"},
		{[]string{"tokens", "issue", "pat", "--label", "laptop", "--ttl", "720h"}, "for pat (shown once):\n" + core.TokenPrefix},
		{[]string{"tokens", "list", "pat"}, "laptop"},
		{[]string{"tokens", "revoke-all", "pat", "--dry-run"}, "would revoke token 2 of pat (laptop, never used)\n"},
		{[]string{"tokens", "revoke", "pat", "2", "--dry-run"}, "would revoke token 2 of pat (laptop, never used)\n"},
		{[]string{"tokens", "revoke", "pat", "2"}, "revoked token 2 of pat (laptop, never used)\n"},
		{[]string{"tokens", "revoke", "pat", "2"}, "token 2 of pat is already revoked\n"},
		{[]string{"tokens", "revoke", "pat", "2", "--dry-run"}, "token 2 of pat is already revoked\n"},
		{[]string{"tokens", "revoke-all", "pat"}, "pat has no tokens to revoke\n"},
		{[]string{"members", "remove", "team", "pat"}, "pat has no role in team"},
	}
	for _, s := range steps {
		r := admin(s.args...)
		if r.code != exitOK || !strings.Contains(r.stdout, s.want) {
			t.Fatalf("admin %v = %+v, want %q", s.args, r, s.want)
		}
	}
	// Revoke takes the token's account, and refuses the caller's own token
	// unless forced.
	for _, args := range [][]string{{"tokens", "revoke", "root", "2"}, {"tokens", "revoke", "pat", "1", "--dry-run"}} {
		if r := admin(args...); r.code != exitError || !strings.Contains(r.stderr, "token_not_found: token") {
			t.Fatalf("admin %v = %+v, want not found", args, r)
		}
	}
	for _, args := range [][]string{{"tokens", "revoke", "root", "1"}, {"tokens", "revoke-all", "root"}} {
		if r := admin(args...); r.code != exitError || !strings.Contains(r.stderr, "own_credential: token 1 is the one this request uses") {
			t.Fatalf("admin %v = %+v, want a refusal", args, r)
		}
	}
	if r := admin("tokens", "revoke", "1"); r.code != exitUsage || !strings.Contains(r.stderr, "takes 2 arguments") {
		t.Fatalf("revoke without account = %+v", r)
	}
	if r := admin("accounts", "demote", "root"); r.code != exitOK {
		t.Fatalf("demote with another admin left = %+v", r)
	}
	if r := admin("members", "set", "team"); r.code != exitUsage || !strings.Contains(r.stderr, "takes 3 arguments") {
		t.Fatalf("missing arguments = %+v", r)
	}
	if r := admin("accounts", "rename", "x"); r.code != exitUsage {
		t.Fatalf("unknown command = %+v", r)
	}
	// Each command takes only its own flags.
	for _, args := range [][]string{
		{"accounts", "list", "--ttl", "5h"},
		{"accounts", "add", "x", "--label", "l"},
		{"tokens", "issue", "pat", "--server-admin"},
		{"feedback", "show", "1", "--limit", "5"},
	} {
		if r := admin(args...); r.code != exitUsage || !strings.Contains(r.stderr, "flag provided but not defined") {
			t.Errorf("admin %v = %+v, want a usage error", args, r)
		}
	}
	if r := runCLIInput(t.Context(), url, "", nil, "admin", "accounts", "list"); r.code != exitError || !strings.Contains(r.stderr, "unauthorized") {
		t.Fatalf("no token = %+v", r)
	}
	// Last: with --force, root revokes the token it is using.
	r := admin("tokens", "revoke", "root", "1", "--force")
	if want := "revoked token 1 of root (bootstrap, last used "; r.code != exitOK || !strings.HasPrefix(r.stdout, want) {
		t.Fatalf("forced revoke = %+v, want %q", r, want)
	}
	if r := admin("accounts", "list"); r.code != exitError || !strings.Contains(r.stderr, "unauthorized") {
		t.Fatalf("after revoking its own token = %+v", r)
	}
}

func TestClientConfigNeverPrintsTheToken(t *testing.T) {
	env := map[string]string{"SMOOT_TOKEN": "smoot_supersecret", "HOME": t.TempDir()}
	for _, profile := range []string{"claude", "codex"} {
		r := runCLIInput(t.Context(), "http://127.0.0.1:9000", "", env, "client", "config", profile, "--with-token", "--stop-hook")
		if r.code != exitOK || strings.Contains(r.stdout, "supersecret") || !strings.Contains(r.stdout, "SMOOT_TOKEN") {
			t.Fatalf("%s: %+v", profile, r)
		}
	}
}

func TestTUIChecksItsToken(t *testing.T) {
	url, _, _ := tokenServer(t)
	r := runCLIInput(t.Context(), url, "", map[string]string{"SMOOT_TOKEN": "smoot_wrong"}, "tui")
	if r.code != exitError || !strings.Contains(r.stderr, "unauthorized") {
		t.Fatalf("tui with a bad token = %+v", r)
	}
}

func TestBackupDumpAndLoad(t *testing.T) {
	url, token, svc := tokenServer(t)
	admin, err := svc.AuthenticateToken(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Start(t.Context(), admin, "default", core.StartParams{Name: "c", Agent: "a", Join: true}); err != nil {
		t.Fatal(err)
	}
	with := map[string]string{"SMOOT_TOKEN": token}
	dir := t.TempDir()
	cli := func(env map[string]string, args ...string) cliResult {
		return runCLIInput(t.Context(), url, "", env, args...)
	}

	backup := filepath.Join(dir, "backup.db")
	if r := cli(nil, "backup", "-o", backup); r.code != exitError {
		t.Fatalf("backup without a token = %+v", r)
	}
	if r := cli(with, "backup", "-o", backup); r.code != exitOK {
		t.Fatalf("backup = %+v", r)
	}
	if info, err := os.Stat(backup); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup file: %v %v", info, err)
	}
	if r := cli(with, "backup", "-o", backup); r.code != exitError || !strings.Contains(r.stderr, "exists") {
		t.Fatalf("backup over an existing file = %+v", r)
	}
	db, err := store.Open(t.Context(), backup)
	if err != nil {
		t.Fatal(err)
	}
	var convs int
	if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM conversations").Scan(&convs); err != nil || convs != 1 {
		t.Fatalf("backup holds %d conversations, %v", convs, err)
	}
	_ = db.Close()

	for _, bad := range [][]string{{"dump", "--all", "--namespace", "default", "-o", "x"}, {"dump", "--all"}, {"dump"}} {
		if r := cli(with, bad...); r.code != exitUsage {
			t.Fatalf("%v = %+v", bad, r)
		}
	}
	full, part := filepath.Join(dir, "full.jsonl"), filepath.Join(dir, "default.jsonl")
	// --all wins over a namespace from the environment.
	withNS := map[string]string{"SMOOT_TOKEN": token, "SMOOT_NAMESPACE": "elsewhere"}
	if r := cli(withNS, "dump", "--all", "-o", full); r.code != exitOK {
		t.Fatalf("dump --all = %+v", r)
	}
	badNS := map[string]string{"SMOOT_TOKEN": token, "SMOOT_NAMESPACE": "bad/name"}
	if r := cli(badNS, "dump", "--all", "-o", filepath.Join(dir, "all2.jsonl")); r.code != exitOK {
		t.Fatalf("dump --all with an invalid SMOOT_NAMESPACE = %+v", r)
	}
	if r := cli(with, "dump", "--namespace", "default", "-o", part); r.code != exitOK {
		t.Fatalf("dump --namespace = %+v", r)
	}
	// Without --namespace, the namespace comes from the environment, then
	// the default.
	fromEnv := filepath.Join(dir, "env.jsonl")
	envDefault := map[string]string{"SMOOT_TOKEN": token, "SMOOT_NAMESPACE": "default"}
	if r := cli(envDefault, "dump", "-o", fromEnv); r.code != exitOK {
		t.Fatalf("dump with SMOOT_NAMESPACE = %+v", r)
	}
	if r := cli(withNS, "dump", "-o", filepath.Join(dir, "missing.jsonl")); r.code != exitError {
		t.Fatalf("dump of a missing namespace from the environment = %+v", r)
	}
	plain := filepath.Join(dir, "plain.jsonl")
	if r := cli(with, "dump", "-o", plain); r.code != exitOK {
		t.Fatalf("dump of the default namespace = %+v", r)
	}
	for _, f := range []string{fromEnv, plain} {
		if got, want := dumpBody(t, f), dumpBody(t, part); got != want {
			t.Fatalf("%s differs from the --namespace default dump:\n%s\nwant\n%s", f, got, want)
		}
	}
	// -o - writes the dump to stdout, which load reads from stdin.
	r := cli(with, "dump", "--namespace", "default", "-o", "-")
	if r.code != exitOK || !strings.HasPrefix(r.stdout, "{") {
		t.Fatalf("dump -o - = %+v", r)
	}
	piped := filepath.Join(dir, "piped.db")
	if r := runCLIInput(t.Context(), url, r.stdout, nil, "db", "load", "--db", piped); r.code != exitOK || !strings.Contains(r.stdout, "namespace default") {
		t.Fatalf("load of a piped dump = %+v", r)
	}

	loaded := filepath.Join(dir, "loaded.db")
	if r := cli(nil, "db", "load", "--db", loaded, "-i", full); r.code != exitOK || !strings.Contains(r.stdout, "loaded") {
		t.Fatalf("load = %+v", r)
	}
	if r := cli(nil, "db", "load", "--db", loaded, "-i", part); r.code != exitError || !strings.Contains(r.stderr, "exists") {
		t.Fatalf("load over an existing database = %+v", r)
	}
	if r := cli(nil, "db", "load", "--db", filepath.Join(dir, "ns.db"), "-i", part); r.code != exitOK || !strings.Contains(r.stdout, "namespace default") {
		t.Fatalf("load namespace dump = %+v", r)
	}

	// Without --db, a namespace dump goes into the running server.
	if r := cli(with, "load", "-i", part, "--name", "copy"); r.code != exitOK || !strings.Contains(r.stdout, "loaded namespace copy into") {
		t.Fatalf("load into the server = %+v", r)
	}
	if r := cli(with, "load", "-i", part, "--name", "copy"); r.code != exitError || !strings.Contains(r.stderr, "already exists") {
		t.Fatalf("load into an existing namespace = %+v", r)
	}
	if r := cli(with, "load", "-i", full); r.code != exitError || !strings.Contains(r.stderr, "full dump") {
		t.Fatalf("load a full dump into the server = %+v", r)
	}
	// db load takes no server flags, and load no database file.
	for _, args := range [][]string{
		{"db", "load", "--db", filepath.Join(dir, "x.db"), "--name", "x", "-i", part},
		{"db", "load", "--db", filepath.Join(dir, "x.db"), "--url", url, "-i", part},
		{"load", "--db", filepath.Join(dir, "x.db"), "-i", part},
		{"load", "--as", "x", "-i", part},
	} {
		if r := cli(with, args...); r.code != exitUsage {
			t.Fatalf("%v = %+v", args, r)
		}
	}

	// A cut-off dump is refused, and no database is left behind.
	raw, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(strings.TrimRight(string(raw), "\n"), "\n")
	cut := filepath.Join(dir, "cut.jsonl")
	if err := os.WriteFile(cut, []byte(strings.Join(lines[:len(lines)-1], "")), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "cut.db")
	if r := cli(nil, "db", "load", "--db", target, "-i", cut); r.code != exitError || !strings.Contains(r.stderr, "cut off") {
		t.Fatalf("load of a cut-off dump = %+v", r)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("a failed load left %s: %v", target, err)
	}
}

// dumpBody is a dump file without its header line, which records when it
// was made.
func dumpBody(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, body, _ := strings.Cut(string(raw), "\n")
	return body
}

func TestMigrate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "smoot.db")
	if r := runCLI(t.Context(), "", "db", "migrate", "--db", path, "--check"); r.code != exitError {
		t.Fatalf("migrate on a missing file = %+v", r)
	}
	// An old database: schema 1.
	db, err := store.OpenAt(t.Context(), path, 1)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if r := runCLI(t.Context(), "", "db", "migrate", "--db", path, "--check"); r.code != exitPending || !strings.Contains(r.stdout, "pending") {
		t.Fatalf("check = %+v", r)
	}
	if r := runCLI(t.Context(), "", "db", "migrate", "--db", path); r.code != exitOK || !strings.Contains(r.stdout, "migrated from schema 1") {
		t.Fatalf("migrate = %+v", r)
	}
	if _, err := os.Stat(path + ".pre-v1"); err != nil {
		t.Fatalf("no snapshot: %v", err)
	}
	if r := runCLI(t.Context(), "", "db", "migrate", "--db", path, "--check"); r.code != exitOK || !strings.Contains(r.stdout, "current") {
		t.Fatalf("check after = %+v", r)
	}
}

// Only serve and migrate change a database's schema; admin commands refuse
// an out-of-date database instead of migrating it unseen.
func TestAdminStatus(t *testing.T) {
	url, token, svc := tokenServer(t)
	env := map[string]string{"SMOOT_TOKEN": token}
	root, err := svc.AuthenticateToken(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Start(t.Context(), root, "default", core.StartParams{Name: "c", Agent: "alice", Join: true}); err != nil {
		t.Fatal(err)
	}
	r := runCLIInput(t.Context(), url, "", env, "admin", "status")
	if r.code != exitOK {
		t.Fatalf("status = %+v", r)
	}
	for _, want := range []string{
		"(version test, auth token)",
		"last message now: default/c #",
		"default/c     open    now   1       0        0",
		"No open MCP sessions.",
		"root     api      127.0.0.1  smoot-cli",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("status lacks %q:\n%s", want, r.stdout)
		}
	}

	r = runCLIInput(t.Context(), url, "", env, "admin", "status", "--json")
	var st client.ServerStatus
	if err := json.Unmarshal([]byte(r.stdout), &st); err != nil || st.AuthMode != "token" || len(st.Callers) != 1 {
		t.Fatalf("status --json = %+v (%v)", r, err)
	}
}

func TestAge(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                            "now",
		42 * time.Second:             "42s ago",
		5*time.Minute + time.Second:  "5m ago",
		2*time.Hour + 13*time.Minute: "2h13m ago",
		50 * time.Hour:               "2d2h ago",
	} {
		if got := age(d); got != want {
			t.Errorf("age(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestBootstrapRefusesAnOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "smoot.db")
	db, err := store.OpenAt(t.Context(), path, 1)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	r := runCLI(t.Context(), "", "db", "bootstrap", "--db", path, "--name", "root")
	if r.code != exitError || !strings.Contains(r.stderr, "smoot db migrate") {
		t.Fatalf("bootstrap on an old database = %+v", r)
	}
	if v, err := store.Inspect(t.Context(), path); err != nil || v != 1 {
		t.Fatalf("schema now %d, %v", v, err)
	}
}

func TestClientConfigUsesInjectedHome(t *testing.T) {
	home, override := t.TempDir(), t.TempDir()
	env := map[string]string{"HOME": home, "CODEX_HOME": override}
	r := runCLIInput(t.Context(), "http://127.0.0.1:9000", "", env, "client", "config", "codex", "--stop-hook")
	if r.code != exitOK || !strings.Contains(r.stdout, filepath.Join(override, "config.toml")) || !strings.Contains(r.stdout, filepath.Join(override, "hooks.json")) {
		t.Fatalf("config override: %+v", r)
	}
}
