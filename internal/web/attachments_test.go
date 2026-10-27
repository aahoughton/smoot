package web_test

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aahoughton/smoot/internal/core"
	"github.com/aahoughton/smoot/internal/web"
)

// seedFiles builds conversation files with a downloadable attachment and
// one whose bytes were deleted.
func (e *env) seedFiles() {
	e.t.Helper()
	ctx, p := e.t.Context(), core.Anonymous()
	if _, err := e.svc.Start(ctx, p, "default", core.StartParams{Name: "files", Agent: "alice", Join: true}); err != nil {
		e.t.Fatal(err)
	}
	for _, f := range []struct{ name, data string }{
		{"old notes.txt", "drafts"},
		{"results <b>.csv", strings.Repeat("lru,91\n", 300)},
	} {
		if _, err := e.svc.Attach(ctx, p, "default", "files", core.AttachParams{Agent: "alice", Name: f.name, Data: []byte(f.data)}); err != nil {
			e.t.Fatal(err)
		}
	}
	if _, _, err := e.svc.DeleteAttachments(ctx, p, core.AttachmentDelete{SHA256: sha256hex("drafts")}); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.svc.Say(ctx, p, "default", "files", core.SayParams{Agent: "alice", Body: "Results attached."}); err != nil {
		e.t.Fatal(err)
	}
}

func TestDownloadFromTheViewer(t *testing.T) {
	e := newEnvWith(t, web.Options{}, core.Options{Attachments: true})
	e.seedFiles()
	_, page := e.get("/n/default/c/files")
	if !strings.Contains(page, `href="/n/default/c/files/files/4" download>results &lt;b&gt;.csv</a>`) ||
		!strings.Contains(page, "no longer downloadable: deleted") {
		t.Fatalf("page:\n%s", page)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, e.url+"/n/default/c/files/files/4", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := e.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(body) != 2100 || resp.Header.Get("Content-Type") != "application/octet-stream" ||
		!strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment;") || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("download: %d %d bytes %v", resp.StatusCode, len(body), resp.Header)
	}
	rr, err := e.svc.Read(t.Context(), core.Anonymous(), "default", "files", core.ReadParams{Tail: 1})
	if err != nil {
		t.Fatal(err)
	}
	if last := rr.Messages[0]; last.Author != "viewer" || last.Body != "viewer downloaded results <b>.csv (#4)" {
		t.Fatalf("last = %+v", last)
	}

	if status, _ := e.get("/n/default/c/files/files/3"); status != http.StatusGone {
		t.Fatalf("deleted file = %d", status)
	}
	if status, _ := e.get("/n/default/c/files/files/1"); status != http.StatusNotFound {
		t.Fatalf("not an attachment = %d", status)
	}
}

func TestNoDownloadsWithAttachmentsOff(t *testing.T) {
	e := newEnv(t, web.Options{})
	e.seed()
	if status, _ := e.get("/n/default/c/cache/files/1"); status != http.StatusNotFound {
		t.Fatalf("status = %d", status)
	}
}

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
