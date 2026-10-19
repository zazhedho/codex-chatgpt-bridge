package security

type CodexPermission struct {
	Extends        string          `json:"extends"`
	WorkspaceRoots map[string]bool `json:"workspace_roots"`
	Filesystem     map[string]any  `json:"filesystem"`
	Network        map[string]bool `json:"network"`
}

type ReadOnlyProfile struct {
	DefaultPermissions string
	WorkspaceRoots     map[string]bool
	Filesystem         map[string]any
	MCPServers         map[string]any
	WebSearch          string
	NetworkEnabled     bool
}

func CodexReadOnlyProfile(workspaceRoot string) ReadOnlyProfile {
	filesystem := map[string]any{
		":root":    "deny",
		":minimal": "read",
		":workspace_roots": map[string]any{
			".":                     "read",
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
	return ReadOnlyProfile{
		DefaultPermissions: "bridge-readonly",
		WorkspaceRoots:     map[string]bool{workspaceRoot: true},
		Filesystem:         filesystem,
		MCPServers:         map[string]any{},
		WebSearch:          "disabled",
		NetworkEnabled:     false,
	}
}

func (p ReadOnlyProfile) AppServerConfig() map[string]any {
	return map[string]any{
		"default_permissions": p.DefaultPermissions,
		"permissions": map[string]any{
			p.DefaultPermissions: CodexPermission{
				Extends:        ":read-only",
				WorkspaceRoots: p.WorkspaceRoots,
				Filesystem:     p.Filesystem,
				Network:        map[string]bool{"enabled": p.NetworkEnabled},
			},
		},
		"mcp_servers": p.MCPServers,
		"web_search":  p.WebSearch,
	}
}
