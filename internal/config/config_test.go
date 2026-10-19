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
	if cfg.Host != "127.0.0.1" || cfg.Port != 8787 {
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
