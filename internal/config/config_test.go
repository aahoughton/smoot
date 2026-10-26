package config

import (
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) Getenv {
	return func(k string) string { return m[k] }
}

func TestParseServeDefaults(t *testing.T) {
	cfg, err := ParseServe(nil, env(map[string]string{"HOME": "/home/u"}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := Serve{
		DB:         "/home/u/.local/share/smoot/smoot.db",
		Addr:       DefaultAddr,
		Web:        false,
		Auth:       AuthNone,
		MCPMaxWait: DefaultMCPMaxWait,
		LogLevel:   slog.LevelInfo,
		LogFormat:  "text",

		WriteLimit:       DefaultWriteLimit,
		AuthFailureLimit: DefaultAuthFailureLimit,

		AttachmentCap:    DefaultAttachmentCapMB << 20,
		AttachmentQuota:  DefaultAttachmentQuotaMB << 20,
		AttachmentExpiry: DefaultAttachmentExpiry,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("got %+v, want %+v", cfg, want)
	}
}

func TestParseServeXDGDataHome(t *testing.T) {
	cfg, err := ParseServe(nil, env(map[string]string{"HOME": "/home/u", "XDG_DATA_HOME": "/xdg"}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DB != "/xdg/smoot/smoot.db" {
		t.Fatalf("DB = %q", cfg.DB)
	}
}

func TestParseServeRelativeXDGDataHome(t *testing.T) {
	_, err := ParseServe(nil, env(map[string]string{"HOME": "/home/u", "XDG_DATA_HOME": "rel"}), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "XDG_DATA_HOME") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseServeNoHome(t *testing.T) {
	_, err := ParseServe(nil, env(nil), io.Discard)
	if err == nil || !strings.Contains(err.Error(), EnvDB) {
		t.Fatalf("err = %v", err)
	}
}

func TestParseServeNoHomeButEnvDB(t *testing.T) {
	cfg, err := ParseServe(nil, env(map[string]string{EnvDB: "/data/smoot.db"}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DB != "/data/smoot.db" {
		t.Fatalf("DB = %q", cfg.DB)
	}
}

func TestParseServeEnvOverridesDefault(t *testing.T) {
	cfg, err := ParseServe(nil, env(map[string]string{
		"HOME":        "/home/u",
		EnvDB:         "/env.db",
		EnvAddr:       ":9000",
		EnvWeb:        "true",
		EnvAuth:       "none",
		EnvMCPMaxWait: "2m",
		EnvLogLevel:   "debug",
		EnvLogFormat:  "json",
	}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := Serve{
		DB: "/env.db", Addr: ":9000", Web: true, Auth: AuthNone, MCPMaxWait: 2 * time.Minute,
		LogLevel: slog.LevelDebug, LogFormat: "json",
		WriteLimit: DefaultWriteLimit, AuthFailureLimit: DefaultAuthFailureLimit,
		AttachmentCap: DefaultAttachmentCapMB << 20, AttachmentQuota: DefaultAttachmentQuotaMB << 20,
		AttachmentExpiry: DefaultAttachmentExpiry,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("got %+v, want %+v", cfg, want)
	}
}

func TestParseServeFlagOverridesEnv(t *testing.T) {
	cfg, err := ParseServe(
		[]string{"--db", "/flag.db", "--addr", ":1", "--web=false"},
		env(map[string]string{EnvDB: "/env.db", EnvAddr: ":9000", EnvWeb: "true"}),
		io.Discard,
	)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DB != "/flag.db" || cfg.Addr != ":1" || cfg.Web {
		t.Fatalf("got %+v", cfg)
	}
}

func TestParseServeErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"bad web", []string{"--web=maybe"}, nil, "invalid boolean value"},
		{"bad web env", nil, map[string]string{EnvWeb: "yes please"}, "invalid SMOOT_WEB value"},
		{"bad feedback env", nil, map[string]string{EnvFeedback: "sure"}, "invalid SMOOT_FEEDBACK value"},
		{"unknown auth", []string{"--auth", "ldap"}, nil, `unknown auth mode "ldap"`},
		{"email not yet", []string{"--auth", "email"}, nil, "not implemented yet"},
		{"stray arg", []string{"extra"}, nil, "unexpected argument"},
		{"bad mcp wait", []string{"--mcp-max-wait", "soon"}, nil, "invalid mcp-max-wait"},
		{"mcp wait too short", []string{"--mcp-max-wait", "10ms"}, nil, "invalid mcp-max-wait"},
		{"unknown flag", []string{"--nope"}, nil, "flag provided but not defined"},
		{"bad log level", []string{"--log-level", "loud"}, nil, "invalid log-level"},
		{"log level offset", nil, map[string]string{EnvLogLevel: "info+2"}, "invalid log-level"},
		{"bad log format", []string{"--log-format", "xml"}, nil, "invalid log-format"},
		{"bad public url", []string{"--public-url", "smoot.example"}, nil, "invalid public-url"},
		{"public url with credentials", []string{"--public-url", "https://u:p@smoot.example"}, nil, "invalid public-url"},
		{"public url with a query", []string{"--public-url", "https://smoot.example/?x=1"}, nil, "invalid public-url"},
		{"public url with a fragment", []string{"--public-url", "https://smoot.example/#x"}, nil, "invalid public-url"},
		{"public url with shell characters", []string{"--public-url", "https://smoot.example/a&b"}, nil, "invalid public-url"},
		{"bad write limit", []string{"--write-limit", "lots"}, nil, "invalid write-limit"},
		{"negative failure limit", nil, map[string]string{EnvAuthFailureLimit: "-1"}, "invalid auth-failure-limit"},
		{"bad proxy", []string{"--trusted-proxies", "10.0.0.0/8,proxy.lan"}, nil, "invalid trusted-proxies entry"},
		{"bad attachments env", nil, map[string]string{EnvAttachments: "sure"}, "invalid SMOOT_ATTACHMENTS value"},
		{"zero attachment cap", []string{"--attachment-cap-mb", "0"}, nil, "invalid attachment-cap-mb"},
		{"bad attachment quota", nil, map[string]string{EnvAttachmentQuota: "lots"}, "invalid attachment-quota-mb"},
		{"quota over cap", []string{"--attachment-cap-mb", "10", "--attachment-quota-mb", "20"}, nil, "larger than attachment-cap-mb"},
		{"bad attachment expiry", []string{"--attachment-expiry", "30d"}, nil, "invalid attachment-expiry"},
		{"attachment expiry too short", []string{"--attachment-expiry", "1s"}, nil, "invalid attachment-expiry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := map[string]string{"HOME": "/home/u"}
			for k, v := range tt.env {
				e[k] = v
			}
			_, err := ParseServe(tt.args, env(e), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestParseServeAllowedHosts(t *testing.T) {
	tests := []struct {
		args    []string
		env     string
		hosts   []string
		any     bool
		wantErr bool
	}{
		{nil, "", nil, false, false},
		{[]string{"--allowed-hosts", "*"}, "", nil, true, false},
		{nil, "smoot.lan, localhost", []string{"smoot.lan", "localhost"}, false, false},
		{[]string{"--allowed-hosts", "a.example"}, "b.example", []string{"a.example"}, false, false},
		{[]string{"--allowed-hosts", "a,*"}, "", nil, false, true},
		{[]string{"--allowed-hosts", "a,,b"}, "", nil, false, true},
	}
	for _, tt := range tests {
		cfg, err := ParseServe(tt.args, env(map[string]string{"HOME": "/h", EnvAllowedHosts: tt.env}), io.Discard)
		if tt.wantErr {
			if err == nil {
				t.Errorf("%v %q: no error", tt.args, tt.env)
			}
			continue
		}
		if err != nil || cfg.AnyHost != tt.any || !slices.Equal(cfg.AllowedHosts, tt.hosts) {
			t.Errorf("%v %q: got %v %v, %v", tt.args, tt.env, cfg.AllowedHosts, cfg.AnyHost, err)
		}
	}
}

func TestParseServeWebIsABooleanFlag(t *testing.T) {
	cfg, err := ParseServe([]string{"--web", "--db", "/x.db"}, env(map[string]string{"HOME": "/h"}), io.Discard)
	if err != nil || !cfg.Web || cfg.DB != "/x.db" {
		t.Fatalf("--web --db = %+v, %v", cfg, err)
	}
	// An explicit flag beats the environment, in both directions.
	cfg, err = ParseServe([]string{"--web=false"}, env(map[string]string{"HOME": "/h", EnvWeb: "true"}), io.Discard)
	if err != nil || cfg.Web {
		t.Fatalf("--web=false with env true = %+v, %v", cfg, err)
	}
	cfg, err = ParseServe(nil, env(map[string]string{"HOME": "/h", EnvWeb: "true"}), io.Discard)
	if err != nil || !cfg.Web {
		t.Fatalf("env true = %+v, %v", cfg, err)
	}
}

func TestParseServeFeedback(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
		want bool
	}{
		{"default off", nil, nil, false},
		{"flag", []string{"--feedback"}, nil, true},
		{"env", nil, map[string]string{EnvFeedback: "true"}, true},
		{"flag beats env", []string{"--feedback=false"}, map[string]string{EnvFeedback: "true"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := map[string]string{"HOME": "/h"}
			for k, v := range tc.env {
				e[k] = v
			}
			cfg, err := ParseServe(tc.args, env(e), io.Discard)
			if err != nil || cfg.Feedback != tc.want {
				t.Fatalf("feedback = %v, %v; want %v", cfg.Feedback, err, tc.want)
			}
		})
	}
}

func TestParseServeAttachments(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		env        map[string]string
		on         bool
		cap, quota int64
		expiry     time.Duration
	}{
		{"defaults", nil, nil, false, DefaultAttachmentCapMB << 20, DefaultAttachmentQuotaMB << 20, DefaultAttachmentExpiry},
		{"flags", []string{"--attachments", "--attachment-cap-mb", "50", "--attachment-quota-mb", "5", "--attachment-expiry", "2h"}, nil, true, 50 << 20, 5 << 20, 2 * time.Hour},
		{"env", nil, map[string]string{EnvAttachments: "true", EnvAttachmentCap: "200", EnvAttachmentQuota: "20", EnvAttachmentExpiry: "1h"}, true, 200 << 20, 20 << 20, time.Hour},
		{"flag beats env", []string{"--attachments=false", "--attachment-cap-mb", "300"}, map[string]string{EnvAttachments: "true", EnvAttachmentCap: "200"}, false, 300 << 20, DefaultAttachmentQuotaMB << 20, DefaultAttachmentExpiry},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := map[string]string{"HOME": "/h"}
			for k, v := range tc.env {
				e[k] = v
			}
			cfg, err := ParseServe(tc.args, env(e), io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Attachments != tc.on || cfg.AttachmentCap != tc.cap || cfg.AttachmentQuota != tc.quota || cfg.AttachmentExpiry != tc.expiry {
				t.Fatalf("got on=%v cap=%d quota=%d expiry=%v", cfg.Attachments, cfg.AttachmentCap, cfg.AttachmentQuota, cfg.AttachmentExpiry)
			}
		})
	}
}

func TestParseServeLogFlagOverridesEnv(t *testing.T) {
	cfg, err := ParseServe([]string{"--log-level", "WARN", "--log-format", "text"},
		env(map[string]string{"HOME": "/h", EnvLogLevel: "debug", EnvLogFormat: "json"}), io.Discard)
	if err != nil || cfg.LogLevel != slog.LevelWarn || cfg.LogFormat != "text" {
		t.Fatalf("got %v %q, %v", cfg.LogLevel, cfg.LogFormat, err)
	}
}

func TestParseServeRateLimits(t *testing.T) {
	cfg, err := ParseServe([]string{"--write-limit", "0", "--trusted-proxies", "10.0.0.0/8, 192.0.2.7"},
		env(map[string]string{"HOME": "/h", EnvAuthFailureLimit: "5"}), io.Discard)
	if err != nil || cfg.WriteLimit != 0 || cfg.AuthFailureLimit != 5 || len(cfg.TrustedProxies) != 2 ||
		cfg.TrustedProxies[1].String() != "192.0.2.7/32" {
		t.Fatalf("got %+v, %v", cfg, err)
	}
}

func TestParseServeAcceptsPublicURLs(t *testing.T) {
	for _, u := range []string{"https://smoot.example", "http://smoot.example:8787/smoot/", "https://[::1]:8443"} {
		cfg, err := ParseServe([]string{"--db", "/x.db", "--public-url", u}, env(nil), io.Discard)
		if err != nil || cfg.PublicURL != u {
			t.Errorf("--public-url %q: %v, got %q", u, err, cfg.PublicURL)
		}
	}
}
