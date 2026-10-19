package security

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestBearerAuthenticationRejectsMissingAndWrongTokens(t *testing.T) {
	token := "local-test-token-that-is-long-enough-123456"
	for _, header := range []string{"", "Bearer wrong-token-that-is-long-enough-123456", "Basic " + token, "Bearer " + token + "x"} {
		if AuthenticateBearer(header, token) {
			t.Fatalf("accepted invalid authorization header %q", header)
		}
	}
	if !AuthenticateBearer("Bearer "+token, token) {
		t.Fatal("rejected matching bearer token")
	}
	if AuthenticateBearer("Bearer short", "short") {
		t.Fatal("accepted insecurely short configured token")
	}
}

func TestWorkspaceRootRejectsTraversalAndProtectedRoots(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := CanonicalWorkspaceRoot(project, base, "")
	wantRoot, canonicalErr := filepath.EvalSymlinks(project)
	if err != nil || canonicalErr != nil || root.Path != wantRoot {
		t.Fatalf("valid project root rejected: root=%+v want=%q err=%v canonicalErr=%v", root, wantRoot, err, canonicalErr)
	}
	for _, unsafe := range []string{project + string(filepath.Separator) + "..", base, string(filepath.Separator)} {
		if _, err := CanonicalWorkspaceRoot(unsafe, base, ""); err == nil {
			t.Errorf("accepted unsafe workspace root %q", unsafe)
		}
	}
}

func TestWorkspacePathRejectsTraversalAndSymlinkEscape(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	outside := filepath.Join(parent, "outside")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "marker.txt"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{"../outside/marker.txt", "escape/marker.txt"} {
		if _, err := ResolveWorkspacePath(root, relative); err == nil {
			t.Errorf("accepted escaping path %q", relative)
		}
	}
	canonicalRoot, canonicalErr := filepath.EvalSymlinks(root)
	resolved, err := ResolveWorkspacePath(root, "new-file.txt")
	if err != nil || canonicalErr != nil || resolved != filepath.Join(canonicalRoot, "new-file.txt") {
		t.Fatalf("valid workspace path failed: path=%q err=%v", resolved, err)
	}
}

func TestAuthMiddlewareDoesNotLeakTokenAndChecksLoopbackHost(t *testing.T) {
	token := "local-test-token-that-is-long-enough-123456"
	handler := AuthMiddleware(token, 8787, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/mcp", nil)
	request.Host = "evil.example:8787"
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("evil host status = %d, want 403", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/mcp", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", response.Code)
	}
	if response.Body.String() == token || response.Body.String() == "" {
		t.Fatal("auth response leaked token or omitted safe error")
	}
}

func TestReadOnlyProfileHasNoWriteOrNetworkPermission(t *testing.T) {
	profile := CodexReadOnlyProfile("/private/workspace")
	if profile.DefaultPermissions != "bridge-readonly" {
		t.Fatalf("unexpected default profile %q", profile.DefaultPermissions)
	}
	if profile.NetworkEnabled {
		t.Fatal("read-only profile enabled network")
	}
	if profile.WorkspaceRoots["/private/workspace"] != true {
		t.Fatal("workspace root is not allowlisted")
	}
	if profile.Filesystem[":root"] != "deny" || profile.Filesystem[":minimal"] != "read" {
		t.Fatalf("profile does not deny broad reads: %#v", profile.Filesystem)
	}
}

func TestReadOnlyProfileBuildsFixedAppServerConfiguration(t *testing.T) {
	profile := CodexReadOnlyProfile("/private/workspace")
	config := profile.AppServerConfig()
	if config["default_permissions"] != "bridge-readonly" || config["web_search"] != "disabled" {
		t.Fatalf("unsafe App Server defaults: %#v", config)
	}
	if len(profile.MCPServers) != 0 || profile.NetworkEnabled {
		t.Fatalf("read-only profile has unsafe capabilities: %#v", profile)
	}
	permissions, ok := config["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("permissions are missing or untyped: %#v", config["permissions"])
	}
	permission, ok := permissions["bridge-readonly"].(CodexPermission)
	if !ok || permission.Extends != ":read-only" || permission.WorkspaceRoots["/private/workspace"] != true || permission.Network["enabled"] {
		t.Fatalf("workspace or network policy was not enforced: %#v", permissions)
	}
	filesystem, ok := permission.Filesystem[":workspace_roots"].(map[string]any)
	if !ok || filesystem["."] != "read" {
		t.Fatalf("workspace read root missing: %#v", permission.Filesystem)
	}
}
