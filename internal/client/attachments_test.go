package client

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFetchChecksWhatTheServerSends serves downloads that a client must
// not trust: a file name with a path in it, and bytes that do not match
// their sha256.
func TestFetchChecksWhatTheServerSends(t *testing.T) {
	body := []byte("payload")
	sum := sha256.Sum256(body)
	good := hex.EncodeToString(sum[:])
	for _, tc := range []struct {
		name, disposition, sha, want string
	}{
		{"ok", `attachment; filename="p.bin"`, good, ""},
		{"path in name", `attachment; filename="../p.bin"`, good, "path separator"},
		{"no name", `attachment`, good, "file name is empty"},
		{"no disposition", ``, good, "no usable file name"},
		{"wrong sha", `attachment; filename="p.bin"`, strings.Repeat("0", 64), "does not match its sha256"},
		{"no sha", `attachment; filename="p.bin"`, "", "does not match its sha256"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/namespaces/default/conversations/c/attachments/3" || r.URL.Query().Get("agent") != "a" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Disposition", tc.disposition)
				w.Header().Set("X-Smoot-Sha256", tc.sha)
				_, _ = w.Write(body)
			}))
			t.Cleanup(srv.Close)
			c, err := New(srv.URL, "")
			if err != nil {
				t.Fatal(err)
			}
			d, err := c.Fetch(t.Context(), "default", "c", "a", 3)
			if tc.want == "" {
				if err != nil || d.Attachment.Name != "p.bin" || string(d.Data) != "payload" || d.Attachment.SHA256 != good {
					t.Fatalf("download = %+v, %v", d.Attachment, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestAttachSendsRawBytes(t *testing.T) {
	var got []byte
	var query, ctype string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		query, ctype = r.URL.RawQuery, r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"message":{"seq":4,"kind":"attachment"},"standing":{}}`))
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Attach(t.Context(), "default", "c", "a", "a b.txt", []byte{0, 1, 2})
	if err != nil || res.Message.Seq != 4 || string(got) != "\x00\x01\x02" || ctype != "application/octet-stream" || query != "agent=a&name=a+b.txt" {
		t.Fatalf("res=%+v err=%v body=%q type=%q query=%q", res, err, got, ctype, query)
	}
}
