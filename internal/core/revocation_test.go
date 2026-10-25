package core_test

import (
	"testing"
	"time"

	"github.com/aahoughton/smoot/internal/core"
)

func TestLongPollsStopWhenAccessEnds(t *testing.T) {
	setup := func(t *testing.T, recheck time.Duration) (*tokenFixture, core.Principal) {
		f := newTokenFixtureWith(t, core.Options{RecheckEvery: recheck})
		pat := f.account(t, "pat")
		must[core.Membership](t)(f.svc.SetMembership(t.Context(), f.admin, "default", "pat", core.RoleMember))
		must[core.Conversation](t)(f.svc.Start(t.Context(), f.admin, "default", core.StartParams{Name: "c", Agent: "boss", Join: true}))
		must[core.Standing](t)(f.svc.Join(t.Context(), pat, "default", "c", core.JoinParams{Agent: "pat"}))
		must[core.ReadResult](t)(f.svc.Read(t.Context(), pat, "default", "c", core.ReadParams{Agent: "pat"}))
		return f, pat
	}
	wait := func(f *tokenFixture, p core.Principal) func() error {
		return func() error {
			_, err := f.svc.Wait(t.Context(), p, "default", "c", core.WaitParams{Agent: "pat", Timeout: 30 * time.Second})
			return err
		}
	}
	follow := func(f *tokenFixture, p core.Principal) func() error {
		return func() error {
			last := must[core.Conversation](t)(f.svc.GetConversation(t.Context(), f.admin, "default", "c")).LastSeq
			_, err := f.svc.Follow(t.Context(), p, "default", "c", last, 30*time.Second)
			return err
		}
	}
	for name, poll := range map[string]func(*tokenFixture, core.Principal) func() error{"wait": wait, "follow": follow} {
		t.Run(name+" re-checks while blocked", func(t *testing.T) {
			f, pat := setup(t, 20*time.Millisecond)
			ch := pending(poll(f, pat))
			time.Sleep(50 * time.Millisecond)
			if _, err := f.svc.RevokeAllTokens(t.Context(), f.admin, "pat", false); err != nil {
				t.Fatal(err)
			}
			wantCode(t, await(t, ch), core.CodeUnauthorized)
		})
		t.Run(name+" re-checks before returning messages", func(t *testing.T) {
			// A long interval: only the check before returning can stop it.
			f, pat := setup(t, time.Hour)
			ch := pending(poll(f, pat))
			time.Sleep(50 * time.Millisecond)
			if err := f.svc.RemoveMembership(t.Context(), f.admin, "default", "pat"); err != nil {
				t.Fatal(err)
			}
			must[core.SayResult](t)(f.svc.Say(t.Context(), f.admin, "default", "c", core.SayParams{Agent: "boss", Body: "secret plan"}))
			wantCode(t, await(t, ch), core.CodeForbidden)
		})
	}
}

// A follow that times out with nothing new also checks access before it
// answers.
func TestIdleFollowRechecksAtTimeout(t *testing.T) {
	f := newTokenFixtureWith(t, core.Options{RecheckEvery: time.Hour})
	ctx := t.Context()
	pat := f.account(t, "pat")
	must[core.Membership](t)(f.svc.SetMembership(ctx, f.admin, "default", "pat", core.RoleViewer))
	must[core.Conversation](t)(f.svc.Start(ctx, f.admin, "default", core.StartParams{Name: "c", Agent: "boss"}))
	last := must[core.Conversation](t)(f.svc.GetConversation(ctx, f.admin, "default", "c")).LastSeq
	ch := pending(func() error {
		_, err := f.svc.Follow(ctx, pat, "default", "c", last, 200*time.Millisecond)
		return err
	})
	time.Sleep(50 * time.Millisecond)
	if err := f.svc.RemoveMembership(ctx, f.admin, "default", "pat"); err != nil {
		t.Fatal(err)
	}
	wantCode(t, await(t, ch), core.CodeForbidden)
}
