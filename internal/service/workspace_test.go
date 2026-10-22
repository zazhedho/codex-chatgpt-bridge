package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zazhedho/codex-chatgpt-bridge/internal/audit"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/domain"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/repository/sqlite"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/security"
)

func authorizedContext(token string) context.Context {
	var result context.Context
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/mcp", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	security.AuthMiddleware(token, 8787, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { result = r.Context() })).ServeHTTP(httptest.NewRecorder(), request)
	return result
}

func TestWorkspaceRegistrationRequiresLocalApprovalAndRevalidatesIdentity(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	auditLogger := audit.New(store)
	home := t.TempDir()
	project := filepath.Join(home, "project")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	workspaces := NewWorkspaceService(store, auditLogger, home, "")
	if _, err := workspaces.List(context.Background()); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("unauthenticated list error = %v", err)
	}
	ctx := authorizedContext("0123456789abcdef0123456789abcdef")
	request, err := workspaces.RequestRegistration(ctx, "project-a", project)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspaces.Get(ctx, "project-a"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("remote registration became trusted: %v", err)
	}
	pending, err := workspaces.PendingRequestsLocal(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%d err=%v", len(pending), err)
	}
	workspace, err := workspaces.ApproveRegistration(context.Background(), request.ID, "local-operator")
	if err != nil {
		t.Fatal(err)
	}
	if workspace.ID != "project-a" || workspace.RootPath == "" {
		t.Fatalf("bad approved workspace: %+v", workspace)
	}
	if _, err := workspaces.Get(ctx, "project-a"); err != nil {
		t.Fatalf("approved workspace unavailable: %v", err)
	}
	if err := os.Rename(project, project+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(project+"-moved", project); err != nil {
		t.Fatal(err)
	}
	if _, err := workspaces.Get(ctx, "project-a"); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("symlink replacement not rejected: %v", err)
	}
}

func TestConcurrentDuplicateWorkspaceRequestsAreRejected(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	home := t.TempDir()
	root := filepath.Join(home, "shared")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	workspaces := NewWorkspaceService(store, nil, home, "")
	ctx := authorizedContext("0123456789abcdef0123456789abcdef")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, id := range []string{"project-a", "project-b"} {
		wg.Add(1)
		go func(id string) { defer wg.Done(); _, err := workspaces.RequestRegistration(ctx, id, root); errs <- err }(id)
	}
	wg.Wait()
	close(errs)
	succeeded, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			succeeded++
		} else if errors.Is(err, domain.ErrConflict) {
			conflicts++
		} else {
			t.Fatalf("unexpected registration error: %v", err)
		}
	}
	if succeeded != 1 || conflicts != 1 {
		t.Fatalf("success=%d conflict=%d", succeeded, conflicts)
	}
}

func TestWorkspaceApprovalExpiresAndRejectsChangedRoot(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	home := t.TempDir()
	root := filepath.Join(home, "workspace")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	workspaces := NewWorkspaceService(store, nil, home, "")
	now := time.Now().UTC()
	workspaces.now = func() time.Time { return now }
	request, err := workspaces.RequestRegistrationLocal(context.Background(), "workspace-a", root, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(home, "other")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, root); err != nil {
		t.Fatal(err)
	}
	if _, err := workspaces.ApproveRegistration(context.Background(), request.ID, "operator"); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("changed root approval error = %v", err)
	}
}
