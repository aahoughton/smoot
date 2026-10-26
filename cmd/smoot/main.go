// Command smoot is a conversation service for agents and the people watching
// them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/aahoughton/smoot/internal/auth"
	"github.com/aahoughton/smoot/internal/config"
	"github.com/aahoughton/smoot/internal/core"
	"github.com/aahoughton/smoot/internal/server"
	"github.com/aahoughton/smoot/internal/store"
	"github.com/aahoughton/smoot/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...". A
// build without it (go install, go build) reports the module version Go
// recorded, when there is one.
var version = "dev"

func init() {
	if bi, ok := debug.ReadBuildInfo(); ok {
		version = buildVersion(version, bi.Main.Version)
	}
}

// buildVersion keeps a version set with -ldflags, else uses the module
// version Go recorded; "(devel)" means Go recorded none.
func buildVersion(set, module string) string {
	if set != "dev" || module == "" || module == "(devel)" {
		return set
	}
	return module
}

const usage = `usage: smoot <command> [flags]

commands:
  serve       run the server (JSON API at /v1, MCP at /mcp)
  mcp         serve MCP on stdio, relaying to a running server
  wait        block until there is something new for an agent
  status      show where an agent is expected
  attach      upload a file to a conversation (server runs --attachments)
  get         download a file attached to a conversation
  tui         browse and follow conversations in the terminal
  client      set up an agent client: its config and skills (smoot client help)
  hook        run as an agent client hook (hook stop)
  whoami      show the server and who your token belongs to
  admin       manage accounts, tokens and members (smoot admin help)
  backup      download a copy of the server's database
  dump        download a dump of a namespace or of everything
  load        add a namespace dump to a running server
  db          work on a database file directly (smoot db help)
  health      check that the server is up
  version     print the version

Run 'smoot <command> -h' for a command's flags.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv config.Getenv) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve":
		cfg, err := config.ParseServe(args[1:], getenv, stderr)
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		logger := cfg.NewLogger(stderr)
		if err := serve(ctx, cfg, logger, nil); err != nil {
			logger.Error("serve failed", "err", err)
			return 1
		}
		return 0
	case "mcp":
		return runMCP(ctx, args[1:], stderr, getenv)
	case "wait":
		return runWait(ctx, args[1:], stdout, stderr, getenv)
	case "status":
		return runStatus(ctx, args[1:], stdout, stderr, getenv)
	case "attach":
		return runAttach(ctx, args[1:], stdout, stderr, getenv)
	case "get":
		return runGet(ctx, args[1:], stdout, stderr, getenv)
	case "client":
		return runClient(ctx, args[1:], stdout, stderr, getenv)
	case "hook":
		return runHook(ctx, args[1:], stdin, stdout, stderr, getenv)
	case "tui":
		return runTUI(ctx, args[1:], stderr, getenv)
	case "whoami":
		return runWhoami(ctx, args[1:], stdout, stderr, getenv)
	case "admin":
		return runAdmin(ctx, args[1:], stdout, stderr, getenv)
	case "backup":
		return runBackup(ctx, args[1:], stdout, stderr, getenv)
	case "dump":
		return runDump(ctx, args[1:], stdout, stderr, getenv)
	case "load":
		return runLoad(ctx, args[1:], stdin, stdout, stderr, getenv)
	case "db":
		return runDB(ctx, args[1:], stdin, stdout, stderr)
	case "health":
		return runHealth(ctx, args[1:], stdout, stderr, getenv)
	case "version", "--version":
		switch {
		case len(args) == 1:
			fmt.Fprintln(stdout, version)
			return 0
		case args[1] == "-h" || args[1] == "--help":
			fmt.Fprintln(stdout, "usage: smoot version")
			return 0
		}
		fmt.Fprintf(stderr, "smoot version: unexpected argument %q\n", args[1])
		return 2
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "smoot: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// serve runs the HTTP server until ctx is done. If ready is non-nil it
// receives the bound address once the listener is open.
func serve(ctx context.Context, cfg config.Serve, logger *slog.Logger, ready chan<- string) error {
	policy, err := auth.PolicyFor(cfg.Auth)
	if err != nil {
		return err
	}
	db, err := store.OpenWith(ctx, cfg.DB, store.Options{Logger: logger, Snapshot: true})
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	removeStaleScratch(filepath.Dir(cfg.DB), time.Now(), logger)
	opts := core.Options{
		Policy: policy, Feedback: cfg.Feedback,
		Attachments: cfg.Attachments, AttachmentCap: cfg.AttachmentCap, AttachmentQuota: cfg.AttachmentQuota,
		AttachmentExpiry: cfg.AttachmentExpiry,
	}
	var failures *auth.Failures
	if cfg.Auth == config.AuthToken {
		opts.WriteLimit = cfg.WriteLimit
		failures = auth.NewFailures(cfg.AuthFailureLimit, cfg.TrustedProxies, nil)
	}
	svc := core.New(db, opts)
	authn, err := auth.For(cfg.Auth, svc)
	if err != nil {
		return err
	}
	activity := auth.NewActivity(cfg.TrustedProxies, nil)
	if cfg.Auth == config.AuthToken {
		authn = auth.Limited{Next: authn, Failures: failures, Activity: activity}
	}
	var viewer http.Handler
	if cfg.Web {
		// Browsers sign in with a session cookie; the API and MCP take
		// bearer tokens only. A rejected cookie is not counted as a failed
		// sign-in: the server issued it, it cannot be guessed, and a tab
		// left open on an ended session keeps sending it. Tokens pasted at
		// the login page are counted there.
		viewerAuth := authn
		if cfg.Auth == config.AuthToken {
			viewerAuth = auth.Limited{Next: auth.Session{Service: svc}, Activity: activity}
		}
		viewer, err = web.Handler(svc, viewerAuth, web.Options{
			ThemeDir: cfg.ThemeDir, Logger: logger, Version: version,
			Login: cfg.Auth == config.AuthToken, SecureCookies: strings.HasPrefix(cfg.PublicURL, "https://"),
			Failures: failures, Activity: activity,
		})
		if err != nil {
			return err
		}
	} else if cfg.ThemeDir != "" {
		return errors.New("--theme-dir has no effect without --web")
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Addr)
	if err != nil {
		return err
	}
	hosts, err := allowedHosts(cfg, ln.Addr())
	if err != nil {
		_ = ln.Close()
		return err
	}
	if cfg.Auth == config.AuthNone {
		logger.Warn("auth is off: anyone who can reach this address can read and post", "addr", ln.Addr().String())
		if cfg.Feedback {
			logger.Warn("auth is off: anyone who can reach this address can read agent feedback")
		}
		if cfg.Attachments {
			logger.Warn("auth is off: anyone who can reach this address can upload and download files")
		}
	}
	logger.Info("listening", "addr", ln.Addr().String(), "db", cfg.DB, "web", cfg.Web, "feedback", cfg.Feedback,
		"attachments", cfg.Attachments, "version", version)
	if ready != nil {
		ready <- ln.Addr().String()
	}

	// Request contexts derive from base, which is cancelled at shutdown so
	// long-poll waits return instead of holding Shutdown open.
	base, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	srv := &http.Server{
		Handler: server.New(server.Deps{
			Service: svc, Authenticator: authn, Logger: logger, MCPMaxWait: cfg.MCPMaxWait, Version: version, AuthMode: string(cfg.Auth),
			AllowedHosts: hosts, Web: viewer, Activity: activity, PublicURL: cfg.PublicURL,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return base },
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	if cfg.Attachments {
		go expireAttachments(base, svc, logger, attachmentSweepEvery)
	}

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	cancelBase()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	logger.Info("stopped")
	return nil
}

// allowedHosts resolves the Host allowlist: explicit configuration wins;
// otherwise a loopback listener answers only loopback names and any other
// listener answers any host. It refuses that last case with auth off,
// where a web page could reach the open server by rebinding a name.
func allowedHosts(cfg config.Serve, addr net.Addr) ([]string, error) {
	switch {
	case cfg.AnyHost:
		return nil, nil
	case len(cfg.AllowedHosts) > 0:
		return cfg.AllowedHosts, nil
	}
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return nil, nil
	}
	if tcp.IP.IsLoopback() {
		return server.LoopbackHosts, nil
	}
	if cfg.Auth == config.AuthNone {
		return nil, fmt.Errorf("listening on %s with auth off needs --allowed-hosts: list the names clients use, or * for any", addr)
	}
	return nil, nil
}

// staleScratch is how old a scratch directory left by a load or backup must
// be before serve removes it; younger ones may belong to one in progress.
const staleScratch = time.Hour

// removeStaleScratch removes the private scratch directories a load or
// backup that crashed left beside the database; they hold copies of
// conversations.
func removeStaleScratch(dir string, now time.Time, logger *slog.Logger) {
	for _, pattern := range []string{".smoot-load-*", ".smoot-backup-*"} {
		matches, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil {
			continue
		}
		for _, m := range matches {
			info, err := os.Lstat(m)
			if err != nil || !info.IsDir() || now.Sub(info.ModTime()) < staleScratch {
				continue
			}
			if err := os.RemoveAll(m); err != nil {
				logger.Warn("could not remove a stale scratch directory", "path", m, "err", err)
				continue
			}
			logger.Info("removed a stale scratch directory", "path", m)
		}
	}
}

// attachmentSweepEvery is how often the server drops the bytes of
// attachments in conversations concluded longer ago than the expiry.
const attachmentSweepEvery = time.Hour

// expireAttachments runs sweepAttachments now and every interval until
// ctx is done.
func expireAttachments(ctx context.Context, svc *core.Service, logger *slog.Logger, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		sweepAttachments(ctx, svc, logger)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// sweepAttachments expires attachment bytes once and logs what it did.
func sweepAttachments(ctx context.Context, svc *core.Service, logger *slog.Logger) {
	n, err := svc.ExpireAttachments(ctx)
	switch {
	case err != nil && ctx.Err() == nil:
		logger.Error("attachment expiry failed", "err", err)
	case n > 0:
		logger.Info("attachments expired", "files", n)
	}
}
