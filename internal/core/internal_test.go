package core

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/aahoughton/smoot/internal/store"
)

func TestWaitLeavesNoHubEntries(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "smoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := New(db, Options{})
	ctx := t.Context()
	p := Anonymous()

	for _, name := range []string{"one", "two", "three"} {
		if _, err := s.Start(ctx, p, "default", StartParams{Name: name, Agent: "alice", Join: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Read(ctx, p, "default", name, ReadParams{Agent: "alice"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Wait(ctx, p, "default", name, WaitParams{Agent: "alice"}); err != nil {
			t.Fatal(err)
		}
		cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		_, _ = s.Wait(cctx, p, "default", name, WaitParams{Agent: "alice", Timeout: time.Minute})
		cancel()
		if _, err := s.Follow(ctx, p, "default", name, 1000, 10*time.Millisecond); err != nil {
			t.Fatal(err)
		}
	}

	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	w, v := s.hub.waiters, s.hub.watchers
	if len(s.hub.subs)+len(w.all)+len(w.byAccount)+len(v.all)+len(v.byAccount) != 0 {
		t.Fatalf("hub retained entries: subs=%d waiters=%+v watchers=%+v", len(s.hub.subs), w, v)
	}
}

func TestReadsStayWithinTheLoadedSnapshot(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "smoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := New(db, Options{})
	ctx := t.Context()
	p := Anonymous()
	if _, err := s.Start(ctx, p, "default", StartParams{Name: "c", Agent: "alice", Join: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Join(ctx, p, "default", "c", JoinParams{Agent: "bob"}); err != nil {
		t.Fatal(err)
	}

	snap, err := s.loadConv(ctx, db, "default", "c")
	if err != nil {
		t.Fatal(err)
	}
	// A write lands after the snapshot was taken.
	if _, err := s.Say(ctx, p, "default", "c", SayParams{Agent: "bob", Body: "late"}); err != nil {
		t.Fatal(err)
	}

	page, _, err := s.page(ctx, db, snap, snap.LastSeq-1, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.Messages[0].Seq != snap.LastSeq || page.More || page.LastSeq != snap.LastSeq {
		t.Fatalf("page = %+v (snapshot last_seq %d)", page, snap.LastSeq)
	}

	st, err := s.standing(ctx, db, snap, "alice")
	if err != nil {
		t.Fatal(err)
	}
	var unread int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE c.name = 'c' AND m.seq <= ? AND m.author != 'alice'`, snap.LastSeq).Scan(&unread); err != nil {
		t.Fatal(err)
	}
	if st.Unread != unread {
		t.Fatalf("unread = %d, want %d (messages up to the snapshot only)", st.Unread, unread)
	}
}

// Inline delivery re-checks access inside its own transaction: a token
// revoked after the wait's last check does not receive the message.
func TestInlineDeliveryRechecksAccess(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "smoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := New(db, Options{Policy: RolePolicy()})
	ctx := t.Context()
	tok, err := s.Bootstrap(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.AuthenticateToken(ctx, tok.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAccount(ctx, root, "pat", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetMembership(ctx, root, "default", "pat", RoleMember); err != nil {
		t.Fatal(err)
	}
	pt, err := s.IssueToken(ctx, root, "pat", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	pat, err := s.AuthenticateToken(ctx, pt.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(ctx, root, "default", StartParams{Name: "c", Agent: "boss", Join: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Join(ctx, pat, "default", "c", JoinParams{Agent: "p"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(ctx, pat, "default", "c", ReadParams{Agent: "p"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Say(ctx, root, "default", "c", SayParams{Agent: "boss", Body: "secret"}); err != nil {
		t.Fatal(err)
	}
	testHookBeforeInline = func() {
		if _, err := s.RevokeAllTokens(context.WithoutCancel(ctx), root, "pat", false); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { testHookBeforeInline = nil })
	d, err := s.Wait(ctx, pat, "default", "c", WaitParams{Agent: "p", Timeout: time.Second, InlineBytes: 1000})
	if CodeOf(err) != CodeUnauthorized {
		t.Fatalf("wait after revocation = %+v, %v", d, err)
	}
}
