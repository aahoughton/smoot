package api_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/aahoughton/smoot/internal/api"
	"github.com/aahoughton/smoot/internal/auth"
	"github.com/aahoughton/smoot/internal/core"
	"github.com/aahoughton/smoot/internal/store"
)

// newTokenServer serves the API in auth mode token and returns a client
// holding the bootstrapped server admin's token.
func newTokenServer(t *testing.T) *client {
	t.Helper()
	return newTokenServerLogging(t, slog.New(slog.DiscardHandler))
}

func newTokenServerLogging(t *testing.T, logger *slog.Logger) *client {
	t.Helper()
	return newTokenServerWith(t, api.Options{Logger: logger})
}

func newTokenServerWith(t *testing.T, opts api.Options) *client {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	opts.AuthMode, opts.Version = "token", "test"
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
	mux := http.NewServeMux()
	api.Register(mux, svc, auth.Bearer{Service: svc}, opts)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &client{t: t, url: srv.URL, token: tok.Secret}
}

func TestAdminFlow(t *testing.T) {
	root := newTokenServer(t)
	anon := root.as("")
	anon.wantError("GET", "/v1/whoami", nil, http.StatusUnauthorized, "unauthorized")

	var who struct {
		AuthMode string        `json:"auth_mode"`
		Version  string        `json:"version"`
		Account  *core.Account `json:"account"`
	}
	root.ok("GET", "/v1/whoami", nil, http.StatusOK, &who)
	if who.AuthMode != "token" || who.Version != "test" || who.Account == nil || who.Account.Name != "root" || !who.Account.ServerAdmin {
		t.Fatalf("whoami = %+v", who)
	}

	root.ok("POST", "/v1/accounts", map[string]any{"name": "pat"}, http.StatusCreated, nil)
	root.wantError("POST", "/v1/accounts", map[string]any{"name": "PAT"}, http.StatusConflict, "account_exists")
	var issued struct {
		ID     int64  `json:"id"`
		Secret string `json:"secret"`
	}
	root.ok("POST", "/v1/accounts/pat/tokens", map[string]any{"label": "laptop"}, http.StatusCreated, &issued)
	pat := root.as(issued.Secret)

	// pat is in no namespace yet.
	pat.wantError("GET", "/v1/namespaces/default/conversations", nil, http.StatusForbidden, "forbidden")
	root.ok("PUT", "/v1/namespaces/default/members/pat", map[string]any{"role": "member"}, http.StatusOK, nil)
	pat.ok("GET", "/v1/namespaces/default/conversations", nil, http.StatusOK, nil)
	root.wantError("PUT", "/v1/namespaces/default/members/pat", map[string]any{"role": "owner"}, http.StatusBadRequest, "invalid")
	var members struct {
		Members []core.Membership `json:"members"`
	}
	root.ok("GET", "/v1/namespaces/default/members", nil, http.StatusOK, &members)
	if len(members.Members) != 1 || members.Members[0].Role != core.RoleMember {
		t.Fatalf("members = %+v", members)
	}
	pat.wantError("GET", "/v1/namespaces/default/members", nil, http.StatusForbidden, "forbidden")
	pat.wantError("GET", "/v1/accounts", nil, http.StatusForbidden, "forbidden")

	// pat manages its own tokens.
	var toks struct {
		Tokens []core.Token `json:"tokens"`
	}
	pat.ok("GET", "/v1/accounts/pat/tokens", nil, http.StatusOK, &toks)
	if len(toks.Tokens) != 1 || toks.Tokens[0].Label != "laptop" {
		t.Fatalf("tokens = %+v", toks)
	}
	root.wantError("PATCH", "/v1/accounts/root", map[string]any{"server_admin": false}, http.StatusConflict, "last_admin")
	if got := root.do("DELETE", "/v1/namespaces/default/members/pat", nil, nil); got != http.StatusNoContent {
		t.Fatalf("remove member = %d", got)
	}
	revoke := "/v1/accounts/pat/tokens/" + strconv.FormatInt(issued.ID, 10) + "/revoke"
	root.wantError("POST", "/v1/accounts/pat/tokens/999/revoke", map[string]any{}, http.StatusNotFound, "token_not_found")
	root.wantError("POST", "/v1/accounts/root/tokens/"+strconv.FormatInt(issued.ID, 10)+"/revoke", map[string]any{}, http.StatusNotFound, "token_not_found")
	pat.wantError("POST", revoke, map[string]any{}, http.StatusConflict, "own_credential")
	pat.wantError("POST", "/v1/accounts/pat/tokens/revoke", map[string]any{}, http.StatusConflict, "own_credential")
	pat.ok("POST", revoke, map[string]any{"force": true}, http.StatusOK, &toks)
	if len(toks.Tokens) != 1 || toks.Tokens[0].ID != issued.ID || toks.Tokens[0].RevokedAt == nil {
		t.Fatalf("self revoke = %+v", toks)
	}
	pat.wantError("GET", "/v1/whoami", nil, http.StatusUnauthorized, "unauthorized")
}

func TestAdminChangesAreLoggedWithoutSecrets(t *testing.T) {
	var buf bytes.Buffer
	root := newTokenServerLogging(t, slog.New(slog.NewTextHandler(&buf, nil)))
	root.ok("POST", "/v1/accounts", map[string]any{"name": "pat"}, http.StatusCreated, nil)
	var issued struct {
		Secret string `json:"secret"`
	}
	root.ok("POST", "/v1/accounts/pat/tokens", map[string]any{}, http.StatusCreated, &issued)
	root.ok("PUT", "/v1/namespaces/default/members/pat", map[string]any{"role": "viewer"}, http.StatusOK, nil)
	log := buf.String()
	for _, want := range []string{
		"msg=admin actor=1 action=create_account target=pat",
		"msg=admin actor=1 action=issue_token target=pat token=2",
		"msg=admin actor=1 action=set_member target=pat namespace=default role=viewer",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, issued.Secret) || strings.Contains(log, root.token) {
		t.Fatalf("a token reached the log:\n%s", log)
	}
}

func TestRateLimitedRequestsSayWhenToRetry(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "smoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	svc := core.New(db, core.Options{Policy: core.RolePolicy(), WriteLimit: 1})
	tok, err := svc.Bootstrap(t.Context(), "root")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	api.Register(mux, svc, auth.Bearer{Service: svc}, api.Options{Logger: slog.New(slog.DiscardHandler)})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &client{t: t, url: srv.URL, token: tok.Secret}
	c.ok("POST", "/v1/namespaces", map[string]any{"name": "one"}, http.StatusCreated, nil)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/v1/namespaces", strings.NewReader(`{"name":"two"}`))
	req.Header.Set("Authorization", "Bearer "+tok.Secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "60" {
		t.Fatalf("second write = %d, Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

func TestTokenLifetimesAreBounded(t *testing.T) {
	root := newTokenServer(t)
	for _, ttl := range []int64{-1, 36028797018963968, 100*366*24*3600 + 1} {
		root.wantError("POST", "/v1/accounts/root/tokens", map[string]any{"ttl_seconds": ttl}, http.StatusBadRequest, "invalid")
	}
	var t1 struct {
		ExpiresAt *string `json:"expires_at"`
	}
	root.ok("POST", "/v1/accounts/root/tokens", map[string]any{"ttl_seconds": 3600}, http.StatusCreated, &t1)
	if t1.ExpiresAt == nil {
		t.Fatal("a one-hour token has no expiry")
	}
}

func TestBackupAndDumpNeedAdmins(t *testing.T) {
	root := newTokenServer(t)
	root.ok("POST", "/v1/accounts", map[string]any{"name": "pat"}, http.StatusCreated, nil)
	var issued struct {
		Secret string `json:"secret"`
	}
	root.ok("POST", "/v1/accounts/pat/tokens", map[string]any{}, http.StatusCreated, &issued)
	root.ok("PUT", "/v1/namespaces/default/members/pat", map[string]any{"role": "member"}, http.StatusOK, nil)
	pat := root.as(issued.Secret)
	pat.wantError("GET", "/v1/backup", nil, http.StatusForbidden, "forbidden")
	pat.wantError("GET", "/v1/dump", nil, http.StatusForbidden, "forbidden")
	pat.wantError("GET", "/v1/dump?namespace=default", nil, http.StatusForbidden, "forbidden")
	root.ok("PUT", "/v1/namespaces/default/members/pat", map[string]any{"role": "admin"}, http.StatusOK, nil)
	if got := pat.do("GET", "/v1/dump?namespace=default", nil, nil); got != http.StatusOK {
		t.Fatalf("namespace admin dump = %d", got)
	}
}

func TestLoadANamespaceDump(t *testing.T) {
	root := newTokenServerWith(t, api.Options{MaxLoadBytes: 4096})
	root.ok("POST", "/v1/namespaces/default/conversations", map[string]any{"name": "c", "agent": "a"}, http.StatusCreated, nil)
	root.ok("POST", "/v1/namespaces/default/conversations/c/messages", map[string]any{"agent": "a", "body": "hello"}, http.StatusCreated, nil)
	dump := root.text("GET", "/v1/dump?namespace=default")

	var res struct {
		Namespace     string `json:"namespace"`
		Conversations int    `json:"conversations"`
		Messages      int    `json:"messages"`
	}
	root.ok("POST", "/v1/load?as=copy", dump, http.StatusCreated, &res)
	if res.Namespace != "copy" || res.Conversations != 1 || res.Messages != 3 {
		t.Fatalf("load = %+v", res)
	}
	var page struct {
		Messages []struct {
			Body string `json:"body"`
		} `json:"messages"`
	}
	root.ok("GET", "/v1/namespaces/copy/conversations/c/messages", nil, http.StatusOK, &page)
	if len(page.Messages) != 3 || page.Messages[2].Body != "hello" {
		t.Fatalf("copied messages = %+v", page.Messages)
	}

	root.wantError("POST", "/v1/load?as=copy", dump, http.StatusConflict, "namespace_exists")
	root.wantError("POST", "/v1/load?as=other", "not a dump\n", http.StatusBadRequest, "invalid")
	root.wantError("POST", "/v1/load?as=big", dump+strings.Repeat(" ", 4096), http.StatusRequestEntityTooLarge, "message_too_large")
	root.ok("POST", "/v1/accounts", map[string]any{"name": "pat"}, http.StatusCreated, nil)
	var issued struct {
		Secret string `json:"secret"`
	}
	root.ok("POST", "/v1/accounts/pat/tokens", map[string]any{}, http.StatusCreated, &issued)
	root.as(issued.Secret).wantError("POST", "/v1/load?as=mine", dump, http.StatusForbidden, "forbidden")
}
