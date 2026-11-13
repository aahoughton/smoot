package mcpserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHeartbeatSkipsResponsesThatAreNotStreams(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		contentType string
		want        bool
	}{
		{"event stream", http.StatusOK, "text/event-stream", true},
		{"event stream with charset", http.StatusOK, "text/event-stream; charset=utf-8", true},
		{"refused stream", http.StatusConflict, "text/event-stream", false},
		{"json", http.StatusOK, "application/json", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				if tc.status != http.StatusOK {
					w.WriteHeader(tc.status)
				}
				_, _ = w.Write([]byte(": ok\n\n"))
				time.Sleep(50 * time.Millisecond)
			})
			rec := httptest.NewRecorder()
			heartbeat(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/mcp", nil), 5*time.Millisecond, next)
			if got := strings.Contains(rec.Body.String(), heartbeatComment); got != tc.want {
				t.Fatalf("heartbeat written = %v, want %v (body %q)", got, tc.want, rec.Body.String())
			}
		})
	}
}

func TestHeartbeatBeforeTheHandlerWritesIsHeld(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(30 * time.Millisecond) // heartbeats fire before any header
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	rec := httptest.NewRecorder()
	heartbeat(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/mcp", nil), 5*time.Millisecond, next)
	if got := rec.Body.String(); got != `{}` {
		t.Fatalf("body = %q", got)
	}
}

func TestHeartbeatWriterPassesResponseControllerThrough(t *testing.T) {
	var deadlineErr error
	var flusher bool
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		deadlineErr = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Minute))
		_, flusher = w.(http.Flusher)
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		heartbeat(w, r, time.Minute, next)
	}))
	t.Cleanup(srv.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if deadlineErr != nil {
		t.Fatalf("SetWriteDeadline through the wrapper: %v", deadlineErr)
	}
	if !flusher {
		t.Fatal("wrapper is not an http.Flusher")
	}
}
