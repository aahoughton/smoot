package web_test

import (
	"strings"
	"testing"
	"time"

	"github.com/aahoughton/smoot/internal/core"
	"github.com/aahoughton/smoot/internal/web"
)

// A presence change in a quiet conversation reaches an open page at the
// next heartbeat.
func TestStreamRefreshesStateWithoutMessages(t *testing.T) {
	e := newEnv(t, web.Options{StreamHeartbeat: 100 * time.Millisecond})
	if _, err := e.svc.Start(t.Context(), core.Anonymous(), "default", core.StartParams{Name: "quiet", Agent: "alice", Join: true}); err != nil {
		t.Fatal(err)
	}
	resp := openStream(t, e, "/n/default/c/quiet/events?after=99", nil)
	go func() {
		time.Sleep(150 * time.Millisecond)
		_, _ = e.svc.SetPresence(t.Context(), core.Anonymous(), "default", "quiet", core.PresenceParams{Agent: "alice", Presence: "away", Note: "lunch"})
	}()
	readEvents(t, resp, func(evs []sse) bool {
		for _, ev := range evs {
			if ev.event == "participants" && strings.Contains(ev.data, "lunch") {
				return true
			}
		}
		return false
	})
}
