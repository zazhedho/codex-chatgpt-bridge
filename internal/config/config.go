package config

import (
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	AuthModeBearer           = "bearer"
	AuthModeCloudflareAccess = "cloudflare_access"
)

type Config struct {
	Host                    string
	Port                    int
	AuthMode                string
	AuthToken               string
	CloudflareIssuer        string
	CloudflareAudience      string
	CloudflareAllowedEmails []string
	DataDir                 string
	CodexExecutable         string
	MaxConcurrentTasks      int
	MaxQueuedTasks          int
	MaxRequestBytes         int64
	MaxPromptBytes          int
	MaxOutputBytes          int
	TaskTimeoutSeconds      int
	OutputRetentionDays     int
}

func Load() (Config, error) {
	env := make(map[string]string)
	for _, key := range []string{"HOME", "BRIDGE_HOST", "BRIDGE_PORT", "BRIDGE_AUTH_MODE", "BRIDGE_AUTH_TOKEN", "BRIDGE_CLOUDFLARE_ISSUER", "BRIDGE_CLOUDFLARE_AUDIENCE", "BRIDGE_CLOUDFLARE_ALLOWED_EMAILS", "BRIDGE_DATA_DIR", "CODEX_EXECUTABLE", "BRIDGE_OUTPUT_RETENTION_DAYS"} {
		if value, ok := os.LookupEnv(key); ok {
			env[key] = value
		}
	}
	return LoadFromEnv(env)
}

func LoadFromEnv(env map[string]string) (Config, error) {
	authMode := strings.TrimSpace(env["BRIDGE_AUTH_MODE"])
	if authMode == "" {
		authMode = AuthModeBearer
	}
	token := env["BRIDGE_AUTH_TOKEN"]
	var issuer, audience string
	var allowedEmails []string
	switch authMode {
	case AuthModeBearer:
		if len(token) < 32 {
			return Config{}, errors.New("BRIDGE_AUTH_TOKEN must contain at least 32 bytes")
		}
	case AuthModeCloudflareAccess:
		if token != "" {
			return Config{}, errors.New("BRIDGE_AUTH_TOKEN must be unset in cloudflare_access mode")
		}
		issuer = strings.TrimSpace(env["BRIDGE_CLOUDFLARE_ISSUER"])
		if !validCloudflareIssuer(issuer) {
			return Config{}, errors.New("BRIDGE_CLOUDFLARE_ISSUER must be an HTTPS origin")
		}
		audience = strings.TrimSpace(env["BRIDGE_CLOUDFLARE_AUDIENCE"])
		if audience == "" {
			return Config{}, errors.New("BRIDGE_CLOUDFLARE_AUDIENCE is required")
		}
		var err error
		allowedEmails, err = parseAllowedEmails(env["BRIDGE_CLOUDFLARE_ALLOWED_EMAILS"])
		if err != nil {
			return Config{}, err
		}
	default:
		return Config{}, errors.New("BRIDGE_AUTH_MODE must be bearer or cloudflare_access")
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
		Host: host, Port: port, AuthMode: authMode, AuthToken: token, CloudflareIssuer: issuer, CloudflareAudience: audience,
		CloudflareAllowedEmails: allowedEmails, DataDir: filepath.Clean(dataDir), CodexExecutable: codex,
		MaxConcurrentTasks: 1, MaxQueuedTasks: 20, MaxRequestBytes: 64 << 10,
		MaxPromptBytes: 20 << 10, MaxOutputBytes: 64 << 10, TaskTimeoutSeconds: 20 * 60,
		OutputRetentionDays: retention,
	}, nil
}

func validCloudflareIssuer(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == ""
}

func parseAllowedEmails(value string) ([]string, error) {
	seen := make(map[string]struct{})
	var emails []string
	for _, part := range strings.Split(value, ",") {
		email := strings.ToLower(strings.TrimSpace(part))
		parsed, err := mail.ParseAddress(email)
		if email == "" || err != nil || parsed.Address != email {
			return nil, errors.New("BRIDGE_CLOUDFLARE_ALLOWED_EMAILS must contain only email addresses")
		}
		if _, exists := seen[email]; !exists {
			seen[email] = struct{}{}
			emails = append(emails, email)
		}
	}
	if len(emails) == 0 {
		return nil, errors.New("BRIDGE_CLOUDFLARE_ALLOWED_EMAILS is required")
	}
	return emails, nil
}
