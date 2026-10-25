package web_test

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aahoughton/smoot/internal/api"
	"github.com/aahoughton/smoot/internal/auth"
	"github.com/aahoughton/smoot/internal/core"
	"github.com/aahoughton/smoot/internal/markdown"
	"github.com/aahoughton/smoot/internal/store"
	"github.com/aahoughton/smoot/internal/web"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden.html")

var fixed = time.Date(2030, 1, 2, 15, 4, 5, 0, time.UTC)

type env struct {
	t   *testing.T
	svc *core.Service
	url string
	c   *http.Client
	// offset is how far the service's clock has moved past fixed.
	offset *atomic.Int64
}

// advance moves the service's clock forward by d.
func (e *env) advance(d time.Duration) { e.offset.Add(int64(d)) }

func newEnv(t *testing.T, opts web.Options) *env {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "smoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	offset := new(atomic.Int64)
	svc := core.New(db, core.Options{Now: func() time.Time { return fixed.Add(time.Duration(offset.Load())) }})
	if opts.Now == nil {
		opts.Now = func() time.Time { return fixed.Add(10 * time.Minute) }
		opts.Location = time.UTC
		opts.Version = "test"
	}
	h, err := web.Handler(svc, auth.None{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &env{t: t, svc: svc, url: srv.URL, c: c, offset: offset}
}

func (e *env) get(path string) (int, string) {
	e.t.Helper()
	req, err := http.NewRequestWithContext(e.t.Context(), http.MethodGet, e.url+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.do(req)
}

func (e *env) post(path string, form url.Values) (int, string) {
	e.t.Helper()
	req, err := http.NewRequestWithContext(e.t.Context(), http.MethodPost, e.url+path, strings.NewReader(form.Encode()))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return e.do(req)
}

func (e *env) do(req *http.Request) (int, string) {
	e.t.Helper()
	resp, err := e.c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, string(b)
}

func (e *env) seed() {
	e.t.Helper()
	ctx, p := e.t.Context(), core.Anonymous()
	if _, err := e.svc.Start(ctx, p, "default", core.StartParams{Name: "cache", Topic: "pick an eviction policy", Agent: "alice", Join: true, End: core.EndAgreement}); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.svc.Join(ctx, p, "default", "cache", core.JoinParams{Agent: "bob"}); err != nil {
		e.t.Fatal(err)
	}
	for _, s := range []core.SayParams{
		{Agent: "alice", Body: "**LRU** or LFU? See [notes](https://example.com/notes).\n\n<script>alert(1)</script>"},
		{Agent: "bob", Body: "@alice LRU, as in #4:\n\n```\nevict(oldest)\n```", ReplyTo: ptr(int64(4))},
	} {
		if _, err := e.svc.Say(ctx, p, "default", "cache", s); err != nil {
			e.t.Fatal(err)
		}
	}
	if _, err := e.svc.Observe(ctx, p, "default", "cache", core.ObserveParams{Name: "pat", Body: "mind the memory budget, @bob"}); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.svc.Conclude(ctx, p, "default", "cache", core.ConcludeParams{Agent: "alice", Body: "AGREED: #5, LRU"}); err != nil {
		e.t.Fatal(err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestRootRedirectsWithOneNamespace(t *testing.T) {
	e := newEnv(t, web.Options{})
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, e.url+"/", nil)
	resp, err := e.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/n/default" {
		t.Fatalf("got %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if _, err := e.svc.CreateNamespace(t.Context(), core.Anonymous(), "team-blue"); err != nil {
		t.Fatal(err)
	}
	status, body := e.get("/")
	if status != http.StatusOK || !strings.Contains(body, `href="/n/team-blue"`) {
		t.Fatalf("namespaces = %d %s", status, body)
	}
}

func TestConversationPage(t *testing.T) {
	e := newEnv(t, web.Options{})
	e.seed()
	status, body := e.get("/n/default/c/cache")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	for _, want := range []string{
		"<strong>LRU</strong>",
		`<a href="https://example.com/notes">notes</a>`,
		`class="message observer tone-`,
		`class="message conclude tone-`,
		`href="#m4">re #4</a>`,
		"LRU, as in #4:",
		`AGREED: <a class="ref" href="/n/default/c/cache?before=26#m5">#5</a>, LRU`,
		`data-last-seq="7"`,
		`<form class="observe"`,
		`<p class="observe-closed" hidden>`,
		`<button type="button" class="new-below" hidden>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, "<script>alert") {
		t.Error("message HTML was not escaped")
	}
}

func TestErrorsRenderPages(t *testing.T) {
	e := newEnv(t, web.Options{})
	if status, body := e.get("/n/default/c/nope"); status != http.StatusNotFound || !strings.Contains(body, "not found") {
		t.Fatalf("missing conversation = %d %s", status, body)
	}
	if status, _ := e.get("/n/nope"); status != http.StatusNotFound {
		t.Fatalf("missing namespace = %d", status)
	}
}

func TestObserveForm(t *testing.T) {
	e := newEnv(t, web.Options{})
	e.seed()
	status, _ := e.post("/n/default/c/cache/observe", url.Values{"name": {"sam"}, "body": {"try ARC too"}})
	if status != http.StatusSeeOther {
		t.Fatalf("observe = %d", status)
	}
	_, body := e.get("/n/default/c/cache")
	if !strings.Contains(body, "try ARC too") || !strings.Contains(body, `value="sam"`) {
		t.Fatalf("comment or remembered name missing")
	}
	status, body = e.post("/n/default/c/cache/observe", url.Values{"name": {"bad name"}, "body": {"x"}})
	if status != http.StatusBadRequest || !strings.Contains(body, `class="notice"`) {
		t.Fatalf("bad name = %d", status)
	}
}

func TestConcludedConversationSaysCommentsAreClosed(t *testing.T) {
	e := newEnv(t, web.Options{})
	e.seed()
	if _, err := e.svc.Conclude(t.Context(), core.Anonymous(), "default", "cache", core.ConcludeParams{Agent: "bob", Body: "AGREED: #5"}); err != nil {
		t.Fatal(err)
	}
	_, body := e.get("/n/default/c/cache")
	if strings.Contains(body, `<form class="observe"`) {
		t.Fatal("concluded conversation offers a comment form")
	}
	if !strings.Contains(body, `<p class="observe-closed">`) {
		t.Fatal("concluded conversation does not say comments are closed")
	}
}

func TestEventStream(t *testing.T) {
	e := newEnv(t, web.Options{})
	e.seed()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.url+"/n/default/c/cache/events?after=7", nil)
	resp, err := e.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = e.svc.Say(context.Background(), core.Anonymous(), "default", "cache", core.SayParams{Agent: "bob", Body: "live *hello* @pat"})
	}()

	sc := bufio.NewScanner(resp.Body)
	var event, id string
	var data []string
	deadline := time.After(5 * time.Second)
	lines := make(chan string)
	go func() {
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	for {
		select {
		case <-deadline:
			t.Fatal("no message event")
		case line, ok := <-lines:
			if !ok {
				t.Fatal("stream closed")
			}
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "id: "):
				id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				data = append(data, strings.TrimPrefix(line, "data: "))
			case line == "" && event == "message":
				html := strings.Join(data, "\n")
				// pat has commented, so the live message mentions an observer.
				if id != "8" || !strings.Contains(html, "<em>hello</em>") || !strings.Contains(html, `id="m8"`) ||
					!strings.Contains(html, `<strong class="mention observer">@pat</strong>`) {
					t.Fatalf("event id %q data %s", id, html)
				}
				return
			case line == "":
				event, id, data = "", "", nil
			}
		}
	}
}

func TestThemeOverride(t *testing.T) {
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, "templates"), 0o755))
	must(t, os.MkdirAll(filepath.Join(dir, "static"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "templates", "message.html"),
		[]byte(`{{define "message"}}<li class="custom">{{.Author}}: {{markdown .Body}}</li>{{end}}`), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "static", "style.css"), []byte("body{color:red}"), 0o644))

	e := newEnv(t, web.Options{ThemeDir: dir, Now: func() time.Time { return fixed }, Location: time.UTC})
	e.seed()
	_, body := e.get("/n/default/c/cache")
	if !strings.Contains(body, `<li class="custom">alice:`) || !strings.Contains(body, `id="participants"`) {
		t.Fatalf("override not applied or default lost:\n%s", body)
	}
	if _, css := e.get("/static/style.css"); css != "body{color:red}" {
		t.Fatalf("static override = %q", css)
	}
	if status, js := e.get("/static/smoot.js"); status != http.StatusOK || !strings.Contains(js, "EventSource") {
		t.Fatalf("default static missing: %d", status)
	}
}

func TestBrokenThemesFailAtStartup(t *testing.T) {
	write := func(name, content string) string {
		dir := t.TempDir()
		must(t, os.MkdirAll(filepath.Join(dir, "templates"), 0o755))
		must(t, os.WriteFile(filepath.Join(dir, "templates", name), []byte(content), 0o644))
		return dir
	}
	tests := map[string]string{
		"parse error":      write("message.html", `{{define "message"}}{{.Oops{{end}}`),
		"missing define":   write("participants.html", `<p>no define here</p>`),
		"unknown function": write("message.html", `{{define "message"}}{{shout .Body}}{{end}}`),
	}
	for name, dir := range tests {
		if _, err := web.Handler(nil, auth.None{}, web.Options{ThemeDir: dir}); err == nil {
			t.Errorf("%s: theme accepted", name)
		}
	}
	if _, err := web.Handler(nil, auth.None{}, web.Options{ThemeDir: "/does/not/exist"}); err == nil {
		t.Error("missing theme dir accepted")
	}
}

// TestGoldenPages snapshots the default theme's pages for fixed data, so
// markup changes show up as a diff; accept them with -update.
func TestGoldenPages(t *testing.T) {
	e := newEnv(t, web.Options{})
	e.seed()
	runs := newEnv(t, web.Options{})
	runs.seedRuns()
	if _, err := e.svc.CreateNamespace(t.Context(), core.Anonymous(), "team-blue"); err != nil {
		t.Fatal(err)
	}
	for name, page := range map[string]struct {
		e    *env
		path string
	}{
		"namespaces":        {e, "/"},
		"conversations":     {e, "/n/default"},
		"conversation":      {e, "/n/default/c/cache"},
		"conversation-runs": {runs, "/n/default/c/runs"},
		"search":            {e, "/n/default/search?q=lru"},
		"search-empty":      {e, "/n/default/search"},
	} {
		_, body := page.e.get(page.path)
		file := filepath.Join("testdata", name+".golden.html")
		if *update {
			must(t, os.MkdirAll("testdata", 0o755))
			must(t, os.WriteFile(file, []byte(body), 0o644))
			continue
		}
		want, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("%v (run go test ./internal/web -update)", err)
		}
		if body != string(want) {
			t.Errorf("%s changed; review and run go test ./internal/web -update", name)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// readEvents collects SSE events from body until n events of kind arrive or
// the deadline passes. Lines end at CR, LF or CRLF, as EventSource parses them.
type sse struct{ event, id, data string }

func readEvents(t *testing.T, body io.Reader, until func([]sse) bool) []sse {
	t.Helper()
	raw := make(chan string)
	go func() {
		br := bufio.NewReader(body)
		var cur strings.Builder
		for {
			b, err := br.ReadByte()
			if err != nil {
				close(raw)
				return
			}
			switch b {
			case '\r':
				if next, err := br.Peek(1); err == nil && next[0] == '\n' {
					_, _ = br.ReadByte()
				}
				raw <- cur.String()
				cur.Reset()
			case '\n':
				raw <- cur.String()
				cur.Reset()
			default:
				cur.WriteByte(b)
			}
		}
	}()
	var out []sse
	var ev sse
	var data []string
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("timed out; events so far: %+v", out)
		case line, ok := <-raw:
			if !ok {
				return out
			}
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "id: "):
				ev.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				data = append(data, strings.TrimPrefix(line, "data: "))
			case line == "":
				if ev.event != "" {
					ev.data = strings.Join(data, "\n")
					out = append(out, ev)
					if until(out) {
						return out
					}
				}
				ev, data = sse{}, nil
			}
		}
	}
}

// stream is an open event stream; its body is closed when the test ends.
type stream struct {
	StatusCode int
	Header     http.Header
	Body       io.Reader
}

func openStream(t *testing.T, e *env, path string, header map[string]string) stream {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.url+path, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := e.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return stream{StatusCode: resp.StatusCode, Header: resp.Header, Body: resp.Body}
}

func TestStreamCannotBeInjectedThroughCarriageReturns(t *testing.T) {
	e := newEnv(t, web.Options{})
	e.seed()
	resp := openStream(t, e, "/n/default/c/cache/events?after=7", nil)
	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = e.svc.Say(context.Background(), core.Anonymous(), "default", "cache", core.SayParams{Agent: "bob", Body: "hello&#13;id: 999999&#13;event: participants\r\ndata: x\rmore"})
	}()
	evs := readEvents(t, resp.Body, func(evs []sse) bool {
		for _, e := range evs {
			if e.event == "message" {
				return true
			}
		}
		return false
	})
	for _, ev := range evs {
		if ev.id == "999999" {
			t.Fatalf("injected id reached the stream: %+v", evs)
		}
		if ev.event == "message" && ev.id != "8" {
			t.Fatalf("message event id = %q", ev.id)
		}
	}
}

func TestStreamStartsWithStatusAndParticipants(t *testing.T) {
	e := newEnv(t, web.Options{})
	e.seed()
	resp := openStream(t, e, "/n/default/c/cache/events", map[string]string{"Last-Event-ID": "7"})
	evs := readEvents(t, resp.Body, func(evs []sse) bool { return len(evs) >= 2 })
	if evs[0].event != "status" || evs[0].data != "open" || evs[1].event != "participants" || !strings.Contains(evs[1].data, "bob") {
		t.Fatalf("opening events = %+v", evs)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = e.svc.Conclude(context.Background(), core.Anonymous(), "default", "cache", core.ConcludeParams{Agent: "bob", Body: "AGREED: #5"})
	}()
	evs = readEvents(t, resp.Body, func(evs []sse) bool {
		for _, e := range evs {
			if e.event == "status" {
				return true
			}
		}
		return false
	})
	if last := evs[len(evs)-1]; last.data != "concluded" {
		t.Fatalf("status after conclusion = %+v", evs)
	}
}

func TestStreamErrorsAreHTTPErrors(t *testing.T) {
	e := newEnv(t, web.Options{})
	e.seed()
	resp := openStream(t, e, "/n/default/c/nope/events", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing conversation stream = %d", resp.StatusCode)
	}
}

func TestStreamRefusesOverTheViewerCap(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "smoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e := &env{t: t, svc: core.New(db, core.Options{MaxWatchers: 1})}
	h, err := web.Handler(e.svc, auth.None{}, web.Options{Location: time.UTC, Now: func() time.Time { return fixed }})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	e.url, e.c = srv.URL, http.DefaultClient
	e.seed()
	first := openStream(t, e, "/n/default/c/cache/events?after=7", nil)
	readEvents(t, first.Body, func(evs []sse) bool { return len(evs) >= 2 })
	second := openStream(t, e, "/n/default/c/cache/events?after=7", nil)
	if second.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second viewer = %d", second.StatusCode)
	}
}

func TestThemeSymlinksCannotEscape(t *testing.T) {
	outside := t.TempDir()
	must(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644))
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, "static"), 0o755))
	must(t, os.Symlink(outside, filepath.Join(dir, "static", "escape")))
	must(t, os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "static", "secret.txt")))
	e := newEnv(t, web.Options{ThemeDir: dir, Now: func() time.Time { return fixed }, Location: time.UTC})
	for _, p := range []string{"/static/escape/secret.txt", "/static/secret.txt"} {
		if status, body := e.get(p); status == http.StatusOK || strings.Contains(body, "secret") {
			t.Errorf("%s = %d %q", p, status, body)
		}
	}
}

func TestMissingReferencedTemplateFailsAtStartup(t *testing.T) {
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, "templates"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "templates", "message.html"), []byte(`{{define "message"}}{{template "bubble" .}}{{end}}`), 0o644))
	if _, err := web.Handler(nil, auth.None{}, web.Options{ThemeDir: dir}); err == nil || !strings.Contains(err.Error(), "bubble") {
		t.Fatalf("err = %v", err)
	}
}

func TestRejectedCommentKeepsTheDraft(t *testing.T) {
	e := newEnv(t, web.Options{})
	e.seed()
	status, body := e.post("/n/default/c/cache/observe", url.Values{"name": {"bad name"}, "body": {"a long thoughtful comment"}})
	if status != http.StatusBadRequest || !strings.Contains(body, "a long thoughtful comment") || !strings.Contains(body, `value="bad name"`) {
		t.Fatalf("draft lost: %d", status)
	}
}

func TestTimeboxShowsLocalEndAndCountdown(t *testing.T) {
	e := newEnv(t, web.Options{})
	ends := fixed.Add(2 * time.Hour)
	if _, err := e.svc.Start(t.Context(), core.Anonymous(), "default", core.StartParams{Name: "spike", Agent: "a", Join: true, End: core.EndTimebox, EndsAt: &ends}); err != nil {
		t.Fatal(err)
	}
	_, body := e.get("/n/default/c/spike")
	// The viewer's clock is fixed+10m, so 1h 50m remain.
	if !strings.Contains(body, ">Jan 2 17:04</time> (in 1h 50m)") {
		t.Fatalf("timebox line missing:\n%s", body)
	}
}

func TestBidiControlsAreNotShown(t *testing.T) {
	e := newEnv(t, web.Options{})
	ctx, p := t.Context(), core.Anonymous()
	if _, err := e.svc.Start(ctx, p, "default", core.StartParams{Name: "spoof", Topic: "safe \u202eetirw", Agent: "alice", Join: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetPresence(ctx, p, "default", "spoof", core.PresenceParams{Agent: "alice", Presence: core.PresenceWorking, Note: "note \u2066x\u2069"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Say(ctx, p, "default", "spoof", core.SayParams{Agent: "alice", Body: "body \u202dtxt"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/n/default/c/spoof", "/n/default"} {
		_, body := e.get(path)
		if strings.ContainsFunc(body, markdown.IsBidiControl) {
			t.Errorf("%s shows a bidi control", path)
		}
		if !strings.Contains(body, "safe etirw") {
			t.Errorf("%s lost the topic text", path)
		}
	}

	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx2, http.MethodGet, e.url+"/n/default/c/spoof/events?after=0", nil)
	resp, err := e.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.ContainsFunc(line, markdown.IsBidiControl) {
			t.Fatalf("event stream shows a bidi control: %q", line)
		}
		// The stream opens with status and participants, then the messages.
		if strings.Contains(line, "body txt") {
			return
		}
	}
	t.Fatal("stream ended before the message")
}

func TestStreamKeepsAboutCurrent(t *testing.T) {
	e := newEnv(t, web.Options{})
	e.seed()
	resp := openStream(t, e, "/n/default/c/cache/events", map[string]string{"Last-Event-ID": "7"})
	evs := readEvents(t, resp.Body, func(evs []sse) bool { return len(evs) >= 3 })
	if evs[2].event != "about" || !strings.Contains(evs[2].data, "<dd>7, last") {
		t.Fatalf("opening events = %+v", evs)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = e.svc.Say(context.Background(), core.Anonymous(), "default", "cache", core.SayParams{Agent: "bob", Body: "one more"})
	}()
	evs = readEvents(t, resp.Body, func(evs []sse) bool {
		return len(evs) > 0 && evs[len(evs)-1].event == "about"
	})
	if last := evs[len(evs)-1]; !strings.Contains(last.data, "<dd>8, last") {
		t.Fatalf("about after a message = %+v", last)
	}
}

func TestConversationListPagesAndFilters(t *testing.T) {
	e := newEnv(t, web.Options{})
	ctx, p := t.Context(), core.Anonymous()
	for i := range core.DefaultListLimit + 1 {
		if _, err := e.svc.Start(ctx, p, "default", core.StartParams{Name: fmt.Sprintf("c%03d", i), Agent: "alice", Join: true, End: core.EndAgreement}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.svc.Conclude(ctx, p, "default", "c100", core.ConcludeParams{Agent: "alice", Body: "AGREED: abc1234"}); err != nil {
		t.Fatal(err)
	}

	_, body := e.get("/n/default")
	m := regexp.MustCompile(`href="\?before=([^&"]+)&`).FindStringSubmatch(body)
	if m == nil || strings.Count(body, `class="conversation-row`) != core.DefaultListLimit {
		t.Fatalf("first page has no next link or the wrong size")
	}
	_, body = e.get("/n/default?before=" + m[1])
	if strings.Count(body, `class="conversation-row`) != 1 || strings.Contains(body, "Older conversations") {
		t.Fatalf("second page:\n%s", body)
	}
	_, body = e.get("/n/default?open=1")
	if strings.Contains(body, ">c100<") || strings.Contains(body, "Older conversations") {
		t.Fatal("open-only list shows a concluded conversation or a next page")
	}
	if status, _ := e.get("/n/default?before=nonsense"); status != http.StatusBadRequest {
		t.Fatalf("bad cursor = %d", status)
	}
}

func TestEmptyConversationListSaysWhatIsHidden(t *testing.T) {
	e := newEnv(t, web.Options{})
	ctx, p := t.Context(), core.Anonymous()
	empty := func(path string) string {
		t.Helper()
		_, body := e.get(path)
		m := regexp.MustCompile(`<p class="empty">([^<]*)</p>`).FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("%s: no empty message", path)
		}
		return m[1]
	}
	if got := empty("/n/default"); !strings.HasPrefix(got, "No conversations yet") {
		t.Errorf("new namespace = %q", got)
	}
	if _, err := e.svc.Start(ctx, p, "default", core.StartParams{Name: "done", Agent: "alice", Join: true, End: core.EndAgreement}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Conclude(ctx, p, "default", "done", core.ConcludeParams{Agent: "alice", Body: "AGREED: abc1234"}); err != nil {
		t.Fatal(err)
	}
	if got := empty("/n/default?open=1"); !strings.HasPrefix(got, "No open conversations") {
		t.Errorf("all concluded, open only = %q", got)
	}
	e.advance(core.DefaultArchiveAfter + time.Hour)
	if got := empty("/n/default"); !strings.HasPrefix(got, "Everything here is archived") {
		t.Errorf("all archived = %q", got)
	}
	// The toggle counts what it would add under the current filter.
	for path, want := range map[string]string{
		"/n/default":            ">Show archived (1)</a>",
		"/n/default?open=1":     ">Show archived</a>",
		"/n/default?archived=1": ">Hide archived</a>",
	} {
		if _, body := e.get(path); !strings.Contains(body, want) {
			t.Errorf("%s: no %q", path, want)
		}
	}
}

func TestEarlierMessages(t *testing.T) {
	e := newEnv(t, web.Options{ShownMessages: 3})
	ctx, p := t.Context(), core.Anonymous()
	if _, err := e.svc.Start(ctx, p, "default", core.StartParams{Name: "long", Agent: "alice", Join: true}); err != nil {
		t.Fatal(err)
	}
	for i := range 8 { // seqs 3..10
		if _, err := e.svc.Say(ctx, p, "default", "long", core.SayParams{Agent: "alice", Body: fmt.Sprintf("body-%d", i+3)}); err != nil {
			t.Fatal(err)
		}
	}
	seqs := func(body string) string {
		var out []string
		for _, m := range regexp.MustCompile(`id="m(\d+)"`).FindAllStringSubmatch(body, -1) {
			out = append(out, m[1])
		}
		return strings.Join(out, ",")
	}

	_, body := e.get("/n/default/c/long")
	if seqs(body) != "8,9,10" || !strings.Contains(body, `href="?before=8"`) || !strings.Contains(body, "data-events=") {
		t.Fatalf("latest page: %s", seqs(body))
	}
	_, body = e.get("/n/default/c/long?before=8")
	if seqs(body) != "5,6,7" || !strings.Contains(body, `href="?before=5"`) || strings.Contains(body, "data-events=") ||
		strings.Contains(body, `<form class="observe"`) || !strings.Contains(body, "Back to the latest") {
		t.Fatalf("earlier page: %s", seqs(body))
	}
	_, body = e.get("/n/default/c/long?before=3")
	if seqs(body) != "1,2" || strings.Contains(body, "Earlier messages") {
		t.Fatalf("first page: %s", seqs(body))
	}
	if status, _ := e.get("/n/default/c/long?before=x"); status != http.StatusBadRequest {
		t.Fatalf("bad before = %d", status)
	}
}

// The viewer answers errors with the same status as the JSON API.
func TestViewerStatusesMatchTheAPI(t *testing.T) {
	for _, code := range []core.Code{core.CodeNotCoordinator, core.CodeAccountNotFound, core.CodeLastAdmin, core.CodeOwnCredential, core.CodeUnauthorized, core.CodeNameActive} {
		if got, want := web.StatusOf(&core.Error{Code: code}), api.StatusFor(code); got != want || got == http.StatusInternalServerError {
			t.Errorf("%s: viewer %d, API %d", code, got, want)
		}
	}
}

func TestObserverCookieFollowsSecureCookies(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprint(secure), func(t *testing.T) {
			e := newEnv(t, web.Options{SecureCookies: secure})
			e.seed()
			form := url.Values{"name": {"sam"}, "body": {"try ARC too"}}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, e.url+"/n/default/c/cache/observe", strings.NewReader(form.Encode()))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			resp, err := e.c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			cookie := resp.Header.Get("Set-Cookie")
			if !strings.Contains(cookie, "sam") || strings.Contains(cookie, "Secure") != secure {
				t.Fatalf("Set-Cookie = %q", cookie)
			}
		})
	}
}
