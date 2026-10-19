package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Host                string
	Port                int
	AuthToken           string
	DataDir             string
	CodexExecutable     string
	MaxConcurrentTasks  int
	MaxQueuedTasks      int
	MaxRequestBytes     int64
	MaxPromptBytes      int
	MaxOutputBytes      int
	TaskTimeoutSeconds  int
	OutputRetentionDays int
}

func Load() (Config, error) {
	env := make(map[string]string)
	for _, key := range []string{"HOME", "BRIDGE_HOST", "BRIDGE_PORT", "BRIDGE_AUTH_TOKEN", "BRIDGE_DATA_DIR", "CODEX_EXECUTABLE", "BRIDGE_OUTPUT_RETENTION_DAYS"} {
		if value, ok := os.LookupEnv(key); ok {
			env[key] = value
		}
	}
	return LoadFromEnv(env)
}

func LoadFromEnv(env map[string]string) (Config, error) {
	token := env["BRIDGE_AUTH_TOKEN"]
	if len(token) < 32 {
		return Config{}, errors.New("BRIDGE_AUTH_TOKEN must contain at least 32 bytes")
	}
	host := env["BRIDGE_HOST"]
	if host == "" {
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return Config{}, errors.New("BRIDGE_HOST must be a loopback IP address")
	}
	port := 8787
	if raw := env["BRIDGE_PORT"]; raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 65535 {
			return Config{}, errors.New("BRIDGE_PORT must be between 1 and 65535")
		}
		port = parsed
	}
	home := env["HOME"]
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return Config{}, fmt.Errorf("resolve home directory: %w", err)
		}
	}
	dataDir := env["BRIDGE_DATA_DIR"]
	if dataDir == "" {
		dataDir = filepath.Join(home, "Library", "Application Support", "codex-chatgpt-bridge-go")
	}
	if !filepath.IsAbs(dataDir) {
		return Config{}, errors.New("BRIDGE_DATA_DIR must be an absolute path")
	}
	retention := 30
	if raw := env["BRIDGE_OUTPUT_RETENTION_DAYS"]; raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 || parsed > 3650 {
			return Config{}, errors.New("BRIDGE_OUTPUT_RETENTION_DAYS must be between 0 and 3650")
		}
		retention = parsed
	}
	codex := strings.TrimSpace(env["CODEX_EXECUTABLE"])
	if codex == "" {
		codex = "codex"
	}
	return Config{
		Host: host, Port: port, AuthToken: token, DataDir: filepath.Clean(dataDir), CodexExecutable: codex,
		MaxConcurrentTasks: 1, MaxQueuedTasks: 20, MaxRequestBytes: 64 << 10,
		MaxPromptBytes: 20 << 10, MaxOutputBytes: 64 << 10, TaskTimeoutSeconds: 20 * 60,
		OutputRetentionDays: retention,
	}, nil
}
