package mcpserver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aahoughton/smoot/internal/core"
	"github.com/aahoughton/smoot/internal/store"
)

type oneAccount struct{}

func (oneAccount) Authenticate(*http.Request) (core.Principal, error) {
	return core.Principal{AccountID: 1}, nil
}

// TestRefusedStartFreesItsPlace: a request without a session id that gets
// no session (the SDK client's server/discover probe) gives back its place
// before its response reaches the client, even if the handler has not yet
// returned; the client's initialize follows at once.
func TestRefusedStartFreesItsPlace(t *testing.T) {
	release := make(chan struct{})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("probe") {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-release
			return
		}
		w.Header().Set(sessionHeader, "s1")
		w.WriteHeader(http.StatusOK)
	})
	g := &guard{authn: oneAccount{}, next: next, ttl: SessionTimeout, owners: map[string]owner{}, perAccount: 1, total: 10}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	post := func(url string) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	probe := post(srv.URL + "?probe")
	defer func() { _ = probe.Body.Close() }()
	resp := post(srv.URL)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize after a refused probe: status %d", resp.StatusCode)
	}
}

// TestNameUsesAreBoundedPerAccount: one account filling its share does not
// cost another account its warnings.
func TestNameUsesAreBoundedPerAccount(t *testing.T) {
	g := &guard{owners: map[string]owner{"s": {}}, names: map[nameUse]string{}}
	for i := range maxNameUsesPerAccount + 10 {
		g.useName("s", nameUse{ns: "n", account: 1, agent: "a", conversation: strconv.Itoa(i)})
	}
	if got := len(g.names); got != maxNameUsesPerAccount {
		t.Fatalf("account 1 holds %d entries, want %d", got, maxNameUsesPerAccount)
	}
	// An existing use can still move to the session that used it last.
	g.useName("t", nameUse{ns: "n", account: 1, agent: "a", conversation: "0"})
	if got := g.names[nameUse{ns: "n", account: 1, agent: "a", conversation: "0"}]; got != "t" {
		t.Errorf("holder at the cap = %q, want t", got)
	}
	other := nameUse{ns: "n", account: 2, agent: "a", conversation: "x"}
	g.useName("s", other)
	if _, ok := g.names[other]; !ok {
		t.Error("account 2 lost its entry to account 1's cap")
	}
}

// TestFinishedUsesAreForgotten: leave and close free a session's name uses.
func TestFinishedUsesAreForgotten(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "smoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := Handler(core.New(db, core.Options{}), oneAnonymous{}, Options{})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(t.Context(),
		&mcp.StreamableClientTransport{Endpoint: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	call := func(tool, conv string) {
		t.Helper()
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: map[string]any{"conversation": conv, "agent": "a"}})
		if err != nil || res.IsError {
			t.Fatalf("%s %s: %v %+v", tool, conv, err, res)
		}
	}
	uses := func() int {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.names)
	}
	call("start", "left")
	call("start", "closed")
	if got := uses(); got != 2 {
		t.Fatalf("recorded %d uses, want 2", got)
	}
	call("leave", "left")
	call("close", "closed")
	if got := uses(); got != 0 {
		t.Errorf("%d uses left after leave and close", got)
	}
}

type oneAnonymous struct{}

func (oneAnonymous) Authenticate(*http.Request) (core.Principal, error) {
	return core.Anonymous(), nil
}
