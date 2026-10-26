package web

import (
	"bytes"
	"fmt"
	"html/template"

	"github.com/aahoughton/smoot/internal/core"
	"github.com/aahoughton/smoot/internal/markdown"
)

// Message is a message as the message template sees it: the record's
// fields, the conversation's People for marking mentions, the references
// to earlier messages it may link, and the message just before it.
type Message struct {
	core.Message
	People markdown.People
	// Refs marks "#N" as a link to message N; only a conclusion has them.
	Refs markdown.Refs
	// Prev is the message before this one, or nil for the first message
	// on a page and when the one before it is not at hand.
	Prev *core.Message
	// Unseen holds the seqs of messages from others that the author had
	// not read when posting this one (see core.Unseen).
	Unseen []int64
	// Download is where to download an attachment's file; empty when its
	// bytes are gone or the server has attachments off.
	Download string
}

// links give the addresses of a conversation's messages, and of its
// attachments' files (nil when the server has attachments off).
type links struct {
	message  func(seq int64) string
	download func(seq int64) string
}

// newMessage wraps m for the message template.
func newMessage(m core.Message, people markdown.People, prev *core.Message, l links) Message {
	msg := Message{Message: m, People: people, Prev: prev}
	if m.Kind == core.KindConclude {
		msg.Refs = markdown.Refs{Below: m.Seq, Link: l.message}
	}
	if m.Attachment != nil && m.Attachment.Gone == "" && l.download != nil {
		msg.Download = l.download(m.Seq)
	}
	return msg
}

// Continues reports whether m continues a run of messages from one
// participant after Prev (see core.Continues).
func (m Message) Continues() bool { return core.Continues(m.Prev, m.Message) }

// peopleOf returns the names c's messages can mention.
func peopleOf(c core.Conversation) markdown.People {
	ps := make([]string, len(c.Participants))
	for i, p := range c.Participants {
		ps[i] = p.Agent
	}
	os := make([]string, len(c.Observers))
	for i, o := range c.Observers {
		os[i] = o.Name
	}
	return markdown.NewPeople(ps, os)
}

// renderMarkdown renders a message body to safe HTML, marking mentions of
// people and references to messages when given a markdown.People and a
// markdown.Refs, in either order.
func renderMarkdown(body string, args ...any) (template.HTML, error) {
	var people markdown.People
	var refs markdown.Refs
	for _, a := range args {
		switch a := a.(type) {
		case markdown.People:
			people = a
		case markdown.Refs:
			refs = a
		default:
			return "", fmt.Errorf("markdown: unexpected argument of type %T", a)
		}
	}
	var b bytes.Buffer
	body = markdown.StripBidi(body)
	if err := markdown.Parser.Convert([]byte(body), &b, markdown.With(people, refs)); err != nil {
		b.Reset()
		b.WriteString("<p>" + template.HTMLEscapeString(body) + "</p>")
	}
	return template.HTML(b.String()), nil
}
