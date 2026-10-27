// Package api serves the versioned JSON API under /v1. It is a thin layer:
// decode, authenticate, call core, encode.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/aahoughton/smoot/internal/auth"
	"github.com/aahoughton/smoot/internal/core"
	"github.com/aahoughton/smoot/internal/mcpserver"
)

// Wait timeout bounds, in seconds.
const (
	DefaultWaitSeconds = 30
	MaxWaitSeconds     = 300
)

// maxRequestBytes bounds request bodies. Message bodies are limited further
// by core.
const maxRequestBytes = 1 << 20

// DefaultBodyReadTimeout bounds how long a client may take to send a request
// body. It applies only while a body is read, so long-poll waits (which have
// no body) are unaffected.
const DefaultBodyReadTimeout = 30 * time.Second

// Options configure the API. Zero values select the defaults.
type Options struct {
	Logger          *slog.Logger
	BodyReadTimeout time.Duration
	// AuthMode and Version are reported by GET /v1/whoami.
	AuthMode string
	Version  string
	// Activity, if set, records each authenticated caller; GET /v1/server
	// reports it.
	Activity *auth.Activity
	// MCPSessions, if set, reports open MCP sessions for GET /v1/server.
	MCPSessions func() []mcpserver.SessionUse
	// Now defaults to time.Now.
	Now func() time.Time
	// MaxLoadBytes bounds a dump sent to POST /v1/load; default
	// DefaultMaxLoadBytes.
	MaxLoadBytes int64
	// StreamWriteTimeout bounds each write of a dump or backup to the
	// client; default DefaultStreamWriteTimeout.
	StreamWriteTimeout time.Duration
}

// DefaultMaxLoadBytes bounds a dump sent to POST /v1/load.
const DefaultMaxLoadBytes = 256 << 20

// DefaultStreamWriteTimeout bounds each write of a dump or backup. A dump
// holds a read transaction while it streams, so a client that stops
// reading would otherwise keep SQLite from checkpointing its log.
const DefaultStreamWriteTimeout = 30 * time.Second

type handler struct {
	svc          *core.Service
	authn        auth.Authenticator
	logger       *slog.Logger
	bodyTimeout  time.Duration
	authMode     string
	version      string
	activity     *auth.Activity
	mcpSessions  func() []mcpserver.SessionUse
	now          func() time.Time
	streamWrite  time.Duration
	maxLoadBytes int64
}

type route struct {
	method  string
	pattern string
	fn      func(*handler, http.ResponseWriter, *http.Request, core.Principal) error
}

const convPath = "/v1/namespaces/{ns}/conversations/{conv}"

// routes is the full API surface. The OpenAPI test checks it against
// openapi.yaml.
var routes = []route{
	{"GET", "/v1/namespaces", (*handler).listNamespaces},
	{"POST", "/v1/namespaces", (*handler).createNamespace},
	{"GET", "/v1/namespaces/{ns}/conversations", (*handler).listConversations},
	{"POST", "/v1/namespaces/{ns}/conversations", (*handler).start},
	{"GET", "/v1/namespaces/{ns}/status", (*handler).status},
	{"GET", "/v1/namespaces/{ns}/search", (*handler).search},
	{"GET", convPath, (*handler).getConversation},
	{"GET", convPath + "/messages", (*handler).read},
	{"POST", convPath + "/messages", (*handler).say},
	{"POST", convPath + "/read", (*handler).readAsAgent},
	{"POST", convPath + "/wait", (*handler).wait},
	{"GET", convPath + "/follow", (*handler).follow},
	{"POST", convPath + "/join", (*handler).join},
	{"POST", convPath + "/leave", (*handler).leave},
	{"POST", convPath + "/conclude", (*handler).conclude},
	{"POST", convPath + "/close", (*handler).closeConv},
	{"POST", convPath + "/presence", (*handler).presence},
	{"POST", convPath + "/observe", (*handler).observe},
	{"GET", "/v1/whoami", (*handler).whoami},
	{"GET", "/v1/accounts", (*handler).listAccounts},
	{"POST", "/v1/accounts", (*handler).createAccount},
	{"PATCH", "/v1/accounts/{account}", (*handler).updateAccount},
	{"GET", "/v1/accounts/{account}/tokens", (*handler).listTokens},
	{"POST", "/v1/accounts/{account}/tokens", (*handler).issueToken},
	{"POST", "/v1/accounts/{account}/tokens/revoke", (*handler).revokeAllTokens},
	{"POST", "/v1/accounts/{account}/tokens/{id}/revoke", (*handler).revokeToken},
	{"GET", "/v1/namespaces/{ns}/members", (*handler).listMembers},
	{"PUT", "/v1/namespaces/{ns}/members/{account}", (*handler).setMember},
	{"DELETE", "/v1/namespaces/{ns}/members/{account}", (*handler).removeMember},
	{"GET", "/v1/server", (*handler).serverStatus},
	{"GET", "/v1/feedback", (*handler).listFeedback},
	{"GET", "/v1/feedback/{id}", (*handler).getFeedback},
	{"GET", "/v1/backup", (*handler).backup},
	{"GET", "/v1/dump", (*handler).dump},
	{"POST", "/v1/load", (*handler).load},
}

// Register adds the API routes and GET /v1/openapi.yaml to mux.
func Register(mux *http.ServeMux, svc *core.Service, authn auth.Authenticator, opts Options) {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.BodyReadTimeout == 0 {
		opts.BodyReadTimeout = DefaultBodyReadTimeout
	}
	h := &handler{
		svc: svc, authn: authn, logger: opts.Logger, bodyTimeout: opts.BodyReadTimeout,
		authMode: opts.AuthMode, version: opts.Version, activity: opts.Activity,
		mcpSessions: opts.MCPSessions, now: opts.Now, maxLoadBytes: opts.MaxLoadBytes,
		streamWrite: opts.StreamWriteTimeout,
	}
	if h.streamWrite == 0 {
		h.streamWrite = DefaultStreamWriteTimeout
	}
	if h.maxLoadBytes == 0 {
		h.maxLoadBytes = DefaultMaxLoadBytes
	}
	if h.now == nil {
		h.now = time.Now
	}
	all := routes
	// Tests of other routes may pass no service.
	if svc != nil && svc.AttachmentsEnabled() {
		all = append(slices.Clip(routes), attachmentRoutes...)
	}
	for _, rt := range all {
		mux.HandleFunc(rt.method+" "+rt.pattern, func(w http.ResponseWriter, r *http.Request) {
			p, err := h.authn.Authenticate(r)
			if err == nil {
				h.activity.Seen(r, p, auth.SurfaceAPI)
				err = rt.fn(h, w, r, p)
			}
			if err != nil {
				h.writeError(w, r, err)
			}
		})
	}
	mux.HandleFunc("GET /v1/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(openapiSpec)
	})
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		h.writeError(w, r, &core.Error{Code: "not_found", Message: "no such endpoint; see /v1/openapi.yaml"})
	})
}

// errorBody is the JSON shape of every error response.
type errorBody struct {
	Error struct {
		Code    core.Code `json:"code"`
		Message string    `json:"message"`
		LastSeq *int64    `json:"last_seq,omitempty"`
	} `json:"error"`
}

// StatusFor is the HTTP status for an error code, shared by every HTTP
// surface.
func StatusFor(code core.Code) int {
	switch code {
	case core.CodeInvalid:
		return http.StatusBadRequest
	case core.CodeUnauthorized:
		return http.StatusUnauthorized
	case core.CodeForbidden, core.CodeNotCoordinator:
		return http.StatusForbidden
	case core.CodeNamespaceNotFound, core.CodeConversationNotFound, core.CodeAccountNotFound, core.CodeTokenNotFound, core.CodeFeedbackNotFound,
		core.CodeAttachmentNotFound, "not_found":
		return http.StatusNotFound
	case core.CodeAttachmentGone:
		return http.StatusGone
	case core.CodeAttachmentQuota, core.CodeAttachmentCap:
		return http.StatusInsufficientStorage
	case core.CodeNamespaceExists, core.CodeConversationExists, core.CodeConversationConcluded,
		core.CodeNotParticipant, core.CodeNameActive, core.CodeStaleContext, core.CodeAccountExists, core.CodeLastAdmin, core.CodeOwnCredential,
		core.CodeIdempotencyConflict:
		return http.StatusConflict
	case core.CodeMessageTooLarge, core.CodeAttachmentTooLarge:
		return http.StatusRequestEntityTooLarge
	case core.CodeTooManyWaiters, core.CodeTooManyWatchers, core.CodeRateLimited:
		return http.StatusTooManyRequests
	case codeRequestTimeout:
		return http.StatusRequestTimeout
	case core.CodeBusy:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func (h *handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var body errorBody
	if errors.Is(err, context.Canceled) {
		body.Error.Code = "unavailable"
		body.Error.Message = "request canceled; the server may be shutting down, retry"
		writeJSON(w, http.StatusServiceUnavailable, body)
		return
	}
	ce, ok := errors.AsType[*core.Error](err)
	if !ok {
		h.logger.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		body.Error.Code = "internal"
		body.Error.Message = "internal error"
		writeJSON(w, http.StatusInternalServerError, body)
		return
	}
	body.Error.Code = ce.Code
	body.Error.Message = ce.Message
	if ce.Code == core.CodeUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="smoot"`)
	}
	if ce.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(ce.RetryAfter.Seconds())))
	}
	if ce.Code == core.CodeStaleContext {
		body.Error.LastSeq = &ce.LastSeq
	}
	writeJSON(w, StatusFor(ce.Code), body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func invalid(format string, args ...any) error {
	return &core.Error{Code: core.CodeInvalid, Message: fmt.Sprintf(format, args...)}
}

const codeRequestTimeout core.Code = "request_timeout"

// decode reads exactly one JSON object from the request body into v. It
// rejects unknown fields, trailing data, bodies over maxRequestBytes, and
// bodies not received within the body read timeout.
//
// On failure the connection is marked for closing: otherwise the server
// would try to drain the rest of an unread body before replying, which for a
// stalled client never finishes.
func (h *handler) decode(w http.ResponseWriter, r *http.Request, v any) error {
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(h.bodyTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	err := h.decodeBody(w, r, v)
	if err != nil {
		w.Header().Set("Connection", "close")
		return err
	}
	_ = rc.SetReadDeadline(time.Time{})
	return nil
}

func (h *handler) decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return invalid("request body is empty; send a JSON object")
		}
		return bodyError(err)
	}
	switch err := dec.Decode(&struct{}{}); {
	case errors.Is(err, io.EOF):
		return nil
	case err == nil:
		return invalid("request body has data after the JSON object")
	default:
		if be := bodyError(err); core.CodeOf(be) != core.CodeInvalid {
			return be
		}
		return invalid("request body has data after the JSON object")
	}
}

func bodyError(err error) error {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return &core.Error{Code: core.CodeMessageTooLarge, Message: fmt.Sprintf("request body is over %d bytes", maxRequestBytes)}
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return &core.Error{Code: codeRequestTimeout, Message: "request body was not received in time"}
	}
	return invalid("invalid JSON body: %v", err)
}

func queryInt(r *http.Request, name string) (*int64, error) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return nil, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil, invalid("query parameter %s must be an integer", name)
	}
	return &v, nil
}

// queryCount parses a count (a limit or tail) as an int, so a value too
// large for int is refused rather than truncated.
func queryCount(r *http.Request, name string) (*int, error) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return nil, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return nil, invalid("query parameter %s must be an integer", name)
	}
	return &v, nil
}

func queryBool(r *http.Request, name string) (bool, error) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return false, nil
	}
	v, err := strconv.ParseBool(s)
	if err != nil {
		return false, invalid("query parameter %s must be true or false", name)
	}
	return v, nil
}

func (h *handler) listNamespaces(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	ns, err := h.svc.ListNamespaces(r.Context(), p)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"namespaces": ns})
	return nil
}

func (h *handler) createNamespace(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	var req struct {
		Name string `json:"name"`
	}
	if err := h.decode(w, r, &req); err != nil {
		return err
	}
	ns, err := h.svc.CreateNamespace(r.Context(), p, req.Name)
	if err != nil {
		return err
	}
	h.audit(r, p, "create_namespace", ns.Name)
	writeJSON(w, http.StatusCreated, ns)
	return nil
}

func (h *handler) listConversations(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	archived, err := queryBool(r, "include_archived")
	if err != nil {
		return err
	}
	lp := core.ListParams{IncludeArchived: archived, Before: r.URL.Query().Get("before")}
	switch status := r.URL.Query().Get("status"); status {
	case "", "all":
	default:
		lp.Status = core.Status(status)
	}
	limit, err := queryCount(r, "limit")
	if err != nil {
		return err
	}
	if limit != nil {
		if *limit < 1 || *limit > core.MaxListLimit {
			return invalid("limit must be between 1 and %d", core.MaxListLimit)
		}
		lp.Limit = *limit
	}
	page, err := h.svc.ListConversations(r.Context(), p, r.PathValue("ns"), lp)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, page)
	return nil
}

func (h *handler) start(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	var req struct {
		Name   string            `json:"name"`
		Topic  string            `json:"topic"`
		End    core.EndCondition `json:"end"`
		EndsAt *time.Time        `json:"ends_at"`
		Agent  string            `json:"agent"`
		Join   *bool             `json:"join"`
	}
	if err := h.decode(w, r, &req); err != nil {
		return err
	}
	join := req.Join == nil || *req.Join
	c, err := h.svc.Start(r.Context(), p, r.PathValue("ns"), core.StartParams{
		Name: req.Name, Topic: req.Topic, End: req.End, EndsAt: req.EndsAt, Agent: req.Agent, Join: join,
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, c)
	return nil
}

func (h *handler) status(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	sts, err := h.svc.Status(r.Context(), p, r.PathValue("ns"), r.URL.Query().Get("agent"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"standings": sts})
	return nil
}

func (h *handler) getConversation(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	c, err := h.svc.GetConversation(r.Context(), p, r.PathValue("ns"), r.PathValue("conv"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, c)
	return nil
}

// read is a plain read of the log: it changes nothing. Reading as an agent,
// which moves its read position, is POST .../read.
func (h *handler) read(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	if r.URL.Query().Has("agent") {
		return invalid("to read as an agent (which marks messages read), POST to .../read")
	}
	after, err := queryInt(r, "after")
	if err != nil {
		return err
	}
	limit, err := queryCount(r, "limit")
	if err != nil {
		return err
	}
	tail, err := queryCount(r, "tail")
	if err != nil {
		return err
	}
	rp := core.ReadParams{After: after}
	if tail != nil {
		rp.Tail = *tail
	}
	if limit != nil {
		rp.Limit = *limit
	}
	return h.readWith(w, r, p, rp, limit != nil, tail != nil)
}

// readAsAgent reads as an agent and moves its read position.
func (h *handler) readAsAgent(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	var req struct {
		Agent string `json:"agent"`
		After *int64 `json:"after"`
		Limit *int   `json:"limit"`
		Tail  *int   `json:"tail"`
	}
	if err := h.decode(w, r, &req); err != nil {
		return err
	}
	if req.Agent == "" {
		return invalid("agent is required; to read without marking anything read, GET .../messages")
	}
	rp := core.ReadParams{Agent: req.Agent, After: req.After}
	if req.Tail != nil {
		rp.Tail = *req.Tail
	}
	if req.Limit != nil {
		rp.Limit = *req.Limit
	}
	return h.readWith(w, r, p, rp, req.Limit != nil, req.Tail != nil)
}

func (h *handler) readWith(w http.ResponseWriter, r *http.Request, p core.Principal, rp core.ReadParams, limitSet, tailSet bool) error {
	if tailSet && rp.Tail < 1 {
		return invalid("tail must be between 1 and %d", core.MaxReadLimit)
	}
	if limitSet && rp.Limit == 0 {
		return invalid("limit must be between 1 and %d", core.MaxReadLimit)
	}
	res, err := h.svc.Read(r.Context(), p, r.PathValue("ns"), r.PathValue("conv"), rp)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, res)
	return nil
}

func (h *handler) say(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	var req struct {
		Agent           string `json:"agent"`
		Body            string `json:"body"`
		ReplyTo         *int64 `json:"reply_to"`
		ExpectedLastSeq *int64 `json:"expected_last_seq"`
		IdempotencyKey  string `json:"idempotency_key"`
	}
	if err := h.decode(w, r, &req); err != nil {
		return err
	}
	res, err := h.svc.Say(r.Context(), p, r.PathValue("ns"), r.PathValue("conv"), core.SayParams{
		Agent: req.Agent, Body: req.Body, ReplyTo: req.ReplyTo, ExpectedLastSeq: req.ExpectedLastSeq,
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, res)
	return nil
}

func (h *handler) wait(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	var req struct {
		Agent          string `json:"agent"`
		After          *int64 `json:"after"`
		TimeoutSeconds *int64 `json:"timeout_seconds"`
		InlineBytes    *int64 `json:"inline_bytes"`
	}
	if err := h.decode(w, r, &req); err != nil {
		return err
	}
	secs := int64(DefaultWaitSeconds)
	if req.TimeoutSeconds != nil {
		if *req.TimeoutSeconds < 0 || *req.TimeoutSeconds > MaxWaitSeconds {
			return invalid("timeout_seconds must be between 0 and %d", MaxWaitSeconds)
		}
		secs = *req.TimeoutSeconds
	}
	wp := core.WaitParams{Agent: req.Agent, After: req.After, Timeout: time.Duration(secs) * time.Second}
	if req.InlineBytes != nil {
		if *req.InlineBytes < 0 || *req.InlineBytes > maxRequestBytes {
			return invalid("inline_bytes must be between 0 and %d", maxRequestBytes)
		}
		wp.InlineBytes = int(*req.InlineBytes)
	}
	d, err := h.svc.Wait(r.Context(), p, r.PathValue("ns"), r.PathValue("conv"), wp)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, d)
	return nil
}

// FollowResult is the response of GET .../follow: new messages plus the
// conversation as it stands after them.
type FollowResult struct {
	core.ReadResult
	Conversation core.Conversation `json:"conversation"`
}

func (h *handler) follow(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	after, err := queryInt(r, "after")
	if err != nil {
		return err
	}
	timeout, err := queryInt(r, "timeout")
	if err != nil {
		return err
	}
	var from int64
	if after != nil {
		from = *after
	}
	secs := int64(DefaultWaitSeconds)
	if timeout != nil {
		if *timeout < 0 || *timeout > MaxWaitSeconds {
			return invalid("timeout must be between 0 and %d seconds", MaxWaitSeconds)
		}
		secs = *timeout
	}
	ns, name := r.PathValue("ns"), r.PathValue("conv")
	res, err := h.svc.Follow(r.Context(), p, ns, name, from, time.Duration(secs)*time.Second)
	if err != nil {
		return err
	}
	c, err := h.svc.GetConversation(r.Context(), p, ns, name)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, FollowResult{ReadResult: res, Conversation: c})
	return nil
}

type agentBody struct {
	Agent string `json:"agent"`
	Body  string `json:"body"`
}

func (h *handler) join(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	var req struct {
		Agent  string `json:"agent"`
		Resume bool   `json:"resume"`
	}
	if err := h.decode(w, r, &req); err != nil {
		return err
	}
	st, err := h.svc.Join(r.Context(), p, r.PathValue("ns"), r.PathValue("conv"), core.JoinParams{Agent: req.Agent, Resume: req.Resume})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, st)
	return nil
}

func (h *handler) leave(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	var req agentBody
	if err := h.decode(w, r, &req); err != nil {
		return err
	}
	st, err := h.svc.Leave(r.Context(), p, r.PathValue("ns"), r.PathValue("conv"), core.LeaveParams{Agent: req.Agent, Body: req.Body})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, st)
	return nil
}

func (h *handler) conclude(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	var req agentBody
	if err := h.decode(w, r, &req); err != nil {
		return err
	}
	st, err := h.svc.Conclude(r.Context(), p, r.PathValue("ns"), r.PathValue("conv"), core.ConcludeParams{Agent: req.Agent, Body: req.Body})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, st)
	return nil
}

func (h *handler) closeConv(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	var req agentBody
	if err := h.decode(w, r, &req); err != nil {
		return err
	}
	st, err := h.svc.Close(r.Context(), p, r.PathValue("ns"), r.PathValue("conv"), core.CloseParams{Agent: req.Agent, Body: req.Body})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, st)
	return nil
}

func (h *handler) presence(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	var req struct {
		Agent       string     `json:"agent"`
		Presence    string     `json:"presence"`
		Note        string     `json:"note"`
		CheckBackBy *time.Time `json:"check_back_by"`
	}
	if err := h.decode(w, r, &req); err != nil {
		return err
	}
	pt, err := h.svc.SetPresence(r.Context(), p, r.PathValue("ns"), r.PathValue("conv"), core.PresenceParams{
		Agent: req.Agent, Presence: req.Presence, Note: req.Note, CheckBackBy: req.CheckBackBy,
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, pt)
	return nil
}

func (h *handler) observe(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	var req struct {
		Name string `json:"name"`
		Body string `json:"body"`
	}
	if err := h.decode(w, r, &req); err != nil {
		return err
	}
	m, err := h.svc.Observe(r.Context(), p, r.PathValue("ns"), r.PathValue("conv"), core.ObserveParams{Name: req.Name, Body: req.Body})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, m)
	return nil
}
