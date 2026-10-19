package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFromEnvUsesLoopbackAndSeparateGoDataDirectory(t *testing.T) {
	home := t.TempDir()
	cfg, err := LoadFromEnv(map[string]string{
		"HOME":              home,
		"BRIDGE_AUTH_TOKEN": strings.Repeat("x", 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "127.0.0.1" || cfg.Port != 8787 || cfg.AuthMode != AuthModeBearer {
		t.Fatalf("unexpected listener config: %s:%d", cfg.Host, cfg.Port)
	}
	wantDataDir := filepath.Join(home, "Library", "Application Support", "codex-chatgpt-bridge-go")
	if cfg.DataDir != wantDataDir {
		t.Fatalf("data dir = %q, want Go-isolated %q", cfg.DataDir, wantDataDir)
	}
	if cfg.DataDir == filepath.Join(home, "Code", "Project", "codex-chatgpt-bridge", "data") {
		t.Fatal("Go default overlaps the TypeScript data directory")
	}
	if cfg.OutputRetentionDays != 30 || cfg.MaxConcurrentTasks != 1 {
		t.Fatalf("unexpected safe defaults: %+v", cfg)
	}
}

func TestLoadFromEnvSupportsExplicitCloudflareAccessMode(t *testing.T) {
	cfg, err := LoadFromEnv(map[string]string{
		"HOME":                             t.TempDir(),
		"BRIDGE_AUTH_MODE":                 "cloudflare_access",
		"BRIDGE_CLOUDFLARE_ISSUER":         "https://team.cloudflareaccess.com",
		"BRIDGE_CLOUDFLARE_AUDIENCE":       "app-audience",
		"BRIDGE_CLOUDFLARE_ALLOWED_EMAILS": "Zaqi@example.com, other@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthMode != AuthModeCloudflareAccess || cfg.AuthToken != "" || len(cfg.CloudflareAllowedEmails) != 2 || cfg.CloudflareAllowedEmails[0] != "zaqi@example.com" {
		t.Fatalf("unexpected Cloudflare auth config: %+v", cfg)
	}
}

func TestLoadFromEnvRejectsIncompleteOrUnsafeCloudflareAccessConfig(t *testing.T) {
	base := map[string]string{
		"BRIDGE_AUTH_MODE":                 "cloudflare_access",
		"BRIDGE_CLOUDFLARE_ISSUER":         "https://team.cloudflareaccess.com",
		"BRIDGE_CLOUDFLARE_AUDIENCE":       "app-audience",
		"BRIDGE_CLOUDFLARE_ALLOWED_EMAILS": "zaqi@example.com",
	}
	for name, change := range map[string]func(map[string]string){
		"missing issuer":   func(env map[string]string) { delete(env, "BRIDGE_CLOUDFLARE_ISSUER") },
		"non-https issuer": func(env map[string]string) { env["BRIDGE_CLOUDFLARE_ISSUER"] = "http://team.cloudflareaccess.com" },
		"issuer path": func(env map[string]string) {
			env["BRIDGE_CLOUDFLARE_ISSUER"] = "https://team.cloudflareaccess.com/other"
		},
		"missing audience":    func(env map[string]string) { delete(env, "BRIDGE_CLOUDFLARE_AUDIENCE") },
		"empty allowlist":     func(env map[string]string) { env["BRIDGE_CLOUDFLARE_ALLOWED_EMAILS"] = "" },
		"invalid email":       func(env map[string]string) { env["BRIDGE_CLOUDFLARE_ALLOWED_EMAILS"] = "zaqi@example.com,invalid" },
		"bearer fallback set": func(env map[string]string) { env["BRIDGE_AUTH_TOKEN"] = strings.Repeat("x", 32) },
		"unknown mode":        func(env map[string]string) { env["BRIDGE_AUTH_MODE"] = "anything_else" },
	} {
		t.Run(name, func(t *testing.T) {
			env := make(map[string]string, len(base))
			for key, value := range base {
				env[key] = value
			}
			change(env)
			if _, err := LoadFromEnv(env); err == nil {
				t.Fatal("unsafe Cloudflare configuration accepted")
			}
		})
	}
}

func TestLoadFromEnvRejectsWeakTokenAndNonLoopbackHost(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"weak token":    {"BRIDGE_AUTH_TOKEN": "short"},
		"host override": {"BRIDGE_AUTH_TOKEN": strings.Repeat("x", 32), "BRIDGE_HOST": "0.0.0.0"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadFromEnv(env); err == nil {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
}
