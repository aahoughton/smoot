package api_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aahoughton/smoot/internal/api"
	"github.com/aahoughton/smoot/internal/auth"
	"github.com/aahoughton/smoot/internal/core"
	"github.com/aahoughton/smoot/internal/mcpserver"
	"github.com/aahoughton/smoot/internal/store"
)

var update = flag.Bool("update", false, "rewrite testdata/contract.golden")

// TestResponsesMatchTheContract records the JSON of every endpoint in a
// scripted run (fixed clock, secrets masked) and compares it with
// testdata/contract.golden. The response shapes come from core structs, so
// a renamed or removed field changes the public API; this test makes that
// a visible diff. After an intended change, run go test ./internal/api
// -update and review the golden alongside openapi.yaml.
func TestResponsesMatchTheContract(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "smoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clock := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	svc := core.New(db, core.Options{Policy: core.RolePolicy(), Now: func() time.Time { return clock }, Feedback: true, Attachments: true})
	root, err := svc.Bootstrap(t.Context(), "root")
	if err != nil {
		t.Fatal(err)
	}
	rootP, err := svc.AuthenticateToken(t.Context(), root.Secret)
	if err != nil {
		t.Fatal(err)
	}
	now := func() time.Time { return clock }
	activity := auth.NewActivity(nil, now)
	activity.Failed(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	mux := http.NewServeMux()
	api.Register(mux, svc, auth.Bearer{Service: svc}, api.Options{
		Logger: slog.New(slog.DiscardHandler), AuthMode: "token", Version: "test", Now: now, Activity: activity,
		MCPSessions: func() []mcpserver.SessionUse {
			return []mcpserver.SessionUse{{AccountID: rootP.AccountID, Sessions: 2, LastUsed: clock}}
		},
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	token := root.Secret
	call := func(method, path string, body any) map[string]any {
		t.Helper()
		var rd io.Reader
		switch b := body.(type) {
		case nil:
		case []byte: // sent as is
			rd = bytes.NewReader(b)
		default:
			raw, err := json.Marshal(b)
			if err != nil {
				t.Fatal(err)
			}
			rd = bytes.NewReader(raw)
		}
		req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, rd)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		out.WriteString("\n## " + method + " " + path + " -> " + http.StatusText(resp.StatusCode) + "\n")
		var v map[string]any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &v); err != nil {
				t.Fatalf("%s %s: %s", method, path, raw)
			}
			pretty, _ := json.MarshalIndent(v, "", "  ")
			out.Write(mask(pretty))
			out.WriteString("\n")
		}
		return v
	}

	// download records a file response: its status, the headers that make
	// it a download, and the body.
	download := func(path string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		out.WriteString("\n## GET " + path + " -> " + http.StatusText(resp.StatusCode) + "\n")
		for _, h := range []string{"Content-Type", "Content-Disposition", "Content-Length", "X-Content-Type-Options", "Cache-Control", "Content-Security-Policy", "X-Smoot-Sha256"} {
			fmt.Fprintf(&out, "%s: %s\n", h, resp.Header.Get(h))
		}
		fmt.Fprintf(&out, "%q\n", raw)
	}

	const c = "/v1/namespaces/default/conversations/design"
	call("GET", "/v1/whoami", nil)
	call("POST", "/v1/namespaces", map[string]any{"name": "team"})
	call("GET", "/v1/namespaces", nil)
	call("POST", "/v1/accounts", map[string]any{"name": "pat"})
	call("GET", "/v1/accounts", nil)
	call("PATCH", "/v1/accounts/pat", map[string]any{"disabled": false})
	issued := call("POST", "/v1/accounts/pat/tokens", map[string]any{"label": "laptop", "ttl_seconds": 3600})
	call("GET", "/v1/accounts/pat/tokens", nil)
	call("PUT", "/v1/namespaces/default/members/pat", map[string]any{"role": "member"})
	call("GET", "/v1/namespaces/default/members", nil)
	call("POST", "/v1/namespaces/default/conversations", map[string]any{"name": "design", "topic": "pick a cache", "end": "agreement", "agent": "alice"})
	call("GET", "/v1/namespaces/default/conversations?status=all", nil)
	call("GET", c, nil)
	token = issued["secret"].(string)
	call("POST", c+"/join", map[string]any{"agent": "bob"})
	call("POST", c+"/presence", map[string]any{"agent": "bob", "presence": "working", "note": "reading"})
	token = root.Secret
	call("POST", c+"/messages", map[string]any{"agent": "alice", "body": "LRU or LFU?", "idempotency_key": "q1"})
	call("POST", c+"/observe", map[string]any{"name": "ignored", "body": "mind the budget"})
	token = issued["secret"].(string)
	call("POST", c+"/wait", map[string]any{"agent": "bob", "timeout_seconds": 1, "inline_bytes": 1000})
	call("POST", c+"/messages", map[string]any{"agent": "bob", "body": "LRU", "reply_to": 3})
	call("POST", c+"/read", map[string]any{"agent": "bob"})
	call("GET", c+"/messages?tail=2", nil)
	call("GET", c+"/follow?after=4&timeout=0", nil)
	call("GET", "/v1/namespaces/default/status?agent=bob", nil)
	call("GET", "/v1/namespaces/default/search?q=lru&order=oldest&limit=1", nil)
	call("GET", "/v1/namespaces/default/search?q=budget&author=root", nil)
	call("GET", "/v1/namespaces/default/search?q=%22%22", nil)
	attached := call("POST", c+"/attachments?agent=bob&name=bench+r%C3%A9sults.csv", []byte("policy,hits\nlru,91\n"))
	attachSeq := int(attached["message"].(map[string]any)["seq"].(float64))
	download(fmt.Sprintf("%s/attachments/%d?agent=bob", c, attachSeq))
	token = root.Secret
	download(fmt.Sprintf("%s/attachments/%d", c, attachSeq))
	call("GET", fmt.Sprintf("%s/attachments/%d?agent=alice", c, attachSeq-1), nil)
	call("GET", "/v1/attachments", nil)
	token = issued["secret"].(string)
	call("POST", c+"/conclude", map[string]any{"agent": "bob", "body": "AGREED: abc1234"})
	call("POST", c+"/leave", map[string]any{"agent": "bob"})
	token = root.Secret
	call("POST", c+"/conclude", map[string]any{"agent": "alice", "body": "AGREED: abc1234"})
	call("POST", "/v1/namespaces/default/conversations", map[string]any{"name": "led", "agent": "lead", "end": "coordinator"})
	call("POST", "/v1/namespaces/default/conversations/led/close", map[string]any{"agent": "lead", "body": "done"})
	var dump bytes.Buffer
	if err := svc.Dump(t.Context(), rootP, "default", &dump); err != nil {
		t.Fatal(err)
	}
	call("POST", "/v1/load?as=copy", dump.Bytes())
	token = issued["secret"].(string)
	call("GET", "/v1/server", nil)
	token = root.Secret
	call("GET", "/v1/server", nil)
	// Feedback is submitted over MCP; the API only reads it.
	patP, err := svc.AuthenticateToken(t.Context(), issued["secret"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitFeedback(t.Context(), patP, "default", "design", core.FeedbackParams{Agent: "bob", Body: "clear"}); err != nil {
		t.Fatal(err)
	}
	token = issued["secret"].(string)
	call("GET", "/v1/feedback", nil)
	token = root.Secret
	call("GET", "/v1/feedback?namespace=default&conversation=design&since=2030-01-01T00:00:00Z&limit=10", nil)
	call("GET", "/v1/feedback/1", nil)
	call("GET", "/v1/feedback/2", nil)
	call("POST", fmt.Sprintf("/v1/accounts/pat/tokens/%v/revoke", issued["id"]), map[string]any{})
	call("POST", "/v1/accounts/pat/tokens/revoke", map[string]any{"force": false})
	call("POST", "/v1/accounts/root/tokens/1/revoke", map[string]any{})
	call("DELETE", "/v1/namespaces/default/members/pat", nil)
	call("DELETE", "/v1/attachments/"+attached["message"].(map[string]any)["attachment"].(map[string]any)["sha256"].(string), nil)
	call("DELETE", "/v1/namespaces/copy/conversations/design/attachments", nil)
	call("GET", fmt.Sprintf("%s/attachments/%d?agent=alice", c, attachSeq), nil)
	call("POST", c+"/messages", map[string]any{"agent": "alice", "body": "late"})

	path := filepath.Join("testdata", "contract.golden")
	if *update {
		if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/api -update to create it)", err)
	}
	if got := out.String(); got != string(want) {
		gl, wl := strings.Split(got, "\n"), strings.Split(string(want), "\n")
		for i := range min(len(gl), len(wl)) {
			if gl[i] != wl[i] {
				t.Fatalf("API responses changed at line %d:\n got: %s\nwant: %s\nreview, then run go test ./internal/api -update", i+1, gl[i], wl[i])
			}
		}
		t.Fatalf("API responses changed in length (%d vs %d lines); run go test ./internal/api -update after review", len(gl), len(wl))
	}
}

var (
	secretRE = regexp.MustCompile(`"smoots?_[a-z0-9]+"`)
	timeRE   = regexp.MustCompile(`"\d{4}-\d\d-\d\dT[0-9:.]+Z"`)
)

// mask hides token secrets, which are random, and times not on the test's
// clock (the default namespace is created by the migration, at real time).
func mask(b []byte) []byte {
	b = secretRE.ReplaceAll(b, []byte(`"smoot_<secret>"`))
	return timeRE.ReplaceAllFunc(b, func(ts []byte) []byte {
		if bytes.HasPrefix(ts, []byte(`"2030-`)) {
			return ts
		}
		return []byte(`"<real time>"`)
	})
}
