package core_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aahoughton/smoot/internal/core"
)

func attachOn(opts core.Options) core.Options {
	opts.Attachments = true
	return opts
}

func (f *fixture) attach(t *testing.T, conv, agent, name string, data []byte) core.AttachResult {
	t.Helper()
	return must[core.AttachResult](t)(f.svc.Attach(t.Context(), f.p, "default", conv, core.AttachParams{Agent: agent, Name: name, Data: data}))
}

func (f *fixture) fetch(t *testing.T, conv string, fp core.FetchParams) core.Download {
	t.Helper()
	return must[core.Download](t)(f.svc.Fetch(t.Context(), f.p, "default", conv, fp))
}

func (f *fixture) closeConv(t *testing.T, conv, agent string) {
	t.Helper()
	must[core.Standing](t)(f.svc.Close(t.Context(), f.p, "default", conv, core.CloseParams{Agent: agent}))
}

func sha(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

func TestAttachmentsOff(t *testing.T) {
	f := newFixture(t, core.Options{})
	f.start(t, "c", core.EndAgreement, "a")
	_, err := f.svc.Attach(t.Context(), f.p, "default", "c", core.AttachParams{Agent: "a", Name: "x", Data: []byte("x")})
	wantCode(t, err, core.CodeForbidden)
	_, err = f.svc.Fetch(t.Context(), f.p, "default", "c", core.FetchParams{Agent: "a", Seq: 1})
	wantCode(t, err, core.CodeForbidden)
	if f.svc.AttachmentsEnabled() {
		t.Fatal("enabled")
	}
}

func TestAttachAndFetch(t *testing.T) {
	f := newFixture(t, attachOn(core.Options{}))
	ctx := t.Context()
	f.start(t, "c", core.EndAgreement, "a", "b")
	f.read(t, "c", "a")
	data := []byte("col1,col2\n1,2\n")

	res := f.attach(t, "c", "a", "data.csv", data)
	m := res.Message
	want := core.Attachment{Name: "data.csv", Size: int64(len(data)), SHA256: sha(data)}
	if m.Kind != core.KindAttachment || m.Author != "a" || m.Body != "data.csv" || m.Attachment == nil || *m.Attachment != want {
		t.Fatalf("message = %+v", m)
	}
	if res.Standing.Unread != 0 {
		t.Fatalf("uploader standing = %+v", res.Standing)
	}

	// The record is a message from someone else for b, like a join line.
	rr := f.read(t, "c", "b")
	last := rr.Messages[len(rr.Messages)-1]
	if last.Seq != m.Seq || last.Attachment == nil || *last.Attachment != want {
		t.Fatalf("b read %+v", rr.Messages)
	}

	d := f.fetch(t, "c", core.FetchParams{Agent: "b", Seq: m.Seq})
	if !bytes.Equal(d.Data, data) || d.Attachment != want {
		t.Fatalf("download = %+v", d.Attachment)
	}
	// The first download is announced once; a repeat is not.
	f.fetch(t, "c", core.FetchParams{Agent: "b", Seq: m.Seq})
	rr = f.read(t, "c", "a")
	var lines []string
	for _, msg := range rr.Messages {
		if msg.Kind == core.KindSystem {
			lines = append(lines, msg.Author+": "+msg.Body)
		}
	}
	if len(lines) != 1 || lines[0] != "b: b downloaded data.csv (#"+itoa(m.Seq)+")" {
		t.Fatalf("system lines = %q", lines)
	}

	for _, tc := range []struct {
		name  string
		fp    core.FetchParams
		code  core.Code
		convo string
	}{
		{"not an attachment", core.FetchParams{Agent: "b", Seq: 1}, core.CodeAttachmentNotFound, "c"},
		{"past the log", core.FetchParams{Agent: "b", Seq: 999}, core.CodeAttachmentNotFound, "c"},
		{"never joined", core.FetchParams{Agent: "zed", Seq: m.Seq}, core.CodeNotParticipant, "c"},
		{"no conversation", core.FetchParams{Agent: "b", Seq: m.Seq}, core.CodeConversationNotFound, "nope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.Fetch(ctx, f.p, "default", tc.convo, tc.fp)
			wantCode(t, err, tc.code)
		})
	}
}

func TestAttachRules(t *testing.T) {
	f := newFixture(t, attachOn(core.Options{}))
	ctx := t.Context()
	f.start(t, "c", core.EndAgreement, "a", "b")
	for _, tc := range []struct {
		name, agent, file string
		size              int
		code              core.Code
	}{
		{"never joined", "zed", "x", 1, core.CodeNotParticipant},
		{"empty name", "a", "", 1, core.CodeInvalid},
		{"slash", "a", "../x", 1, core.CodeInvalid},
		{"backslash", "a", `a\b`, 1, core.CodeInvalid},
		{"dot dot", "a", "..", 1, core.CodeInvalid},
		{"control", "a", "a\nb", 1, core.CodeInvalid},
		{"long name", "a", strings.Repeat("x", core.MaxFileNameBytes+1), 1, core.CodeInvalid},
		{"bad utf8", "a", "\xff", 1, core.CodeInvalid},
		{"too large", "a", "x", core.MaxAttachmentBytes + 1, core.CodeAttachmentTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.Attach(ctx, f.p, "default", "c", core.AttachParams{Agent: tc.agent, Name: tc.file, Data: make([]byte, tc.size)})
			wantCode(t, err, tc.code)
		})
	}
	// Names may hold spaces and non-ASCII letters, and files may be empty.
	f.attach(t, "c", "a", "résumé final.txt", nil)

	// Attaching leaves a concluded agent concluded.
	must[core.Standing](t)(f.svc.Conclude(ctx, f.p, "default", "c", core.ConcludeParams{Agent: "a", Body: "AGREED: #1"}))
	f.attach(t, "c", "a", "late.txt", []byte("x"))
	if p := participant(t, f.get(t, "c"), "a"); p.State != core.StateConcluded {
		t.Fatalf("a = %+v", p)
	}

	// A concluded conversation takes no uploads.
	must[core.Standing](t)(f.svc.Conclude(ctx, f.p, "default", "c", core.ConcludeParams{Agent: "b", Body: "AGREED: #1"}))
	_, err := f.svc.Attach(ctx, f.p, "default", "c", core.AttachParams{Agent: "a", Name: "x", Data: []byte("x")})
	wantCode(t, err, core.CodeConversationConcluded)
}

func TestAttachSameBytesStoredOnce(t *testing.T) {
	f := newFixture(t, attachOn(core.Options{AttachmentQuota: 10, AttachmentCap: 100}))
	f.start(t, "c", core.EndCoordinator, "a", "b")
	data := []byte("12345678")
	first := f.attach(t, "c", "a", "one.bin", data)
	// The same bytes under another name and uploader cost nothing more.
	second := f.attach(t, "c", "b", "two.bin", data)
	if second.Message.Attachment.SHA256 != first.Message.Attachment.SHA256 {
		t.Fatalf("second = %+v", second.Message)
	}
	_, err := f.svc.Attach(t.Context(), f.p, "default", "c", core.AttachParams{Agent: "a", Name: "more.bin", Data: []byte("abc")})
	wantCode(t, err, core.CodeAttachmentQuota)

	// One blob, one download line per downloader, whichever record it is
	// fetched through.
	f.fetch(t, "c", core.FetchParams{Agent: "b", Seq: first.Message.Seq})
	d := f.fetch(t, "c", core.FetchParams{Agent: "b", Seq: second.Message.Seq})
	if d.Attachment.Name != "two.bin" {
		t.Fatalf("download = %+v", d.Attachment)
	}
	rep := must[core.AttachmentReport](t)(f.svc.AttachmentStorage(t.Context(), f.p))
	if len(rep.Conversations) != 1 || rep.Conversations[0].Blobs != 1 || rep.Bytes != int64(len(data)) {
		t.Fatalf("report = %+v", rep)
	}
	n := 0
	for _, m := range f.read(t, "c", "a").Messages {
		if m.Kind == core.KindSystem && strings.Contains(m.Body, "downloaded") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d download lines", n)
	}
}

func TestAttachEvictsFromConcludedConversations(t *testing.T) {
	f := newFixture(t, attachOn(core.Options{AttachmentQuota: 10, AttachmentCap: 20}))
	ctx := t.Context()
	f.start(t, "old", core.EndCoordinator, "a")
	f.start(t, "older", core.EndCoordinator, "a")
	f.start(t, "live", core.EndCoordinator, "a")
	oldest := f.attach(t, "older", "a", "older.bin", bytes.Repeat([]byte("o"), 8))
	f.clock.Advance(time.Minute)
	old := f.attach(t, "old", "a", "old.bin", bytes.Repeat([]byte("p"), 8))
	f.closeConv(t, "older", "a")
	f.closeConv(t, "old", "a")

	// 16 of 20 bytes are stored; 6 more evict only the least recently
	// uploaded blob.
	f.attach(t, "live", "a", "new.bin", bytes.Repeat([]byte("n"), 6))
	_, err := f.svc.Fetch(ctx, f.p, "default", "older", core.FetchParams{Agent: "a", Seq: oldest.Message.Seq})
	wantCode(t, err, core.CodeAttachmentGone)
	f.fetch(t, "old", core.FetchParams{Agent: "a", Seq: old.Message.Seq})
	rr := must[core.ReadResult](t)(f.svc.Read(ctx, f.p, "default", "older", core.ReadParams{}))
	for _, m := range rr.Messages {
		if m.Kind == core.KindAttachment && (m.Attachment == nil || m.Attachment.Gone != core.GoneEvicted) {
			t.Fatalf("older record = %+v", m.Attachment)
		}
	}

	// When evicting everything concluded is not enough, nothing is
	// evicted: 19 bytes stored, 8 of them in a concluded conversation.
	f.start(t, "live2", core.EndCoordinator, "a")
	f.start(t, "live3", core.EndCoordinator, "a")
	f.attach(t, "live2", "a", "fill.bin", bytes.Repeat([]byte("f"), 5))
	_, err = f.svc.Attach(ctx, f.p, "default", "live3", core.AttachParams{Agent: "a", Name: "big.bin", Data: bytes.Repeat([]byte("b"), 10)})
	wantCode(t, err, core.CodeAttachmentCap)
	f.fetch(t, "old", core.FetchParams{Agent: "a", Seq: old.Message.Seq})
}

func TestAttachmentsExpireAfterConclusion(t *testing.T) {
	f := newFixture(t, attachOn(core.Options{AttachmentExpiry: time.Hour}))
	ctx := t.Context()
	f.start(t, "c", core.EndCoordinator, "a", "b")
	f.start(t, "open", core.EndCoordinator, "a")
	m := f.attach(t, "c", "a", "x.txt", []byte("x")).Message
	f.attach(t, "open", "a", "y.txt", []byte("y"))
	f.closeConv(t, "c", "a")
	all := func() int {
		return len(must[core.ReadResult](t)(f.svc.Read(ctx, f.p, "default", "c", core.ReadParams{})).Messages)
	}
	before := all()

	// Downloads after conclusion work and are recorded without a line.
	f.fetch(t, "c", core.FetchParams{Agent: "b", Seq: m.Seq})
	if got := all(); got != before {
		t.Fatalf("log grew from %d to %d after conclusion", before, got)
	}

	f.clock.Advance(59 * time.Minute)
	if n := must[int64](t)(f.svc.ExpireAttachments(ctx)); n != 0 {
		t.Fatalf("expired %d early", n)
	}
	f.clock.Advance(time.Minute)
	if n := must[int64](t)(f.svc.ExpireAttachments(ctx)); n != 1 {
		t.Fatalf("expired %d, want 1 (open conversations keep theirs)", n)
	}
	_, err := f.svc.Fetch(ctx, f.p, "default", "c", core.FetchParams{Agent: "b", Seq: m.Seq})
	wantCode(t, err, core.CodeAttachmentGone)
}

func TestDeleteAttachments(t *testing.T) {
	f := newFixture(t, attachOn(core.Options{}))
	ctx := t.Context()
	f.start(t, "c", core.EndCoordinator, "a")
	f.start(t, "d", core.EndCoordinator, "a")
	shared := []byte("shared")
	m1 := f.attach(t, "c", "a", "s.txt", shared).Message
	f.attach(t, "d", "a", "s.txt", shared)
	f.attach(t, "d", "a", "own.txt", []byte("own"))

	blobs, size := must2(t)(f.svc.DeleteAttachments(ctx, f.p, core.AttachmentDelete{SHA256: strings.ToUpper(sha(shared))}))
	if blobs != 2 || size != 2*int64(len(shared)) {
		t.Fatalf("deleted %d blobs, %d bytes", blobs, size)
	}
	_, err := f.svc.Fetch(ctx, f.p, "default", "c", core.FetchParams{Agent: "a", Seq: m1.Seq})
	wantCode(t, err, core.CodeAttachmentGone)

	blobs, _ = must2(t)(f.svc.DeleteAttachments(ctx, f.p, core.AttachmentDelete{Namespace: "default", Conversation: "d"}))
	if blobs != 1 {
		t.Fatalf("deleted %d blobs", blobs)
	}
	rep := must[core.AttachmentReport](t)(f.svc.AttachmentStorage(ctx, f.p))
	if rep.Bytes != 0 || len(rep.Conversations) != 2 || rep.Conversations[0].Gone == 0 {
		t.Fatalf("report = %+v", rep)
	}

	for _, d := range []core.AttachmentDelete{{}, {SHA256: "ab", Namespace: "default", Conversation: "c"}, {Namespace: "default"}} {
		_, _, err := f.svc.DeleteAttachments(ctx, f.p, d)
		wantCode(t, err, core.CodeInvalid)
	}
	_, _, err = f.svc.DeleteAttachments(ctx, f.p, core.AttachmentDelete{Namespace: "default", Conversation: "nope"})
	wantCode(t, err, core.CodeConversationNotFound)
}

func TestFetchAsObserver(t *testing.T) {
	f := newFixture(t, attachOn(core.Options{}))
	f.start(t, "c", core.EndCoordinator, "a")
	m := f.attach(t, "c", "a", "x.txt", []byte("x")).Message
	f.fetch(t, "c", core.FetchParams{Observer: "pat", Seq: m.Seq})
	msgs := f.read(t, "c", "a").Messages
	if last := msgs[len(msgs)-1]; last.Kind != core.KindSystem || last.Author != "pat" || last.Body != "pat downloaded x.txt (#"+itoa(m.Seq)+")" {
		t.Fatalf("last = %+v", last)
	}
	_, err := f.svc.Fetch(t.Context(), f.p, "default", "c", core.FetchParams{Observer: "Not Valid", Seq: m.Seq})
	wantCode(t, err, core.CodeInvalid)
}

func TestFetchAsObserverUsesAccountName(t *testing.T) {
	f := newTokenFixtureWith(t, attachOn(core.Options{}))
	ctx := t.Context()
	pat := f.account(t, "pat")
	viewer := f.account(t, "vic")
	must[core.Membership](t)(f.svc.SetMembership(ctx, f.admin, "default", "pat", core.RoleMember))
	must[core.Membership](t)(f.svc.SetMembership(ctx, f.admin, "default", "vic", core.RoleViewer))
	must[core.Conversation](t)(f.svc.Start(ctx, pat, "default", core.StartParams{Name: "c", Agent: "p", Join: true}))
	m := must[core.AttachResult](t)(f.svc.Attach(ctx, pat, "default", "c", core.AttachParams{Agent: "p", Name: "x.txt", Data: []byte("x")})).Message
	must[core.Download](t)(f.svc.Fetch(ctx, viewer, "default", "c", core.FetchParams{Observer: "someone-else", Seq: m.Seq}))
	rr := must[core.ReadResult](t)(f.svc.Read(ctx, pat, "default", "c", core.ReadParams{}))
	if last := rr.Messages[len(rr.Messages)-1]; last.Author != "vic" {
		t.Fatalf("last = %+v", last)
	}
	// A viewer cannot upload.
	_, err := f.svc.Attach(ctx, viewer, "default", "c", core.AttachParams{Agent: "p", Name: "y", Data: []byte("y")})
	wantCode(t, err, core.CodeForbidden)
}

func TestDumpLoadKeepsAttachmentRecordsWithoutBytes(t *testing.T) {
	f := newFixture(t, attachOn(core.Options{}))
	ctx := t.Context()
	f.start(t, "c", core.EndCoordinator, "a", "b")
	data := []byte("payload")
	m := f.attach(t, "c", "a", "p.bin", data).Message
	f.fetch(t, "c", core.FetchParams{Agent: "b", Seq: m.Seq})

	for _, ns := range []string{"", "default"} {
		t.Run("ns="+ns, func(t *testing.T) {
			var buf bytes.Buffer
			if err := f.svc.Dump(ctx, f.p, ns, &buf); err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(buf.Bytes(), []byte(`"data"`)) {
				t.Fatalf("dump holds bytes:\n%s", buf.String())
			}
			db, err := loadInto(t, buf.Bytes(), t.TempDir()+"/loaded.db")
			if err != nil {
				t.Fatal(err)
			}
			svc := core.New(db, attachOn(core.Options{Now: f.clock.Now}))
			rr := must[core.ReadResult](t)(svc.Read(ctx, f.p, "default", "c", core.ReadParams{}))
			var got *core.Attachment
			for _, msg := range rr.Messages {
				if msg.Seq == m.Seq {
					got = msg.Attachment
				}
			}
			want := core.Attachment{Name: "p.bin", Size: int64(len(data)), SHA256: sha(data), Gone: core.GoneMissing}
			if got == nil || *got != want {
				t.Fatalf("loaded record = %+v", got)
			}
			if n := count(t, db, `SELECT count(*) FROM attachment_downloads`); n != 1 {
				t.Fatalf("%d downloads", n)
			}
			_, err = svc.Fetch(ctx, f.p, "default", "c", core.FetchParams{Agent: "b", Seq: m.Seq})
			wantCode(t, err, core.CodeAttachmentGone)
			// Uploading the same bytes again stores them again.
			must[core.AttachResult](t)(svc.Attach(ctx, f.p, "default", "c", core.AttachParams{Agent: "a", Name: "again.bin", Data: data}))
			must[core.Download](t)(svc.Fetch(ctx, f.p, "default", "c", core.FetchParams{Agent: "b", Seq: m.Seq}))
		})
	}
}

func TestLoadRejectsBadAttachments(t *testing.T) {
	f := newFixture(t, attachOn(core.Options{}))
	f.start(t, "c", core.EndCoordinator, "a")
	f.attach(t, "c", "a", "p.bin", []byte("payload"))
	var buf bytes.Buffer
	if err := f.svc.Dump(t.Context(), f.p, "", &buf); err != nil {
		t.Fatal(err)
	}
	good := buf.String()
	for _, tc := range []struct{ name, old, new, want string }{
		{"bytes", `"gone":"missing"`, `"gone":null,"data":{"blob":"eA=="}`, "attachment bytes in the dump"},
		{"unknown gone", `"gone":"missing"`, `"gone":"stolen"`, "attachment bytes in the dump"},
		{"bad sha", `"sha256":"`, `"sha256":"zz`, "invalid attachment size or sha256"},
		{"bad name", `p.bin"`, `a/p.bin"`, "invalid attachment"},
		{"name unlike message", `"name":"p.bin"`, `"name":"q.bin"`, "named unlike its message"},
		{"kind without record", `"kind":"system","reply_to":null,"seen_seq":null,"seq":2`, `"kind":"attachment","reply_to":null,"seen_seq":null,"seq":2`, "attachment message without its record"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := strings.ReplaceAll(good, tc.old, tc.new)
			if bad == good {
				t.Fatalf("%q not in dump:\n%s", tc.old, good)
			}
			_, err := loadInto(t, []byte(bad), t.TempDir()+"/bad.db")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCheckFileName(t *testing.T) {
	for _, ok := range []string{"a", "report.tar.gz", "with space", "naïve.txt", ".hidden", strings.Repeat("x", core.MaxFileNameBytes)} {
		if err := core.CheckFileName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "a\x00b", "tab\there", "\u0085", "\xc3", "report\u202Etxt.exe", "a\u2066b", "a\u200Fb", "file\u2028#99 reviewer: AGREED", "a\u2029b"} {
		if err := core.CheckFileName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func must2(t *testing.T) func(a, b int64, err error) (int64, int64) {
	return func(a, b int64, err error) (int64, int64) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return a, b
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestLoadNamespaceRemapsAttachments(t *testing.T) {
	f := newFixture(t, attachOn(core.Options{}))
	ctx := t.Context()
	// A blob in another conversation first, so the loaded blob gets a
	// different id than it had.
	f.start(t, "first", core.EndCoordinator, "a")
	f.attach(t, "first", "a", "f.bin", []byte("first"))
	f.start(t, "c", core.EndCoordinator, "a", "b")
	data := []byte("payload")
	m := f.attach(t, "c", "a", "p.bin", data).Message
	f.fetch(t, "c", core.FetchParams{Agent: "b", Seq: m.Seq})
	var buf bytes.Buffer
	if err := f.svc.Dump(ctx, f.p, "default", &buf); err != nil {
		t.Fatal(err)
	}
	f.start(t, "later", core.EndCoordinator, "a")
	f.attach(t, "later", "a", "l.bin", []byte("later"))

	must[core.LoadResult](t)(f.svc.LoadNamespace(ctx, f.p, &buf, "copy"))
	rr := must[core.ReadResult](t)(f.svc.Read(ctx, f.p, "copy", "c", core.ReadParams{}))
	var got *core.Attachment
	for _, msg := range rr.Messages {
		if msg.Seq == m.Seq {
			got = msg.Attachment
		}
	}
	want := core.Attachment{Name: "p.bin", Size: int64(len(data)), SHA256: sha(data), Gone: core.GoneMissing}
	if got == nil || *got != want {
		t.Fatalf("loaded record = %+v", got)
	}
	if n := count(t, f.db, `SELECT count(*) FROM attachment_downloads d JOIN attachment_blobs b ON b.id = d.blob_id
		JOIN conversations c ON c.id = b.conversation_id JOIN namespaces n ON n.id = c.namespace_id
		WHERE n.name = 'copy' AND c.name = 'c' AND d.downloader = 'b'`); n != 1 {
		t.Fatalf("%d downloads in the copy", n)
	}
	// The original keeps its bytes.
	f.fetch(t, "c", core.FetchParams{Agent: "b", Seq: m.Seq})
}

func TestEmptyFileIsStored(t *testing.T) {
	f := newFixture(t, attachOn(core.Options{AttachmentExpiry: time.Hour}))
	ctx := t.Context()
	f.start(t, "c", core.EndCoordinator, "a")
	m := f.attach(t, "c", "a", "empty.txt", nil).Message
	if d := f.fetch(t, "c", core.FetchParams{Agent: "a", Seq: m.Seq}); len(d.Data) != 0 || d.Attachment.Gone != "" {
		t.Fatalf("download = %+v", d.Attachment)
	}
	rep := must[core.AttachmentReport](t)(f.svc.AttachmentStorage(ctx, f.p))
	if len(rep.Conversations) != 1 || rep.Conversations[0].Blobs != 1 || rep.Conversations[0].Gone != 0 {
		t.Fatalf("report = %+v", rep)
	}
	f.closeConv(t, "c", "a")
	f.clock.Advance(2 * time.Hour)
	if n := must[int64](t)(f.svc.ExpireAttachments(ctx)); n != 1 {
		t.Fatalf("expired %d", n)
	}
	f.start(t, "d", core.EndCoordinator, "a")
	f.attach(t, "d", "a", "empty.txt", []byte{})
	if blobs, _ := must2(t)(f.svc.DeleteAttachments(ctx, f.p, core.AttachmentDelete{Namespace: "default", Conversation: "d"})); blobs != 1 {
		t.Fatalf("deleted %d", blobs)
	}
}

// An elapsed timebox is concluded for storage even before anything records
// it: its files may be evicted and expire, counted from its deadline.
func TestElapsedTimeboxFreesStorage(t *testing.T) {
	f := newFixture(t, attachOn(core.Options{AttachmentQuota: 4, AttachmentCap: 4, AttachmentExpiry: time.Hour}))
	ctx := t.Context()
	box := func(name string) {
		must[core.Conversation](t)(f.svc.Start(ctx, f.p, "default", core.StartParams{
			Name: name, Agent: "a", Join: true, End: core.EndTimebox, EndsAt: ptr(f.clock.Now().Add(time.Minute)),
		}))
	}
	box("t1")
	m := f.attach(t, "t1", "a", "x", []byte("1234")).Message
	f.clock.Advance(2 * time.Minute)
	f.start(t, "next", core.EndCoordinator, "a")
	f.attach(t, "next", "a", "y", []byte("5678"))
	_, err := f.svc.Fetch(ctx, f.p, "default", "t1", core.FetchParams{Agent: "a", Seq: m.Seq})
	wantCode(t, err, core.CodeAttachmentGone)

	f2 := newFixture(t, attachOn(core.Options{AttachmentExpiry: time.Hour}))
	must[core.Conversation](t)(f2.svc.Start(ctx, f2.p, "default", core.StartParams{
		Name: "t2", Agent: "a", Join: true, End: core.EndTimebox, EndsAt: ptr(f2.clock.Now().Add(time.Minute)),
	}))
	f2.attach(t, "t2", "a", "x", []byte("x"))
	f2.clock.Advance(time.Hour)
	if n := must[int64](t)(f2.svc.ExpireAttachments(ctx)); n != 0 {
		t.Fatalf("expired %d before an hour past the deadline", n)
	}
	f2.clock.Advance(time.Minute)
	if n := must[int64](t)(f2.svc.ExpireAttachments(ctx)); n != 1 {
		t.Fatalf("expired %d, want 1", n)
	}

	// A read after the deadline records the conclusion late; retention
	// still counts from the deadline. An early close counts from the close.
	f3 := newFixture(t, attachOn(core.Options{AttachmentExpiry: time.Hour}))
	for _, name := range []string{"read", "closed"} {
		must[core.Conversation](t)(f3.svc.Start(ctx, f3.p, "default", core.StartParams{
			Name: name, Agent: "a", Join: true, End: core.EndTimebox, EndsAt: ptr(f3.clock.Now().Add(2 * time.Hour)),
		}))
		f3.attach(t, name, "a", "x", []byte(name))
	}
	f3.clock.Advance(time.Minute)
	must[core.Standing](t)(f3.svc.Close(ctx, f3.p, "default", "closed", core.CloseParams{Agent: "a"}))
	f3.clock.Advance(time.Hour)
	if n := must[int64](t)(f3.svc.ExpireAttachments(ctx)); n != 1 {
		t.Fatalf("expired %d an hour after the early close, want 1", n)
	}
	f3.clock.Advance(2 * time.Hour)
	must[core.ReadResult](t)(f3.svc.Read(ctx, f3.p, "default", "read", core.ReadParams{}))
	if n := must[int64](t)(f3.svc.ExpireAttachments(ctx)); n != 1 {
		t.Fatalf("expired %d after a late read, want 1", n)
	}
}
