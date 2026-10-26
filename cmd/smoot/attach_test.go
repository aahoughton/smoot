package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aahoughton/smoot/internal/core"
	"github.com/aahoughton/smoot/internal/store"
)

func TestAttachAndGet(t *testing.T) {
	url, token, svc := tokenServerWith(t, core.Options{Attachments: true})
	ctx := t.Context()
	env := map[string]string{"SMOOT_TOKEN": token}
	cli := func(args ...string) cliResult { return runCLIInput(ctx, url, "", env, args...) }
	root, err := svc.AuthenticateToken(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Start(ctx, root, "default", core.StartParams{Name: "c", Agent: "a", Join: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Join(ctx, root, "default", "c", core.JoinParams{Agent: "b"}); err != nil {
		t.Fatal(err)
	}

	src := filepath.Join(t.TempDir(), "results.csv")
	if err := os.WriteFile(src, []byte("a,b\n1,2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := cli("attach", "c", src, "--agent", "a")
	if r.code != exitOK || !strings.HasPrefix(r.stdout, "attached results.csv as #4 (8 bytes, sha256 ") || !strings.Contains(r.stdout, "c: open; you: active") {
		t.Fatalf("attach = %+v", r)
	}
	if r := cli("attach", "c", src, "--agent", "a", "--name", "renamed.csv"); r.code != exitOK || !strings.Contains(r.stdout, "attached renamed.csv as #5") {
		t.Fatalf("attach --name = %+v", r)
	}

	dir := t.TempDir()
	r = cli("get", "c", "4", "--agent", "b", "-o", dir)
	if r.code != exitOK || !strings.HasPrefix(r.stdout, "wrote "+filepath.Join(dir, "results.csv")+" (8 bytes") {
		t.Fatalf("get = %+v", r)
	}
	got, err := os.ReadFile(filepath.Join(dir, "results.csv"))
	if err != nil || string(got) != "a,b\n1,2\n" {
		t.Fatalf("file = %q, %v", got, err)
	}
	if info, err := os.Stat(filepath.Join(dir, "results.csv")); err != nil || info.Mode().Perm()&0o111 != 0 {
		t.Fatalf("mode = %v, %v", info.Mode(), err)
	}

	// It never overwrites, and a symlink in the way is not followed.
	if r := cli("get", "c", "4", "--agent", "b", "-o", dir); r.code != exitError || !strings.Contains(r.stderr, "already exists") {
		t.Fatalf("second get = %+v", r)
	}
	outside := filepath.Join(t.TempDir(), "target")
	if err := os.Symlink(outside, filepath.Join(dir, "renamed.csv")); err != nil {
		t.Fatal(err)
	}
	if r := cli("get", "c", "5", "--agent", "b", "-o", dir); r.code != exitError {
		t.Fatalf("get over a symlink = %+v", r)
	}
	if _, err := os.Lstat(outside); !os.IsNotExist(err) {
		t.Fatalf("wrote through the symlink: %v", err)
	}

	big := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(big, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(big, core.MaxAttachmentBytes+1); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		code int
		want string
	}{
		{"bad name", []string{"attach", "c", src, "--agent", "a", "--name", "../x"}, exitUsage, "pick another with --name"},
		{"no file", []string{"attach", "c", "--agent", "a"}, exitUsage, "want a conversation name and a file"},
		{"too large", []string{"attach", "c", big, "--agent", "a"}, exitError, "is over 10485760 bytes"},
		{"directory", []string{"attach", "c", dir, "--agent", "a"}, exitError, "not a regular file"},
		{"missing file", []string{"attach", "c", filepath.Join(dir, "nope"), "--agent", "a"}, exitError, "no such file"},
		{"not a participant", []string{"attach", "c", src, "--agent", "zed"}, exitError, "not_participant"},
		{"bad seq", []string{"get", "c", "x", "--agent", "b"}, exitUsage, "not a positive number"},
		{"no dir", []string{"get", "c", "4", "--agent", "b", "-o", filepath.Join(dir, "nope")}, exitError, "no such file"},
		{"not an attachment", []string{"get", "c", "1", "--agent", "b", "-o", t.TempDir()}, exitError, "attachment_not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runCLIInput(ctx, url, "", env, tc.args...)
			if r.code != tc.code || !strings.Contains(r.stderr, tc.want) {
				t.Fatalf("got %+v, want exit %d and %q", r, tc.code, tc.want)
			}
		})
	}
}

func TestSweepAttachments(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "smoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	svc := core.New(db, core.Options{Attachments: true, AttachmentExpiry: time.Hour, Now: func() time.Time { return now }})
	ctx := t.Context()
	p := core.Anonymous()
	if _, err := svc.Start(ctx, p, "default", core.StartParams{Name: "c", Agent: "a", Join: true, End: core.EndCoordinator}); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Attach(ctx, p, "default", "c", core.AttachParams{Agent: "a", Name: "x", Data: []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Close(ctx, p, "default", "c", core.CloseParams{Agent: "a"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	var logs bytes.Buffer
	sweepAttachments(ctx, svc, slog.New(slog.NewTextHandler(&logs, nil)))
	if !strings.Contains(logs.String(), "attachments expired") || !strings.Contains(logs.String(), "files=1") {
		t.Fatalf("logs = %q", logs.String())
	}
	if _, err := svc.Fetch(ctx, p, "default", "c", core.FetchParams{Agent: "a", Seq: res.Message.Seq}); core.CodeOf(err) != core.CodeAttachmentGone {
		t.Fatalf("fetch after expiry: %v", err)
	}
}
