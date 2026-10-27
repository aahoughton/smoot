package tui

import (
	"bytes"
	"fmt"
	"image/color"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/aahoughton/smoot/internal/textfile"
	"github.com/aahoughton/smoot/internal/tone"
)

// Text keys. LoadSkin requires exactly these sections in tui/text.md.
var textKeys = []string{
	"brand",
	"namespaces_title", "namespaces_empty",
	"namespace_conversations", "namespace_open", "namespace_unused",
	"conversations_title", "conversations_empty", "archived", "people",
	"status_open", "status_concluded",
	"ends_agreement", "ends_coordinator", "ends_timebox_open", "ends_timebox_done", "ends_none",
	"started", "messages_count", "truncated",
	"participants_title", "participants_empty", "participant_state", "participant_stale",
	"participant_yours", "participants_yours_note",
	"observers_title", "observer_summary",
	"kind_conclude", "kind_observer", "kind_attachment", "attachment", "attachment_gone", "reply_to", "gap", "missed",
	"new_below", "following", "loading",
	"composer_placeholder", "composer_as", "composer_name_prompt", "composer_name_invalid",
	"composer_closed", "composer_sending",
	"filter", "error", "retrying",
	"search_title", "search_order", "search_prompt", "search_empty", "search_count",
	"hints_namespaces", "hints_conversations", "hints_conversation", "hints_concluded", "hints_composer",
	"hints_search", "hints_search_input",
	"help_title", "help",
	"glyph_sep", "glyph_bar", "glyph_dot", "glyph_dot_stale", "glyph_bullet", "glyph_rule",
	"glyph_column", "glyph_cursor", "glyph_more", "glyph_caret",
}

// Palette keys. LoadSkin requires exactly these sections in tui/palette.md.
var paletteKeys = func() []string {
	keys := []string{"fg", "muted", "faint", "line", "accent", "open", "done", "observer", "missed"}
	for i := range tone.Count {
		keys = append(keys, fmt.Sprintf("tone_%d", i))
	}
	return keys
}()

// Skin is the TUI's text and colors, loaded from a theme's tui/ directory.
type Skin struct {
	text  map[string]*template.Template
	g     glyphs
	light map[string]color.Color
	dark  map[string]color.Color
}

var hexRE = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// LoadSkin reads tui/text.md and tui/palette.md from a theme (see
// themes.Open) and checks that every key is present and known.
func LoadSkin(theme fs.FS) (*Skin, error) {
	raw, err := fs.ReadFile(theme, "tui/text.md")
	if err != nil {
		return nil, fmt.Errorf("tui: %w", err)
	}
	s := &Skin{light: map[string]color.Color{}, dark: map[string]color.Color{}}
	if s.text, err = textfile.Templates(string(raw), textKeys); err != nil {
		return nil, fmt.Errorf("tui: text.md: %w", err)
	}
	if s.g, err = s.loadGlyphs(); err != nil {
		return nil, err
	}

	raw, err = fs.ReadFile(theme, "tui/palette.md")
	if err != nil {
		return nil, fmt.Errorf("tui: %w", err)
	}
	secs, err := textfile.Sections(string(raw))
	if err != nil {
		return nil, fmt.Errorf("tui: palette.md: %w", err)
	}
	for _, sec := range secs {
		if !slices.Contains(paletteKeys, sec.Key) {
			return nil, fmt.Errorf("tui: palette.md: unknown key %q", sec.Key)
		}
		f := strings.Fields(sec.Body)
		if len(f) != 2 || !hexRE.MatchString(f[0]) || !hexRE.MatchString(f[1]) {
			return nil, fmt.Errorf("tui: palette.md: %s: want two colors like #4f46e5 #8b8cf6", sec.Key)
		}
		s.light[sec.Key], s.dark[sec.Key] = lipgloss.Color(f[0]), lipgloss.Color(f[1])
	}
	for _, k := range paletteKeys {
		if _, ok := s.light[k]; !ok {
			return nil, fmt.Errorf("tui: palette.md: missing %s", k)
		}
	}
	return s, nil
}

// say renders the text for key with data.
func (s *Skin) say(key string, data any) string {
	t, ok := s.text[key]
	if !ok {
		panic("tui: unknown text key " + key)
	}
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		// Keys and fields are fixed by the code and covered by tests.
		panic("tui: " + err.Error())
	}
	return b.String()
}

// glyphs are the single-cell symbols the layout is drawn with.
type glyphs struct {
	sep, bar, dot, dotStale, bullet, rule, column, cursor, more, caret string
}

func (s *Skin) loadGlyphs() (glyphs, error) {
	var g glyphs
	for key, dst := range map[string]*string{
		"glyph_sep": &g.sep, "glyph_bar": &g.bar, "glyph_dot": &g.dot, "glyph_dot_stale": &g.dotStale,
		"glyph_bullet": &g.bullet, "glyph_rule": &g.rule, "glyph_column": &g.column,
		"glyph_cursor": &g.cursor, "glyph_more": &g.more, "glyph_caret": &g.caret,
	} {
		*dst = s.say(key, nil)
		if ansi.StringWidth(*dst) != 1 {
			return glyphs{}, fmt.Errorf("tui: text.md: %s must be one character cell wide", key)
		}
	}
	return g, nil
}

// styles are the lipgloss styles for one terminal background, and the
// glyphs drawn with them.
type styles struct {
	g                                                            glyphs
	fg, muted, faint, line, accent, open, done, observer, missed lipgloss.Style
	tones                                                        [tone.Count]lipgloss.Style
}

func (s *Skin) styles(dark bool) styles {
	pal := s.light
	if dark {
		pal = s.dark
	}
	fg := func(k string) lipgloss.Style { return lipgloss.NewStyle().Foreground(pal[k]) }
	st := styles{
		g:  s.g,
		fg: fg("fg"), muted: fg("muted"), faint: fg("faint"), line: fg("line"),
		accent: fg("accent"), open: fg("open"), done: fg("done"), observer: fg("observer"), missed: fg("missed"),
	}
	for i := range st.tones {
		st.tones[i] = fg(fmt.Sprintf("tone_%d", i))
	}
	return st
}

// toneOf returns the style for a participant name.
func (st styles) toneOf(name string) lipgloss.Style { return st.tones[tone.Of(name)] }
