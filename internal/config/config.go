// Package config resolves runtime settings for smoot subcommands.
//
// Precedence for every setting is: command-line flag, then environment
// variable, then built-in default.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Environment variable names.
const (
	EnvDB   = "SMOOT_DB"
	EnvAddr = "SMOOT_ADDR"
	EnvWeb  = "SMOOT_WEB"
	EnvAuth = "SMOOT_AUTH"

	EnvAllowedHosts = "SMOOT_ALLOWED_HOSTS"
	EnvThemeDir     = "SMOOT_THEME_DIR"

	EnvMCPMaxWait = "SMOOT_MCP_MAX_WAIT"

	EnvPublicURL = "SMOOT_PUBLIC_URL"
	EnvFeedback  = "SMOOT_FEEDBACK"

	EnvAttachments      = "SMOOT_ATTACHMENTS"
	EnvAttachmentCap    = "SMOOT_ATTACHMENT_CAP_MB"
	EnvAttachmentQuota  = "SMOOT_ATTACHMENT_QUOTA_MB"
	EnvAttachmentExpiry = "SMOOT_ATTACHMENT_EXPIRY"

	EnvWriteLimit       = "SMOOT_WRITE_LIMIT"
	EnvAuthFailureLimit = "SMOOT_AUTH_FAILURE_LIMIT"
	EnvTrustedProxies   = "SMOOT_TRUSTED_PROXIES"

	EnvLogLevel  = "SMOOT_LOG_LEVEL"
	EnvLogFormat = "SMOOT_LOG_FORMAT"
)

// DefaultMCPMaxWait keeps MCP waits under the 60s tool-call timeout several
// clients use by default.
const DefaultMCPMaxWait = 50 * time.Second

// Rate limit defaults, in auth mode token.
const (
	DefaultWriteLimit       = 300
	DefaultAuthFailureLimit = 10
)

// Attachment storage defaults.
const (
	DefaultAttachmentCapMB   = 1024
	DefaultAttachmentQuotaMB = 100
	DefaultAttachmentExpiry  = 30 * 24 * time.Hour
)

// DefaultAddr listens on loopback only. Exposing the server to a network is a
// deliberate choice made with --addr or SMOOT_ADDR.
const DefaultAddr = "127.0.0.1:8787"

// AuthMode selects how requests are authenticated.
type AuthMode string

// Auth modes.
const (
	// AuthNone accepts every request as an anonymous principal with full
	// access.
	AuthNone AuthMode = "none"
	// AuthToken requires a bearer token (API, MCP) or a browser session
	// (viewer) and applies per-namespace roles.
	AuthToken AuthMode = "token"
)

// urlSafe are the characters a public URL may use. Agents are given it in
// a shell command, single-quoted (brackets, for IPv6 hosts, are glob
// characters), and none of these ends a quote.
const urlSafe = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~:/%[]"

// ShellSafeURL reports whether s uses only urlSafe characters, so it can
// go single-quoted in a command given to an agent.
func ShellSafeURL(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool { return !strings.ContainsRune(urlSafe, r) })
}

// Serve holds the settings for `smoot serve`.
type Serve struct {
	DB   string
	Addr string
	Web  bool
	Auth AuthMode
	// MCPMaxWait caps the MCP wait tool. Raise it only if every MCP client
	// in use has a longer tool-call timeout.
	MCPMaxWait time.Duration
	// AllowedHosts lists the Host names the server answers to. AnyHost
	// disables the check. When neither is set, the server allows loopback
	// names on a loopback address and any host otherwise.
	AllowedHosts []string
	AnyHost      bool
	// ThemeDir holds HTML theme files that replace the default theme's.
	ThemeDir string
	// WriteLimit is how many writes each account may make per minute in
	// auth mode token; 0 turns the limit off.
	WriteLimit int
	// AuthFailureLimit is how many failed authentications a client address
	// may have per minute in auth mode token; 0 turns the limit off.
	AuthFailureLimit int
	// TrustedProxies are proxies whose X-Forwarded-For names the client.
	TrustedProxies []netip.Prefix
	// PublicURL is the address browsers and agents use to reach the
	// server, when it differs from Addr (for example behind a TLS proxy).
	// The MCP instructions quote it, and an https URL marks the viewer's
	// session cookie Secure.
	PublicURL string
	// Feedback offers agents the MCP feedback tool, for notes on smoot
	// itself that only server admins read.
	Feedback bool
	// Attachments lets participants upload files to a conversation and
	// download them.
	Attachments bool
	// AttachmentCap bounds the bytes of all stored attachments.
	AttachmentCap int64
	// AttachmentQuota bounds the bytes stored for one conversation.
	AttachmentQuota int64
	// AttachmentExpiry is how long after a conversation concludes its
	// attachments' bytes are kept.
	AttachmentExpiry time.Duration
	// LogLevel is the least severe level logged. At info the server logs
	// each MCP tool call; at debug, each HTTP request too.
	LogLevel slog.Level
	// LogFormat is "text" (logfmt) or "json".
	LogFormat string
}

// NewLogger returns the logger cfg describes, writing to w.
func (cfg Serve) NewLogger(w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// Getenv looks up an environment variable; os.Getenv satisfies it.
type Getenv func(string) string

// ParseServe parses `smoot serve` arguments.
func ParseServe(args []string, getenv Getenv, output io.Writer) (Serve, error) {
	var cfg Serve
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.Usage = func() {
		fmt.Fprint(output, `usage: smoot serve --db PATH [flags]

Runs the server: the JSON API at /v1 and MCP at /mcp, and with --web the
HTML viewer. Each flag also reads the environment variable it names.

`)
		fs.PrintDefaults()
	}
	fs.StringVar(&cfg.DB, "db", "", "SQLite database path (env "+EnvDB+")")
	fs.StringVar(&cfg.Addr, "addr", "", "listen address (env "+EnvAddr+", default "+DefaultAddr+")")
	web := fs.Bool("web", false, "serve the HTML viewer (env "+EnvWeb+"=true)")
	feedback := fs.Bool("feedback", false, "offer agents a feedback tool for notes on smoot itself, read with smoot admin feedback (env "+EnvFeedback+"=true)")
	attachments := fs.Bool("attachments", false, "let participants upload files to a conversation and download them, with smoot attach and smoot get (env "+EnvAttachments+"=true)")
	attachCap := fs.String("attachment-cap-mb", "", "megabytes of attachments the server stores in all; full, it frees space from concluded conversations, oldest upload first (env "+EnvAttachmentCap+", default "+strconv.Itoa(DefaultAttachmentCapMB)+")")
	attachQuota := fs.String("attachment-quota-mb", "", "megabytes of attachments one conversation may hold (env "+EnvAttachmentQuota+", default "+strconv.Itoa(DefaultAttachmentQuotaMB)+")")
	attachExpiry := fs.String("attachment-expiry", "", "how long after a conversation concludes its attachments are kept, e.g. 720h (env "+EnvAttachmentExpiry+", default "+DefaultAttachmentExpiry.String()+")")
	auth := fs.String("auth", "", "auth mode: none or token (env "+EnvAuth+", default none)")
	fs.StringVar(&cfg.ThemeDir, "theme-dir", "", "directory of theme files overriding the default HTML theme (env "+EnvThemeDir+")")
	allowed := fs.String("allowed-hosts", "", "comma-separated Host names to answer, or * for any (env "+EnvAllowedHosts+", default: loopback names on a loopback address, else any; required beyond loopback with auth off)")
	publicURL := fs.String("public-url", "", "the URL browsers and agents reach the server at, e.g. https://smoot.example; MCP instructions quote it, and https marks session cookies Secure (env "+EnvPublicURL+")")
	writeLimit := fs.String("write-limit", "", "writes per minute each account may make with --auth token, and as many agent reads and waits; 0 for no limit (env "+EnvWriteLimit+", default "+strconv.Itoa(DefaultWriteLimit)+")")
	failLimit := fs.String("auth-failure-limit", "", "failed sign-ins per minute per client address with --auth token; 0 for no limit (env "+EnvAuthFailureLimit+", default "+strconv.Itoa(DefaultAuthFailureLimit)+")")
	proxies := fs.String("trusted-proxies", "", "comma-separated proxy addresses or CIDRs whose X-Forwarded-For names the client (env "+EnvTrustedProxies+")")
	logLevel := fs.String("log-level", "", "least severe level logged: debug, info, warn or error (env "+EnvLogLevel+", default info)")
	logFormat := fs.String("log-format", "", "log format: text or json (env "+EnvLogFormat+", default text)")
	mcpMaxWait := fs.String("mcp-max-wait", "", "longest MCP wait, e.g. 50s; keep it under your MCP clients' tool timeout (env "+EnvMCPMaxWait+", default 50s)")
	if err := fs.Parse(args); err != nil {
		return Serve{}, err
	}
	if fs.NArg() > 0 {
		return Serve{}, fmt.Errorf("serve: unexpected argument %q", fs.Arg(0))
	}

	if cfg.DB == "" {
		cfg.DB = getenv(EnvDB)
	}
	if cfg.DB == "" {
		p, err := DefaultDBPath(getenv)
		if err != nil {
			return Serve{}, err
		}
		cfg.DB = p
	}

	cfg.Addr = firstNonEmpty(cfg.Addr, getenv(EnvAddr), DefaultAddr)
	cfg.ThemeDir = firstNonEmpty(cfg.ThemeDir, getenv(EnvThemeDir))

	for _, b := range []struct {
		flag, env string
		val       bool
		dst       *bool
	}{
		{"web", EnvWeb, *web, &cfg.Web},
		{"feedback", EnvFeedback, *feedback, &cfg.Feedback},
		{"attachments", EnvAttachments, *attachments, &cfg.Attachments},
	} {
		*b.dst = b.val
		if !flagSet(fs, b.flag) {
			if s := getenv(b.env); s != "" {
				v, err := strconv.ParseBool(s)
				if err != nil {
					return Serve{}, fmt.Errorf("serve: invalid %s value %q: want true or false", b.env, s)
				}
				*b.dst = v
			}
		}
	}

	mode, err := parseAuth(firstNonEmpty(*auth, getenv(EnvAuth), string(AuthNone)))
	if err != nil {
		return Serve{}, err
	}
	cfg.Auth = mode

	waitStr := firstNonEmpty(*mcpMaxWait, getenv(EnvMCPMaxWait), DefaultMCPMaxWait.String())
	d, err := time.ParseDuration(waitStr)
	if err != nil || d < time.Second || d > 10*time.Minute {
		return Serve{}, fmt.Errorf("serve: invalid mcp-max-wait %q: want a duration between 1s and 10m", waitStr)
	}
	cfg.MCPMaxWait = d

	for _, l := range []struct {
		name, flag, env string
		def             int
		dst             *int64
	}{
		{"attachment-cap-mb", *attachCap, EnvAttachmentCap, DefaultAttachmentCapMB, &cfg.AttachmentCap},
		{"attachment-quota-mb", *attachQuota, EnvAttachmentQuota, DefaultAttachmentQuotaMB, &cfg.AttachmentQuota},
	} {
		v := firstNonEmpty(l.flag, getenv(l.env), strconv.Itoa(l.def))
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 || n > 1<<20 {
			return Serve{}, fmt.Errorf("serve: invalid %s %q: want a number of megabytes from 1 to 1048576", l.name, v)
		}
		*l.dst = n << 20
	}
	if cfg.AttachmentQuota > cfg.AttachmentCap {
		return Serve{}, fmt.Errorf("serve: attachment-quota-mb %d is larger than attachment-cap-mb %d", cfg.AttachmentQuota>>20, cfg.AttachmentCap>>20)
	}
	expStr := firstNonEmpty(*attachExpiry, getenv(EnvAttachmentExpiry), DefaultAttachmentExpiry.String())
	exp, err := time.ParseDuration(expStr)
	if err != nil || exp < time.Minute {
		return Serve{}, fmt.Errorf("serve: invalid attachment-expiry %q: want a duration of at least 1m, e.g. 720h", expStr)
	}
	cfg.AttachmentExpiry = exp

	cfg.PublicURL = firstNonEmpty(*publicURL, getenv(EnvPublicURL))
	if cfg.PublicURL != "" {
		// It is quoted to agents in a shell command, so it may hold only a
		// scheme, host and path (no credentials, query or fragment), in
		// characters a shell takes literally.
		u, err := url.Parse(cfg.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
			!ShellSafeURL(cfg.PublicURL) {
			return Serve{}, fmt.Errorf("serve: invalid public-url %q: want http(s)://host[:port][/path]", cfg.PublicURL)
		}
	}

	for _, l := range []struct {
		name, flag, env string
		def             int
		dst             *int
	}{
		{"write-limit", *writeLimit, EnvWriteLimit, DefaultWriteLimit, &cfg.WriteLimit},
		{"auth-failure-limit", *failLimit, EnvAuthFailureLimit, DefaultAuthFailureLimit, &cfg.AuthFailureLimit},
	} {
		v := firstNonEmpty(l.flag, getenv(l.env), strconv.Itoa(l.def))
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return Serve{}, fmt.Errorf("serve: invalid %s %q: want a number, 0 for no limit", l.name, v)
		}
		*l.dst = n
	}
	if list := strings.TrimSpace(firstNonEmpty(*proxies, getenv(EnvTrustedProxies))); list != "" {
		for item := range strings.SplitSeq(list, ",") {
			item = strings.TrimSpace(item)
			pfx, err := netip.ParsePrefix(item)
			if err != nil {
				addr, aerr := netip.ParseAddr(item)
				if aerr != nil {
					return Serve{}, fmt.Errorf("serve: invalid trusted-proxies entry %q: want an address or CIDR", item)
				}
				pfx = netip.PrefixFrom(addr, addr.BitLen())
			}
			cfg.TrustedProxies = append(cfg.TrustedProxies, pfx)
		}
	}

	lvl := firstNonEmpty(*logLevel, getenv(EnvLogLevel), "info")
	switch strings.ToLower(lvl) {
	case "debug", "info", "warn", "error":
		_ = cfg.LogLevel.UnmarshalText([]byte(lvl))
	default:
		return Serve{}, fmt.Errorf("serve: invalid log-level %q: want debug, info, warn or error", lvl)
	}
	cfg.LogFormat = firstNonEmpty(*logFormat, getenv(EnvLogFormat), "text")
	if cfg.LogFormat != "text" && cfg.LogFormat != "json" {
		return Serve{}, fmt.Errorf("serve: invalid log-format %q: want text or json", cfg.LogFormat)
	}

	switch hosts := strings.TrimSpace(firstNonEmpty(*allowed, getenv(EnvAllowedHosts))); hosts {
	case "":
	case "*":
		cfg.AnyHost = true
	default:
		for h := range strings.SplitSeq(hosts, ",") {
			if h = strings.TrimSpace(h); h == "" || h == "*" {
				return Serve{}, fmt.Errorf("serve: invalid allowed-hosts %q: want names separated by commas, or * alone", hosts)
			}
			cfg.AllowedHosts = append(cfg.AllowedHosts, h)
		}
	}
	return cfg, nil
}

// DefaultDBPath returns $XDG_DATA_HOME/smoot/smoot.db, falling back to
// $HOME/.local/share/smoot/smoot.db.
func DefaultDBPath(getenv Getenv) (string, error) {
	if dir := getenv("XDG_DATA_HOME"); dir != "" {
		if !filepath.IsAbs(dir) {
			return "", fmt.Errorf("XDG_DATA_HOME must be absolute, got %q", dir)
		}
		return filepath.Join(dir, "smoot", "smoot.db"), nil
	}
	home := getenv("HOME")
	if home == "" {
		return "", errors.New("cannot choose a default database path: set " + EnvDB + " or HOME")
	}
	return filepath.Join(home, ".local", "share", "smoot", "smoot.db"), nil
}

func parseAuth(s string) (AuthMode, error) {
	switch AuthMode(s) {
	case AuthNone, AuthToken:
		return AuthMode(s), nil
	case "email":
		return "", fmt.Errorf("serve: auth mode %q is not implemented yet", s)
	default:
		return "", fmt.Errorf("serve: unknown auth mode %q: want none or token", s)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// flagSet reports whether the named flag was given on the command line.
func flagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}
