package core_test

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aahoughton/smoot/internal/core"
)

func (f *fixture) limit(t *testing.T, conv string, ch core.AttachmentLimitsChange) core.AttachmentLimits {
	t.Helper()
	return must[core.AttachmentLimits](t)(f.svc.SetAttachmentLimits(t.Context(), f.p, "default", conv, ch))
}

func TestAConversationsOwnAttachmentLimits(t *testing.T) {
	f := newFixture(t, attachOn(core.Options{AttachmentQuota: 10}))
	ctx := t.Context()
	f.start(t, "c", core.EndCoordinator, "a")
	f.start(t, "other", core.EndCoordinator, "a")
	try := func(conv string, size int) error {
		_, err := f.svc.Attach(ctx, f.p, "default", conv, core.AttachParams{Agent: "a", Name: "f", Data: bytes.Repeat([]byte{byte(size)}, size)})
		return err
	}

	got := must[core.AttachmentLimits](t)(f.svc.AttachmentLimits(ctx, f.p, "default", "c"))
	if want := (core.AttachmentLimits{MaxBytes: core.DefaultAttachmentMax, QuotaBytes: 10}); got != want {
		t.Fatalf("default limits = %+v", got)
	}

	// A smaller file limit refuses what the default takes.
	if got := f.limit(t, "c", core.AttachmentLimitsChange{MaxBytes: ptr[int64](4)}); got != (core.AttachmentLimits{MaxBytes: 4, QuotaBytes: 10, MaxOverridden: true}) {
		t.Fatalf("limits = %+v", got)
	}
	wantCode(t, try("c", 5), core.CodeAttachmentTooLarge)
	if err := try("c", 4); err != nil {
		t.Fatal(err)
	}
	// A larger quota takes what the default refuses; the file limit stays.
	wantCode(t, try("other", 11), core.CodeAttachmentQuota)
	f.limit(t, "c", core.AttachmentLimitsChange{MaxBytes: ptr[int64](20)})
	wantCode(t, try("c", 11), core.CodeAttachmentQuota)
	if got := f.limit(t, "c", core.AttachmentLimitsChange{QuotaBytes: ptr[int64](30)}); got != (core.AttachmentLimits{MaxBytes: 20, QuotaBytes: 30, MaxOverridden: true, QuotaOverridden: true}) {
		t.Fatalf("limits = %+v", got)
	}
	if err := try("c", 11); err != nil {
		t.Fatal(err)
	}
	// Another conversation is not affected.
	wantCode(t, try("other", 11), core.CodeAttachmentQuota)

	// A quota under what is stored keeps the files and refuses more.
	f.limit(t, "c", core.AttachmentLimitsChange{QuotaBytes: ptr[int64](5)})
	wantCode(t, try("c", 1), core.CodeAttachmentQuota)
	f.fetch(t, "c", core.FetchParams{Agent: "a", Seq: 4})

	rep := must[core.AttachmentReport](t)(f.svc.AttachmentStorage(ctx, f.p))
	if len(rep.Conversations) != 1 || rep.Conversations[0].MaxBytes != 20 || rep.Conversations[0].QuotaBytes != 5 || rep.Max != core.DefaultAttachmentMax {
		t.Fatalf("report = %+v", rep)
	}

	// 0 returns a limit to the default.
	if got := f.limit(t, "c", core.AttachmentLimitsChange{MaxBytes: ptr[int64](0), QuotaBytes: ptr[int64](0)}); got != (core.AttachmentLimits{MaxBytes: core.DefaultAttachmentMax, QuotaBytes: 10}) {
		t.Fatalf("reset limits = %+v", got)
	}

	// A conversation with limits and no files is still reported.
	f.limit(t, "other", core.AttachmentLimitsChange{MaxBytes: ptr[int64](7)})
	rep = must[core.AttachmentReport](t)(f.svc.AttachmentStorage(ctx, f.p))
	if len(rep.Conversations) != 2 || rep.Conversations[1].Conversation != "other" || rep.Conversations[1].Blobs != 0 || rep.Conversations[1].MaxBytes != 7 {
		t.Fatalf("report = %+v", rep)
	}

	for _, tc := range []struct {
		name string
		conv string
		ch   core.AttachmentLimitsChange
		code core.Code
	}{
		{"over the ceiling", "c", core.AttachmentLimitsChange{MaxBytes: ptr[int64](core.MaxAttachmentBytes + 1)}, core.CodeInvalid},
		{"negative max", "c", core.AttachmentLimitsChange{MaxBytes: ptr[int64](-1)}, core.CodeInvalid},
		{"negative quota", "c", core.AttachmentLimitsChange{QuotaBytes: ptr[int64](-1)}, core.CodeInvalid},
		{"no conversation", "nope", core.AttachmentLimitsChange{MaxBytes: ptr[int64](1)}, core.CodeConversationNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.SetAttachmentLimits(ctx, f.p, "default", tc.conv, tc.ch)
			wantCode(t, err, tc.code)
		})
	}
}

func TestOnlyServerAdminsSetAttachmentLimits(t *testing.T) {
	f := newTokenFixtureWith(t, attachOn(core.Options{}))
	ctx := t.Context()
	pat := f.account(t, "pat")
	must[core.Membership](t)(f.svc.SetMembership(ctx, f.admin, "default", "pat", core.RoleAdmin))
	must[core.Conversation](t)(f.svc.Start(ctx, pat, "default", core.StartParams{Name: "c", Agent: "a", Join: true}))
	_, err := f.svc.SetAttachmentLimits(ctx, pat, "default", "c", core.AttachmentLimitsChange{MaxBytes: ptr[int64](1)})
	wantCode(t, err, core.CodeForbidden)
	must[core.AttachmentLimits](t)(f.svc.SetAttachmentLimits(ctx, f.admin, "default", "c", core.AttachmentLimitsChange{MaxBytes: ptr[int64](1)}))
	// Anyone who may read the conversation may see its limits.
	if l := must[core.AttachmentLimits](t)(f.svc.AttachmentLimits(ctx, pat, "default", "c")); l.MaxBytes != 1 {
		t.Fatalf("limits = %+v", l)
	}
}

func TestObserversAttachFiles(t *testing.T) {
	f := newFixture(t, attachOn(core.Options{}))
	ctx := t.Context()
	f.start(t, "c", core.EndCoordinator, "a")
	f.read(t, "c", "a")
	data := []byte("take one")

	res := must[core.AttachResult](t)(f.svc.Attach(ctx, f.p, "default", "c", core.AttachParams{Observer: "sam", Name: "take.wav", Data: data}))
	m := res.Message
	want := core.Attachment{Name: "take.wav", Size: int64(len(data)), SHA256: sha(data), Observer: true}
	if m.Kind != core.KindAttachment || m.Author != "sam" || m.Attachment == nil || *m.Attachment != want {
		t.Fatalf("message = %+v, attachment %+v", m, m.Attachment)
	}
	// The observer does not join, and a read returns the file.
	if c := f.get(t, "c"); len(c.Participants) != 1 {
		t.Fatalf("participants = %+v", c.Participants)
	}
	rr := f.read(t, "c", "a")
	if len(rr.Messages) != 1 || rr.Messages[0].Attachment == nil || !rr.Messages[0].Attachment.Observer {
		t.Fatalf("agent read %+v", rr.Messages)
	}
	if d := f.fetch(t, "c", core.FetchParams{Agent: "a", Seq: m.Seq}); !bytes.Equal(d.Data, data) {
		t.Fatalf("download = %+v", d.Attachment)
	}
	// A participant's own file is not marked.
	if a := f.attach(t, "c", "a", "reply.wav", []byte("reply")).Message.Attachment; a.Observer {
		t.Fatalf("participant's attachment = %+v", a)
	}

	for _, tc := range []struct {
		name string
		conv string
		ap   core.AttachParams
		code core.Code
	}{
		{"a participant's name", "c", core.AttachParams{Observer: "a", Name: "x", Data: data}, core.CodeNameActive},
		{"no name", "c", core.AttachParams{Name: "x", Data: data}, core.CodeInvalid},
		{"bad file name", "c", core.AttachParams{Observer: "sam", Name: "../x", Data: data}, core.CodeInvalid},
		{"no conversation", "nope", core.AttachParams{Observer: "sam", Name: "x", Data: data}, core.CodeConversationNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.Attach(ctx, f.p, "default", tc.conv, tc.ap)
			wantCode(t, err, tc.code)
		})
	}
	f.limit(t, "c", core.AttachmentLimitsChange{MaxBytes: ptr[int64](3)})
	_, err := f.svc.Attach(ctx, f.p, "default", "c", core.AttachParams{Observer: "sam", Name: "x", Data: data})
	wantCode(t, err, core.CodeAttachmentTooLarge)

	f.closeConv(t, "c", "a")
	_, err = f.svc.Attach(ctx, f.p, "default", "c", core.AttachParams{Observer: "sam", Name: "late.wav", Data: []byte("x")})
	wantCode(t, err, core.CodeConversationConcluded)
}

func TestAccountsAttachUnderTheirOwnName(t *testing.T) {
	f := newTokenFixtureWith(t, attachOn(core.Options{}))
	ctx := t.Context()
	pat, vic := f.account(t, "pat"), f.account(t, "vic")
	must[core.Membership](t)(f.svc.SetMembership(ctx, f.admin, "default", "pat", core.RoleObserver))
	must[core.Membership](t)(f.svc.SetMembership(ctx, f.admin, "default", "vic", core.RoleViewer))
	must[core.Conversation](t)(f.svc.Start(ctx, f.admin, "default", core.StartParams{Name: "c", Agent: "a", Join: true}))

	res := must[core.AttachResult](t)(f.svc.Attach(ctx, pat, "default", "c", core.AttachParams{Observer: "someone-else", Name: "x", Data: []byte("x")}))
	if res.Message.Author != "pat" || !res.Message.YourAccount || !res.Message.Attachment.Observer {
		t.Fatalf("message = %+v", res.Message)
	}
	rr := must[core.ReadResult](t)(f.svc.Read(ctx, f.admin, "default", "c", core.ReadParams{Agent: "a"}))
	if last := rr.Messages[len(rr.Messages)-1]; last.Seq != res.Message.Seq || last.YourAccount {
		t.Fatalf("the agent's account read %+v", last)
	}
	// An account's name may be a participant's, as for a comment, and the
	// file still reaches that participant.
	a := f.account(t, "a")
	must[core.Membership](t)(f.svc.SetMembership(ctx, f.admin, "default", "a", core.RoleObserver))
	own := must[core.AttachResult](t)(f.svc.Attach(ctx, a, "default", "c", core.AttachParams{Name: "z", Data: []byte("z")})).Message
	rr = must[core.ReadResult](t)(f.svc.Read(ctx, f.admin, "default", "c", core.ReadParams{Agent: "a"}))
	if len(rr.Messages) != 1 || rr.Messages[0].Seq != own.Seq {
		t.Fatalf("a file from an observer named like the agent: read %+v", rr.Messages)
	}
	// A viewer may read and download, not upload.
	_, err := f.svc.Attach(ctx, vic, "default", "c", core.AttachParams{Name: "y", Data: []byte("y")})
	wantCode(t, err, core.CodeForbidden)
	must[core.Download](t)(f.svc.Fetch(ctx, vic, "default", "c", core.FetchParams{Seq: res.Message.Seq}))
}

func TestDumpsCarryLimitsAndObserverMarks(t *testing.T) {
	f := newFixture(t, attachOn(core.Options{}))
	ctx := t.Context()
	f.start(t, "c", core.EndCoordinator, "a")
	f.limit(t, "c", core.AttachmentLimitsChange{MaxBytes: ptr[int64](100), QuotaBytes: ptr[int64](200)})
	m := must[core.AttachResult](t)(f.svc.Attach(ctx, f.p, "default", "c", core.AttachParams{Observer: "sam", Name: "take.wav", Data: []byte("x")})).Message
	var buf bytes.Buffer
	if err := f.svc.Dump(ctx, f.p, "default", &buf); err != nil {
		t.Fatal(err)
	}
	good := buf.String()

	must[core.LoadResult](t)(f.svc.LoadNamespace(ctx, f.p, strings.NewReader(good), "copy"))
	if l := must[core.AttachmentLimits](t)(f.svc.AttachmentLimits(ctx, f.p, "copy", "c")); l != (core.AttachmentLimits{MaxBytes: 100, QuotaBytes: 200, MaxOverridden: true, QuotaOverridden: true}) {
		t.Fatalf("loaded limits = %+v", l)
	}
	rr := must[core.ReadResult](t)(f.svc.Read(ctx, f.p, "copy", "c", core.ReadParams{After: ptr(m.Seq - 1)}))
	if len(rr.Messages) != 1 || rr.Messages[0].Attachment == nil || !rr.Messages[0].Attachment.Observer {
		t.Fatalf("loaded %+v", rr.Messages)
	}

	for _, tc := range []struct{ name, old, new, want string }{
		{"file limit over the ceiling", `"attachment_max_bytes":100`, `"attachment_max_bytes":536870913`, "attachment limit out of range"},
		{"zero quota", `"attachment_quota_bytes":200`, `"attachment_quota_bytes":0`, "attachment limit out of range"},
		{"text limit", `"attachment_max_bytes":100`, `"attachment_max_bytes":"abc"`, "attachment limit out of range"},
		{"observer mark", `"observer":1`, `"observer":2`, "observer mark is not 0 or 1"},
		{"quiet mark", `"quiet":1`, `"quiet":2`, "quiet mark is not 0 or 1"},
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

// An observer's files do not call on the agents until a message follows
// them; a participant's file does at once.
func TestObserversFilesWaitForAMessage(t *testing.T) {
	f := newFixture(t, attachOn(core.Options{}))
	ctx := t.Context()
	f.start(t, "c", core.EndCoordinator, "a", "b")
	f.read(t, "c", "a")
	upload := func(name string) {
		t.Helper()
		must[core.AttachResult](t)(f.svc.Attach(ctx, f.p, "default", "c", core.AttachParams{Observer: "sam", Name: name, Data: []byte(name)}))
	}
	wait := func() core.Digest {
		t.Helper()
		return must[core.Digest](t)(f.svc.Wait(ctx, f.p, "default", "c", core.WaitParams{Agent: "a", Timeout: 20 * time.Millisecond}))
	}
	standing := func() core.Standing {
		t.Helper()
		return must[core.Standing](t)(f.svc.StandingOf(ctx, f.p, "default", "c", "a"))
	}

	upload("one.wav")
	upload("two.wav")
	if d := wait(); !d.TimedOut || d.New != 0 || len(d.Previews) != 0 || d.Standing.Unread != 0 || d.Standing.Next != core.NextWait {
		t.Fatalf("wait with only an observer's files = %+v", d)
	}
	// Posting does not report them as missed either.
	if r := f.say(t, "c", "a", "still here"); r.Missed != nil || r.Standing.Unread != 0 {
		t.Fatalf("say = %+v", r)
	}

	// The comment that follows brings the files with it.
	must[core.Message](t)(f.svc.Observe(ctx, f.p, "default", "c", core.ObserveParams{Name: "sam", Body: "two takes, as played"}))
	upload("three.wav")
	d := wait()
	if d.TimedOut || d.New != 3 || d.Standing.Unread != 3 || d.Standing.Next != core.NextRead {
		t.Fatalf("wait after the comment = %+v", d)
	}
	// A read returns everything, the file after the comment too.
	rr := f.read(t, "c", "a")
	if len(rr.Messages) != 4 || rr.Messages[3].Body != "three.wav" || rr.Standing.Unread != 0 {
		t.Fatalf("read = %+v", rr.Messages)
	}

	// So with downloads: an observer's is recorded without calling on
	// anyone, a participant's counts at once.
	m := f.attach(t, "c", "a", "mix.wav", []byte("mix")).Message
	f.read(t, "c", "a")
	f.fetch(t, "c", core.FetchParams{Seq: m.Seq, Observer: "sam"})
	if d := wait(); !d.TimedOut || d.Standing.Unread != 0 {
		t.Fatalf("wait after an observer's download = %+v", d)
	}
	f.fetch(t, "c", core.FetchParams{Seq: m.Seq, Agent: "b"})
	if d := wait(); d.TimedOut || d.New != 2 {
		t.Fatalf("wait after a participant's download = %+v", d)
	}
	if rr := f.read(t, "c", "a"); len(rr.Messages) != 2 || rr.Messages[0].Body != "sam downloaded mix.wav (#"+itoa(m.Seq)+")" {
		t.Fatalf("read = %+v", rr.Messages)
	}

	// A participant's file counts at once.
	upload("four.wav")
	f.attach(t, "c", "b", "reply.wav", []byte("reply"))
	if st := standing(); st.Unread != 2 || st.Next != core.NextRead {
		t.Fatalf("standing after a participant's file = %+v", st)
	}
}

// An agent that joins under a name an observer uploaded with still gets the
// observer's file: it is not the agent's own message.
func TestObserversFileReachesALaterAgentOfThatName(t *testing.T) {
	f := newFixture(t, attachOn(core.Options{}))
	ctx := t.Context()
	f.start(t, "c", core.EndCoordinator, "a")
	m := must[core.AttachResult](t)(f.svc.Attach(ctx, f.p, "default", "c", core.AttachParams{Observer: "sam", Name: "spec.pdf", Data: []byte("spec")})).Message
	must[core.Standing](t)(f.svc.Join(ctx, f.p, "default", "c", core.JoinParams{Agent: "sam"}))
	f.say(t, "c", "a", "see the spec")
	var got []int64
	for _, msg := range f.read(t, "c", "sam").Messages {
		got = append(got, msg.Seq)
	}
	if !slices.Contains(got, m.Seq) {
		t.Fatalf("sam read %v, without the file at #%d", got, m.Seq)
	}
}
