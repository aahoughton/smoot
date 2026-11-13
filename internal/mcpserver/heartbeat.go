package mcpserver

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// heartbeatComment is a server-sent events comment line, which clients
// discard.
const heartbeatComment = ": keepalive\n\n"

// heartbeat serves next and, once the response is an event stream, writes
// an SSE comment to it every interval until next returns.
func heartbeat(w http.ResponseWriter, r *http.Request, interval time.Duration, next http.Handler) {
	hw := &heartbeatWriter{ResponseWriter: w}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-r.Context().Done():
				return
			case <-t.C:
				hw.beat()
			}
		}
	}()
	next.ServeHTTP(hw, r)
	close(stop)
	<-done
}

// heartbeatWriter serializes the handler's writes with the heartbeat's, so
// a comment never lands inside an event. The SDK writes each event with one
// Write.
type heartbeatWriter struct {
	http.ResponseWriter
	mu          sync.Mutex
	wroteHeader bool
	streaming   bool
}

func (h *heartbeatWriter) WriteHeader(code int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.header(code)
	h.ResponseWriter.WriteHeader(code)
}

// header notes whether the response being started is an event stream. The
// caller holds h.mu.
func (h *heartbeatWriter) header(code int) {
	if h.wroteHeader {
		return
	}
	h.wroteHeader = true
	h.streaming = code == http.StatusOK && strings.HasPrefix(h.Header().Get("Content-Type"), "text/event-stream")
}

func (h *heartbeatWriter) Write(b []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.header(http.StatusOK)
	return h.ResponseWriter.Write(b)
}

// FlushError keeps streaming working through the wrapper.
func (h *heartbeatWriter) FlushError() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return http.NewResponseController(h.ResponseWriter).Flush()
}

// Flush serves callers that assert http.Flusher.
func (h *heartbeatWriter) Flush() { _ = h.FlushError() }

// Unwrap passes the other http.ResponseController calls through.
func (h *heartbeatWriter) Unwrap() http.ResponseWriter { return h.ResponseWriter }

func (h *heartbeatWriter) beat() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.streaming {
		return
	}
	if _, err := h.ResponseWriter.Write([]byte(heartbeatComment)); err != nil {
		return
	}
	_ = http.NewResponseController(h.ResponseWriter).Flush()
}
