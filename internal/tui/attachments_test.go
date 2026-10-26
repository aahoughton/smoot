package tui

import (
	"strings"
	"testing"

	"github.com/aahoughton/smoot/internal/client"
	"github.com/aahoughton/smoot/internal/core"
)

// TestAttachmentView shows a downloadable attachment, one whose bytes are
// gone, and a file name trying to pass terminal escapes.
func TestAttachmentView(t *testing.T) {
	m := newTestModel(t, Options{Namespace: "default", Conversation: "api-shape"}, 80, 24)
	conv := sampleConversation()
	conv.LastSeq = 4
	sha := strings.Repeat("ab", 32)
	msgs := []core.Message{
		{Seq: 1, Kind: core.KindSystem, Author: "claude-implementer", Body: "claude-implementer started the conversation (end: agreement)", CreatedAt: at(0)},
		{
			Seq: 2, Kind: core.KindAttachment, Author: "claude-implementer", Body: "bench.csv", CreatedAt: at(1),
			Attachment: &core.Attachment{Name: "bench.csv", Size: 2100, SHA256: sha},
		},
		{
			Seq: 3, Kind: core.KindAttachment, Author: "codex-reviewer", Body: "trace.json", CreatedAt: at(2),
			Attachment: &core.Attachment{Name: "trace\x1b[31m.json", Size: 3 << 20, SHA256: sha, Gone: core.GoneExpired},
		},
		{Seq: 4, Kind: core.KindSystem, Author: "codex-reviewer", Body: "codex-reviewer downloaded bench.csv (#2)", CreatedAt: at(3)},
	}
	m.Update(conversationMsg{gen: m.gen, conv: conv})
	m.Update(followMsg{gen: m.gen, res: client.FollowResult{ReadResult: core.ReadResult{Messages: msgs, LastSeq: 4}, Conversation: conv}})
	got := shot(m)
	if strings.Contains(got, "\x1b") {
		t.Fatalf("escape reached the screen:\n%q", got)
	}
	golden(t, "conversation-files", got)
}
