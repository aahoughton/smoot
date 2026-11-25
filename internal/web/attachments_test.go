package web_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math"
	"mime/multipart"
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
	if _, err := e.svc.Attach(ctx, p, "default", "files", core.AttachParams{Observer: "pat", Name: "take one.wav", Data: []byte("RIFF")}); err != nil {
		e.t.Fatal(err)
	}
}

// field is one part of an upload form; a file part has a file name.
type field struct{ name, file, value string }

// upload posts the composer's multipart form to conversation files.
func (e *env) upload(fields ...field) (int, string) {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range fields {
		var w io.Writer
		var err error
		if f.name == "file" {
			w, err = mw.CreateFormFile("file", f.file)
		} else {
			w, err = mw.CreateFormField(f.name)
		}
		if err == nil {
			_, err = io.WriteString(w, f.value)
		}
		if err != nil {
			e.t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		e.t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(e.t.Context(), http.MethodPost, e.url+"/n/default/c/files/files", &buf)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return e.do(req)
}

func (e *env) tail(n int) []core.Message {
	e.t.Helper()
	rr, err := e.svc.Read(e.t.Context(), core.Anonymous(), "default", "files", core.ReadParams{Tail: n})
	if err != nil {
		e.t.Fatal(err)
	}
	return rr.Messages
}

func TestUploadFromTheViewer(t *testing.T) {
	e := newEnvWith(t, web.Options{}, core.Options{Attachments: true})
	e.seedFiles()
	_, page := e.get("/n/default/c/files")
	for _, want := range []string{
		`action="/n/default/c/files/files" enctype="multipart/form-data"`,
		`<input type="file" name="file" multiple`,
		"up to 50.0 MiB each",
		`class="message attachment observer tone-`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("page lacks %q:\n%s", want, page)
		}
	}

	// Files become messages in order, then the comment.
	status, page := e.upload(field{name: "name", value: "sam"}, field{name: "body", value: "two takes"},
		field{"file", "a.wav", "first"}, field{"file", "b.wav", "second"})
	if status != http.StatusSeeOther {
		t.Fatalf("upload = %d\n%s", status, page)
	}
	got := e.tail(3)
	if got[0].Body != "a.wav" || got[1].Body != "b.wav" || got[2].Body != "two takes" || got[2].Kind != core.KindObserver ||
		got[0].Author != "sam" || got[0].Attachment == nil || !got[0].Attachment.Observer || got[1].Attachment.Size != 6 {
		t.Fatalf("log = %+v", got)
	}
	last := got[2].Seq

	// Files alone, and a comment alone with an empty file input.
	if status, _ := e.upload(field{name: "name", value: "sam"}, field{name: "body"}, field{"file", "c.wav", "third"}); status != http.StatusSeeOther {
		t.Fatalf("files alone = %d", status)
	}
	if status, _ := e.upload(field{name: "name", value: "sam"}, field{name: "body", value: "that is all"}, field{"file", "", ""}); status != http.StatusSeeOther {
		t.Fatalf("comment alone = %d", status)
	}
	if got := e.tail(2); got[0].Body != "c.wav" || got[1].Body != "that is all" || got[1].Seq != last+2 {
		t.Fatalf("log = %+v", got)
	}

	for _, tc := range []struct {
		name   string
		fields []field
		status int
		want   string
	}{
		{"nothing", []field{{name: "name", value: "sam"}, {name: "body", value: " "}}, http.StatusBadRequest, "write a comment or choose a file"},
		{"no name", []field{{"file", "x.wav", "x"}}, http.StatusBadRequest, "invalid"},
		{"a participant's name", []field{{name: "name", value: "alice"}, {"file", "x.wav", "x"}}, http.StatusBadRequest, "is a participant"},
		{"file before the name", []field{{"file", "x.wav", "x"}, {name: "name", value: "sam"}}, http.StatusBadRequest, "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, page := e.upload(tc.fields...)
			if status != tc.status || !strings.Contains(page, tc.want) {
				t.Fatalf("got %d, want %d and %q:\n%s", status, tc.status, tc.want, page)
			}
		})
	}

	// A file over the conversation's limit is refused; the one before it
	// stays, and the comment is kept in the box.
	if _, err := e.svc.SetAttachmentLimits(t.Context(), core.Anonymous(), "default", "files", core.AttachmentLimitsChange{MaxBytes: ptr(int64(4))}); err != nil {
		t.Fatal(err)
	}
	status, page = e.upload(field{name: "name", value: "sam"}, field{name: "body", value: "kept draft"},
		field{"file", "ok.wav", "fits"}, field{"file", "big.wav", "too big"})
	if status != http.StatusRequestEntityTooLarge || !strings.Contains(page, "big.wav is over 4 B") ||
		!strings.Contains(page, "(1 attached before it)") || !strings.Contains(page, ">kept draft</textarea>") {
		t.Fatalf("over the limit = %d\n%s", status, page)
	}
	if got := e.tail(1); got[0].Body != "ok.wav" {
		t.Fatalf("log = %+v", got)
	}

	// A quota near the largest number still takes uploads.
	if _, err := e.svc.SetAttachmentLimits(t.Context(), core.Anonymous(), "default", "files", core.AttachmentLimitsChange{QuotaBytes: ptr(int64(math.MaxInt64))}); err != nil {
		t.Fatal(err)
	}
	if status, page := e.upload(field{name: "name", value: "sam"}, field{"file", "q.wav", "q"}); status != http.StatusSeeOther {
		t.Fatalf("upload under a huge quota = %d\n%s", status, page)
	}

	// Once the conversation concludes, uploads are refused.
	if _, err := e.svc.Close(t.Context(), core.Anonymous(), "default", "files", core.CloseParams{Agent: "alice"}); err != nil {
		t.Fatal(err)
	}
	if status, page := e.upload(field{name: "name", value: "sam"}, field{"file", "l.wav", "late"}); status != http.StatusBadRequest || !strings.Contains(page, "has concluded") {
		t.Fatalf("after conclusion = %d\n%s", status, page)
	}
}

func TestNoUploadsWithAttachmentsOff(t *testing.T) {
	e := newEnv(t, web.Options{})
	e.seed()
	_, page := e.get("/n/default/c/cache")
	if strings.Contains(page, `type="file"`) || !strings.Contains(page, `action="/n/default/c/cache/observe"`) {
		t.Fatalf("page:\n%s", page)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, e.url+"/n/default/c/cache/files", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := e.do(req); status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
		t.Fatalf("upload with attachments off = %d", status)
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
