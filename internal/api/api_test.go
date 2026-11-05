package api_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aahoughton/smoot/internal/api"
	"github.com/aahoughton/smoot/internal/auth"
	"github.com/aahoughton/smoot/internal/core"
	"github.com/aahoughton/smoot/internal/store"
)

type client struct {
	t     *testing.T
	url   string
	token string
}

// as returns a client that sends token as a bearer token.
func (c *client) as(token string) *client { return &client{t: c.t, url: c.url, token: token} }

func newServer(t *testing.T, authn auth.Authenticator, opts core.Options) *client {
	return newServerWith(t, authn, opts, api.Options{Logger: slog.New(slog.DiscardHandler)})
}

func newServerWith(t *testing.T, authn auth.Authenticator, opts core.Options, apiOpts api.Options) *client {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "smoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	mux := http.NewServeMux()
	api.Register(mux, core.New(db, opts), authn, apiOpts)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &client{t: t, url: srv.URL}
}

// do sends a request and decodes the JSON response into out (if non-nil).
// body may be a string (sent verbatim) or a value (JSON-encoded).
func (c *client) do(method, path string, body any, out any) int {
	c.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			c.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(c.t.Context(), method, c.url+path, rd)
	if err != nil {
		c.t.Fatal(err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		c.t.Fatal(err)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			c.t.Fatalf("%s %s: decode %q: %v", method, path, raw, err)
		}
	}
	return resp.StatusCode
}

// text sends a request without a body and returns the response body,
// failing unless the status is 200.
func (c *client) text(method, path string) string {
	c.t.Helper()
	req, err := http.NewRequestWithContext(c.t.Context(), method, c.url+path, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		c.t.Fatalf("%s %s = %d %s %v", method, path, resp.StatusCode, raw, err)
	}
	return string(raw)
}

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		LastSeq *int64 `json:"last_seq"`
	} `json:"error"`
}

func (c *client) wantError(method, path string, body any, status int, code string) apiError {
	c.t.Helper()
	var e apiError
	if got := c.do(method, path, body, &e); got != status || e.Error.Code != code {
		c.t.Fatalf("%s %s = %d %+v, want %d %s", method, path, got, e, status, code)
	}
	return e
}

func (c *client) ok(method, path string, body any, status int, out any) {
	c.t.Helper()
	var raw json.RawMessage
	if got := c.do(method, path, body, &raw); got != status {
		c.t.Fatalf("%s %s = %d %s, want %d", method, path, got, raw, status)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			c.t.Fatal(err)
		}
	}
}

const conv = "/v1/namespaces/default/conversations/design"

func TestFlow(t *testing.T) {
	c := newServer(t, auth.None{}, core.Options{})

	var started core.Conversation
	c.ok("POST", "/v1/namespaces/default/conversations", map[string]any{
		"name": "design", "topic": "pick a cache", "end": "agreement", "agent": "alice",
	}, 201, &started)
	if started.Status != core.StatusOpen || len(started.Participants) != 1 {
		t.Fatalf("started = %+v", started)
	}

	var st core.Standing
	c.ok("POST", conv+"/join", map[string]any{"agent": "bob"}, 200, &st)
	if !st.Expected || st.Next != core.NextRead {
		t.Fatalf("bob joined: %+v", st)
	}

	var said core.SayResult
	c.ok("POST", conv+"/messages", map[string]any{"agent": "alice", "body": "LRU or LFU?"}, 201, &said)

	var d core.Digest
	c.ok("POST", conv+"/wait", map[string]any{"agent": "bob", "timeout_seconds": 5}, 200, &d)
	if d.New == 0 || d.TimedOut || d.Previews[len(d.Previews)-1].Text != "LRU or LFU?" {
		t.Fatalf("digest = %+v", d)
	}

	var page core.ReadResult
	c.ok("POST", conv+"/read", map[string]any{"agent": "bob"}, 200, &page)
	if page.Standing == nil || page.Standing.Unread != 0 {
		t.Fatalf("read = %+v", page)
	}

	c.ok("POST", conv+"/messages", map[string]any{
		"agent": "bob", "body": "LRU", "reply_to": said.Message.Seq, "expected_last_seq": page.LastSeq,
	}, 201, nil)

	c.ok("POST", conv+"/conclude", map[string]any{"agent": "alice", "body": "AGREED: abc1234"}, 200, nil)
	c.ok("POST", conv+"/conclude", map[string]any{"agent": "bob", "body": "AGREED: abc1234"}, 200, &st)
	// bob has not read alice's conclusion yet.
	if st.Status != core.StatusConcluded || st.Next != core.NextRead {
		t.Fatalf("after conclusion: %+v", st)
	}

	var got core.Conversation
	c.ok("GET", conv, nil, 200, &got)
	if got.Status != core.StatusConcluded || got.Topic != "pick a cache" {
		t.Fatalf("conversation = %+v", got)
	}

	var list struct {
		Conversations []core.Conversation `json:"conversations"`
	}
	c.ok("GET", "/v1/namespaces/default/conversations", nil, 200, &list)
	if len(list.Conversations) != 1 {
		t.Fatalf("list = %+v", list)
	}
}

func TestNamespacesAndStatus(t *testing.T) {
	c := newServer(t, auth.None{}, core.Options{})
	c.ok("POST", "/v1/namespaces", map[string]any{"name": "team-blue"}, 201, nil)
	c.wantError("POST", "/v1/namespaces", map[string]any{"name": "team-blue"}, 409, "namespace_exists")

	var ns struct {
		Namespaces []core.Namespace `json:"namespaces"`
	}
	c.ok("GET", "/v1/namespaces", nil, 200, &ns)
	if len(ns.Namespaces) != 2 {
		t.Fatalf("namespaces = %+v", ns)
	}

	c.ok("POST", "/v1/namespaces/team-blue/conversations", map[string]any{"name": "a", "agent": "x", "end": "agreement"}, 201, nil)
	var status struct {
		Standings []core.Standing `json:"standings"`
	}
	c.ok("GET", "/v1/namespaces/team-blue/status?agent=x", nil, 200, &status)
	if len(status.Standings) != 1 || !status.Standings[0].Expected {
		t.Fatalf("status = %+v", status)
	}
}

func TestStartWithoutJoin(t *testing.T) {
	c := newServer(t, auth.None{}, core.Options{})
	var started core.Conversation
	c.ok("POST", "/v1/namespaces/default/conversations", map[string]any{"name": "design", "agent": "coord", "join": false}, 201, &started)
	if len(started.Participants) != 0 {
		t.Fatalf("participants = %+v", started.Participants)
	}
}

func TestErrors(t *testing.T) {
	c := newServer(t, auth.None{}, core.Options{MaxBodyBytes: 8})
	c.ok("POST", "/v1/namespaces/default/conversations", map[string]any{"name": "design", "agent": "alice"}, 201, nil)

	c.wantError("POST", conv+"/messages", `{"agent":"alice","body":"x","bogus":1}`, 400, "invalid")
	c.wantError("POST", conv+"/messages", ``, 400, "invalid")
	c.wantError("POST", conv+"/messages", `{"agent":"alice","body":"x"} {}`, 400, "invalid")
	c.wantError("POST", conv+"/messages", map[string]any{"agent": "alice", "body": "123456789"}, 413, "message_too_large")
	c.wantError("POST", conv+"/messages", map[string]any{"agent": "zed", "body": "hi"}, 409, "not_participant")
	c.wantError("GET", conv+"/messages?after=x", nil, 400, "invalid")
	c.wantError("GET", conv+"/messages?limit=0", nil, 400, "invalid")
	c.wantError("GET", "/v1/namespaces/default/conversations?include_archived=maybe", nil, 400, "invalid")
	c.wantError("GET", "/v1/namespaces/nope/conversations", nil, 404, "namespace_not_found")
	c.wantError("GET", "/v1/namespaces/default/conversations/nope", nil, 404, "conversation_not_found")
	c.wantError("GET", "/v1/nothing-here", nil, 404, "not_found")
	c.wantError("POST", conv+"/close", map[string]any{"agent": "bob"}, 403, "not_coordinator")
	c.wantError("POST", conv+"/join", map[string]any{"agent": "alice"}, 409, "name_active")

	e := c.wantError("POST", conv+"/messages", map[string]any{"agent": "alice", "body": "x", "expected_last_seq": 0}, 409, "stale_context")
	if e.Error.LastSeq == nil || *e.Error.LastSeq == 0 {
		t.Fatalf("stale error = %+v", e)
	}
}

func TestWaitBounds(t *testing.T) {
	c := newServer(t, auth.None{}, core.Options{})
	c.ok("POST", "/v1/namespaces/default/conversations", map[string]any{"name": "design", "agent": "alice", "end": "agreement"}, 201, nil)
	c.ok("POST", conv+"/read", map[string]any{"agent": "alice"}, 200, nil)

	c.wantError("POST", conv+"/wait", map[string]any{"agent": "alice", "timeout_seconds": 301}, 400, "invalid")
	c.wantError("POST", conv+"/wait", map[string]any{"agent": "alice", "timeout_seconds": -1}, 400, "invalid")
	c.wantError("POST", conv+"/wait", map[string]any{"timeout_seconds": 0}, 400, "invalid")

	var d core.Digest
	c.ok("POST", conv+"/wait", map[string]any{"agent": "alice", "timeout_seconds": 0}, 200, &d)
	if !d.TimedOut || !d.Standing.Expected {
		t.Fatalf("digest = %+v", d)
	}
}

type forbidAll struct{}

func (forbidAll) Authenticate(*http.Request) (core.Principal, error) {
	return core.Principal{}, &core.Error{Code: core.CodeForbidden, Message: "no"}
}

func TestAuthenticatorErrorsStopTheRequest(t *testing.T) {
	c := newServer(t, forbidAll{}, core.Options{})
	c.wantError("GET", "/v1/namespaces", nil, 403, "forbidden")
}

func TestOpenAPIServed(t *testing.T) {
	c := newServer(t, auth.None{}, core.Options{})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, c.url+"/v1/openapi.yaml", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !bytes.HasPrefix(body, []byte("openapi: 3.1.0")) {
		t.Fatalf("status %d, body %.40q", resp.StatusCode, body)
	}
}

// specOperations returns "METHOD /path" for every operation in openapi.yaml.
func specOperations(t *testing.T) []string {
	t.Helper()
	raw := api.OpenAPISpec()
	pathRE := regexp.MustCompile(`^  (/\S+):$`)
	methodRE := regexp.MustCompile(`^    (get|post|put|patch|delete):`)
	var ops []string
	var current string
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		line := sc.Text()
		if m := pathRE.FindStringSubmatch(line); m != nil {
			current = m[1]
			continue
		}
		if !strings.HasPrefix(line, " ") {
			current = ""
		}
		if m := methodRE.FindStringSubmatch(line); m != nil && current != "" {
			ops = append(ops, strings.ToUpper(m[1])+" "+current)
		}
	}
	slices.Sort(ops)
	return ops
}

func TestOpenAPIMatchesRoutes(t *testing.T) {
	spec := specOperations(t)
	routes := api.Routes()
	slices.Sort(routes)
	if !slices.Equal(spec, routes) {
		t.Fatalf("openapi.yaml and routes differ\nspec:   %v\nroutes: %v", spec, routes)
	}
}

func TestBodyDecodingIsStrict(t *testing.T) {
	c := newServer(t, auth.None{}, core.Options{})
	c.wantError("POST", "/v1/namespaces", `{"name":"x"}}`, 400, "invalid")
	c.wantError("POST", "/v1/namespaces", `{"name":"x"} {"name":"y"}`, 400, "invalid")
	c.wantError("POST", "/v1/namespaces", `{"name":"x"}`+strings.Repeat(" ", 1<<20)+`{}`, 413, "message_too_large")
	c.ok("POST", "/v1/namespaces", "{\"name\":\"x\"}\n\n", 201, nil)
}

func TestSlowBodyIsCutOff(t *testing.T) {
	c := newServerWith(t, auth.None{}, core.Options{}, api.Options{BodyReadTimeout: 200 * time.Millisecond})
	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "tcp", strings.TrimPrefix(c.url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// Headers promise 100 bytes; send a few and stall.
	if _, err := io.WriteString(conn, "POST /v1/namespaces HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"na"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var e apiError
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusRequestTimeout || e.Error.Code != "request_timeout" {
		t.Fatalf("got %d %+v", resp.StatusCode, e)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("cut off after %s", elapsed)
	}
}

func TestTooManyWaiters(t *testing.T) {
	c := newServer(t, auth.None{}, core.Options{MaxWaiters: 1})
	c.ok("POST", "/v1/namespaces/default/conversations", map[string]any{"name": "design", "agent": "alice"}, 201, nil)
	c.ok("POST", conv+"/join", map[string]any{"agent": "bob"}, 200, nil)
	c.ok("POST", conv+"/read", map[string]any{"agent": "alice"}, 200, nil)
	c.ok("POST", conv+"/read", map[string]any{"agent": "bob"}, 200, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, c.url+conv+"/wait", strings.NewReader(`{"agent":"alice","timeout_seconds":2}`))
		if err != nil {
			return
		}
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var got core.Conversation
		c.ok("GET", conv, nil, 200, &got)
		if got.Participants[0].Presence == core.PresenceWaiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first wait never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.wantError("POST", conv+"/wait", map[string]any{"agent": "bob", "timeout_seconds": 1}, 429, "too_many_waiters")
	<-done
}

func TestConcludeNeedsABody(t *testing.T) {
	c := newServer(t, auth.None{}, core.Options{})
	c.ok("POST", "/v1/namespaces/default/conversations", map[string]any{"name": "design", "agent": "alice"}, 201, nil)
	c.wantError("POST", conv+"/conclude", map[string]any{"agent": "alice"}, 400, "invalid")
}

func TestFollow(t *testing.T) {
	c := newServer(t, auth.None{}, core.Options{})
	c.ok("POST", "/v1/namespaces/default/conversations", map[string]any{"name": "design", "agent": "alice", "topic": "shape"}, 201, nil)
	c.ok("POST", conv+"/messages", map[string]any{"agent": "alice", "body": "hello"}, 201, nil)

	var got api.FollowResult
	c.ok("GET", conv+"/follow?after=0&timeout=0", nil, 200, &got)
	if len(got.Messages) != 3 || got.LastSeq != 3 || got.Conversation.Topic != "shape" || len(got.Conversation.Participants) != 1 {
		t.Fatalf("follow = %+v", got)
	}

	// A timeout is an empty batch that still carries the conversation.
	c.ok("GET", conv+"/follow?after=3&timeout=0", nil, 200, &got)
	if len(got.Messages) != 0 || got.LastSeq != 3 || got.Conversation.Name != "design" {
		t.Fatalf("timed-out follow = %+v", got)
	}

	// A pending follow wakes on a new message from anyone, including an
	// observer.
	done := make(chan api.FollowResult, 1)
	go func() {
		var res api.FollowResult
		defer func() { done <- res }()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, c.url+conv+"/follow?after=3&timeout=10", nil)
		if err != nil {
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		defer func() { _ = resp.Body.Close() }()
		_ = json.NewDecoder(resp.Body).Decode(&res)
	}()
	time.Sleep(50 * time.Millisecond)
	c.ok("POST", conv+"/observe", map[string]any{"name": "carol", "body": "context"}, 201, nil)
	select {
	case res := <-done:
		if len(res.Messages) != 1 || res.Messages[0].Kind != core.KindObserver {
			t.Fatalf("woken follow = %+v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follow did not wake")
	}

	c.wantError("GET", conv+"/follow?timeout=301", nil, 400, "invalid")
	c.wantError("GET", conv+"/follow?after=-1", nil, 400, "invalid")
	c.wantError("GET", "/v1/namespaces/default/conversations/nope/follow", nil, 404, "conversation_not_found")
}

func TestTooManyWatchers(t *testing.T) {
	c := newServer(t, auth.None{}, core.Options{MaxWatchers: 1})
	c.ok("POST", "/v1/namespaces/default/conversations", map[string]any{"name": "design", "agent": "alice"}, 201, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Each poll below holds the slot briefly, so this follow can be
		// refused too; it retries until it holds the slot.
		for {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, c.url+conv+"/follow?after=2&timeout=2", nil)
			if err != nil {
				return
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusTooManyRequests {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	// The first follow holds the only slot until it times out; a second
	// one is refused once it has started.
	deadline := time.Now().Add(2 * time.Second)
	for {
		var out json.RawMessage
		if status := c.do("GET", conv+"/follow?after=2&timeout=0", nil, &out); status == 429 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second follow was never refused")
		}
		time.Sleep(10 * time.Millisecond)
	}
	<-done
}

func TestListPagesAndFilters(t *testing.T) {
	c := newServer(t, auth.None{}, core.Options{})
	for _, n := range []string{"a", "b", "c"} {
		c.ok("POST", "/v1/namespaces/default/conversations", map[string]any{"name": n, "agent": "alice", "end": "agreement"}, 201, nil)
	}
	c.ok("POST", "/v1/namespaces/default/conversations/a/conclude", map[string]any{"agent": "alice", "body": "AGREED: abc1234"}, 200, nil)
	c.ok("POST", "/v1/namespaces/default/conversations/a/leave", map[string]any{"agent": "alice"}, 200, nil)

	// Each response decodes into a fresh page: omitted fields such as next
	// must not carry over.
	var page core.ListPage
	get := func(path string) {
		t.Helper()
		page = core.ListPage{}
		c.ok("GET", path, nil, 200, &page)
	}
	get("/v1/namespaces/default/conversations?status=open&limit=1")
	if len(page.Conversations) != 1 || page.Next == "" || page.Conversations[0].Status != core.StatusOpen {
		t.Fatalf("first page = %+v", page)
	}
	first := page.Conversations[0].Name
	get("/v1/namespaces/default/conversations?status=open&limit=1&before=" + page.Next)
	if len(page.Conversations) != 1 || page.Conversations[0].Name == first || page.Next != "" {
		t.Fatalf("second page = %+v", page)
	}
	get("/v1/namespaces/default/conversations?status=concluded")
	if len(page.Conversations) != 1 || page.Conversations[0].Name != "a" {
		t.Fatalf("concluded = %+v", page)
	}
	get("/v1/namespaces/default/conversations?status=all")
	if len(page.Conversations) != 3 {
		t.Fatalf("all = %+v", page)
	}
	c.wantError("GET", "/v1/namespaces/default/conversations?status=done", nil, 400, "invalid")
	c.wantError("GET", "/v1/namespaces/default/conversations?limit=0", nil, 400, "invalid")
	c.wantError("GET", "/v1/namespaces/default/conversations?before=x", nil, 400, "invalid")
}

func TestReadTail(t *testing.T) {
	c := newServer(t, auth.None{}, core.Options{})
	c.ok("POST", "/v1/namespaces/default/conversations", map[string]any{"name": "design", "agent": "alice"}, 201, nil)
	for i := range 5 {
		c.ok("POST", conv+"/messages", map[string]any{"agent": "alice", "body": fmt.Sprintf("m%d", i)}, 201, nil)
	}
	var res core.ReadResult
	c.ok("GET", conv+"/messages?tail=2", nil, 200, &res)
	if len(res.Messages) != 2 || res.Messages[1].Body != "m4" {
		t.Fatalf("tail = %+v", res.Messages)
	}
	c.wantError("GET", conv+"/messages?tail=0", nil, 400, "invalid")
	c.wantError("GET", conv+"/messages?tail=2&after=1", nil, 400, "invalid")
	// 2^32+1 would truncate to 1 in a 32-bit int.
	c.wantError("GET", conv+"/messages?tail=4294967297", nil, 400, "invalid")
	c.wantError("GET", conv+"/messages?limit=4294967297", nil, 400, "invalid")
	c.wantError("GET", "/v1/namespaces/default/conversations?limit=4294967297", nil, 400, "invalid")
}

// Reading as an agent changes state, so it is a POST; the GET refuses an
// agent instead of quietly moving its read position.
func TestReadingAsAnAgentIsAPost(t *testing.T) {
	c := newServer(t, auth.None{}, core.Options{})
	c.ok("POST", "/v1/namespaces/default/conversations", map[string]any{"name": "design", "agent": "alice", "join": true}, 201, nil)
	c.wantError("GET", conv+"/messages?agent=alice", nil, 400, "invalid")
	if got := c.do("GET", conv+"/wait?agent=alice", nil, nil); got != http.StatusNotFound {
		t.Fatalf("GET wait = %d", got)
	}
}
