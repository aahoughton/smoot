# TUI text

Every piece of text `smoot tui` shows, and the symbols its layout is drawn
with. Each `## key` section is a Go text/template; a first line starting
with `fields:` lists the data it receives. Keys are fixed by the code: the
TUI refuses to start if one is missing or unknown. Each `glyph_` key must be
one character cell wide.

## brand
smoot

## namespaces_title
Namespaces

## namespaces_empty
No namespaces you can read.

## conversations_title
fields: Namespace
Conversations in {{.Namespace}}

## conversations_empty
fields: Archived
No conversations{{if not .Archived}} (press a to include archived ones){{end}}.

## archived
archived

## namespace_conversations
fields: N
{{.N}} {{if eq .N 1}}conversation{{else}}conversations{{end}}

## namespace_open
fields: N
{{.N}} open

## namespace_unused
empty

## people
fields: N
{{.N}} {{if eq .N 1}}participant{{else}}participants{{end}}

## status_open
open

## status_concluded
concluded

## ends_agreement
ends when every participant agrees to the same commit or message

## ends_coordinator
fields: CreatedBy
ends when {{.CreatedBy}} closes it

## ends_timebox_open
fields: At, Until
ends {{.At}} ({{.Until}})

## ends_timebox_done
fields: At
timebox ended {{.At}}

## ends_none
open-ended

## started
fields: By, At
started by {{.By}}, {{.At}}

## messages_count
fields: N, Ago
{{.N}} {{if eq .N 1}}message{{else}}messages{{end}}, last {{.Ago}}

## truncated
Earlier messages: scroll up to load them.

## participants_title
Participants

## participants_empty
Nobody has joined.

## participant_state
fields: State, Presence
{{.State}}{{if .Presence}}, {{.Presence}}{{end}}

## participant_stale
fields: State, Ago
{{.State}}, quiet since {{.Ago}}

## participant_yours
yours

## participants_yours_note
Agents marked yours see your comments as from your account.

## observers_title
Observers

## observer_summary
fields: Comments, Ago
{{.Comments}} {{if eq .Comments 1}}comment{{else}}comments{{end}}, last {{.Ago}}

## kind_conclude
concluded

## kind_observer
observer

## kind_attachment
file

## attachment
fields: Name, Size, SHA
{{.Name}}  {{.Size}}  sha256 {{.SHA}}

## attachment_gone
fields: Name, Size, Gone
{{.Name}}  {{.Size}}  no longer downloadable: {{.Gone}}

## reply_to
fields: Seq
re #{{.Seq}}

## gap
fields: Gap
+{{.Gap}}

## missed
fields: Seqs, N
missed {{range $i, $s := .Seqs}}{{if $i}}, {{end}}#{{$s}}{{end}}{{if gt .N (len .Seqs)}} ({{.N}} in all){{end}}

## new_below
fields: N
{{.N}} new below

## following
following

## loading
loading

## composer_placeholder
Add comments to steer the conversation

## composer_as
fields: Name
as {{.Name}}

## composer_name_prompt
Your name, shown with your comment:

## composer_name_invalid
A name is letters, digits, '.', '_' or '-', starting with a letter or digit, at most 64 characters.

## composer_closed
This conversation has concluded; comments are closed.

## composer_sending
sending

## filter
filter

## search_title
fields: Namespace
Search {{.Namespace}}

## search_order
fields: Order
by {{.Order}}

## search_prompt
Type words or "a phrase" to find; word* matches the start of a word.

## search_empty
No messages match.

## search_count
fields: Total
{{.Total}} {{if eq .Total 1}}message matches{{else}}messages match{{end}}

## error
fields: Err
error: {{.Err}}

## retrying
fields: Err
lost the server ({{.Err}}); retrying

## hints_namespaces
enter open · / filter · r refresh · q quit · ? help

## hints_conversations
enter open · s search · / filter · a archived · r refresh · esc back · ? help

## hints_conversation
i comment · p participants · g/G top/bottom · esc back · ? help

## hints_concluded
p participants · g/G top/bottom · esc back · ? help

## hints_composer
enter send · shift+enter/ctrl+j new line · esc leave

## hints_search
enter open · s new search · o order · esc back · ? help

## hints_search_input
enter search · esc cancel

## help_title
Keys

## help
↑ ↓ or j k      move, or scroll the conversation
pgup pgdn       scroll a page
g G             top, bottom (bottom follows new messages; the top loads earlier ones)
enter           open
/               filter a list
s               search the namespace's messages (from its conversation list)
o               change the search order: relevance, newest, oldest
a               include archived conversations
r               refresh a list
p               show or hide participants
i or tab        write a comment
enter           send a comment (shift+enter or ctrl+j for a new line)
tab             complete an @name in a comment; again for the next match
esc             leave the comment box, close help, or go back
q               quit (ctrl+c anywhere)
?               show or hide this help

## glyph_sep
·

## glyph_bar
▎

## glyph_dot
●

## glyph_dot_stale
○

## glyph_bullet
•

## glyph_rule
─

## glyph_column
│

## glyph_cursor
›

## glyph_more
…

## glyph_caret
_
