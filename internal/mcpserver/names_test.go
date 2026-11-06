package mcpserver_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aahoughton/smoot/internal/core"
	"github.com/aahoughton/smoot/internal/mcpserver"
)

const nameWarning = "is also live in"

// testAccounts authenticates X-Test-User as one of its real accounts.
type testAccounts map[string]core.Principal

func (a testAccounts) Authenticate(r *http.Request) (core.Principal, error) {
	p, ok := a[r.Header.Get("X-Test-User")]
	if !ok {
		return core.Principal{}, errors.New("no credentials")
	}
	return p, nil
}

// accountsAuth creates the accounts, each a member of the default namespace.
func accountsAuth(t *testing.T, svc *core.Service, users ...string) testAccounts {
	t.Helper()
	ctx := t.Context()
	root, err := svc.Bootstrap(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := svc.AuthenticateToken(ctx, root.Secret)
	if err != nil {
		t.Fatal(err)
	}
	out := testAccounts{}
	for _, u := range users {
		name := "user" + u
		if _, err := svc.CreateAccount(ctx, admin, name, false); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.SetMembership(ctx, admin, "default", name, core.RoleMember); err != nil {
			t.Fatal(err)
		}
		tok, err := svc.IssueToken(ctx, admin, name, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		if out[u], err = svc.AuthenticateToken(ctx, tok.Secret); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// TestSharedNameWarning: a session taking a name that another open session
// is live under in another conversation hears about it, since the two would
// share a read position and the Stop hook's attention.
func TestSharedNameWarning(t *testing.T) {
	type env struct {
		a, b  *session
		other *session // another account
		close func()   // ends session a
		tick  func(time.Duration)
	}
	args := func(conv string) map[string]any {
		return map[string]any{"conversation": conv, "agent": "reviewer"}
	}
	tests := []struct {
		name string
		// setup runs with session a live as reviewer in "first".
		setup func(e env)
		// act makes the call under test.
		act  func(e env) string
		want bool
	}{
		{"join from another session", nil, func(e env) string {
			e.other.ok("start", map[string]any{"conversation": "second", "agent": "host"})
			return e.b.ok("join", args("second"))
		}, true},
		{"start from another session", nil, func(e env) string {
			return e.b.ok("start", args("second"))
		}, true},
		{"holder concluded in an agreement", func(e env) {
			e.other.ok("join", map[string]any{"conversation": "first", "agent": "host"})
			e.a.ok("conclude", map[string]any{"conversation": "first", "agent": "reviewer", "body": "AGREED: #1"})
		}, func(e env) string {
			return e.b.ok("start", args("second"))
		}, true},
		{"the same session in a second conversation", nil, func(e env) string {
			return e.a.ok("start", args("second"))
		}, false},
		{"another account under the same name", nil, func(e env) string {
			return e.other.ok("start", args("second"))
		}, false},
		{"a different name", nil, func(e env) string {
			return e.b.ok("start", map[string]any{"conversation": "second", "agent": "reviewer-2"})
		}, false},
		{"holder went stale", func(e env) {
			e.tick(core.DefaultStaleAfter + time.Minute)
		}, func(e env) string {
			return e.b.ok("start", args("second"))
		}, false},
		{"holder said when it would be back", func(e env) {
			e.a.ok("presence", map[string]any{"conversation": "first", "agent": "reviewer", "presence": "away", "check_back_in_minutes": 60})
			e.tick(core.DefaultStaleAfter + time.Minute)
		}, func(e env) string {
			return e.b.ok("start", args("second"))
		}, true},
		{"holder's check-back time passed", func(e env) {
			e.a.ok("presence", map[string]any{"conversation": "first", "agent": "reviewer", "presence": "away", "check_back_in_minutes": 60})
			e.tick(61 * time.Minute)
		}, func(e env) string {
			return e.b.ok("start", args("second"))
		}, false},
		{"holder's conversation closed", func(e env) {
			e.a.ok("close", args("first"))
		}, func(e env) string {
			return e.b.ok("start", args("second"))
		}, false},
		{"holder left", func(e env) {
			e.a.ok("leave", args("first"))
		}, func(e env) string {
			return e.b.ok("start", args("second"))
		}, false},
		{"holder's session closed", func(e env) {
			e.close()
		}, func(e env) string {
			return e.b.ok("start", args("second"))
		}, false},
		{"resuming the holder's own conversation", nil, func(e env) string {
			return e.b.ok("join", map[string]any{"conversation": "first", "agent": "reviewer", "resume": true})
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var offset atomic.Int64
			base := time.Now()
			svc := newServiceWith(t, core.Options{Policy: core.RolePolicy(), Now: func() time.Time {
				return base.Add(time.Duration(offset.Load()))
			}})
			srv := httptest.NewServer(mcpserver.Handler(svc, accountsAuth(t, svc, "1", "2"), mcpserver.Options{}))
			t.Cleanup(srv.Close)
			open := func(user string) (*session, *mcp.ClientSession) {
				t.Helper()
				cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(t.Context(), &mcp.StreamableClientTransport{
					Endpoint:   srv.URL,
					HTTPClient: &http.Client{Transport: addHeader{user: user, rt: http.DefaultTransport}},
				}, nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = cs.Close() })
				return &session{t: t, cs: cs}, cs
			}
			a, csA := open("1")
			b, _ := open("1")
			other, _ := open("2")
			e := env{
				a: a, b: b, other: other,
				close: func() {
					if err := csA.Close(); err != nil {
						t.Fatal(err)
					}
				},
				tick: func(d time.Duration) { offset.Add(int64(d)) },
			}
			a.ok("start", map[string]any{"conversation": "first", "agent": "reviewer", "end": "agreement"})
			if tt.setup != nil {
				tt.setup(e)
			}
			out := tt.act(e)
			if got := strings.Contains(out, nameWarning); got != tt.want {
				t.Fatalf("warned = %v, want %v:\n%s", got, tt.want, out)
			}
			if tt.want {
				if !strings.Contains(out, "reviewer "+nameWarning+" first") {
					t.Errorf("warning does not name the agent and conversation:\n%s", out)
				}
				// Leaving to rename can settle an agreement or strand a
				// conversation the agent started, so the warning sends the
				// agent to its user instead.
				if !strings.Contains(out, "tell your user") || !strings.Contains(out, "do not leave") {
					t.Errorf("warning does not send the agent to its user:\n%s", out)
				}
				if last := lastLine(out); strings.Contains(last, nameWarning) || !strings.Contains(last, "next:") {
					t.Errorf("the standing line is not last:\n%s", out)
				}
			}
		})
	}
}
