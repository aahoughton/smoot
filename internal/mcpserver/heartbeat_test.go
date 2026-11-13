package mcpserver_test

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aahoughton/smoot/internal/auth"
	"github.com/aahoughton/smoot/internal/mcpserver"
)

func TestSDKClientIgnoresHeartbeats(t *testing.T) {
	srv := httptest.NewServer(mcpserver.Handler(newService(t), auth.None{}, mcpserver.Options{StreamHeartbeat: 5 * time.Millisecond}))
	t.Cleanup(srv.Close)

	// The SDK client holds the session's standalone stream open.
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(t.Context(),
		&mcp.StreamableClientTransport{Endpoint: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	s := &session{t: t, cs: cs}
	s.ok("list", map[string]any{})
	time.Sleep(50 * time.Millisecond) // several heartbeats
	s.ok("list", map[string]any{})
}

func TestHeartbeatCommentsOnARawStream(t *testing.T) {
	srv := httptest.NewServer(mcpserver.Handler(newService(t), auth.None{}, mcpserver.Options{StreamHeartbeat: 10 * time.Millisecond}))
	t.Cleanup(srv.Close)

	// post sends one message and returns the session id the server names.
	post := func(sid, body string) string {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if sid != "" {
			req.Header.Set("Mcp-Session-Id", sid)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.Header.Get("Mcp-Session-Id")
	}
	sid := post("", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"raw","version":"0"}}}`)
	if sid == "" {
		t.Fatal("no session id")
	}
	post(sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Mcp-Session-Id", sid)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status %d", resp.StatusCode)
	}
	timeout := time.AfterFunc(5*time.Second, func() { _ = resp.Body.Close() })
	defer timeout.Stop()
	beats := 0
	sc := bufio.NewScanner(resp.Body)
	for beats < 3 && sc.Scan() {
		line := sc.Text()
		switch {
		case line == ": keepalive":
			beats++
		case line != "" && !strings.HasPrefix(line, ":"):
			t.Fatalf("unexpected line on an idle stream: %q", line)
		}
	}
	if beats < 3 {
		t.Fatalf("got %d heartbeats, want 3 (scan error: %v)", beats, sc.Err())
	}
}
