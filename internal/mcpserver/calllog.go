package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"regexp"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aahoughton/smoot/internal/core"
)

// Every tool call is logged at info as one "mcp call" line: who called what,
// the outcome, and what the agent was told to do next. Message bodies are
// never logged. Internal errors are logged at error with their cause, which
// the agent does not see.

type notesKey struct{}

// callNotes collects attributes a tool adds to its call's log line.
type callNotes struct {
	attrs []slog.Attr
	code  string
	err   error
}

// note adds attributes to the log line of the call running in ctx.
func note(ctx context.Context, attrs ...slog.Attr) {
	if n, ok := ctx.Value(notesKey{}).(*callNotes); ok {
		n.attrs = append(n.attrs, attrs...)
	}
}

// noteStanding records the agent's standing as the result reports it.
func noteStanding(ctx context.Context, st core.Standing) {
	note(ctx, slog.String("next", st.Next), slog.Bool("expected", st.Expected), slog.Int("unread", st.Unread))
}

func noteFailure(ctx context.Context, err error) {
	if n, ok := ctx.Value(notesKey{}).(*callNotes); ok {
		n.code = string(core.CodeOf(err))
		n.err = err
	}
}

// logCalls is receiving middleware that logs initialize and tools/call.
func logCalls(l *slog.Logger, p core.Principal, ns string) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch method {
			case "initialize":
				res, err := next(ctx, method, req)
				attrs := []any{"ns", ns, "account", p.AccountID, "session", sessionTag(req.GetSession())}
				if ip, ok := req.GetParams().(*mcp.InitializeParams); ok && ip.ClientInfo != nil {
					attrs = append(attrs, "client", ip.ClientInfo.Name, "client_version", ip.ClientInfo.Version)
				}
				l.InfoContext(ctx, "mcp session", attrs...)
				return res, err
			case "tools/call":
			default:
				return next(ctx, method, req)
			}
			n := &callNotes{}
			start := time.Now()
			res, err := next(context.WithValue(ctx, notesKey{}, n), method, req)
			logCall(ctx, l, p, ns, req, n, time.Since(start), res, err)
			return res, err
		}
	}
}

func logCall(ctx context.Context, l *slog.Logger, p core.Principal, ns string, req mcp.Request, n *callNotes, d time.Duration, res mcp.Result, err error) {
	attrs := []slog.Attr{slog.String("ns", ns), slog.Int64("account", p.AccountID), slog.String("session", sessionTag(req.GetSession()))}
	if params, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok {
		attrs = append(attrs, slog.String("tool", params.Name))
		conv, agent := callTarget(params)
		if conv != "" {
			attrs = append(attrs, slog.String("conv", conv))
		}
		if agent != "" {
			attrs = append(attrs, slog.String("agent", agent))
		}
	}
	outcome, level := "ok", slog.LevelInfo
	switch r, _ := res.(*mcp.CallToolResult); {
	case err != nil:
		outcome, level = "rpc_error", slog.LevelWarn
		attrs = append(attrs, slog.String("err", err.Error()))
	case n.err != nil && n.code == "":
		outcome, level = "internal", slog.LevelError
		attrs = append(attrs, slog.String("err", n.err.Error()))
	case n.code != "":
		outcome = n.code
	case r != nil && r.IsError:
		// Rejected before the tool ran, such as arguments that do not
		// match the schema. The schema error quotes the offending value,
		// which may be a message body, so only the parameter is logged.
		outcome = "rejected"
		if m := rejectedParam.FindStringSubmatch(resultText(r)); m != nil {
			attrs = append(attrs, slog.String("param", m[1]))
		}
	}
	attrs = append(attrs, slog.String("outcome", outcome), slog.Duration("dur", d.Round(time.Millisecond)))
	if r, ok := res.(*mcp.CallToolResult); ok && r != nil {
		attrs = append(attrs, slog.Int("out_bytes", len(resultText(r))))
	}
	attrs = append(attrs, n.attrs...)
	l.LogAttrs(ctx, level, "mcp call", attrs...)
}

// callTarget returns the conversation and agent a tool call names, if any.
func callTarget(params *mcp.CallToolParamsRaw) (conversation, agent string) {
	var target struct {
		Conversation string `json:"conversation"`
		Agent        string `json:"agent"`
	}
	if json.Unmarshal(params.Arguments, &target) != nil {
		return "", ""
	}
	return target.Conversation, target.Agent
}

// rejectedParam finds the parameter a schema validation error names.
var rejectedParam = regexp.MustCompile(`/properties/([A-Za-z_]+)`)

func resultText(r *mcp.CallToolResult) string {
	var s string
	for _, c := range r.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			s += t.Text
		}
	}
	return s
}

// sessionTag identifies a session in logs without writing its id, which
// grants use of the session.
func sessionTag(s mcp.Session) string {
	if s == nil || s.ID() == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s.ID()))
	return hex.EncodeToString(sum[:4])
}
