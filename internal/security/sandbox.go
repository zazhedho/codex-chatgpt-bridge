package security

import (
	"errors"
	"path/filepath"
)

type CodexPermission struct {
	Extends        string          `json:"extends"`
	WorkspaceRoots map[string]bool `json:"workspace_roots"`
	Filesystem     map[string]any  `json:"filesystem"`
	Network        map[string]bool `json:"network"`
}

type PermissionProfile struct {
	DefaultPermissions string
	WorkspaceRoots     map[string]bool
	Filesystem         map[string]any
	MCPServers         map[string]any
	WebSearch          string
	NetworkEnabled     bool
	WorkspaceWrite     map[string]any
	parent             string
}

func CodexReadOnlyProfile(workspaceRoot string) PermissionProfile {
	return codexProfile(workspaceRoot, "bridge-readonly", ":read-only", "read", nil)
}

func CodexWorkspaceWriteProfile(workspaceRoot, codexExecutable string) (PermissionProfile, error) {
	if !filepath.IsAbs(workspaceRoot) || !filepath.IsAbs(codexExecutable) {
		return PermissionProfile{}, errors.New("workspace and Codex executable paths must be absolute")
	}
	profile := codexProfile(workspaceRoot, "bridge-workspace-write", ":workspace", "write", nil)
	roots := profile.Filesystem[":workspace_roots"].(map[string]any)
	roots[".git"] = "read"
	roots[".git/**"] = "read"
	roots[".codex"] = "read"
	roots[".codex/**"] = "read"
	profile.Filesystem[":tmpdir"] = "deny"
	profile.Filesystem[":slash_tmp"] = "deny"
	// Codex starts its own local sandbox helper using this executable.
	profile.Filesystem[filepath.Clean(codexExecutable)] = "read"
	return profile, nil
}

func codexProfile(workspaceRoot, id, parent, workspaceAccess string, writeConfig map[string]any) PermissionProfile {
	filesystem := map[string]any{
		":root":    "deny",
		":minimal": "read",
		":workspace_roots": map[string]any{
			".":                     workspaceAccess,
			"**/.env":               "deny",
			"**/.env.*":             "deny",
			"**/*.env":              "deny",
			"**/*.pem":              "deny",
			"**/*.key":              "deny",
			"**/*.p12":              "deny",
			"**/*.pfx":              "deny",
			"**/*secret*":           "deny",
			"**/*credential*":       "deny",
			"**/*service-account*":  "deny",
			"**/terraform.tfstate*": "deny",
			"**/id_rsa*":            "deny",
			"**/id_ed25519*":        "deny",
			".aws":                  "deny",
			".kube":                 "deny",
			".ssh":                  "deny",
			".git/config":           "deny",
			".git-credentials":      "deny",
		},
	}
	return PermissionProfile{
		DefaultPermissions: id,
		WorkspaceRoots:     map[string]bool{workspaceRoot: true},
		Filesystem:         filesystem,
		MCPServers:         map[string]any{},
		WebSearch:          "disabled",
		NetworkEnabled:     false,
		WorkspaceWrite:     writeConfig,
		parent:             parent,
	}
}

func (p PermissionProfile) AppServerConfig() map[string]any {
	config := map[string]any{
		"default_permissions": p.DefaultPermissions,
		"permissions": map[string]any{
			p.DefaultPermissions: CodexPermission{
				Extends:        p.parent,
				WorkspaceRoots: p.WorkspaceRoots,
				Filesystem:     p.Filesystem,
				Network:        map[string]bool{"enabled": p.NetworkEnabled},
			},
		},
		"mcp_servers": p.MCPServers,
		"web_search":  p.WebSearch,
	}
	if p.WorkspaceWrite != nil {
		config["sandbox_workspace_write"] = p.WorkspaceWrite
	}
	return config
}
