package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zazhedho/codex-chatgpt-bridge/internal/audit"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/domain"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/repository/sqlite"
)

func TestTrustedRootRequiresLocalApprovalAndRevocationSurvivesRestart(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	base := t.TempDir()
	home := filepath.Join(base, "home")
	rootPath := filepath.Join(base, "projects")
	project := filepath.Join(rootPath, "repo")
	if err := os.MkdirAll(filepath.Join(project, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(project, "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}

	store, err := sqlite.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	logger := audit.New(store)
	workspaces := NewWorkspaceService(store, logger, home, "", store)
	roots := NewTrustedWorkspaceRootService(store, workspaces, logger, home, "")
	ctx := authorizedContext("0123456789abcdef0123456789abcdef")
	request, err := roots.AddLocal(context.Background(), rootPath, "owner")
	if err != nil || request.Status != domain.TrustedRootPending {
		t.Fatalf("add root=%+v err=%v", request, err)
	}
	if _, err := roots.ResolveSession(ctx, filepath.Join(project, "subdir")); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("unapproved root resolve error=%v", err)
	}
	approved, err := roots.ApproveLocal(context.Background(), request.ID, "owner")
	if err != nil || approved.Status != domain.TrustedRootApproved {
		t.Fatalf("approve root=%+v err=%v", approved, err)
	}
	canonicalProject, _ := filepath.EvalSymlinks(project)
	resolution, err := roots.ResolveSession(ctx, filepath.Join(project, "subdir"))
	if err != nil || resolution.ProjectPath != canonicalProject || resolution.Root.ID != approved.ID {
		t.Fatalf("resolve=%+v err=%v", resolution, err)
	}
	workspace, err := roots.EnsureWorkspace(ctx, resolution)
	if err != nil || workspace.TrustedRootID != approved.ID || !workspace.RootSessionOnly || workspace.WriteEnabled {
		t.Fatalf("logical workspace=%+v err=%v", workspace, err)
	}

	store.Close()
	store, err = sqlite.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	workspaces = NewWorkspaceService(store, nil, home, "", store)
	roots = NewTrustedWorkspaceRootService(store, workspaces, nil, home, "")
	if _, err := roots.VerifyLocal(context.Background(), approved.ID, "owner"); err != nil {
		t.Fatalf("trusted root did not survive restart: %v", err)
	}
	if err := roots.SetStatusLocal(context.Background(), approved.ID, domain.TrustedRootRevoked, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := workspaces.Get(authorizedContext("0123456789abcdef0123456789abcdef"), workspace.ID); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("revoked root workspace remains authorized: %v", err)
	}
	if _, err := roots.ResolveSession(ctx, filepath.Join(project, "subdir")); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("revoked root still resolves sessions: %v", err)
	}
}

func TestTrustedRootProjectBoundaryAndFilesystemContainment(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	rootPath := filepath.Join(base, "trusted")
	repo := filepath.Join(rootPath, "repo")
	outside := filepath.Join(base, "outside")
	for _, path := range []string{home, filepath.Join(repo, ".git"), outside} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	store, err := sqlite.Open(filepath.Join(base, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	workspaces := NewWorkspaceService(store, nil, home, "", store)
	roots := NewTrustedWorkspaceRootService(store, workspaces, nil, home, "")
	ctx := authorizedContext("0123456789abcdef0123456789abcdef")
	request, err := roots.AddLocal(context.Background(), rootPath, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := roots.ApproveLocal(context.Background(), request.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	canonicalRepo, _ := filepath.EvalSymlinks(repo)
	resolution, err := roots.ResolveSession(ctx, repo)
	if err != nil || resolution.ProjectPath != canonicalRepo {
		t.Fatalf("repository boundary=%+v err=%v", resolution, err)
	}
	if _, err := roots.ResolveSession(ctx, outside); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("outside directory resolved under trusted root: %v", err)
	}
	ambiguous := filepath.Join(rootPath, "not-a-repository")
	if err := os.Mkdir(ambiguous, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := roots.ResolveSession(ctx, ambiguous); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("ambiguous project boundary error=%v", err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	rootResolution, err := roots.ResolveSession(ctx, rootPath)
	canonicalRoot, _ := filepath.EvalSymlinks(rootPath)
	if err != nil || rootResolution.Root.ID != request.ID || rootResolution.SessionPath != canonicalRoot || rootResolution.ProjectPath != canonicalRoot || rootResolution.WorkspaceID != logicalWorkspaceID(canonicalRoot) {
		t.Fatalf("session started at the trusted root should resolve to a root-scoped read-only workspace: resolution=%+v err=%v", rootResolution, err)
	}
	rootWorkspace, err := roots.EnsureWorkspace(ctx, rootResolution)
	if err != nil || rootWorkspace.RootPath != canonicalRoot || rootWorkspace.TrustedRootID != request.ID || !rootWorkspace.RootSessionOnly || rootWorkspace.WriteEnabled {
		t.Fatalf("trusted-root workspace=%+v err=%v", rootWorkspace, err)
	}
	if err := workspaces.SetWriteEnabledLocal(context.Background(), rootWorkspace.ID, true, "owner"); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("trusted root itself became writable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(rootPath, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := roots.ResolveSession(ctx, filepath.Join(rootPath, "escape")); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("symlink escape resolved under trusted root: %v", err)
	}
}

func TestTrustedRootsChooseMostSpecificValidBoundaryAndRebindDisabledRoot(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	outer := filepath.Join(base, "projects")
	repo := filepath.Join(outer, "repo")
	nestedRoot := filepath.Join(repo, "nested")
	sessionPath := filepath.Join(nestedRoot, "work")
	for _, path := range []string{home, filepath.Join(repo, ".git"), sessionPath} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	store, err := sqlite.Open(filepath.Join(base, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	workspaces := NewWorkspaceService(store, nil, home, "", store)
	roots := NewTrustedWorkspaceRootService(store, workspaces, nil, home, "")
	ctx := authorizedContext("0123456789abcdef0123456789abcdef")

	outerRequest, err := roots.AddLocal(context.Background(), outer, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := roots.ApproveLocal(context.Background(), outerRequest.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	nestedRequest, err := roots.AddLocal(context.Background(), nestedRoot, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := roots.ApproveLocal(context.Background(), nestedRequest.ID, "owner"); err != nil {
		t.Fatal(err)
	}

	resolution, err := roots.ResolveSession(ctx, sessionPath)
	canonicalRepo, _ := filepath.EvalSymlinks(repo)
	if err != nil || resolution.Root.ID != outerRequest.ID || resolution.ProjectPath != canonicalRepo {
		t.Fatalf("root enclosing the repository should resolve session: resolution=%+v err=%v", resolution, err)
	}
	workspace, err := roots.EnsureWorkspace(ctx, resolution)
	if err != nil || workspace.TrustedRootID != outerRequest.ID {
		t.Fatalf("ensure workspace=%+v err=%v", workspace, err)
	}
}

func TestTrustedRootOverlapKeepsActiveWorkspaceBindingAndAllowsApprovedFallback(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	outer := filepath.Join(base, "projects")
	repo := filepath.Join(outer, "repo")
	sessionPath := filepath.Join(repo, "work")
	for _, path := range []string{home, filepath.Join(repo, ".git"), sessionPath} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	store, err := sqlite.Open(filepath.Join(base, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	workspaces := NewWorkspaceService(store, nil, home, "", store)
	roots := NewTrustedWorkspaceRootService(store, workspaces, nil, home, "")
	ctx := authorizedContext("0123456789abcdef0123456789abcdef")

	addAndApprove := func(path string) domain.TrustedWorkspaceRoot {
		t.Helper()
		request, err := roots.AddLocal(context.Background(), path, "owner")
		if err != nil {
			t.Fatal(err)
		}
		root, err := roots.ApproveLocal(context.Background(), request.ID, "owner")
		if err != nil {
			t.Fatal(err)
		}
		return root
	}
	outerRoot := addAndApprove(outer)
	narrowRoot := addAndApprove(repo)

	resolution, err := roots.ResolveSession(ctx, sessionPath)
	if err != nil || resolution.Root.ID != narrowRoot.ID {
		t.Fatalf("most-specific valid root was not selected: resolution=%+v err=%v", resolution, err)
	}
	workspace, err := roots.EnsureWorkspace(ctx, resolution)
	if err != nil {
		t.Fatal(err)
	}
	if err := roots.SetStatusLocal(context.Background(), narrowRoot.ID, domain.TrustedRootDisabled, "owner"); err != nil {
		t.Fatal(err)
	}
	resolution, err = roots.ResolveSession(ctx, sessionPath)
	if err != nil || resolution.Root.ID != outerRoot.ID {
		t.Fatalf("approved enclosing root should be fallback: resolution=%+v err=%v", resolution, err)
	}
	workspace, err = roots.EnsureWorkspace(ctx, resolution)
	if err != nil || workspace.TrustedRootID != outerRoot.ID {
		t.Fatalf("workspace should rebind only to the independently approved fallback: workspace=%+v err=%v", workspace, err)
	}
	if err := roots.SetStatusLocal(context.Background(), narrowRoot.ID, domain.TrustedRootApproved, "owner"); err != nil {
		t.Fatal(err)
	}
	resolution, err = roots.ResolveSession(ctx, sessionPath)
	if err != nil || resolution.Root.ID != outerRoot.ID {
		t.Fatalf("active workspace binding should remain stable after re-enable: resolution=%+v err=%v", resolution, err)
	}
}

func TestTrustedRootApprovalExpiresAndIdentityChangesFailClosed(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	rootPath := filepath.Join(base, "root")
	for _, path := range []string{home, rootPath} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	store, err := sqlite.Open(filepath.Join(base, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	workspaces := NewWorkspaceService(store, nil, home, "", store)
	roots := NewTrustedWorkspaceRootService(store, workspaces, nil, home, "")
	now := time.Now().UTC()
	roots.now = func() time.Time { return now }
	request, err := roots.AddLocal(context.Background(), rootPath, "owner")
	if err != nil {
		t.Fatal(err)
	}
	other := rootPath + "-replacement"
	if err := os.Rename(rootPath, other); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := roots.ApproveLocal(context.Background(), request.ID, "owner"); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("changed root identity approval error=%v", err)
	}
	expiring := filepath.Join(base, "expiring")
	if err := os.Mkdir(expiring, 0o700); err != nil {
		t.Fatal(err)
	}
	expiringRequest, err := roots.AddLocal(context.Background(), expiring, "owner")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(trustedRootRequestTTL + time.Second)
	if _, err := roots.ApproveLocal(context.Background(), expiringRequest.ID, "owner"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expired root approval error=%v", err)
	}
}
