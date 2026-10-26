// Package client is a Go client for the smoot JSON API. The CLI and the TUI
// use it; neither opens the database.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aahoughton/smoot/internal/core"
)

// Client talks to one smoot server.
type Client struct {
	base      *url.URL
	http      *http.Client
	token     string
	userAgent string
}

// DefaultUserAgent identifies the smoot CLI to the server, which shows it
// in smoot admin status.
const DefaultUserAgent = "smoot-cli"

// New returns a client for the server at baseURL (for example
// http://127.0.0.1:8787). token, if set, is sent as a bearer token.
func New(baseURL, token string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("client: invalid server URL %q: want http(s)://host[:port]", baseURL)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	// No client timeout: waits are long by design and bounded by ctx.
	// Redirects must stay on the server, which the token is for.
	hc := &http.Client{CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		if !strings.EqualFold(req.URL.Scheme, u.Scheme) || !strings.EqualFold(req.URL.Host, u.Host) {
			return fmt.Errorf("client: refusing a redirect from %s to %s://%s", u.Host, req.URL.Scheme, req.URL.Host)
		}
		return nil
	}}
	return &Client{base: u, http: hc, token: token, userAgent: DefaultUserAgent}, nil
}

// SetUserAgent replaces the User-Agent the client sends.
func (c *Client) SetUserAgent(ua string) { c.userAgent = ua }

// URL returns the server base URL.
func (c *Client) URL() string { return c.base.String() }

// Error is an error response from the server.
type Error struct {
	Status  int
	Code    string
	Message string
	LastSeq *int64
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Temporary reports whether retrying the same request later may succeed.
func (e *Error) Temporary() bool {
	return e.Status == http.StatusServiceUnavailable || e.Status == http.StatusTooManyRequests ||
		e.Status == http.StatusBadGateway || e.Status == http.StatusGatewayTimeout
}

// IsTemporary reports whether err is a transport failure or a server
// response worth retrying.
func IsTemporary(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Temporary()
	}
	return err != nil
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	u := *c.base
	u.Path += path
	u.RawQuery = query.Encode()
	var rd io.Reader
	contentType := "application/json"
	switch b := body.(type) {
	case nil:
	case jsonl:
		rd, contentType = b.Reader, "application/jsonl"
	case octets:
		rd, contentType = b.Reader, "application/octet-stream"
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("User-Agent", c.userAgent)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var eb struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
				LastSeq *int64 `json:"last_seq"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &eb) != nil || eb.Error.Code == "" {
			return &Error{Status: resp.StatusCode, Code: "http_" + strconv.Itoa(resp.StatusCode), Message: http.StatusText(resp.StatusCode)}
		}
		return &Error{Status: resp.StatusCode, Code: eb.Error.Code, Message: eb.Error.Message, LastSeq: eb.Error.LastSeq}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// jsonl is a request body sent as is, as JSON Lines.
type jsonl struct{ io.Reader }

// octets is a request body sent as is, as raw bytes.
type octets struct{ io.Reader }

// LoadResult is what POST /v1/load added.
type LoadResult struct {
	Namespace     string `json:"namespace"`
	Conversations int    `json:"conversations"`
	Messages      int    `json:"messages"`
}

// LoadNamespace sends a namespace dump to POST /v1/load, to be added as
// namespace as (the dump's own name when empty). Server admins only.
func (c *Client) LoadNamespace(ctx context.Context, dump io.Reader, as string) (LoadResult, error) {
	var q url.Values
	if as != "" {
		q = url.Values{"as": {as}}
	}
	var out LoadResult
	err := c.do(ctx, http.MethodPost, "/v1/load", q, jsonl{dump}, &out)
	return out, err
}

func convPath(ns, conv string) string {
	return "/v1/namespaces/" + url.PathEscape(ns) + "/conversations/" + url.PathEscape(conv)
}

// Health checks GET /healthz.
func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/healthz", nil, nil, nil)
}

// WaitParams describe a wait.
type WaitParams struct {
	Agent string
	After *int64
	// Timeout is sent in whole seconds; the server caps it.
	Timeout time.Duration
}

// Wait blocks on the server until there is something new for the agent.
func (c *Client) Wait(ctx context.Context, ns, conv string, p WaitParams) (core.Digest, error) {
	body := map[string]any{"agent": p.Agent, "timeout_seconds": int(p.Timeout / time.Second)}
	if p.After != nil {
		body["after"] = *p.After
	}
	var d core.Digest
	err := c.do(ctx, http.MethodPost, convPath(ns, conv)+"/wait", nil, body, &d)
	return d, err
}

// Status returns the agent's standing in every open conversation it is part
// of.
func (c *Client) Status(ctx context.Context, ns, agent string) ([]core.Standing, error) {
	var out struct {
		Standings []core.Standing `json:"standings"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/namespaces/"+url.PathEscape(ns)+"/status", url.Values{"agent": {agent}}, nil, &out)
	return out.Standings, err
}

// Whoami describes the caller and the server.
type Whoami struct {
	AuthMode    string            `json:"auth_mode"`
	Version     string            `json:"version"`
	Account     *core.Account     `json:"account"`
	Memberships []core.Membership `json:"memberships"`
}

// Whoami returns GET /v1/whoami.
func (c *Client) Whoami(ctx context.Context) (Whoami, error) {
	var out Whoami
	err := c.do(ctx, http.MethodGet, "/v1/whoami", nil, nil, &out)
	return out, err
}

// ServerStatus is GET /v1/server: who is using the server.
type ServerStatus struct {
	Version       string                  `json:"version"`
	AuthMode      string                  `json:"auth_mode"`
	StartedAt     time.Time               `json:"started_at"`
	Now           time.Time               `json:"now"`
	LastMessage   *core.LastMessage       `json:"last_message"`
	Conversations []core.LiveConversation `json:"conversations"`
	MCPSessions   []MCPSessions           `json:"mcp_sessions"`
	Callers       []Caller                `json:"callers"`
	FailedSignIns []FailedSignIn          `json:"failed_sign_ins"`
}

// MCPSessions is one account's open MCP sessions.
type MCPSessions struct {
	Account  string    `json:"account"`
	Sessions int       `json:"sessions"`
	LastUsed time.Time `json:"last_used"`
}

// Caller is one account's requests through one surface, from one address
// and client.
type Caller struct {
	Account   string    `json:"account"`
	Surface   string    `json:"surface"`
	Address   string    `json:"address"`
	Client    string    `json:"client"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Requests  int64     `json:"requests"`
}

// FailedSignIn is the rejected credentials from one address.
type FailedSignIn struct {
	Address string    `json:"address"`
	Count   int64     `json:"count"`
	LastAt  time.Time `json:"last_at"`
}

// ServerStatus returns GET /v1/server. Server admins only.
func (c *Client) ServerStatus(ctx context.Context) (ServerStatus, error) {
	var out ServerStatus
	err := c.do(ctx, http.MethodGet, "/v1/server", nil, nil, &out)
	return out, err
}

// ListFeedback returns agents' feedback notes, most recently written
// first. Server admins only.
func (c *Client) ListFeedback(ctx context.Context, f core.FeedbackFilter) (core.FeedbackPage, error) {
	q := url.Values{}
	if f.Namespace != "" {
		q.Set("namespace", f.Namespace)
	}
	if f.Conversation != "" {
		q.Set("conversation", f.Conversation)
	}
	if f.Since != nil {
		q.Set("since", f.Since.UTC().Format(time.RFC3339))
	}
	if f.Limit != 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	var out core.FeedbackPage
	err := c.do(ctx, http.MethodGet, "/v1/feedback", q, nil, &out)
	return out, err
}

// GetFeedback returns one feedback note. Server admins only.
func (c *Client) GetFeedback(ctx context.Context, id int64) (core.Feedback, error) {
	var out core.Feedback
	err := c.do(ctx, http.MethodGet, "/v1/feedback/"+strconv.FormatInt(id, 10), nil, nil, &out)
	return out, err
}

// ListNamespaces returns the namespaces the caller can read.
func (c *Client) ListNamespaces(ctx context.Context) ([]core.Namespace, error) {
	var out struct {
		Namespaces []core.Namespace `json:"namespaces"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/namespaces", nil, nil, &out)
	return out.Namespaces, err
}

// ListConversations returns a page of a namespace's conversations, most
// recently active first. Pass the page's Next as lp.Before for the next one.
func (c *Client) ListConversations(ctx context.Context, ns string, lp core.ListParams) (core.ListPage, error) {
	q := url.Values{}
	if lp.Status != "" {
		q.Set("status", string(lp.Status))
	}
	if lp.IncludeArchived {
		q.Set("include_archived", "true")
	}
	if lp.Limit != 0 {
		q.Set("limit", strconv.Itoa(lp.Limit))
	}
	if lp.Before != "" {
		q.Set("before", lp.Before)
	}
	var out core.ListPage
	err := c.do(ctx, http.MethodGet, "/v1/namespaces/"+url.PathEscape(ns)+"/conversations", q, nil, &out)
	return out, err
}

// Search returns a page of the messages in namespace ns that match
// sp.Query.
func (c *Client) Search(ctx context.Context, ns string, sp core.SearchParams) (core.SearchPage, error) {
	q := url.Values{"q": {sp.Query}}
	for k, v := range map[string]string{"conversation": sp.Conversation, "author": sp.Author, "order": sp.Order, "cursor": sp.Cursor} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if sp.Since != nil {
		q.Set("since", sp.Since.Format(time.RFC3339Nano))
	}
	if sp.Until != nil {
		q.Set("until", sp.Until.Format(time.RFC3339Nano))
	}
	if sp.Limit != 0 {
		q.Set("limit", strconv.Itoa(sp.Limit))
	}
	var out core.SearchPage
	err := c.do(ctx, http.MethodGet, "/v1/namespaces/"+url.PathEscape(ns)+"/search", q, nil, &out)
	return out, err
}

// GetConversation returns one conversation with its participants.
func (c *Client) GetConversation(ctx context.Context, ns, conv string) (core.Conversation, error) {
	var out core.Conversation
	err := c.do(ctx, http.MethodGet, convPath(ns, conv), nil, nil, &out)
	return out, err
}

// FollowResult is a batch of new messages and the conversation after them.
type FollowResult struct {
	core.ReadResult
	Conversation core.Conversation `json:"conversation"`
}

// Follow blocks on the server until the log moves past after or timeout
// passes. A timeout returns no messages and the current conversation.
func (c *Client) Follow(ctx context.Context, ns, conv string, after int64, timeout time.Duration) (FollowResult, error) {
	q := url.Values{"after": {strconv.FormatInt(after, 10)}, "timeout": {strconv.Itoa(int(timeout / time.Second))}}
	var out FollowResult
	err := c.do(ctx, http.MethodGet, convPath(ns, conv)+"/follow", q, nil, &out)
	return out, err
}

// Observe posts an observer comment as name.
func (c *Client) Observe(ctx context.Context, ns, conv, name, body string) (core.Message, error) {
	var out core.Message
	err := c.do(ctx, http.MethodPost, convPath(ns, conv)+"/observe", nil, map[string]string{"name": name, "body": body}, &out)
	return out, err
}

// IssuedToken is a new token; Secret is shown only once.
type IssuedToken struct {
	core.Token
	Secret string `json:"secret"`
}

// ListAccounts lists every account (server admins only).
func (c *Client) ListAccounts(ctx context.Context) ([]core.Account, error) {
	var out struct {
		Accounts []core.Account `json:"accounts"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/accounts", nil, nil, &out)
	return out.Accounts, err
}

// CreateAccount creates an account (server admins only).
func (c *Client) CreateAccount(ctx context.Context, name string, serverAdmin bool) (core.Account, error) {
	var out core.Account
	err := c.do(ctx, http.MethodPost, "/v1/accounts", nil, map[string]any{"name": name, "server_admin": serverAdmin}, &out)
	return out, err
}

// UpdateAccount disables, enables, promotes or demotes an account; nil
// fields stay as they are.
func (c *Client) UpdateAccount(ctx context.Context, name string, disabled, serverAdmin *bool) (core.Account, error) {
	body := map[string]any{}
	if disabled != nil {
		body["disabled"] = *disabled
	}
	if serverAdmin != nil {
		body["server_admin"] = *serverAdmin
	}
	var out core.Account
	err := c.do(ctx, http.MethodPatch, "/v1/accounts/"+url.PathEscape(name), nil, body, &out)
	return out, err
}

// ListTokens lists an account's tokens, without secrets.
func (c *Client) ListTokens(ctx context.Context, account string) ([]core.Token, error) {
	var out struct {
		Tokens []core.Token `json:"tokens"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/accounts/"+url.PathEscape(account)+"/tokens", nil, nil, &out)
	return out.Tokens, err
}

// IssueToken issues a token for an account; ttl 0 never expires.
func (c *Client) IssueToken(ctx context.Context, account, label string, ttl time.Duration) (IssuedToken, error) {
	var out IssuedToken
	err := c.do(ctx, http.MethodPost, "/v1/accounts/"+url.PathEscape(account)+"/tokens", nil,
		map[string]any{"label": label, "ttl_seconds": int64(ttl / time.Second)}, &out)
	return out, err
}

// RevokeToken revokes token id of account and returns it, or none if it
// was already revoked. force allows revoking the token this client uses.
func (c *Client) RevokeToken(ctx context.Context, account string, id int64, force bool) ([]core.Token, error) {
	var out struct {
		Tokens []core.Token `json:"tokens"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/accounts/"+url.PathEscape(account)+"/tokens/"+strconv.FormatInt(id, 10)+"/revoke",
		nil, map[string]any{"force": force}, &out)
	return out.Tokens, err
}

// RevokeAllTokens revokes every token of an account and returns the ones
// it revoked. force allows including the token this client uses.
func (c *Client) RevokeAllTokens(ctx context.Context, account string, force bool) ([]core.Token, error) {
	var out struct {
		Tokens []core.Token `json:"tokens"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/accounts/"+url.PathEscape(account)+"/tokens/revoke", nil, map[string]any{"force": force}, &out)
	return out.Tokens, err
}

// ListMembers lists a namespace's members.
func (c *Client) ListMembers(ctx context.Context, ns string) ([]core.Membership, error) {
	var out struct {
		Members []core.Membership `json:"members"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/namespaces/"+url.PathEscape(ns)+"/members", nil, nil, &out)
	return out.Members, err
}

// SetMember gives an account a role in a namespace.
func (c *Client) SetMember(ctx context.Context, ns, account string, role core.Role) (core.Membership, error) {
	var out core.Membership
	err := c.do(ctx, http.MethodPut, "/v1/namespaces/"+url.PathEscape(ns)+"/members/"+url.PathEscape(account), nil,
		map[string]any{"role": role}, &out)
	return out, err
}

// RemoveMember removes an account's role in a namespace.
func (c *Client) RemoveMember(ctx context.Context, ns, account string) error {
	return c.do(ctx, http.MethodDelete, "/v1/namespaces/"+url.PathEscape(ns)+"/members/"+url.PathEscape(account), nil, nil, nil)
}

// CreateNamespace creates a namespace (server admins only in token mode).
func (c *Client) CreateNamespace(ctx context.Context, name string) (core.Namespace, error) {
	var out core.Namespace
	err := c.do(ctx, http.MethodPost, "/v1/namespaces", nil, map[string]any{"name": name}, &out)
	return out, err
}

// stream sends a GET and returns the response for the caller to read and
// close, or the server's error.
func (c *Client) stream(ctx context.Context, path string, query url.Values) (*http.Response, error) {
	u := *c.base
	u.Path += path
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var eb struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &eb) != nil || eb.Error.Code == "" {
			return nil, &Error{Status: resp.StatusCode, Code: "http_" + strconv.Itoa(resp.StatusCode), Message: http.StatusText(resp.StatusCode)}
		}
		return nil, &Error{Status: resp.StatusCode, Code: eb.Error.Code, Message: eb.Error.Message}
	}
	return resp, nil
}

// Backup writes a copy of the server's database to w and checks that all
// of it arrived.
func (c *Client) Backup(ctx context.Context, w io.Writer) error {
	resp, err := c.stream(ctx, "/v1/backup", nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	n, err := io.Copy(w, resp.Body)
	if err != nil {
		return err
	}
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		return fmt.Errorf("client: backup cut off at %d of %d bytes", n, resp.ContentLength)
	}
	return nil
}

// Dump writes a dump of namespace ns (or, with ns empty, of everything) to
// w.
func (c *Client) Dump(ctx context.Context, ns string, w io.Writer) error {
	q := url.Values{}
	if ns != "" {
		q.Set("namespace", ns)
	}
	resp, err := c.stream(ctx, "/v1/dump", q)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return copyDump(w, resp.Body)
}

// copyDump copies a dump line by line, checking that it starts with a dump
// header and ends with an end record that counts the rows before it: the
// server cannot report a failure once it has started sending.
func copyDump(w io.Writer, body io.Reader) error {
	r := bufio.NewReaderSize(body, 1<<20)
	rows, ended := 0, false
	for first := true; ; first = false {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var rec struct {
				Format string `json:"format"`
				Table  string `json:"table"`
				End    bool   `json:"end"`
				Rows   int    `json:"rows"`
			}
			if jerr := json.Unmarshal(line, &rec); jerr != nil {
				return fmt.Errorf("client: the dump has a malformed line: %w", jerr)
			}
			switch {
			case first && rec.Format != core.DumpFormat:
				return errors.New("client: the server did not send a smoot dump")
			case ended:
				return errors.New("client: the dump continues after its end record")
			case rec.End:
				if rec.Rows != rows {
					return fmt.Errorf("client: the dump ends claiming %d rows after %d", rec.Rows, rows)
				}
				ended = true
			case rec.Table != "":
				rows++
			}
			if _, werr := w.Write(line); werr != nil {
				return werr
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	if !ended {
		return errors.New("client: the dump was cut off (no end record); the server may have failed")
	}
	return nil
}
