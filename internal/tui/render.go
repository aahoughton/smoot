package tui

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/aahoughton/smoot/internal/core"
	"github.com/aahoughton/smoot/internal/markdown"
)

// renderer turns API records into terminal lines. Its methods are pure:
// the same input and clock give the same lines.
type renderer struct {
	sk  *Skin
	st  styles
	now time.Time
	loc *time.Location
	// people are the names message bodies can mention.
	people markdown.People
}

func (r renderer) clock(t time.Time) string    { return t.In(r.loc).Format("15:04") }
func (r renderer) datetime(t time.Time) string { return t.In(r.loc).Format("Jan 2 15:04") }

// ago describes a past time relative to now, such as "5m ago".
func (r renderer) ago(t time.Time) string {
	d := r.now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m ago"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h ago"
	default:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d ago"
	}
}

// gap describes the time between two messages, such as "4s" or "6m".
func gap(d time.Duration) string {
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(max(d, 0)/time.Second)) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	default:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
}

// until describes a time still to come, such as "in 1h 58m".
func (r renderer) until(t time.Time) string {
	d := t.Sub(r.now)
	switch {
	case d <= 0:
		return "now"
	case d < time.Minute:
		return "in under a minute"
	case d < time.Hour:
		return "in " + strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 48*time.Hour:
		return "in " + strconv.Itoa(int(d/time.Hour)) + "h " + strconv.Itoa(int(d%time.Hour/time.Minute)) + "m"
	default:
		return "in " + strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
}

// gutterWidth is the width of the seq column for a conversation whose last
// message is lastSeq, so every seq lines up.
func gutterWidth(lastSeq int64) int {
	return len("#" + strconv.FormatInt(lastSeq, 10))
}

// message renders one message: the seq in a gutter of width gutter, a bar
// in the author's color, then the header and body. A message that
// continues a run after prev (see core.Continues) shows the gap since prev
// in place of its author and time. unseen lists the messages its author had
// not read when posting it (see core.Unseen). System messages are a single
// faint run of text.
func (r renderer) message(m core.Message, prev *core.Message, unseen []int64, gutter, width int) []string {
	seq := r.st.faint.Render(pad("#"+strconv.FormatInt(m.Seq, 10), gutter))
	blank := strings.Repeat(" ", gutter)
	inner := width - gutter - 3
	if m.Kind == core.KindSystem {
		lines := wrap(words(oneLine(m.Body), r.st.faint), max(inner, minWidth))
		out := make([]string, len(lines))
		for i, l := range lines {
			lead := blank
			if i == 0 {
				lead = seq
			}
			out[i] = lead + "   " + l
		}
		return out
	}

	color := r.st.toneOf(m.Author)
	if m.Kind == core.KindObserver {
		color = r.st.observer
	}
	bar := color.Render(r.st.g.bar)
	head := color.Bold(true).Render(sanitize(m.Author)) + "  " + r.st.faint.Render(r.clock(m.CreatedAt))
	if core.Continues(prev, m) {
		head = r.st.faint.Render(r.sk.say("gap", map[string]any{"Gap": gap(m.CreatedAt.Sub(prev.CreatedAt))}))
	}
	switch m.Kind {
	case core.KindConclude:
		head += "  " + r.st.open.Render(r.sk.say("kind_conclude", nil))
	case core.KindObserver:
		head += "  " + r.st.observer.Render(r.sk.say("kind_observer", nil))
	case core.KindAttachment:
		head += "  " + r.st.faint.Render(r.sk.say("kind_attachment", nil))
	}
	if m.ReplyTo != nil {
		head += "  " + r.st.faint.Render(r.sk.say("reply_to", map[string]any{"Seq": *m.ReplyTo}))
	}
	if len(unseen) > 0 {
		head += "  " + r.st.missed.Render(r.sk.say("missed", map[string]any{"Seqs": unseen[:min(len(unseen), 3)], "N": len(unseen)}))
	}
	out := []string{seq + " " + bar + " " + ansi.Truncate(head, max(inner, minWidth), r.st.g.more)}
	if a := m.Attachment; a != nil {
		key, data := "attachment", map[string]any{"Name": sanitize(a.Name), "Size": core.FormatSize(a.Size), "SHA": a.SHA256[:min(len(a.SHA256), 12)]}
		if a.Gone != "" {
			key, data["Gone"] = "attachment_gone", a.Gone
		}
		for _, l := range wrap(words(oneLine(r.sk.say(key, data)), r.st.fg), max(inner, minWidth)) {
			out = append(out, blank+" "+bar+" "+l)
		}
		return out
	}
	for _, l := range renderBody(m.Body, inner, r.st, r.people) {
		out = append(out, blank+" "+bar+" "+l)
	}
	return out
}

// oneLine makes s safe to print and collapses its whitespace, newlines
// included, to single spaces.
func oneLine(s string) string {
	return strings.Join(strings.Fields(sanitize(s)), " ")
}

// pad right-aligns s in width cells.
func pad(s string, width int) string {
	if n := width - ansi.StringWidth(s); n > 0 {
		return strings.Repeat(" ", n) + s
	}
	return s
}

func (r renderer) status(c core.Conversation) string {
	if c.Status == core.StatusConcluded {
		return r.st.done.Render(r.sk.say("status_concluded", nil))
	}
	return r.st.open.Render(r.sk.say("status_open", nil))
}

// ends describes how the conversation ends.
func (r renderer) ends(c core.Conversation) string {
	switch c.End {
	case core.EndAgreement:
		return r.sk.say("ends_agreement", nil)
	case core.EndCoordinator:
		return r.sk.say("ends_coordinator", map[string]any{"CreatedBy": c.CreatedBy})
	case core.EndTimebox:
		if c.EndsAt == nil {
			break
		}
		if c.Status == core.StatusOpen {
			return r.sk.say("ends_timebox_open", map[string]any{"At": r.datetime(*c.EndsAt), "Until": r.until(*c.EndsAt)})
		}
		return r.sk.say("ends_timebox_done", map[string]any{"At": r.datetime(*c.EndsAt)})
	}
	return r.sk.say("ends_none", nil)
}

// header is the conversation context that stays at the top of the screen:
// name, status and topic, then how it ends and how much has been said.
func (r renderer) header(c core.Conversation, width int) []string {
	line1 := r.st.fg.Bold(true).Render(c.Name) + "  " + r.status(c)
	if c.Topic != "" {
		line1 += "  " + r.st.muted.Render(oneLine(c.Topic))
	}
	line2 := r.st.faint.Render(r.ends(c) + " " + r.st.g.sep + " " +
		r.sk.say("messages_count", map[string]any{"N": c.LastSeq, "Ago": r.ago(c.LastAt)}))
	return []string{ansi.Truncate(line1, width, r.st.g.more), ansi.Truncate(line2, width, r.st.g.more)}
}

// participants renders the participant panel.
func (r renderer) participants(c core.Conversation, width int) []string {
	out := []string{r.st.fg.Bold(true).Render(r.sk.say("participants_title", nil)) + " " +
		r.st.faint.Render(strconv.Itoa(len(c.Participants)))}
	if len(c.Participants) == 0 {
		out = append(out, r.st.muted.Render(r.sk.say("participants_empty", nil)))
	}
	for _, p := range c.Participants {
		dot := " "
		if p.State == core.StateActive || p.State == core.StateConcluded {
			switch {
			case p.Stale:
				dot = r.st.faint.Render(r.st.g.dotStale)
			case p.Presence == core.PresenceWorking:
				dot = r.st.open.Render(r.st.g.dot)
			case p.Presence == core.PresenceWaiting:
				dot = r.st.accent.Render(r.st.g.dot)
			case p.Presence == core.PresenceAway:
				dot = r.st.observer.Render(r.st.g.dot)
			default:
				dot = r.st.faint.Render(r.st.g.dot)
			}
		}
		name := dot + " " + r.st.toneOf(p.Agent).Bold(true).Render(p.Agent)
		if p.YourAccount {
			name += " " + r.st.muted.Render(r.sk.say("participant_yours", nil))
		}
		out = append(out, ansi.Truncate(name, width, r.st.g.more))
		var state string
		switch {
		case p.State != core.StateActive && p.State != core.StateConcluded:
			state = r.sk.say("participant_state", map[string]any{"State": p.State, "Presence": ""})
		case p.Stale:
			state = r.sk.say("participant_stale", map[string]any{"State": p.State, "Ago": r.ago(p.LastSeenAt)})
		default:
			state = r.sk.say("participant_state", map[string]any{"State": p.State, "Presence": p.Presence})
		}
		out = append(out, ansi.Truncate("  "+r.st.muted.Render(state), width, r.st.g.more))
		if p.Note != "" {
			for _, l := range wrap(words(oneLine(p.Note), r.st.faint), width-2) {
				out = append(out, "  "+l)
			}
		}
	}
	if slices.ContainsFunc(c.Participants, func(p core.Participant) bool { return p.YourAccount }) {
		out = append(out, "")
		out = append(out, wrap(words(r.sk.say("participants_yours_note", nil), r.st.faint), width)...)
	}
	if len(c.Observers) > 0 {
		out = append(out, "", r.st.fg.Bold(true).Render(r.sk.say("observers_title", nil))+" "+
			r.st.faint.Render(strconv.Itoa(len(c.Observers))))
		for _, o := range c.Observers {
			out = append(out, ansi.Truncate(r.st.observer.Render(r.st.g.dot)+" "+r.st.observer.Bold(true).Render(o.Name), width, r.st.g.more))
			summary := r.sk.say("observer_summary", map[string]any{"Comments": o.Comments, "Ago": r.ago(o.LastAt)})
			out = append(out, ansi.Truncate("  "+r.st.muted.Render(summary), width, r.st.g.more))
		}
	}
	out = append(out, "")
	for _, s := range []string{r.ends(c), r.sk.say("started", map[string]any{"By": c.CreatedBy, "At": r.datetime(c.CreatedAt)})} {
		out = append(out, wrap(words(s, r.st.faint), width)...)
	}
	return out
}

// namespaceRow is one line of the namespace list.
func (r renderer) namespaceRow(n core.Namespace, width int) string {
	meta := r.sk.say("namespace_unused", nil)
	if n.Conversations > 0 {
		meta = r.sk.say("namespace_conversations", map[string]any{"N": n.Conversations})
		if n.Open > 0 {
			meta += " " + r.st.g.sep + " " + r.sk.say("namespace_open", map[string]any{"N": n.Open})
		}
		if n.LastAt != nil {
			meta += " " + r.st.g.sep + " " + r.ago(*n.LastAt)
		}
	}
	return r.withMeta(r.st.fg.Bold(true).Render(n.Name), meta, width)
}

// conversationRow is one line of the conversation list.
func (r renderer) conversationRow(c core.Conversation, width int) string {
	name := r.st.fg.Bold(true)
	if c.Status == core.StatusConcluded {
		name = r.st.muted
	}
	line := name.Render(c.Name) + "  " + r.status(c)
	if c.Archived {
		line += " " + r.st.faint.Render(r.sk.say("archived", nil))
	}
	meta := r.sk.say("people", map[string]any{"N": len(c.Participants)}) + " " + r.st.g.sep + " " + r.ago(c.LastAt)
	if c.Topic != "" {
		line += "  " + r.st.muted.Render(oneLine(c.Topic))
	}
	return r.withMeta(line, meta, width)
}

// withMeta right-aligns meta after line, or leaves it out when the line
// would have fewer than 20 cells.
func (r renderer) withMeta(line, meta string, width int) string {
	metaW := ansi.StringWidth(meta)
	if width-metaW-2 < 20 {
		return ansi.Truncate(line, width, r.st.g.more)
	}
	line = ansi.Truncate(line, width-metaW-2, r.st.g.more)
	return line + strings.Repeat(" ", width-metaW-ansi.StringWidth(line)) + r.st.faint.Render(meta)
}

// rule is a horizontal line.
func (r renderer) rule(width int) string {
	return r.st.line.Render(strings.Repeat(r.st.g.rule, max(width, 0)))
}

// fit pads or cuts lines to exactly height lines of at most width cells.
func fit(lines []string, width, height int) []string {
	out := make([]string, height)
	for i := range out {
		if i < len(lines) {
			out[i] = ansi.Truncate(lines[i], width, "")
		}
	}
	return out
}

// sideBySide joins two columns of equal height with a separator.
func sideBySide(left []string, leftWidth int, sep string, right []string) []string {
	out := make([]string, max(len(left), len(right)))
	for i := range out {
		var l, rt string
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			rt = right[i]
		}
		out[i] = l + strings.Repeat(" ", max(leftWidth-ansi.StringWidth(l), 0)) + sep + rt
	}
	return out
}
