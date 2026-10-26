package mcpserver_test

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aahoughton/smoot/internal/auth"
	"github.com/aahoughton/smoot/internal/core"
	"github.com/aahoughton/smoot/internal/mcpserver"
	"github.com/aahoughton/smoot/internal/store"
	"github.com/aahoughton/smoot/internal/wording"
)

// serveAt serves the MCP handler on addr ("" picks a free port) and returns
// the address and a stop function.
func serveAt(t *testing.T, h http.Handler, addr string) (string, func()) {
	t.Helper()
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	stop := func() { _ = srv.Close() }
	t.Cleanup(stop)
	return ln.Addr().String(), stop
}

func TestBridge(t *testing.T) {
	svc := newService(t)
	h := mcpserver.Handler(svc, auth.None{}, mcpserver.Options{})
	addr, stop := serveAt(t, h, "")

	local, peer := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		errc <- mcpserver.Bridge(ctx, "http://"+addr+"/?namespace=default", "", local, "test", slog.New(slog.DiscardHandler))
	}()

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(t.Context(), peer, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	s := &session{t: t, cs: cs}

	if cs.InitializeResult().Instructions != wording.Default().Instructions("http://"+addr, "default", false) {
		t.Fatal("instructions were not mirrored")
	}
	var n int
	for _, err := range cs.Tools(t.Context(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 12 {
		t.Fatalf("mirrored %d tools", n)
	}

	s.ok("start", map[string]any{"conversation": "c", "agent": "alice"})
	if out := s.ok("say", map[string]any{"conversation": "c", "agent": "alice", "body": "hi"}); !strings.HasPrefix(out, "posted #3") {
		t.Fatalf("say = %q", out)
	}
	out, isErr := s.call("say", map[string]any{"conversation": "c", "agent": "zed", "body": "hi"})
	if !isErr || !strings.HasPrefix(out, "not_participant") {
		t.Fatalf("remote tool error not passed through: %q", out)
	}

	// The server goes away: the call fails clearly and is not replayed.
	stop()
	out, isErr = s.call("status", map[string]any{"agent": "alice"})
	if !isErr || !strings.HasPrefix(out, "bridge_error:") {
		t.Fatalf("status with server down = %q, error %v", out, isErr)
	}

	// It comes back on the same address: the bridge reconnects.
	serveAt(t, h, addr)
	out = s.ok("status", map[string]any{"agent": "alice"})
	if !strings.HasPrefix(out, "c: open") {
		t.Fatalf("status after reconnect = %q", out)
	}

	cancel()
	select {
	case <-errc:
	case <-time.After(5 * time.Second):
		t.Fatal("bridge did not stop")
	}
}

func TestBridgeFailsFastWhenServerIsDown(t *testing.T) {
	local, _ := mcp.NewInMemoryTransports()
	err := mcpserver.Bridge(t.Context(), "http://127.0.0.1:1/", "", local, "test", slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "connect to") {
		t.Fatalf("err = %v", err)
	}
}

func TestBridgeSendsTheToken(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "smoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	svc := core.New(db, core.Options{Policy: core.RolePolicy()})
	tok, err := svc.Bootstrap(t.Context(), "root")
	if err != nil {
		t.Fatal(err)
	}
	addr, _ := serveAt(t, mcpserver.Handler(svc, auth.Bearer{Service: svc}, mcpserver.Options{}), "")
	endpoint := "http://" + addr + "/?namespace=default"

	// Without the token the bridge cannot even start.
	local, _ := mcp.NewInMemoryTransports()
	if err := mcpserver.Bridge(t.Context(), endpoint, "", local, "test", slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("bridge started without a token")
	}

	local, peer := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = mcpserver.Bridge(ctx, endpoint, tok.Secret, local, "test", slog.New(slog.DiscardHandler)) }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(t.Context(), peer, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	(&session{t: t, cs: cs}).ok("start", map[string]any{"conversation": "c", "agent": "root-agent"})
}

func TestBridgeKeepsTheTokenOnItsServer(t *testing.T) {
	leaked := make(chan string, 16)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("Authorization"); h != "" {
			leaked <- h
		}
		http.Error(w, "no", http.StatusNotFound)
	}))
	t.Cleanup(other.Close)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/mcp", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirect.Close)

	local, _ := mcp.NewInMemoryTransports()
	if err := mcpserver.Bridge(t.Context(), redirect.URL+"/mcp", "smoot_secret", local, "test", slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("bridge followed a redirect to another server")
	}
	select {
	case h := <-leaked:
		t.Fatalf("the other server received %q", h)
	default:
	}
}
