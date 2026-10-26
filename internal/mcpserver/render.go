package mcpserver

import (
	"fmt"
	"strings"

	"github.com/aahoughton/smoot/internal/core"
	"github.com/aahoughton/smoot/internal/wording"
)

// Results are plain text, not JSON: agents pay for every token they read, and
// the text carries the same facts. Every agent-facing result ends with the
// standing line so the next step is the last thing the agent sees.

func renderStanding(st core.Standing) string {
	return st.Line
}

func renderDigest(w *wording.Wording, d core.Digest, fc fetchCmd) string {
	var b strings.Builder
	switch {
	case d.New == 0 && d.TimedOut:
		b.WriteString(w.Phrase(wording.WaitTimedOut, nil) + "\n")
	case d.New == 0:
		b.WriteString(w.Phrase(wording.WaitNothingNew, nil) + "\n")
	default:
		fmt.Fprintf(&b, "%d new (%s)", d.New, authorList(d.Authors))
		if len(d.Messages) > 0 {
			b.WriteString(":\n")
			writeMessages(&b, w, fc, d.Messages)
			break
		}
		if len(d.Previews) < d.New {
			fmt.Fprintf(&b, "; latest %d:", len(d.Previews))
		}
		b.WriteString("\n")
		writePreviews(&b, w, d.Previews)
	}
	b.WriteString(renderStanding(d.Standing))
	return b.String()
}

func renderSay(w *wording.Wording, r core.SayResult, fc fetchCmd) string {
	var b strings.Builder
	fmt.Fprintf(&b, "posted #%d\n", r.Message.Seq)
	if m := r.Missed; m != nil {
		b.WriteString(w.Phrase(wording.SayMissed, map[string]any{"New": m.New, "Authors": authorList(m.Authors)}))
		if len(m.Messages) > 0 {
			b.WriteString(":\n")
			writeMessages(&b, w, fc, m.Messages)
		} else {
			if len(m.Previews) < m.New {
				fmt.Fprintf(&b, "; latest %d", len(m.Previews))
			}
			b.WriteString(":\n")
			writePreviews(&b, w, m.Previews)
		}
	}
	b.WriteString(renderStanding(r.Standing))
	return b.String()
}

func authorList(as []core.AuthorCount) string {
	parts := make([]string, len(as))
	for i, a := range as {
		parts[i] = fmt.Sprintf("%s x%d", a.Author, a.Count)
	}
	return strings.Join(parts, ", ")
}

func writePreviews(b *strings.Builder, w *wording.Wording, ps []core.Preview) {
	for _, p := range ps {
		fmt.Fprintf(b, "  #%d %s%s%s: %s\n", p.Seq, p.Author, kindTag(w, p.Kind, p.YourAccount), replyTag(p.ReplyTo), p.Text)
	}
}

func renderRead(w *wording.Wording, r core.ReadResult, fc fetchCmd) string {
	var b strings.Builder
	if len(r.Messages) == 0 {
		b.WriteString(w.Phrase(wording.ReadEmpty, nil) + "\n")
	}
	writeMessages(&b, w, fc, r.Messages)
	if r.More {
		b.WriteString(w.Phrase(wording.ReadMore, map[string]any{"LastSeq": r.LastSeq}) + "\n")
	}
	if r.Standing != nil {
		b.WriteString(renderStanding(*r.Standing))
	} else {
		fmt.Fprintf(&b, "last_seq: %d", r.LastSeq)
	}
	return strings.TrimRight(b.String(), "\n")
}

// fetchCmd is what an attachment's download command needs besides its seq:
// the conversation and agent of the call, and the server it reached.
type fetchCmd struct {
	Conversation, Agent, Namespace, URL string
}

// writeMessages writes one line per single-line message, and a header line
// plus the body for multi-line ones. Every body line is indented, so only a
// real message starts a line with "#": a body cannot pass off its lines as
// other messages. Lines are split on every line terminator, not only "\n",
// so a carriage return cannot start an unindented line either. An
// attachment whose bytes are stored gets an indented line with the command
// that downloads it, as the reader.
func writeMessages(b *strings.Builder, w *wording.Wording, fc fetchCmd, msgs []core.Message) {
	for _, m := range msgs {
		head := fmt.Sprintf("#%d %s%s%s:", m.Seq, m.Author, kindTag(w, m.Kind, m.YourAccount), replyTag(m.ReplyTo))
		body := strings.TrimSpace(core.NormalizeLineBreaks(m.Body))
		if m.Attachment != nil {
			fmt.Fprintf(b, "%s %s\n", head, attachmentText(w, *m.Attachment))
			if m.Attachment.Gone == "" && fc.Agent != "" {
				b.WriteString("  " + w.Phrase(wording.AttachmentFetch, map[string]any{
					"Conversation": fc.Conversation, "Seq": m.Seq, "Agent": fc.Agent, "Namespace": fc.Namespace, "URL": fc.URL,
				}) + "\n")
			}
			continue
		}
		if !strings.Contains(body, "\n") {
			fmt.Fprintf(b, "%s %s\n", head, body)
			continue
		}
		b.WriteString(head + "\n")
		for line := range strings.Lines(body) {
			b.WriteString("  " + strings.TrimRight(line, "\n") + "\n")
		}
		b.WriteString("\n")
	}
}

func renderConversation(_ *wording.Wording, c core.Conversation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s [%s, end: %s", c.Name, c.Status, c.End)
	if c.EndsAt != nil {
		fmt.Fprintf(&b, " at %s", c.EndsAt.Format("2006-01-02T15:04Z07:00"))
	}
	fmt.Fprintf(&b, ", created by %s, last #%d]", c.CreatedBy, c.LastSeq)
	if c.Archived {
		b.WriteString(" archived")
	}
	if c.Topic != "" {
		fmt.Fprintf(&b, " %s", c.Topic)
	}
	if len(c.Participants) > 0 {
		ps := make([]string, len(c.Participants))
		for i, p := range c.Participants {
			ps[i] = p.Agent + " (" + p.State
			if p.Stale {
				ps[i] += ", stale"
			} else if p.State == core.StateActive || p.State == core.StateConcluded {
				ps[i] += ", " + p.Presence
			}
			ps[i] += ")"
		}
		fmt.Fprintf(&b, "\n  participants: %s", strings.Join(ps, ", "))
	}
	return b.String()
}

func renderList(w *wording.Wording, page core.ListPage) string {
	if len(page.Conversations) == 0 {
		return w.Phrase(wording.ListNone, nil)
	}
	parts := make([]string, 0, len(page.Conversations)+1)
	for _, c := range page.Conversations {
		parts = append(parts, renderConversation(w, c))
	}
	if page.Next != "" {
		parts = append(parts, w.Phrase(wording.ListMore, map[string]any{"Cursor": page.Next}))
	}
	return strings.Join(parts, "\n")
}

// renderSearch writes the match count, then one indented line per hit, so a
// snippet cannot pass for a message, then how to get more.
func renderSearch(w *wording.Wording, page core.SearchPage) string {
	if page.Total == 0 {
		return w.Phrase(wording.SearchNone, nil)
	}
	var b strings.Builder
	b.WriteString(w.Phrase(wording.SearchCount, map[string]any{"Total": page.Total, "Shown": len(page.Hits)}) + "\n")
	for _, h := range page.Hits {
		fmt.Fprintf(&b, "  %s #%d %s%s %s: %s\n", h.Conversation, h.Seq, h.Author, kindTag(w, h.Kind, h.YourAccount),
			h.CreatedAt.UTC().Format("2006-01-02T15:04Z"), h.Text())
	}
	if page.Next != "" {
		b.WriteString(w.Phrase(wording.SearchMore, map[string]any{"Cursor": page.Next}) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderStatus(w *wording.Wording, sts []core.Standing) string {
	if len(sts) == 0 {
		return w.Phrase(wording.StatusNone, nil)
	}
	lines := make([]string, len(sts))
	for i, st := range sts {
		lines[i] = renderStanding(st)
	}
	return strings.Join(lines, "\n")
}

// kindTag marks a message by kind. An observer comment from the caller's own
// account says so; the server recorded it, so a body cannot claim it.
func kindTag(w *wording.Wording, kind string, yours bool) string {
	switch {
	case kind == core.KindSay:
		return ""
	case kind == core.KindObserver && yours:
		return " " + w.Phrase(wording.ObserverYoursTag, nil)
	case kind == core.KindObserver:
		return " " + w.Phrase(wording.ObserverTag, nil)
	default:
		return " [" + kind + "]"
	}
}

// attachmentText describes an attachment message's file on one line. Core
// refuses file names with line breaks; as a second guard, any line break
// becomes a space here, so no name can start a line of its own.
func attachmentText(w *wording.Wording, a core.Attachment) string {
	a.Name = strings.ReplaceAll(core.NormalizeLineBreaks(a.Name), "\n", " ")
	data := map[string]any{"Name": a.Name, "Size": formatSize(a.Size), "SHA": a.SHA256[:min(len(a.SHA256), 12)], "Gone": a.Gone}
	if a.Gone != "" {
		return w.Phrase(wording.AttachmentUnavailable, data)
	}
	return w.Phrase(wording.AttachmentRecord, data)
}

// formatSize writes a byte count for people: 512 B, 1.5 KiB, 2.0 MiB.
func formatSize(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
}

func replyTag(replyTo *int64) string {
	if replyTo == nil {
		return ""
	}
	return fmt.Sprintf(" (re #%d)", *replyTo)
}
