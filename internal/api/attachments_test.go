package api_test

import (
	"io"
	"log/slog"
	"mime"
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

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// tokenServer serves the API in auth mode token with opts and returns a
// client and the bootstrapped server admin's token.
func tokenServer(t *testing.T, opts core.Options) (*client, string) {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "smoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	opts.Policy = core.RolePolicy()
	svc := core.New(db, opts)
	tok, err := svc.Bootstrap(t.Context(), "root")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	api.Register(mux, svc, auth.Bearer{Service: svc}, api.Options{Logger: slog.New(slog.DiscardHandler)})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &client{t: t, url: srv.URL}, tok.Secret
}

const attachConv = "/v1/namespaces/default/conversations/c"

func TestAttachmentRoutesOnlyWhenEnabled(t *testing.T) {
	c := newServer(t, auth.None{}, core.Options{})
	c.ok("POST", "/v1/namespaces/default/conversations", map[string]any{"name": "c", "agent": "a"}, 201, nil)
	c.wantError("POST", attachConv+"/attachments?agent=a&name=x", "x", 404, "not_found")
	c.wantError("GET", attachConv+"/attachments/1?agent=a", nil, 404, "not_found")
	c.wantError("GET", "/v1/attachments", nil, 404, "not_found")
}

func TestAttachOverHTTP(t *testing.T) {
	c := newServer(t, auth.None{}, core.Options{Attachments: true, AttachmentQuota: 4, AttachmentCap: 8})
	c.ok("POST", "/v1/namespaces/default/conversations", map[string]any{"name": "c", "agent": "a"}, 201, nil)
	c.ok("POST", attachConv+"/join", map[string]any{"agent": "b"}, 200, nil)

	var res core.AttachResult
	c.ok("POST", attachConv+"/attachments?agent=a&name=%22q%22.txt", "abc", 201, &res)
	if a := res.Message.Attachment; a == nil || a.Name != `"q".txt` || a.Size != 3 {
		t.Fatalf("attached %+v", res.Message)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, c.url+attachConv+"/attachments/"+itoa(res.Message.Seq)+"?agent=b", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
	if resp.StatusCode != http.StatusOK || string(body) != "abc" || err != nil || params["filename"] != `"q".txt` ||
		resp.Header.Get("X-Smoot-Sha256") != res.Message.Attachment.SHA256 || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("download: %d %q %v %v", resp.StatusCode, body, resp.Header, err)
	}

	for _, tc := range []struct {
		name, method, path string
		body               any
		status             int
		code               string
	}{
		{"no name", "POST", attachConv + "/attachments?agent=a", "x", 400, "invalid"},
		{"no agent", "POST", attachConv + "/attachments?name=x", "x", 400, "invalid"},
		{"path in name", "POST", attachConv + "/attachments?agent=a&name=..%2Fx", "x", 400, "invalid"},
		{"over quota", "POST", attachConv + "/attachments?agent=a&name=y", "xy", 507, "attachment_quota"},
		{"too large", "POST", attachConv + "/attachments?agent=a&name=big", strings.Repeat("x", core.MaxAttachmentBytes+1), 413, "attachment_too_large"},
		{"bad seq", "GET", attachConv + "/attachments/zero?agent=b", nil, 400, "invalid"},
		{"not an attachment", "GET", attachConv + "/attachments/1?agent=b", nil, 404, "attachment_not_found"},
		{"not a participant", "GET", attachConv + "/attachments/" + itoa(res.Message.Seq) + "?agent=zed", nil, 409, "not_participant"},
		{"observer without a name", "GET", attachConv + "/attachments/" + itoa(res.Message.Seq), nil, 400, "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			(&client{t: t, url: c.url}).wantError(tc.method, tc.path, tc.body, tc.status, tc.code)
		})
	}

	var del struct{ Blobs, Bytes int64 }
	c.ok("DELETE", attachConv+"/attachments", nil, 200, &del)
	if del.Blobs != 1 || del.Bytes != 3 {
		t.Fatalf("deleted %+v", del)
	}
	c.wantError("GET", attachConv+"/attachments/"+itoa(res.Message.Seq)+"?agent=b", nil, 410, "attachment_gone")
}

func TestAttachmentAdminRoutesNeedServerAdmin(t *testing.T) {
	c, rootToken := tokenServer(t, core.Options{Attachments: true})
	root := c.as(rootToken)
	var issued struct{ Secret string }
	root.ok("POST", "/v1/accounts", map[string]any{"name": "pat"}, 201, nil)
	root.ok("POST", "/v1/accounts/pat/tokens", map[string]any{"label": "t"}, 201, &issued)
	root.ok("PUT", "/v1/namespaces/default/members/pat", map[string]any{"role": "admin"}, 200, nil)
	pat := c.as(issued.Secret)
	pat.wantError("GET", "/v1/attachments", nil, 403, "forbidden")
	pat.wantError("DELETE", "/v1/attachments/"+strings.Repeat("0", 64), nil, 403, "forbidden")
	pat.wantError("DELETE", attachConv+"/attachments", nil, 403, "forbidden")
	root.ok("GET", "/v1/attachments", nil, 200, nil)
}
