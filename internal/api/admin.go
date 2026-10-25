package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/aahoughton/smoot/internal/auth"
	"github.com/aahoughton/smoot/internal/core"
)

// Accounts, tokens and memberships. Every call is authorized in core: server
// admins manage accounts, an account manages its own tokens, and namespace
// admins manage their namespace's members. Each change is logged once, with
// the acting account, the action and its target; secrets never are.

func (h *handler) audit(r *http.Request, p core.Principal, action, target string, attrs ...any) {
	h.logger.InfoContext(r.Context(), "admin", append([]any{"actor", p.AccountID, "action", action, "target", target}, attrs...)...)
}

func (h *handler) whoami(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	id, err := h.svc.Whoami(r.Context(), p)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_mode": h.authMode, "version": h.version, "account": id.Account, "memberships": id.Memberships,
	})
	return nil
}

func (h *handler) listAccounts(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	as, err := h.svc.ListAccounts(r.Context(), p)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": as})
	return nil
}

func (h *handler) createAccount(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	var req struct {
		Name        string `json:"name"`
		ServerAdmin bool   `json:"server_admin"`
	}
	if err := h.decode(w, r, &req); err != nil {
		return err
	}
	a, err := h.svc.CreateAccount(r.Context(), p, req.Name, req.ServerAdmin)
	if err != nil {
		return err
	}
	h.audit(r, p, "create_account", a.Name, "server_admin", a.ServerAdmin)
	writeJSON(w, http.StatusCreated, a)
	return nil
}

func (h *handler) updateAccount(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	var req struct {
		Disabled    *bool `json:"disabled"`
		ServerAdmin *bool `json:"server_admin"`
	}
	if err := h.decode(w, r, &req); err != nil {
		return err
	}
	a, err := h.svc.UpdateAccount(r.Context(), p, r.PathValue("account"),
		core.AccountUpdate{Disabled: req.Disabled, ServerAdmin: req.ServerAdmin})
	if err != nil {
		return err
	}
	h.audit(r, p, "update_account", a.Name, "disabled", a.Disabled, "server_admin", a.ServerAdmin)
	writeJSON(w, http.StatusOK, a)
	return nil
}

func (h *handler) listTokens(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	ts, err := h.svc.ListTokens(r.Context(), p, r.PathValue("account"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": ts})
	return nil
}

func (h *handler) issueToken(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	var req struct {
		Label      string `json:"label"`
		TTLSeconds int64  `json:"ttl_seconds"`
	}
	if err := h.decode(w, r, &req); err != nil {
		return err
	}
	if req.TTLSeconds < 0 || req.TTLSeconds > maxTokenTTLSeconds {
		return invalid("ttl_seconds must be between 0 (never expires) and %d", maxTokenTTLSeconds)
	}
	t, err := h.svc.IssueToken(r.Context(), p, r.PathValue("account"), req.Label, time.Duration(req.TTLSeconds)*time.Second)
	if err != nil {
		return err
	}
	h.audit(r, p, "issue_token", t.Account, "token", t.ID)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, t)
	return nil
}

// maxTokenTTLSeconds is the longest token lifetime, 100 years.
const maxTokenTTLSeconds = 100 * 366 * 24 * 3600

// revokeRequest is the body of both revoke calls. Force allows revoking
// the token the request itself uses.
type revokeRequest struct {
	Force bool `json:"force"`
}

func (h *handler) revokeAllTokens(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	var req revokeRequest
	if err := h.decode(w, r, &req); err != nil {
		return err
	}
	ts, err := h.svc.RevokeAllTokens(r.Context(), p, r.PathValue("account"), req.Force)
	if err != nil {
		return err
	}
	h.audit(r, p, "revoke_all_tokens", r.PathValue("account"), "revoked", len(ts))
	writeJSON(w, http.StatusOK, map[string]any{"tokens": ts})
	return nil
}

func (h *handler) revokeToken(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return invalid("token id must be a positive integer")
	}
	var req revokeRequest
	if err := h.decode(w, r, &req); err != nil {
		return err
	}
	ts, err := h.svc.RevokeToken(r.Context(), p, r.PathValue("account"), id, req.Force)
	if err != nil {
		return err
	}
	h.audit(r, p, "revoke_token", r.PathValue("account"), "token", id, "revoked", len(ts))
	writeJSON(w, http.StatusOK, map[string]any{"tokens": ts})
	return nil
}

func (h *handler) listMembers(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	ms, err := h.svc.ListMemberships(r.Context(), p, r.PathValue("ns"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": ms})
	return nil
}

func (h *handler) setMember(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	var req struct {
		Role core.Role `json:"role"`
	}
	if err := h.decode(w, r, &req); err != nil {
		return err
	}
	m, err := h.svc.SetMembership(r.Context(), p, r.PathValue("ns"), r.PathValue("account"), req.Role)
	if err != nil {
		return err
	}
	h.audit(r, p, "set_member", m.Account, "namespace", m.Namespace, "role", m.Role)
	writeJSON(w, http.StatusOK, m)
	return nil
}

func (h *handler) removeMember(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	if err := h.svc.RemoveMembership(r.Context(), p, r.PathValue("ns"), r.PathValue("account")); err != nil {
		return err
	}
	h.audit(r, p, "remove_member", r.PathValue("account"), "namespace", r.PathValue("ns"))
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// deadlined returns a writer to w whose every write must finish within
// h.streamWrite, and a function that lifts the deadline once streaming is
// done, so it cannot cut off a later response on the same connection. A
// writer that cannot take a deadline (a wrapper without Unwrap) fails the
// write rather than streaming without one.
func (h *handler) deadlined(w http.ResponseWriter) (io.Writer, func()) {
	rc := http.NewResponseController(w)
	return writerFunc(func(b []byte) (int, error) {
		if err := rc.SetWriteDeadline(time.Now().Add(h.streamWrite)); err != nil {
			return 0, err
		}
		return w.Write(b)
	}), func() { _ = rc.SetWriteDeadline(time.Time{}) }
}

// backup streams a copy of the database (server admins).
func (h *handler) backup(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	out, lift := h.deadlined(w)
	defer lift()
	err := h.svc.Backup(r.Context(), p, func(size int64, body io.Reader) error {
		w.Header().Set("Content-Type", "application/vnd.sqlite3")
		w.Header().Set("Content-Disposition", `attachment; filename="smoot.db"`)
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.Header().Set("Cache-Control", "no-store")
		_, err := io.Copy(out, body)
		return err
	})
	if err == nil {
		h.audit(r, p, "backup", "database")
	}
	return err
}

// dump streams a dump: of one namespace with ?namespace=, else of everything.
func (h *handler) dump(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	ns := r.URL.Query().Get("namespace")
	started := false
	out, lift := h.deadlined(w)
	defer lift()
	sw := writerFunc(func(b []byte) (int, error) {
		if !started {
			started = true
			w.Header().Set("Content-Type", "application/jsonl")
			w.Header().Set("Cache-Control", "no-store")
		}
		return out.Write(b)
	})
	err := h.svc.Dump(r.Context(), p, ns, sw)
	switch {
	case err == nil:
		target := ns
		if target == "" {
			target = "all"
		}
		h.audit(r, p, "dump", target)
		return nil
	case started:
		// The status is sent; the missing end record tells the reader.
		h.logger.Error("dump cut off", "err", err)
		return nil
	default:
		return err
	}
}

// loadReadTimeout replaces the usual body deadline for a dump, which may
// be far larger than other requests.
const loadReadTimeout = 10 * time.Minute

func (h *handler) load(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	// Only a server admin gets the longer deadline; anyone else is refused
	// within the usual one.
	if err := h.svc.RequireServerAdmin(r.Context(), p); err != nil {
		return err
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(loadReadTimeout))
	body := &capped{r: http.MaxBytesReader(w, r.Body, h.maxLoadBytes)}
	res, err := h.svc.LoadNamespace(r.Context(), p, body, r.URL.Query().Get("as"))
	if err != nil {
		if body.over {
			return &core.Error{Code: core.CodeMessageTooLarge, Message: fmt.Sprintf("the dump is over %d bytes", h.maxLoadBytes)}
		}
		return err
	}
	h.audit(r, p, "load", res.Namespace, "conversations", res.Conversations, "messages", res.Messages)
	writeJSON(w, http.StatusCreated, res)
	return nil
}

// capped notes when a MaxBytesReader refuses to read further, since the
// error that reaches the handler may no longer say so.
type capped struct {
	r    io.Reader
	over bool
}

func (c *capped) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		c.over = true
	}
	return n, err
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

// StatusWindow is how far back GET /v1/server lists quiet open
// conversations.
const StatusWindow = 24 * time.Hour

type callerJSON struct {
	Account string `json:"account"`
	auth.Caller
}

type mcpSessionsJSON struct {
	Account  string    `json:"account"`
	Sessions int       `json:"sessions"`
	LastUsed time.Time `json:"last_used"`
}

// serverStatus reports who is using the server, for an operator deciding
// whether to restart it. Server admins only. Callers, failures and MCP
// sessions are kept in memory since the server started.
func (h *handler) serverStatus(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	now := h.now().UTC()
	act, err := h.svc.ServerActivity(r.Context(), p, now.Add(-StatusWindow))
	if err != nil {
		return err
	}
	accounts, err := h.svc.ListAccounts(r.Context(), p)
	if err != nil {
		return err
	}
	names := make(map[int64]string, len(accounts))
	for _, a := range accounts {
		names[a.ID] = a.Name
	}
	callers, failures := h.activity.Snapshot()
	cs := make([]callerJSON, 0, len(callers))
	for _, c := range callers {
		cs = append(cs, callerJSON{Account: names[c.AccountID], Caller: c})
	}
	ms := []mcpSessionsJSON{}
	if h.mcpSessions != nil {
		for _, u := range h.mcpSessions() {
			ms = append(ms, mcpSessionsJSON{Account: names[u.AccountID], Sessions: u.Sessions, LastUsed: u.LastUsed.UTC()})
		}
	}
	if failures == nil {
		failures = []auth.Failure{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version": h.version, "auth_mode": h.authMode, "started_at": h.activity.Started(), "now": now,
		"last_message": act.LastMessage, "conversations": act.Conversations,
		"mcp_sessions": ms, "callers": cs, "failed_sign_ins": failures,
	})
	return nil
}
