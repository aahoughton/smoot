package mcpserver

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aahoughton/smoot/internal/auth"
	"github.com/aahoughton/smoot/internal/core"
)

const sessionHeader = "Mcp-Session-Id"

type principalKey struct{}

// guard authenticates every MCP request, not only the one that creates a
// session, and refuses requests on a session created by another principal.
type guard struct {
	authn      auth.Authenticator
	next       http.Handler
	ttl        time.Duration
	perAccount int
	total      int
	activity   *auth.Activity

	mu     sync.Mutex
	owners map[string]owner
	// starting counts sessions admitted but not yet recorded, per account;
	// they hold their place against the caps.
	starting map[int64]int
	// names holds the open session that last used each agent name in each
	// conversation. It is in memory like owners, so a server restart clears
	// both and a session reconnecting after one is not taken for a stranger.
	names map[nameUse]string
}

// nameUse is an agent name one account used in one conversation.
type nameUse struct {
	ns           string
	account      int64
	agent        string
	conversation string
}

// maxNameUsesPerAccount bounds one account's entries in g.names, so it cannot
// use up another account's. Past it the account's new uses go unrecorded,
// which only costs the warning they would have given.
const maxNameUsesPerAccount = 1024

type owner struct {
	account int64
	created time.Time
	seen    time.Time
	// srv is the session's server; once it has no session left, the SDK
	// has closed the session and the entry is pruned.
	srv *mcp.Server
}

// serverRef carries the server the SDK creates for a new session back to
// the guard.
type serverRef struct{ srv *mcp.Server }

type serverRefKey struct{}

// ownerMargin keeps an owner entry past the SDK's own session timeout, so
// the SDK always forgets a session before the guard does.
const ownerMargin = time.Hour

func (g *guard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, err := g.authn.Authenticate(r)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	g.activity.Seen(r, p, auth.SurfaceMCP)
	if sid := r.Header.Get(sessionHeader); sid != "" {
		switch g.check(sid, p) {
		case sessionUnknown:
			// The SDK may still hold a session the guard has forgotten;
			// refusing it keeps any other caller from taking it over. The
			// client starts a new session on 404.
			writeError(w, http.StatusNotFound, "session_not_found", "unknown MCP session; initialize a new one")
			return
		case sessionOther:
			writeAuthError(w, &core.Error{Code: core.CodeForbidden, Message: "this MCP session belongs to another caller"})
			return
		}
		if r.Method == http.MethodDelete {
			defer g.forget(sid)
		}
		g.next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
		return
	}
	if err := g.admit(p); err != nil {
		writeError(w, http.StatusTooManyRequests, "too_many_sessions", err.Error())
		return
	}
	ref := &serverRef{}
	// The start is recorded when the response begins, before it reaches the
	// client, which may send its next request at once: on the new session,
	// or, after a request that made none (the SDK client's server/discover
	// probe), another start.
	cw := &captureSession{ResponseWriter: w, started: func(sid string) { g.record(sid, p, ref.srv) }}
	ctx := context.WithValue(context.WithValue(r.Context(), principalKey{}, p), serverRefKey{}, ref)
	g.next.ServeHTTP(cw, r.WithContext(ctx))
	cw.capture() // a handler that wrote nothing still ends its start
}

// admit refuses a new session past the total cap, or past the per-account
// cap for any principal but the anonymous one, after dropping entries for
// sessions the SDK has closed. An admitted session holds its place until
// record, so concurrent starts cannot pass a cap together.
func (g *guard) admit(p core.Principal) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.prune(time.Now())
	mine, all := g.starting[p.AccountID], 0
	for _, n := range g.starting {
		all += n
	}
	for _, o := range g.owners {
		if o.account == p.AccountID {
			mine++
		}
	}
	all += len(g.owners)
	switch {
	case mine >= g.perAccount && !p.IsAnonymous():
		return fmt.Errorf("this account has %d open MCP sessions, the most allowed; end idle ones (they close after %s idle)", mine, g.ttl)
	case all >= g.total:
		return fmt.Errorf("the server has %d open MCP sessions, the most allowed; retry later", all)
	}
	if g.starting == nil {
		g.starting = map[int64]int{}
	}
	g.starting[p.AccountID]++
	return nil
}

// prune drops sessions the SDK has closed and entries well past the idle
// timeout. A session is recorded before the SDK finishes starting it, so
// one younger than startGrace is never taken for closed. The caller holds
// g.mu.
func (g *guard) prune(now time.Time) {
	for id, o := range g.owners {
		closed := o.srv != nil && now.Sub(o.created) > startGrace && !hasSession(o.srv)
		if now.Sub(o.seen) > g.ttl+ownerMargin || closed {
			delete(g.owners, id)
		}
	}
	g.sweepNames()
}

// sweepNames drops the name uses of sessions that are gone. The caller holds
// g.mu.
func (g *guard) sweepNames() {
	for u, sid := range g.names {
		if _, ok := g.owners[sid]; !ok {
			delete(g.names, u)
		}
	}
}

// useName records that session sid called as u.agent in u.conversation.
func (g *guard) useName(sid string, u nameUse) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.names[u]; !ok {
		n := 0
		for other := range g.names {
			if other.account == u.account {
				n++
			}
		}
		if n >= maxNameUsesPerAccount {
			return
		}
	}
	g.names[u] = sid
}

// dropName forgets a use whose conversation or participant is finished.
func (g *guard) dropName(u nameUse) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.names, u)
}

// namesElsewhere returns, sorted, the conversations where another open
// session last used this agent name.
func (g *guard) namesElsewhere(sid, ns string, account int64, agent string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.prune(time.Now())
	var out []string
	for u, holder := range g.names {
		if holder != sid && u.ns == ns && u.account == account && u.agent == agent {
			out = append(out, u.conversation)
		}
	}
	slices.Sort(out)
	return out
}

// recordNames is receiving middleware that records the name each successful
// tool call used, and forgets it after leave or close. It sees every tool, so
// a session that carries on after a server restart without joining again is
// recorded too.
func recordNames(g *guard, p core.Principal, ns string) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			params, isCall := req.GetParams().(*mcp.CallToolParamsRaw)
			if !isCall || method != "tools/call" || err != nil {
				return res, err
			}
			if r, ok := res.(*mcp.CallToolResult); !ok || r == nil || r.IsError {
				return res, err
			}
			conversation, agent := callTarget(params)
			if s := req.GetSession(); s != nil && s.ID() != "" && conversation != "" && agent != "" {
				u := nameUse{ns: ns, account: p.AccountID, agent: agent, conversation: conversation}
				if params.Name == "leave" || params.Name == "close" {
					g.dropName(u)
				} else {
					g.useName(s.ID(), u)
				}
			}
			return res, err
		}
	}
}

// startGrace is how long a new session is kept before the SDK must show it.
const startGrace = time.Minute

func hasSession(s *mcp.Server) bool {
	for range s.Sessions() {
		return true
	}
	return false
}

type sessionCheck int

const (
	sessionMine sessionCheck = iota
	sessionUnknown
	sessionOther
)

// check reports whether p may use sid. A session belongs to an account, not
// to the credential that created it, so another valid token of the same
// account may continue it.
func (g *guard) check(sid string, p core.Principal) sessionCheck {
	g.mu.Lock()
	defer g.mu.Unlock()
	o, ok := g.owners[sid]
	switch {
	case !ok:
		return sessionUnknown
	case o.account != p.AccountID:
		return sessionOther
	}
	o.seen = time.Now()
	g.owners[sid] = o
	return sessionMine
}

// record ends an admitted start: the session it created, if any, now
// counts in its place.
func (g *guard) record(sid string, p core.Principal, srv *mcp.Server) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.starting[p.AccountID]--; g.starting[p.AccountID] <= 0 {
		delete(g.starting, p.AccountID)
	}
	if sid == "" {
		return
	}
	now := time.Now()
	g.prune(now)
	g.owners[sid] = owner{account: p.AccountID, created: now, seen: now, srv: srv}
}

// SessionUse is one account's open MCP sessions.
type SessionUse struct {
	AccountID int64
	Sessions  int
	// LastUsed is the latest request on any of them.
	LastUsed time.Time
}

// Sessions reports the open MCP sessions per account, most recently used
// first.
func (e *Endpoint) Sessions() []SessionUse {
	g := e.guard
	g.mu.Lock()
	g.prune(time.Now())
	by := map[int64]*SessionUse{}
	for _, o := range g.owners {
		u := by[o.account]
		if u == nil {
			u = &SessionUse{AccountID: o.account}
			by[o.account] = u
		}
		u.Sessions++
		if o.seen.After(u.LastUsed) {
			u.LastUsed = o.seen
		}
	}
	g.mu.Unlock()
	out := make([]SessionUse, 0, len(by))
	for _, u := range by {
		out = append(out, *u)
	}
	slices.SortFunc(out, func(x, y SessionUse) int {
		return cmp.Or(y.LastUsed.Compare(x.LastUsed), cmp.Compare(x.AccountID, y.AccountID))
	})
	return out
}

func (g *guard) forget(sid string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.owners, sid)
	g.sweepNames()
}

// captureSession passes started the session id the SDK assigns on
// initialize, or "" when it assigns none, once and before any of the
// response is sent.
type captureSession struct {
	http.ResponseWriter
	done    bool
	started func(sid string)
}

func (c *captureSession) capture() {
	if !c.done {
		c.done = true
		c.started(c.Header().Get(sessionHeader))
	}
}

func (c *captureSession) WriteHeader(code int) {
	c.capture()
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureSession) Write(b []byte) (int, error) {
	c.capture()
	return c.ResponseWriter.Write(b)
}

// Flush keeps streaming (SSE) responses working through the wrapper.
func (c *captureSession) Flush() {
	c.capture()
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (c *captureSession) Unwrap() http.ResponseWriter { return c.ResponseWriter }

func writeAuthError(w http.ResponseWriter, err error) {
	status, code, msg := http.StatusUnauthorized, "unauthorized", "authentication failed"
	if ce, ok := errors.AsType[*core.Error](err); ok {
		code, msg = string(ce.Code), ce.Message
		switch ce.Code {
		case core.CodeForbidden:
			status = http.StatusForbidden
		case core.CodeRateLimited:
			status = http.StatusTooManyRequests
			w.Header().Set("Retry-After", strconv.Itoa(int(ce.RetryAfter.Seconds())))
		}
	}
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="smoot"`)
	}
	writeError(w, status, code, msg)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
}
