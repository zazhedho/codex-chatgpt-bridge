package security

import (
	"fmt"
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

func TestFindGitProjectRootHandlesNestedRepositoriesAndWorktrees(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	nested := filepath.Join(repo, "nested")
	worktree := filepath.Join(root, "worktree")
	for _, path := range []string{filepath.Join(repo, ".git"), filepath.Join(nested, ".git"), worktree} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(nested, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: ../repo/.git/worktrees/wt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	canonicalNested, _ := filepath.EvalSymlinks(nested)
	canonicalWorktree, _ := filepath.EvalSymlinks(worktree)
	if got, ok, err := FindGitProjectRoot(root, filepath.Join(nested, "child")); err != nil || !ok || got != canonicalNested {
		t.Fatalf("nested repository root=%q ok=%t err=%v", got, ok, err)
	}
	if got, ok, err := FindGitProjectRoot(root, worktree); err != nil || !ok || got != canonicalWorktree {
		t.Fatalf("worktree root=%q ok=%t err=%v", got, ok, err)
	}
	if _, ok, err := FindGitProjectRoot(root, filepath.Join(root, "unmarked")); err == nil || ok {
		t.Fatalf("missing session path should not be accepted: ok=%t err=%v", ok, err)
	}
}

func TestFindGitProjectRootsListsScopedChoicesWithoutFollowingSymlinks(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	child := filepath.Join(repo, "nested")
	worktree := filepath.Join(root, "worktree")
	outside := t.TempDir()
	for _, dir := range []string{filepath.Join(repo, ".git"), child, worktree, filepath.Join(outside, ".git")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: ../repo/.git/worktrees/wt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	got, err := FindGitProjectRoots(root)
	if err != nil {
		t.Fatal(err)
	}
	canonicalRepo, _ := filepath.EvalSymlinks(repo)
	canonicalWorktree, _ := filepath.EvalSymlinks(worktree)
	want := []string{canonicalRepo, canonicalWorktree}
	if len(got) != len(want) {
		t.Fatalf("project roots = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("project roots = %v, want %v", got, want)
		}
	}
}

func TestFindGitProjectRootsAllowsLargeRepositoryWorkingTrees(t *testing.T) {
	root := t.TempDir()
	filler := filepath.Join(root, "a-working-tree")
	project := filepath.Join(root, "z-project")
	if err := os.Mkdir(filler, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10_001; i++ {
		if err := os.Mkdir(filepath.Join(filler, fmt.Sprintf("dir-%05d", i)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(project, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}

	got, err := FindGitProjectRoots(root)
	if err != nil {
		t.Fatalf("large trusted-root scan failed: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(project)
	if err != nil || len(got) != 1 || got[0] != canonical {
		t.Fatalf("project roots = %v, want only %q (canonical error: %v)", got, canonical, err)
	}
}

func TestFindGitProjectRootRejectsSymlinkEscapeAndAmbiguousMarker(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "trusted")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(filepath.Join(root, "repo", ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outside, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := FindGitProjectRoot(root, filepath.Join(root, "escape")); err == nil || ok {
		t.Fatalf("symlink escape accepted: ok=%t err=%v", ok, err)
	}
	bad := filepath.Join(root, "bad")
	if err := os.Mkdir(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "repo", ".git"), filepath.Join(bad, ".git")); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := FindGitProjectRoot(root, bad); err != nil || ok {
		t.Fatalf("symlink .git marker should be ambiguous: ok=%t err=%v", ok, err)
	}
	if _, ok, err := FindGitProjectRoot(root, root+"/repo/../repo"); err == nil || ok {
		t.Fatalf("path traversal accepted: ok=%t err=%v", ok, err)
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

func TestWorkspaceWriteProfileIsWorkspaceScopedAndOffline(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "codex")
	profile, err := CodexWorkspaceWriteProfile("/private/workspace", executable)
	if err != nil {
		t.Fatal(err)
	}
	if profile.DefaultPermissions != "bridge-workspace-write" || profile.parent != ":workspace" || profile.NetworkEnabled {
		t.Fatalf("unsafe write profile: %+v", profile)
	}
	if profile.WorkspaceRoots["/private/workspace"] != true || len(profile.WorkspaceRoots) != 1 {
		t.Fatalf("write profile has unexpected roots: %#v", profile.WorkspaceRoots)
	}
	if profile.Filesystem[":root"] != "deny" || profile.Filesystem[":minimal"] != "read" || profile.Filesystem[":tmpdir"] != "deny" || profile.Filesystem[":slash_tmp"] != "deny" {
		t.Fatalf("write profile relaxed non-workspace access: %#v", profile.Filesystem)
	}
	if profile.Filesystem[executable] != "read" {
		t.Fatal("write profile does not narrowly allow the Codex sandbox helper executable")
	}
	roots, ok := profile.Filesystem[":workspace_roots"].(map[string]any)
	if !ok || roots["."] != "write" || roots[".git/config"] != "deny" || roots[".git/**"] != "read" || roots[".codex/**"] != "read" {
		t.Fatalf("write profile lacks workspace write or protected git config: %#v", profile.Filesystem)
	}
	config := profile.AppServerConfig()
	if config["sandbox_workspace_write"] != nil || config["web_search"] != "disabled" || profile.NetworkEnabled {
		t.Fatalf("write profile has unsafe optional sandbox configuration: %#v", config)
	}
}

func TestWorkspaceWriteProfileRequiresAbsoluteRoots(t *testing.T) {
	if _, err := CodexWorkspaceWriteProfile("relative", "/opt/homebrew/bin/codex"); err == nil {
		t.Fatal("accepted relative workspace root")
	}
	if _, err := CodexWorkspaceWriteProfile("/workspace", "codex"); err == nil {
		t.Fatal("accepted relative Codex executable")
	}
}
