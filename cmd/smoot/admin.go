package main

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/aahoughton/smoot/internal/client"
	"github.com/aahoughton/smoot/internal/config"
	"github.com/aahoughton/smoot/internal/core"
)

const adminUsage = `usage: smoot admin <command> [flags]

Through a running server (--url or SMOOT_URL), with a token (--token or
SMOOT_TOKEN). To create the first server admin, or recover one, use smoot
db bootstrap or smoot db recover.

  status [--json]                   who is using the server: recent callers,
                                    open MCP sessions, waiting agents, viewers
  accounts list
  accounts add NAME [--server-admin]
  accounts disable|enable|promote|demote NAME
  tokens list ACCOUNT
  tokens issue ACCOUNT [--label TEXT] [--ttl DURATION]
  tokens revoke ACCOUNT ID [--dry-run] [--force]
  tokens revoke-all ACCOUNT [--dry-run] [--force]
  members list NAMESPACE
  members set NAMESPACE ACCOUNT viewer|observer|member|admin
  members remove NAMESPACE ACCOUNT
  namespaces create NAME
  feedback list [--in NS[/CONVERSATION]] [--since TIME] [--limit N]
                                    agents' notes on smoot (serve --feedback),
                                    newest first; TIME is RFC 3339 or an age
                                    such as 24h
  feedback show ID                  one note in full, with its conversation
  attachments list                  attachment storage by conversation, with
                                    the cap and quota (serve --attachments)
  attachments rm SHA256             drop stored bytes with this sha256, in
                                    every conversation
  attachments rm-conversation NAMESPACE CONVERSATION
                                    drop every stored file in a conversation;
                                    attachment messages stay, marked deleted

A new token is printed once; store it where the client reads it
(SMOOT_TOKEN). Token IDs are numbered across the server; tokens list and
tokens issue show them. Revoking cannot be undone: --dry-run shows what
would be revoked, and --force is needed to revoke the token the command
itself uses. Server admins manage accounts and namespaces; an account
manages its own tokens; namespace admins manage their members.
`

func runAdmin(ctx context.Context, args []string, stdout, stderr io.Writer, getenv config.Getenv) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, adminUsage)
		return exitUsage
	}
	switch args[0] {
	case "accounts", "tokens", "members", "namespaces", "feedback", "attachments":
		return runAdminRemote(ctx, args, stdout, stderr, getenv)
	case "status":
		return runAdminStatus(ctx, args[1:], stdout, stderr, getenv)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, adminUsage)
		return exitOK
	default:
		fmt.Fprintf(stderr, "smoot admin: unknown command %q\n\n%s", args[0], adminUsage)
		return exitUsage
	}
}

// adminRun is what a remote admin command does with its arguments.
type adminRun func(ctx context.Context, cl *client.Client, args []string, out io.Writer) error

// adminCommand is one remote admin command: its arguments for usage, their
// count, and setup, which registers the command's own flags and returns
// what it runs.
type adminCommand struct {
	usage string
	args  int
	setup func(fs *flag.FlagSet) adminRun
}

// noFlags is an adminCommand that takes no flags of its own.
func noFlags(usage string, args int, run adminRun) adminCommand {
	return adminCommand{usage, args, func(*flag.FlagSet) adminRun { return run }}
}

func runAdminRemote(ctx context.Context, args []string, stdout, stderr io.Writer, getenv config.Getenv) int {
	if len(args) < 2 {
		fmt.Fprint(stderr, adminUsage)
		return exitUsage
	}
	group, verb := args[0], args[1]

	setAccount := func(disabled, admin *bool) adminCommand {
		return noFlags("NAME", 1, func(ctx context.Context, cl *client.Client, a []string, out io.Writer) error {
			acct, err := cl.UpdateAccount(ctx, a[0], disabled, admin)
			if err == nil {
				printAccounts(out, []core.Account{acct})
			}
			return err
		})
	}
	yes, no := true, false
	commands := map[string]adminCommand{
		"accounts list": noFlags("", 0, func(ctx context.Context, cl *client.Client, _ []string, out io.Writer) error {
			as, err := cl.ListAccounts(ctx)
			if err == nil {
				printAccounts(out, as)
			}
			return err
		}),
		"accounts add": {"NAME [--server-admin]", 1, func(fs *flag.FlagSet) adminRun {
			serverAdmin := fs.Bool("server-admin", false, "make the account a server admin")
			return func(ctx context.Context, cl *client.Client, a []string, out io.Writer) error {
				acct, err := cl.CreateAccount(ctx, a[0], *serverAdmin)
				if err == nil {
					printAccounts(out, []core.Account{acct})
				}
				return err
			}
		}},
		"accounts disable": setAccount(&yes, nil),
		"accounts enable":  setAccount(&no, nil),
		"accounts promote": setAccount(nil, &yes),
		"accounts demote":  setAccount(nil, &no),
		"tokens list": noFlags("ACCOUNT", 1, func(ctx context.Context, cl *client.Client, a []string, out io.Writer) error {
			ts, err := cl.ListTokens(ctx, a[0])
			if err == nil {
				printTokens(out, ts)
			}
			return err
		}),
		"tokens issue": {"ACCOUNT [--label TEXT] [--ttl DURATION]", 1, func(fs *flag.FlagSet) adminRun {
			label := fs.String("label", "", "a label to recognize the token by")
			ttl := fs.Duration("ttl", 0, "lifetime, e.g. 720h (default: never expires)")
			return func(ctx context.Context, cl *client.Client, a []string, out io.Writer) error {
				t, err := cl.IssueToken(ctx, a[0], *label, *ttl)
				if err == nil {
					fmt.Fprintf(out, "token %d for %s (shown once):\n%s\n", t.ID, t.Account, t.Secret)
				}
				return err
			}
		}},
		"tokens revoke": {"ACCOUNT ID [--dry-run] [--force]", 2, func(fs *flag.FlagSet) adminRun {
			dryRun, force := revokeFlags(fs)
			return func(ctx context.Context, cl *client.Client, a []string, out io.Writer) error {
				id, err := strconv.ParseInt(a[1], 10, 64)
				if err != nil || id < 1 {
					return fmt.Errorf("token id %q is not a positive integer", a[1])
				}
				var ts []core.Token
				if *dryRun {
					ts, err = cl.ListTokens(ctx, a[0])
					if err != nil {
						return err
					}
					i := slices.IndexFunc(ts, func(t core.Token) bool { return t.ID == id })
					if i < 0 {
						return fmt.Errorf("%s: token %d of %s not found", core.CodeTokenNotFound, id, a[0])
					}
					ts = slices.DeleteFunc(ts[i:i+1], revoked)
				} else if ts, err = cl.RevokeToken(ctx, a[0], id, *force); err != nil {
					return err
				}
				if len(ts) == 0 {
					fmt.Fprintf(out, "token %d of %s is already revoked\n", id, a[0])
				}
				printRevoked(out, ts, *dryRun)
				return nil
			}
		}},
		"tokens revoke-all": {"ACCOUNT [--dry-run] [--force]", 1, func(fs *flag.FlagSet) adminRun {
			dryRun, force := revokeFlags(fs)
			return func(ctx context.Context, cl *client.Client, a []string, out io.Writer) error {
				var ts []core.Token
				var err error
				if *dryRun {
					ts, err = cl.ListTokens(ctx, a[0])
					ts = slices.DeleteFunc(ts, revoked)
				} else {
					ts, err = cl.RevokeAllTokens(ctx, a[0], *force)
				}
				if err != nil {
					return err
				}
				if len(ts) == 0 {
					fmt.Fprintf(out, "%s has no tokens to revoke\n", a[0])
				}
				printRevoked(out, ts, *dryRun)
				return nil
			}
		}},
		"members list": noFlags("NAMESPACE", 1, func(ctx context.Context, cl *client.Client, a []string, out io.Writer) error {
			ms, err := cl.ListMembers(ctx, a[0])
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ACCOUNT\tROLE")
			for _, m := range ms {
				fmt.Fprintf(tw, "%s\t%s\n", m.Account, m.Role)
			}
			return tw.Flush()
		}),
		"members set": noFlags("NAMESPACE ACCOUNT viewer|observer|member|admin", 3, func(ctx context.Context, cl *client.Client, a []string, out io.Writer) error {
			m, err := cl.SetMember(ctx, a[0], a[1], core.Role(a[2]))
			if err == nil {
				fmt.Fprintf(out, "%s is %s in %s\n", m.Account, m.Role, m.Namespace)
			}
			return err
		}),
		"members remove": noFlags("NAMESPACE ACCOUNT", 2, func(ctx context.Context, cl *client.Client, a []string, out io.Writer) error {
			if err := cl.RemoveMember(ctx, a[0], a[1]); err != nil {
				return err
			}
			fmt.Fprintf(out, "%s has no role in %s\n", a[1], a[0])
			return nil
		}),
		"feedback list": {"[--in NS[/CONVERSATION]] [--since TIME] [--limit N]", 0, func(fs *flag.FlagSet) adminRun {
			in := fs.String("in", "", "only notes from this namespace, or NS/CONVERSATION")
			since := fs.String("since", "", "only notes written at or after this RFC 3339 time, or this long ago (e.g. 24h)")
			limit := fs.Int("limit", 0, "at most this many notes (default 50)")
			return func(ctx context.Context, cl *client.Client, _ []string, out io.Writer) error {
				f := core.FeedbackFilter{Limit: *limit}
				f.Namespace, f.Conversation, _ = strings.Cut(*in, "/")
				if strings.Contains(f.Conversation, "/") {
					return fmt.Errorf("--in %q: want NAMESPACE or NAMESPACE/CONVERSATION", *in)
				}
				if *since != "" {
					t, err := parseSince(*since, time.Now())
					if err != nil {
						return err
					}
					f.Since = &t
				}
				page, err := cl.ListFeedback(ctx, f)
				if err == nil {
					printFeedbackList(out, page)
				}
				return err
			}
		}},
		"feedback show": noFlags("ID", 1, func(ctx context.Context, cl *client.Client, a []string, out io.Writer) error {
			id, err := strconv.ParseInt(a[0], 10, 64)
			if err != nil {
				return fmt.Errorf("feedback id %q is not a number", a[0])
			}
			f, err := cl.GetFeedback(ctx, id)
			if err == nil {
				printFeedback(out, cl.URL(), f)
			}
			return err
		}),
		"attachments list": noFlags("", 0, func(ctx context.Context, cl *client.Client, _ []string, out io.Writer) error {
			rep, err := cl.AttachmentStorage(ctx)
			if err == nil {
				printAttachmentStorage(out, rep)
			}
			return err
		}),
		"attachments rm": noFlags("SHA256", 1, func(ctx context.Context, cl *client.Client, a []string, out io.Writer) error {
			return deleteAttachments(ctx, cl, core.AttachmentDelete{SHA256: a[0]}, out)
		}),
		"attachments rm-conversation": noFlags("NAMESPACE CONVERSATION", 2, func(ctx context.Context, cl *client.Client, a []string, out io.Writer) error {
			return deleteAttachments(ctx, cl, core.AttachmentDelete{Namespace: a[0], Conversation: a[1]}, out)
		}),
		"namespaces create": noFlags("NAME", 1, func(ctx context.Context, cl *client.Client, a []string, out io.Writer) error {
			ns, err := cl.CreateNamespace(ctx, a[0])
			if err == nil {
				fmt.Fprintf(out, "created namespace %s\n", ns.Name)
			}
			return err
		}),
	}
	cmd, ok := commands[group+" "+verb]
	if !ok {
		fmt.Fprintf(stderr, "smoot admin: unknown command %q\n\n%s", group+" "+verb, adminUsage)
		return exitUsage
	}
	fs := flag.NewFlagSet("admin "+group+" "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: smoot admin %s %s %s\n\n", group, verb, strings.TrimSpace(cmd.usage+" [flags]"))
		fs.PrintDefaults()
	}
	resolveClient := config.ClientFlags(fs, config.WithToken)
	run := cmd.setup(fs)
	if err := fs.Parse(args[2:]); err != nil {
		return flagError(stderr, err)
	}
	// Flags may also follow the arguments.
	rest := fs.Args()
	var positional []string
	for len(rest) > 0 {
		if strings.HasPrefix(rest[0], "-") {
			if err := fs.Parse(rest); err != nil {
				return flagError(stderr, err)
			}
			rest = fs.Args()
			continue
		}
		positional, rest = append(positional, rest[0]), rest[1:]
	}
	if len(positional) != cmd.args {
		return flagError(stderr, fmt.Errorf("%s %s takes %d arguments, got %d", group, verb, cmd.args, len(positional)))
	}
	cc, err := resolveClient(getenv)
	if err != nil {
		return flagError(stderr, err)
	}
	cl, err := client.New(cc.URL, cc.Token)
	if err != nil {
		return flagError(stderr, err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := run(ctx, cl, positional, stdout); err != nil {
		fmt.Fprintln(stderr, "smoot admin:", err)
		return exitError
	}
	return exitOK
}

func printAttachmentStorage(out io.Writer, rep core.AttachmentReport) {
	fmt.Fprintf(out, "%s stored of a %s cap; each conversation may hold %s\n",
		core.FormatSize(rep.Bytes), core.FormatSize(rep.Cap), core.FormatSize(rep.Quota))
	if len(rep.Conversations) == 0 {
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CONVERSATION\tSTATUS\tFILES\tSTORED\tGONE")
	for _, u := range rep.Conversations {
		fmt.Fprintf(tw, "%s/%s\t%s\t%d\t%s\t%d\n", u.Namespace, u.Conversation, u.Status, u.Blobs, core.FormatSize(u.Bytes), u.Gone)
	}
	_ = tw.Flush()
}

func deleteAttachments(ctx context.Context, cl *client.Client, d core.AttachmentDelete, out io.Writer) error {
	blobs, size, err := cl.DeleteAttachments(ctx, d)
	if err == nil {
		fmt.Fprintf(out, "dropped %d stored %s (%s)\n", blobs, plural(blobs, "file", "files"), core.FormatSize(size))
	}
	return err
}

func plural(n int64, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// parseSince reads an RFC 3339 time, or a duration counted back from now.
func parseSince(s string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return time.Time{}, fmt.Errorf("--since %q: want an RFC 3339 time or a duration such as 24h", s)
	}
	return now.Add(-d), nil
}

// feedbackPreview is how much of a note the list shows.
const feedbackPreview = 60

func printFeedbackList(out io.Writer, page core.FeedbackPage) {
	if len(page.Feedback) == 0 {
		fmt.Fprintln(out, "No feedback.")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tWRITTEN\tCONVERSATION\tAGENT\tACCOUNT\tNOTE")
	for _, f := range page.Feedback {
		note := strings.Join(strings.Fields(f.Body), " ")
		if r := []rune(note); len(r) > feedbackPreview {
			note = string(r[:feedbackPreview-3]) + "..."
		}
		fmt.Fprintf(tw, "%d\t%s\t%s/%s\t%s\t%s\t%s\n", f.ID, f.UpdatedAt.Local().Format("2006-01-02 15:04"),
			f.Namespace, f.Conversation, shown(f.Agent), shown(cmp.Or(f.Account, "-")), shown(note))
	}
	_ = tw.Flush()
	if page.More {
		fmt.Fprintln(out, "More notes are older; narrow with --since or raise --limit.")
	}
}

// printFeedback writes one note with what an admin needs to find its
// context: the conversation, how far the agent had read, and the
// conversation's viewer address (served with serve --web).
func printFeedback(out io.Writer, base string, f core.Feedback) {
	fmt.Fprintf(out, "feedback %d from %s", f.ID, shown(f.Agent))
	if f.Account != "" {
		fmt.Fprintf(out, " (account %s)", shown(f.Account))
	}
	fmt.Fprintf(out, "\nconversation: %s/%s, read to #%d\n", f.Namespace, f.Conversation, f.SeenSeq)
	fmt.Fprintf(out, "viewer: %s/n/%s/c/%s\n", strings.TrimRight(base, "/"), f.Namespace, f.Conversation)
	fmt.Fprintf(out, "written: %s", f.CreatedAt.Local().Format("2006-01-02 15:04 MST"))
	if !f.UpdatedAt.Equal(f.CreatedAt) {
		fmt.Fprintf(out, ", replaced %s", f.UpdatedAt.Local().Format("2006-01-02 15:04 MST"))
	}
	fmt.Fprintf(out, "\n\n%s\n", shownBody(f.Body))
}

// shownBody is shown for multi-line text: it keeps newlines and tabs.
func shownBody(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = shown(strings.ReplaceAll(strings.TrimSuffix(l, "\r"), "\t", "    "))
	}
	return strings.Join(lines, "\n")
}

func printAccounts(out io.Writer, as []core.Account) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSERVER ADMIN\tDISABLED\tCREATED")
	for _, a := range as {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", a.Name, yesNo(a.ServerAdmin), yesNo(a.Disabled), a.CreatedAt.Format(time.DateOnly))
	}
	_ = tw.Flush()
}

func printTokens(out io.Writer, ts []core.Token) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tLABEL\tCREATED\tLAST USED\tEXPIRES\tREVOKED")
	for _, t := range ts {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", t.ID, shown(t.Label), t.CreatedAt.Format(time.DateOnly),
			dateOr(t.LastUsedAt, "never"), dateOr(t.ExpiresAt, "never"), dateOr(t.RevokedAt, "-"))
	}
	_ = tw.Flush()
}

func revokeFlags(fs *flag.FlagSet) (dryRun, force *bool) {
	dryRun = fs.Bool("dry-run", false, "show what would be revoked without revoking it")
	force = fs.Bool("force", false, "allow revoking the token this command uses")
	return dryRun, force
}

func revoked(t core.Token) bool { return t.RevokedAt != nil }

// printRevoked prints one line per token, saying what was revoked or, for a
// dry run, what would be.
func printRevoked(out io.Writer, ts []core.Token, dryRun bool) {
	verb := "revoked"
	if dryRun {
		verb = "would revoke"
	}
	for _, t := range ts {
		var about []string
		if t.Label != "" {
			about = append(about, shown(t.Label))
		}
		if t.LastUsedAt == nil {
			about = append(about, "never used")
		} else {
			about = append(about, "last used "+t.LastUsedAt.Format(time.DateOnly))
		}
		fmt.Fprintf(out, "%s token %d of %s (%s)\n", verb, t.ID, shown(t.Account), strings.Join(about, ", "))
	}
}

// shown replaces the characters a terminal would act on (controls and
// bidirectional formatting) with '?', for printing text other people chose.
func shown(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || (r >= 0x7f && r < 0xa0) || unicode.Is(unicode.Bidi_Control, r) {
			return '?'
		}
		return r
	}, s)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func dateOr(t *time.Time, none string) string {
	if t == nil {
		return none
	}
	return t.Format(time.DateOnly)
}

func runAdminStatus(ctx context.Context, args []string, stdout, stderr io.Writer, getenv config.Getenv) int {
	fs := flag.NewFlagSet("admin status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, `usage: smoot admin status [--json] [flags]

Shows who is using the server: recent callers, open MCP sessions, waiting
agents and viewers.

`)
		fs.PrintDefaults()
	}
	resolveClient := config.ClientFlags(fs, config.WithToken)
	asJSON := fs.Bool("json", false, "print the server's JSON response")
	if err := fs.Parse(args); err != nil {
		return flagError(stderr, err)
	}
	if fs.NArg() > 0 {
		return flagError(stderr, fmt.Errorf("unexpected argument %q", fs.Arg(0)))
	}
	cc, err := resolveClient(getenv)
	if err != nil {
		return flagError(stderr, err)
	}
	cl, err := client.New(cc.URL, cc.Token)
	if err != nil {
		return flagError(stderr, err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	st, err := cl.ServerStatus(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "smoot admin:", err)
		return exitError
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(st)
		return exitOK
	}
	printStatus(stdout, cl.URL(), st)
	return exitOK
}

// printStatus writes a server status for a person deciding whether to
// restart. Ages are measured on the server's clock.
func printStatus(out io.Writer, url string, st client.ServerStatus) {
	ago := func(t time.Time) string { return age(st.Now.Sub(t)) }
	fmt.Fprintf(out, "server: %s (version %s, auth %s)\n", url, st.Version, st.AuthMode)
	fmt.Fprintf(out, "up %s, since %s\n", span(st.Now.Sub(st.StartedAt)), st.StartedAt.Local().Format("2006-01-02 15:04 MST"))
	if m := st.LastMessage; m != nil {
		fmt.Fprintf(out, "last message %s: %s/%s #%d by %s (%s)\n", ago(m.At), m.Namespace, m.Conversation, m.Seq, shown(m.Author), shown(m.Kind))
	} else {
		fmt.Fprintln(out, "no messages yet")
	}

	fmt.Fprintln(out)
	if len(st.Conversations) == 0 {
		fmt.Fprintln(out, "No conversations active in the last 24h, waited on or viewed.")
	} else {
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "CONVERSATION\tSTATUS\tLAST\tACTIVE\tWAITING\tVIEWING")
		for _, c := range st.Conversations {
			fmt.Fprintf(tw, "%s/%s\t%s\t%s\t%d\t%d\t%d\n", c.Namespace, c.Name, c.Status, ago(c.LastAt), c.Active, c.Waiting, c.Viewing)
		}
		_ = tw.Flush()
	}

	fmt.Fprintln(out)
	if len(st.MCPSessions) == 0 {
		fmt.Fprintln(out, "No open MCP sessions.")
	} else {
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "MCP SESSIONS\tACCOUNT\tLAST USED")
		for _, m := range st.MCPSessions {
			fmt.Fprintf(tw, "%d\t%s\t%s\n", m.Sessions, accountOr(m.Account), ago(m.LastUsed))
		}
		_ = tw.Flush()
	}

	fmt.Fprintln(out)
	if len(st.Callers) == 0 {
		fmt.Fprintln(out, "No callers since the server started.")
	} else {
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "LAST SEEN\tACCOUNT\tSURFACE\tADDRESS\tCLIENT\tREQUESTS\tFIRST SEEN")
		for _, c := range st.Callers {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n", ago(c.LastSeen), accountOr(c.Account), c.Surface, shown(c.Address),
				shown(cmp.Or(c.Client, "-")), c.Requests, ago(c.FirstSeen))
		}
		_ = tw.Flush()
	}

	if len(st.FailedSignIns) > 0 {
		fmt.Fprintln(out)
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "FAILED SIGN-INS\tADDRESS\tLAST")
		for _, f := range st.FailedSignIns {
			fmt.Fprintf(tw, "%d\t%s\t%s\n", f.Count, shown(f.Address), ago(f.LastAt))
		}
		_ = tw.Flush()
	}
}

func accountOr(name string) string { return shown(cmp.Or(name, "(anonymous)")) }

// age is a duration in the past, such as "5m ago".
func age(d time.Duration) string {
	if d < time.Second {
		return "now"
	}
	return span(d) + " ago"
}

// span is a duration in its two largest units, such as "2h13m".
func span(d time.Duration) string {
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d/time.Second)) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	default:
		return fmt.Sprintf("%dd%dh", int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour))
	}
}
