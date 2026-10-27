// Package wording holds every piece of text smoot shows to agents: the MCP
// server instructions, tool and parameter descriptions, and the short phrases
// in status lines and tool results. The text lives in Markdown files under
// text/, embedded in the binary, so wording can be reviewed and changed
// without touching code.
//
// Load takes an fs.FS so a future deployment could supply its own files;
// Default uses the embedded ones.
package wording

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"sync"
	"text/template"

	"github.com/aahoughton/smoot/internal/textfile"
)

//go:embed text/*.md
var embedded embed.FS

// Phrase keys. Load requires exactly these sections in phrases.md.
const (
	StandingLine = "standing_line"

	AdviceReadAfterConclusion = "advice_read_after_conclusion"
	AdviceDone                = "advice_done"
	AdviceClose               = "advice_close"
	AdviceCoordinatorAway     = "advice_coordinator_away"
	AdviceJoin                = "advice_join"
	AdviceRejoin              = "advice_rejoin"
	AdviceRead                = "advice_read"
	AdviceReadBacklog         = "advice_read_backlog"
	AdviceWaitConcluded       = "advice_wait_concluded"
	AdviceWaitAgreement       = "advice_wait_agreement"
	AdviceWaitExpected        = "advice_wait_expected"
	AdviceWaitFree            = "advice_wait_free"
	AdviceFeedback            = "advice_feedback"

	WaitTimedOut          = "wait_timed_out"
	WaitNothingNew        = "wait_nothing_new"
	ReadEmpty             = "read_empty"
	ReadMore              = "read_more"
	ObserverTag           = "observer_tag"
	ObserverYoursTag      = "observer_yours_tag"
	AttachmentRecord      = "attachment_record"
	AttachmentUnavailable = "attachment_unavailable"
	AttachmentFetch       = "attachment_fetch"
	AccountLine           = "account_line"
	StatusNone            = "status_none"
	ListNone              = "list_none"
	ListMore              = "list_more"
	SearchNone            = "search_none"
	SearchCount           = "search_count"
	SearchMore            = "search_more"
	WaitTimeoutRange      = "wait_timeout_range"

	BridgeUnreachable = "bridge_unreachable"
	BridgeLostRead    = "bridge_lost_read"
	BridgeLostWrite   = "bridge_lost_write"
	BridgeLostOther   = "bridge_lost_other"

	StopHook = "stop_hook"

	SayMissed = "say_missed"

	FeedbackRecorded = "feedback_recorded"
	FeedbackReplaced = "feedback_replaced"
)

// PhraseKeys lists every phrase key.
var PhraseKeys = []string{
	StandingLine, AdviceReadAfterConclusion, AdviceDone, AdviceClose, AdviceCoordinatorAway, AdviceJoin,
	AdviceRejoin, AdviceRead, AdviceReadBacklog, AdviceWaitConcluded, AdviceWaitAgreement, AdviceWaitExpected, AdviceWaitFree,
	AdviceFeedback,
	WaitTimedOut, WaitNothingNew, ReadEmpty, ReadMore, ObserverTag, ObserverYoursTag, AttachmentRecord, AttachmentUnavailable, AttachmentFetch, AccountLine, StatusNone, ListNone, ListMore,
	SearchNone, SearchCount, SearchMore,
	WaitTimeoutRange, BridgeUnreachable, BridgeLostRead, BridgeLostWrite, BridgeLostOther,
	StopHook, SayMissed, FeedbackRecorded, FeedbackReplaced,
}

// Tool is one MCP tool's wording.
type Tool struct {
	Description string
	// Params maps parameter name to description.
	Params map[string]string
}

// Wording is a loaded, validated set of agent-facing text.
type Wording struct {
	instructions *template.Template
	tools        map[string]Tool
	phrases      map[string]*template.Template
}

var (
	defaultOnce sync.Once
	defaultW    *Wording
)

// Default returns the embedded wording. It panics if the embedded files are
// invalid, which the package tests rule out.
func Default() *Wording {
	defaultOnce.Do(func() {
		w, err := Load(embedded)
		if err != nil {
			panic("wording: embedded text is invalid: " + err.Error())
		}
		defaultW = w
	})
	return defaultW
}

// Load reads and validates instructions.md, tools.md and phrases.md from the
// text/ directory of fsys.
func Load(fsys fs.FS) (*Wording, error) {
	read := func(name string) (string, error) {
		b, err := fs.ReadFile(fsys, "text/"+name)
		if err != nil {
			return "", fmt.Errorf("wording: %w", err)
		}
		return string(b), nil
	}
	w := &Wording{tools: map[string]Tool{}, phrases: map[string]*template.Template{}}

	raw, err := read("instructions.md")
	if err != nil {
		return nil, err
	}
	if w.instructions, err = parseTemplate("instructions.md", strings.TrimSpace(raw)); err != nil {
		return nil, err
	}

	raw, err = read("tools.md")
	if err != nil {
		return nil, err
	}
	toolSections, err := textfile.Sections(raw)
	if err != nil {
		return nil, fmt.Errorf("wording: tools.md: %w", err)
	}
	for _, s := range toolSections {
		t, err := parseTool(s)
		if err != nil {
			return nil, err
		}
		w.tools[s.Key] = t
	}

	raw, err = read("phrases.md")
	if err != nil {
		return nil, err
	}
	if w.phrases, err = textfile.Templates(raw, PhraseKeys); err != nil {
		return nil, fmt.Errorf("wording: phrases.md: %w", err)
	}
	return w, nil
}

var paramRE = regexp.MustCompile(`^- ([a-z][a-z0-9_]*): (.+)$`)

func parseTool(s textfile.Section) (Tool, error) {
	t := Tool{Params: map[string]string{}}
	var desc []string
	inParams := false
	for line := range strings.SplitSeq(s.Body, "\n") {
		if m := paramRE.FindStringSubmatch(line); m != nil {
			inParams = true
			if _, dup := t.Params[m[1]]; dup {
				return Tool{}, fmt.Errorf("wording: tools.md %s: duplicate parameter %q", s.Key, m[1])
			}
			t.Params[m[1]] = strings.TrimSpace(m[2])
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if inParams {
			return Tool{}, fmt.Errorf("wording: tools.md %s: text after the parameter list: %q", s.Key, line)
		}
		desc = append(desc, strings.TrimSpace(line))
	}
	t.Description = strings.Join(desc, " ")
	if t.Description == "" {
		return Tool{}, fmt.Errorf("wording: tools.md %s: no description", s.Key)
	}
	return t, nil
}

func parseTemplate(name, text string) (*template.Template, error) {
	t, err := template.New(name).Option("missingkey=error").Parse(text)
	if err != nil {
		return nil, fmt.Errorf("wording: %w", err)
	}
	return t, nil
}

func execute(t *template.Template, data any) string {
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		// Keys and fields are fixed by the code and covered by tests.
		panic("wording: " + err.Error())
	}
	return b.String()
}

// Instructions renders the MCP server instructions. url may be empty;
// attachments adds how to share and download files.
func (w *Wording) Instructions(url, namespace string, attachments bool) string {
	return execute(w.instructions, struct {
		URL, Namespace string
		Attachments    bool
	}{url, namespace, attachments})
}

// Tools returns the names of the tools that have wording.
func (w *Wording) Tools() []string {
	names := make([]string, 0, len(w.tools))
	for n := range w.tools {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// Tool returns a tool's wording and whether it exists.
func (w *Wording) Tool(name string) (Tool, bool) {
	t, ok := w.tools[name]
	return t, ok
}

// Phrase renders the phrase key with data (nil when it has no fields). It
// panics on an unknown key, which is a programming error.
func (w *Wording) Phrase(key string, data any) string {
	t, ok := w.phrases[key]
	if !ok {
		panic("wording: unknown phrase " + key)
	}
	return execute(t, data)
}
