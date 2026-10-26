// Package web serves the HTML viewer. It renders a theme's templates with
// data from core and contains no markup of its own; see themes/README.md for
// the files a theme provides and the data its templates receive.
package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"text/template/parse"
	"time"

	"github.com/aahoughton/smoot/internal/api"
	"github.com/aahoughton/smoot/internal/auth"
	"github.com/aahoughton/smoot/internal/core"
	"github.com/aahoughton/smoot/internal/markdown"
	"github.com/aahoughton/smoot/internal/tone"
	"github.com/aahoughton/smoot/themes"
)

// TemplateAPI is the version of the data contract between this package and
// theme templates. It changes when a field is removed or changes meaning.
const TemplateAPI = 1

// Page is the data every page template receives.
type Page struct {
	// API is TemplateAPI, for themes that want to check it.
	API     int
	Version string
	Title   string
	// Notice is a message to show above the content, such as an error.
	Notice string

	Namespace       string
	Namespaces      []core.Namespace
	Conversations   []core.Conversation
	IncludeArchived bool
	// OpenOnly is true when the conversation list leaves out concluded
	// conversations.
	OpenOnly bool
	// ArchivedCount is how many conversations Show archived would add to
	// the list; 0 when archived ones are already shown.
	ArchivedCount int
	// Hidden is true when the conversation list is empty but the namespace
	// has conversations that ?archived=1 or leaving out ?open=1 would show.
	Hidden bool
	// Next continues the conversation list (as ?before=); empty on the
	// last page.
	Next string

	Conversation *core.Conversation
	Messages     []Message
	// Truncated is true when older messages exist that are not shown.
	Truncated bool
	// Earlier is the seq to pass as ?before= for the messages just before
	// the ones shown; 0 when the first message is shown.
	Earlier int64
	// Before is the ?before= of a page of earlier messages, or 0 on the
	// live page. Earlier pages do not update live.
	Before int64
	// Observer is the name last used for an observer comment.
	Observer string
	// Draft is the text of a comment that was rejected, to show again.
	Draft string

	// Account is the signed-in account's name in auth mode token, which is
	// also the name observer comments are posted under; empty when auth is
	// off.
	Account string
	// ReturnTo is where the login page sends the browser after signing in.
	ReturnTo string

	// Query and Order are the search on /n/{namespace}/search; Search is
	// its results, nil before a query is given.
	Query  string
	Order  string
	Search *SearchResults
}

// pages are the page templates a theme must provide. Each defines
// "content"; layout.html defines "layout", message.html "message",
// participants.html "participants" and about.html "about".
var pages = []string{"namespaces", "conversations", "conversation", "search", "error", "login"}

var partials = []string{"layout", "message", "participants", "about"}

// Options configure the viewer.
type Options struct {
	// ThemeDir, if set, holds files that replace the default theme's files
	// of the same path.
	ThemeDir string
	Logger   *slog.Logger
	Version  string
	// ShownMessages is how many recent messages a conversation page shows.
	ShownMessages int
	// Now and Location set the clock and time zone pages show times in;
	// they default to the real clock and the local zone.
	Now      func() time.Time
	Location *time.Location
	// Login turns on the login page and session cookies (auth mode token);
	// the authenticator passed to Handler must then read the session
	// cookie (auth.Session).
	Login bool
	// SecureCookies marks the session cookie Secure, for a server reached
	// over HTTPS.
	SecureCookies bool
	// SessionTTL is how long a browser session lasts; default
	// core.DefaultSessionTTL.
	SessionTTL time.Duration
	// StreamHeartbeat is how often an event stream sends fresh state when
	// no message arrives; default DefaultStreamHeartbeat.
	StreamHeartbeat time.Duration
	// Failures, if set, limits failed logins per client address.
	Failures *auth.Failures
	// Activity, if set, records signed-in callers and failed logins.
	Activity *auth.Activity
}

type viewer struct {
	svc    *core.Service
	authn  auth.Authenticator
	opts   Options
	pages  map[string]*template.Template
	static http.Handler
}

const observerCookie = "smoot_observer"

// Handler returns the viewer. It fails if the theme is incomplete or its
// templates do not parse.
func Handler(svc *core.Service, authn auth.Authenticator, opts Options) (http.Handler, error) {
	if opts.StreamHeartbeat == 0 {
		opts.StreamHeartbeat = DefaultStreamHeartbeat
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.ShownMessages == 0 {
		opts.ShownMessages = 300
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Location == nil {
		opts.Location = time.Local
	}
	theme, err := themes.Open(opts.ThemeDir)
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}
	v := &viewer{svc: svc, authn: authn, opts: opts, pages: map[string]*template.Template{}}
	if err := v.load(theme); err != nil {
		return nil, err
	}
	static, err := fs.Sub(theme, "static")
	if err != nil {
		return nil, err
	}
	v.static = http.StripPrefix("/static/", http.FileServerFS(static))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", v.root)
	mux.HandleFunc("GET /n/{ns}", v.namespace)
	mux.HandleFunc("GET /n/{ns}/search", v.search)
	mux.HandleFunc("GET /n/{ns}/c/{conv}", v.conversation)
	mux.HandleFunc("GET /n/{ns}/c/{conv}/events", v.events)
	mux.HandleFunc("POST /n/{ns}/c/{conv}/observe", v.observe)
	if svc != nil && svc.AttachmentsEnabled() {
		mux.HandleFunc("GET /n/{ns}/c/{conv}/files/{seq}", v.download)
	}
	mux.Handle("GET /static/", v.static)
	if opts.Login {
		mux.HandleFunc("GET /login", v.loginPage)
		mux.HandleFunc("POST /login", v.login)
		mux.HandleFunc("POST /logout", v.logout)
	}
	// Pages and streams carry conversation content, possibly private to
	// the signed-in account: no cache may keep them. Static files may be
	// cached.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			noStore(w)
		}
		mux.ServeHTTP(w, r)
	}), nil
}

func (v *viewer) funcs() template.FuncMap {
	return template.FuncMap{
		"markdown": renderMarkdown,
		"ts":       func(t any) string { return timeOf(t).UTC().Format(time.RFC3339) },
		"clock":    func(t any) string { return timeOf(t).In(v.opts.Location).Format("15:04") },
		"ago":      func(t any) string { return ago(v.opts.Now().Sub(timeOf(t))) },
		"until":    func(t any) string { return until(timeOf(t).Sub(v.opts.Now())) },
		"gap":      func(from, to any) string { return gap(timeOf(to).Sub(timeOf(from))) },
		"tone":     toneClass,
		"initials": initials,
		"datetime": func(t any) string { return timeOf(t).In(v.opts.Location).Format("Jan 2 15:04") },
		"size":     core.FormatSize,
	}
}

func timeOf(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case *time.Time:
		if t != nil {
			return *t
		}
	}
	return time.Time{}
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m ago"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h ago"
	default:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d ago"
	}
}

// gap describes the time between two messages, such as "4s" or "6m".
func gap(d time.Duration) string {
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(max(d, 0)/time.Second)) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	default:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
}

// toneClass returns a stable color class for a name, so the same participant has
// the same color on every page and in the TUI.
func toneClass(name string) string {
	return "tone-" + strconv.Itoa(tone.Of(name))
}

// initials returns up to two letters for an avatar: the first letter of the
// first two words of a name split on '-', '_', '.' or space.
func initials(name string) string {
	words := strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' || r == '.' || r == ' ' })
	var b strings.Builder
	for _, w := range words {
		if b.Len() == 2 {
			break
		}
		r := []rune(w)
		b.WriteString(strings.ToUpper(string(r[0])))
	}
	if b.Len() == 0 {
		return "?"
	}
	return b.String()
}

// until describes a time still to come, such as "in 1h 58m".
func until(d time.Duration) string {
	switch {
	case d <= 0:
		return "now"
	case d < time.Minute:
		return "in under a minute"
	case d < time.Hour:
		return "in " + strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 48*time.Hour:
		return "in " + strconv.Itoa(int(d/time.Hour)) + "h " + strconv.Itoa(int(d%time.Hour/time.Minute)) + "m"
	default:
		return "in " + strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
}

func (v *viewer) load(theme fs.FS) error {
	read := func(name string) (string, error) {
		b, err := fs.ReadFile(theme, "templates/"+name+".html")
		if err != nil {
			return "", fmt.Errorf("web: theme is missing templates/%s.html: %w", name, err)
		}
		return string(b), nil
	}
	shared := template.New("").Funcs(v.funcs())
	for _, name := range partials {
		src, err := read(name)
		if err != nil {
			return err
		}
		// Files are named by path so that only a define can satisfy the
		// check below for a template of the same name.
		if _, err := shared.New("templates/" + name + ".html").Parse(src); err != nil {
			return fmt.Errorf("web: templates/%s.html: %w", name, err)
		}
	}
	for _, name := range pages {
		src, err := read(name)
		if err != nil {
			return err
		}
		t, err := shared.Clone()
		if err != nil {
			return err
		}
		if _, err := t.New("templates/" + name + ".html").Parse(src); err != nil {
			return fmt.Errorf("web: templates/%s.html: %w", name, err)
		}
		for _, def := range []string{"layout", "content", "message", "participants", "about"} {
			if t.Lookup(def) == nil {
				return fmt.Errorf("web: theme does not define %q (needed by %s)", def, name)
			}
		}
		if err := checkReferences(t); err != nil {
			return fmt.Errorf("web: %s page: %w", name, err)
		}
		v.pages[name] = t
	}
	return nil
}

// checkReferences fails if any template in set calls a template that is not
// defined, which html/template would otherwise report only when rendering.
func checkReferences(set *template.Template) error {
	var walk func(n parse.Node) error
	walk = func(n parse.Node) error {
		switch n := n.(type) {
		case *parse.ListNode:
			if n == nil {
				return nil
			}
			for _, c := range n.Nodes {
				if err := walk(c); err != nil {
					return err
				}
			}
		case *parse.IfNode:
			return walkBranch(walk, &n.BranchNode)
		case *parse.RangeNode:
			return walkBranch(walk, &n.BranchNode)
		case *parse.WithNode:
			return walkBranch(walk, &n.BranchNode)
		case *parse.TemplateNode:
			if set.Lookup(n.Name) == nil {
				return fmt.Errorf("template %q is used but not defined", n.Name)
			}
		}
		return nil
	}
	for _, t := range set.Templates() {
		if t.Tree == nil {
			continue
		}
		if err := walk(t.Tree.Root); err != nil {
			return fmt.Errorf("%s: %w", t.Name(), err)
		}
	}
	return nil
}

func walkBranch(walk func(parse.Node) error, b *parse.BranchNode) error {
	if err := walk(b.List); err != nil {
		return err
	}
	if b.ElseList != nil {
		return walk(b.ElseList)
	}
	return nil
}

func (v *viewer) render(w http.ResponseWriter, status int, page string, data Page) {
	data.API = TemplateAPI
	data.Version = v.opts.Version
	var b bytes.Buffer
	if err := v.pages[page].ExecuteTemplate(&b, "layout", data); err != nil {
		v.opts.Logger.Error("render failed", "page", page, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b.Bytes())
}

// StatusOf is the HTTP status for err: the JSON API's, so both surfaces
// agree.
func StatusOf(err error) int {
	if core.CodeOf(err) == "" {
		return http.StatusInternalServerError
	}
	return api.StatusFor(core.CodeOf(err))
}

// fail renders the error page for err.
func (v *viewer) fail(w http.ResponseWriter, err error, data Page) {
	status := StatusOf(err)
	retryAfter(w, err)
	if core.CodeOf(err) == "" {
		v.opts.Logger.Error("viewer request failed", "err", err)
		err = errors.New("something went wrong")
	}
	data.Notice = err.Error()
	if data.Title == "" {
		data.Title = http.StatusText(status)
	}
	v.render(w, status, "error", data)
}

// principal authenticates the request and returns the caller and its
// account name. With Login on, a signed-out page request is sent to the
// login page and comes back afterwards.
func (v *viewer) principal(w http.ResponseWriter, r *http.Request) (core.Principal, string, bool) {
	p, err := v.authn.Authenticate(r)
	if err != nil {
		if _, cerr := r.Cookie(auth.SessionCookie); cerr == nil && core.CodeOf(err) == core.CodeUnauthorized {
			v.clearSession(w)
		}
		switch {
		case v.opts.Login && core.CodeOf(err) == core.CodeUnauthorized && strings.HasSuffix(r.URL.Path, "/events"):
			http.Error(w, "sign in", http.StatusUnauthorized)
		case v.opts.Login && core.CodeOf(err) == core.CodeUnauthorized:
			back := r.URL.RequestURI()
			if r.Method != http.MethodGet {
				back = "/n/" + r.PathValue("ns") + "/c/" + r.PathValue("conv")
			}
			http.Redirect(w, r, "/login?next="+url.QueryEscape(back), http.StatusSeeOther)
		default:
			v.fail(w, err, Page{})
		}
		return core.Principal{}, "", false
	}
	v.opts.Activity.Seen(r, p, auth.SurfaceWeb)
	var account string
	if !p.IsAnonymous() {
		id, err := v.svc.Whoami(r.Context(), p)
		if err != nil {
			v.fail(w, err, Page{})
			return core.Principal{}, "", false
		}
		account = id.Account.Name
	}
	return p, account, true
}

func (v *viewer) root(w http.ResponseWriter, r *http.Request) {
	p, account, ok := v.principal(w, r)
	if !ok {
		return
	}
	ns, err := v.svc.ListNamespaces(r.Context(), p)
	if err != nil {
		v.fail(w, err, Page{Account: account})
		return
	}
	if len(ns) == 1 {
		http.Redirect(w, r, "/n/"+ns[0].Name, http.StatusFound)
		return
	}
	v.render(w, http.StatusOK, "namespaces", Page{Title: "Namespaces", Namespaces: ns, Account: account})
}

func (v *viewer) namespace(w http.ResponseWriter, r *http.Request) {
	p, account, ok := v.principal(w, r)
	if !ok {
		return
	}
	ns := r.PathValue("ns")
	q := r.URL.Query()
	archived, openOnly := q.Get("archived") == "1", q.Get("open") == "1"
	lp := core.ListParams{IncludeArchived: archived, Before: q.Get("before")}
	if openOnly {
		lp.Status = core.StatusOpen
	}
	page, err := v.svc.ListConversations(r.Context(), p, ns, lp)
	if err != nil {
		v.fail(w, err, Page{Namespace: ns, Account: account})
		return
	}
	cs := page.Conversations
	for i := range cs {
		cs[i] = displayable(cs[i])
	}
	archivedCount := 0
	if !archived {
		archivedCount, err = v.svc.CountArchived(r.Context(), p, ns, lp.Status)
		if err != nil {
			v.fail(w, err, Page{Namespace: ns, Account: account})
			return
		}
	}
	hidden := false
	if len(cs) == 0 && lp.Before == "" && (openOnly || !archived) {
		all, err := v.svc.ListConversations(r.Context(), p, ns, core.ListParams{IncludeArchived: true, Limit: 1})
		if err != nil {
			v.fail(w, err, Page{Namespace: ns, Account: account})
			return
		}
		hidden = len(all.Conversations) > 0
	}
	v.render(w, http.StatusOK, "conversations", Page{
		Title: ns, Namespace: ns, Conversations: cs, IncludeArchived: archived, ArchivedCount: archivedCount, OpenOnly: openOnly, Hidden: hidden, Next: page.Next,
		Account: account,
	})
}

func (v *viewer) conversationPage(r *http.Request, p core.Principal, account string) (Page, error) {
	ns, name := r.PathValue("ns"), r.PathValue("conv")
	data := Page{Namespace: ns, Title: name, Account: account}
	c, err := v.svc.GetConversation(r.Context(), p, ns, name)
	if err != nil {
		return data, err
	}
	// The page shows up to ShownMessages messages ending at upto: the
	// latest, or those before ?before=.
	upto := c.LastSeq
	if raw := r.URL.Query().Get("before"); raw != "" {
		before, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || before < 1 {
			return data, &core.Error{Code: core.CodeInvalid, Message: "before must be a message number"}
		}
		if before <= c.LastSeq {
			upto, data.Before = before-1, before
		}
	}
	after := max(upto-int64(v.opts.ShownMessages), 0)
	data.Messages = []Message{}
	if upto > after {
		res, err := v.svc.Read(r.Context(), p, ns, name, core.ReadParams{After: &after, Limit: int(upto - after)})
		if err != nil {
			return data, err
		}
		// The first message shown has no Prev, so it names its author
		// even when it continues a run from the page before.
		people, link := peopleOf(c), v.linker(ns, name)
		var prev *core.Message
		for _, m := range res.Messages {
			data.Messages = append(data.Messages, newMessage(m, people, prev, link))
			prev = &m
		}
		if _, err := v.markUnseen(r.Context(), p, ns, name, nil, data.Messages); err != nil {
			return data, err
		}
	}
	c = displayable(c)
	data.Conversation, data.Truncated = &c, after > 0
	if after > 0 {
		data.Earlier = after + 1
	}
	if ck, err := r.Cookie(observerCookie); err == nil && core.ValidName(ck.Value) {
		data.Observer = ck.Value
	}
	return data, nil
}

func (v *viewer) conversation(w http.ResponseWriter, r *http.Request) {
	p, account, ok := v.principal(w, r)
	if !ok {
		return
	}
	data, err := v.conversationPage(r, p, account)
	if err != nil {
		v.fail(w, err, data)
		return
	}
	v.render(w, http.StatusOK, "conversation", data)
}

func (v *viewer) observe(w http.ResponseWriter, r *http.Request) {
	p, account, ok := v.principal(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		v.fail(w, &core.Error{Code: core.CodeInvalid, Message: "could not read the form"}, Page{})
		return
	}
	ns, name := r.PathValue("ns"), r.PathValue("conv")
	observer := strings.TrimSpace(r.PostForm.Get("name"))
	_, err := v.svc.Observe(r.Context(), p, ns, name, core.ObserveParams{Name: observer, Body: r.PostForm.Get("body")})
	if err != nil {
		data, perr := v.conversationPage(r, p, account)
		if perr != nil {
			v.fail(w, perr, data)
			return
		}
		data.Notice = err.Error()
		data.Observer, data.Draft = observer, r.PostForm.Get("body")
		status := http.StatusBadRequest
		if core.CodeOf(err) == core.CodeRateLimited {
			status = http.StatusTooManyRequests
			retryAfter(w, err)
		}
		v.render(w, status, "conversation", data)
		return
	}
	if account == "" {
		http.SetCookie(w, &http.Cookie{
			Name: observerCookie, Value: observer, Path: "/", MaxAge: 365 * 24 * 3600,
			HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: v.opts.SecureCookies,
		})
	}
	// The page's script posts in the background and needs success told
	// apart from a redirect to the login page.
	if r.Header.Get(backgroundHeader) != "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/n/"+ns+"/c/"+name+"#messages", http.StatusSeeOther)
}

// backgroundHeader marks a comment posted by the page's script.
const backgroundHeader = "X-Smoot-Background"

// DefaultStreamHeartbeat bounds each server-side wait in the event stream;
// fresh conversation state is sent between waits.
const DefaultStreamHeartbeat = 25 * time.Second

// streamWriteTimeout bounds each write to an event stream.
const streamWriteTimeout = 30 * time.Second

func (v *viewer) events(w http.ResponseWriter, r *http.Request) {
	p, _, ok := v.principal(w, r)
	if !ok {
		return
	}
	ns, name := r.PathValue("ns"), r.PathValue("conv")
	after, err := strconv.ParseInt(firstNonEmpty(r.Header.Get("Last-Event-ID"), r.URL.Query().Get("after"), "0"), 10, 64)
	if err != nil || after < 0 {
		http.Error(w, "after must be a non-negative integer", http.StatusBadRequest)
		return
	}
	// Check access, existence and the viewer cap before committing to a
	// 200, so a client that cannot stream gets an error status instead of
	// an empty stream it would reconnect to forever.
	c, err := v.svc.GetConversation(r.Context(), p, ns, name)
	if err != nil {
		http.Error(w, err.Error(), StatusOf(err))
		return
	}
	// The message at after, so the first one sent knows the one before it.
	// known holds the latest messages sent, for marking unseen ones.
	var prev *core.Message
	var known []core.Message
	if after > 0 {
		from := after - 1
		res, err := v.svc.Read(r.Context(), p, ns, name, core.ReadParams{After: &from, Limit: 1})
		if err != nil {
			http.Error(w, err.Error(), StatusOf(err))
			return
		}
		if len(res.Messages) == 1 && res.Messages[0].Seq == after {
			prev = &res.Messages[0]
		}
	}
	// The stream holds one viewer slot for as long as it is connected.
	viewer, err := v.svc.OpenViewer(r.Context(), p, ns, name)
	if err != nil {
		http.Error(w, err.Error(), StatusOf(err))
		return
	}
	defer viewer.Close()
	batch, err := viewer.Follow(r.Context(), after, 0)
	if err != nil {
		http.Error(w, err.Error(), StatusOf(err))
		return
	}

	rc := http.NewResponseController(w)
	// Each write and flush must finish within streamWriteTimeout, so a
	// client that stops reading is dropped instead of holding the stream.
	writeBy := func() { _ = rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout)) }
	w.Header().Set("Content-Type", "text/event-stream")
	writeBy()
	w.WriteHeader(http.StatusOK)
	// Start with fresh state, so a reconnecting page catches up on anything
	// it missed between events.
	if v.snapshot(w, c) != nil || rc.Flush() != nil {
		return
	}
	connecting := true
	for {
		writeBy()
		// Fresh state after new messages, and at every heartbeat, which
		// keeps proxies from closing the connection and shows presence
		// changes and countdowns in a quiet conversation. The state sent on
		// connecting needs no repeat. It is read before the messages are
		// rendered, so they mention the people who are there now.
		fresh := len(batch.Messages) > 0 || !connecting
		if fresh {
			if c, err = v.svc.GetConversation(r.Context(), p, ns, name); err != nil {
				return
			}
		}
		people, link := peopleOf(c), v.linker(ns, name)
		msgs := make([]Message, len(batch.Messages))
		for i, m := range batch.Messages {
			msgs[i] = newMessage(m, people, prev, link)
			prev = &batch.Messages[i]
		}
		if known, err = v.markUnseen(r.Context(), p, ns, name, known, msgs); err != nil {
			return
		}
		for _, m := range msgs {
			if err := v.event(w, "message", strconv.FormatInt(m.Seq, 10), m); err != nil {
				return
			}
			after = m.Seq
		}
		if fresh {
			if v.snapshot(w, c) != nil {
				return
			}
		} else if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
			return
		}
		connecting = false
		if rc.Flush() != nil {
			return
		}
		batch, err = viewer.Follow(r.Context(), after, v.opts.StreamHeartbeat)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				v.opts.Logger.Info("event stream ended", "err", err)
			}
			return
		}
	}
}

// snapshot sends the conversation's status, participant panel and about
// panel.
func (v *viewer) snapshot(w http.ResponseWriter, c core.Conversation) error {
	if err := writeEvent(w, "status", "", string(c.Status)); err != nil {
		return err
	}
	c = displayable(c)
	if err := v.event(w, "participants", "", c); err != nil {
		return err
	}
	return v.event(w, "about", "", c)
}

// linker returns the addresses of messages and attachments in
// conversation name.
func (v *viewer) linker(ns, name string) links {
	l := links{message: func(seq int64) string { return messageLink(ns, name, seq, v.opts.ShownMessages) }}
	if v.svc.AttachmentsEnabled() {
		l.download = func(seq int64) string { return "/n/" + ns + "/c/" + name + "/files/" + strconv.FormatInt(seq, 10) }
	}
	return l
}

// download sends an attachment's file. The download is recorded under the
// signed-in account's name or, with auth off, the name last used for an
// observer comment ("viewer" if none).
func (v *viewer) download(w http.ResponseWriter, r *http.Request) {
	p, _, ok := v.principal(w, r)
	if !ok {
		return
	}
	seq, err := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	if err != nil || seq < 1 {
		v.fail(w, &core.Error{Code: core.CodeInvalid, Message: "no such attachment"}, Page{})
		return
	}
	observer := "viewer"
	if ck, err := r.Cookie(observerCookie); err == nil && core.ValidName(ck.Value) {
		observer = ck.Value
	}
	d, err := v.svc.Fetch(r.Context(), p, r.PathValue("ns"), r.PathValue("conv"), core.FetchParams{Seq: seq, Observer: observer})
	if err != nil {
		v.fail(w, err, Page{})
		return
	}
	api.WriteDownload(w, d)
}

// displayable drops bidirectional controls from the free text of a
// conversation, its topic and participant notes; message bodies lose them
// in renderMarkdown.
func displayable(c core.Conversation) core.Conversation {
	c.Topic = markdown.StripBidi(c.Topic)
	ps := make([]core.Participant, len(c.Participants))
	for i, p := range c.Participants {
		p.Note = markdown.StripBidi(p.Note)
		ps[i] = p
	}
	c.Participants = ps
	return c
}

// keepKnown bounds the messages an event stream keeps for marking unseen
// ones; markUnseen reads older ones from the store when it needs them.
const keepKnown = 300

// markUnseen sets Unseen on msgs, which follow known in the log without a
// gap (known may be empty). Messages it needs from before both are read
// from the store, at most core.MaxReadLimit of them; any older ones are not
// reported. It returns the latest messages, up to keepKnown, to pass as
// known with the next batch.
func (v *viewer) markUnseen(ctx context.Context, p core.Principal, ns, name string, known []core.Message, msgs []Message) ([]core.Message, error) {
	if len(msgs) == 0 {
		return known, nil
	}
	all := slices.Clone(known)
	for _, m := range msgs {
		all = append(all, m.Message)
	}
	first := all[0].Seq
	from := first - 1
	for _, m := range msgs {
		if m.SeenSeq != nil && *m.SeenSeq < from {
			from = *m.SeenSeq
		}
	}
	if from = max(from, first-1-core.MaxReadLimit); from < first-1 {
		res, err := v.svc.Read(ctx, p, ns, name, core.ReadParams{After: &from, Limit: int(first - 1 - from)})
		if err != nil {
			return known, err
		}
		all = append(res.Messages, all...)
	}
	for i := range msgs {
		msgs[i].Unseen = core.Unseen(msgs[i].Message, all)
	}
	return all[max(len(all)-keepKnown, 0):], nil
}

// event writes one server-sent event whose data is the named template
// rendered with data ("message" for a Message (this package's), "participants" and "about"
// for a Conversation).
func (v *viewer) event(w http.ResponseWriter, name, id string, data any) error {
	var b bytes.Buffer
	if err := v.pages["conversation"].ExecuteTemplate(&b, name, data); err != nil {
		v.opts.Logger.Error("render failed", "template", name, "err", err)
		return err
	}
	return writeEvent(w, name, id, b.String())
}

// writeEvent frames data as a server-sent event. CR, LF and CRLF all end a
// line in the event-stream format, so every line break in data becomes its
// own data: line; nothing in data can start a new field.
func writeEvent(w io.Writer, name, id, data string) error {
	data = strings.ReplaceAll(data, "\r\n", "\n")
	data = strings.ReplaceAll(data, "\r", "\n")
	var out strings.Builder
	out.WriteString("event: " + name + "\n")
	if id != "" {
		out.WriteString("id: " + id + "\n")
	}
	for line := range strings.SplitSeq(strings.TrimRight(data, "\n"), "\n") {
		out.WriteString("data: " + line + "\n")
	}
	out.WriteString("\n")
	_, err := io.WriteString(w, out.String())
	return err
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// returnTo keeps a post-login redirect on this site: a path, never another
// host. Browsers drop tabs and newlines from a Location and read a
// backslash as a slash, so "/\t/evil" would become "//evil"; any control
// character or backslash is refused.
func returnTo(next string) string {
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || next == "" || next[0] != '/' ||
		len(next) > 1 && (next[1] == '/' || next[1] == '\\') ||
		strings.ContainsFunc(next, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '\\' }) {
		return "/"
	}
	return next
}

func noStore(w http.ResponseWriter) { w.Header().Set("Cache-Control", "no-store") }

// retryAfter sets Retry-After for a rate_limited error.
func retryAfter(w http.ResponseWriter, err error) {
	if ce, ok := errors.AsType[*core.Error](err); ok && ce.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(ce.RetryAfter.Seconds())))
	}
}

func (v *viewer) loginPage(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	v.render(w, http.StatusOK, "login", Page{Title: "Sign in", ReturnTo: returnTo(r.URL.Query().Get("next"))})
}

// login exchanges a pasted token for a browser session. The session cookie
// replaces (and revokes) any session the browser had.
func (v *viewer) login(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		v.render(w, http.StatusBadRequest, "login", Page{Title: "Sign in", Notice: "Could not read the form.", ReturnTo: "/"})
		return
	}
	next := returnTo(r.PostForm.Get("next"))
	p, err := v.svc.AuthenticateToken(r.Context(), strings.TrimSpace(r.PostForm.Get("token")))
	if err != nil {
		if core.CodeOf(err) != core.CodeUnauthorized {
			v.fail(w, err, Page{})
			return
		}
		// A valid token always signs in; only failures are limited.
		v.opts.Activity.Failed(r)
		if cerr := v.opts.Failures.Check(r); cerr != nil {
			notice := "Too many failed sign-ins; try again later."
			if ce, ok := errors.AsType[*core.Error](cerr); ok {
				notice = ce.Message
			}
			retryAfter(w, cerr)
			v.render(w, http.StatusTooManyRequests, "login", Page{Title: "Sign in", ReturnTo: next, Notice: notice})
			return
		}
		v.opts.Failures.Fail(r)
		v.render(w, http.StatusUnauthorized, "login", Page{
			Title: "Sign in", ReturnTo: next,
			Notice: "That token is not valid, or it has been revoked or has expired.",
		})
		return
	}
	var replaces string
	if old, err := r.Cookie(auth.SessionCookie); err == nil {
		replaces = old.Value
	}
	secret, expires, err := v.svc.CreateSession(r.Context(), p, v.opts.SessionTTL, replaces)
	if core.CodeOf(err) == core.CodeRateLimited {
		retryAfter(w, err)
		v.render(w, http.StatusTooManyRequests, "login", Page{Title: "Sign in", ReturnTo: next, Notice: err.Error()})
		return
	}
	if err != nil {
		v.fail(w, err, Page{})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: auth.SessionCookie, Value: secret, Path: "/", Expires: expires,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: v.opts.SecureCookies,
	})
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (v *viewer) logout(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if c, err := r.Cookie(auth.SessionCookie); err == nil {
		if err := v.svc.EndSession(r.Context(), c.Value); err != nil {
			v.fail(w, err, Page{})
			return
		}
	}
	v.clearSession(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// clearSession tells the browser to drop its session cookie.
func (v *viewer) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: auth.SessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: v.opts.SecureCookies,
	})
}
