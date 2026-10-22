package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zazhedho/codex-chatgpt-bridge/internal/audit"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/domain"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/repository/sqlite"
)

type codexSessionCatalogStub struct {
	sessions map[string]domain.CodexSession
	listed   []domain.CodexSession
}

func (s *codexSessionCatalogStub) ListCLISessions(_ context.Context, workspacePath string) ([]domain.CodexSession, error) {
	if s.listed != nil {
		return append([]domain.CodexSession(nil), s.listed...), nil
	}
	var result []domain.CodexSession
	for _, session := range s.sessions {
		if session.IsCodexCLISession() && session.WorkspacePath == workspacePath {
			result = append(result, session)
		}
	}
	return result, nil
}

func (s *codexSessionCatalogStub) ListCLISessionsPage(_ context.Context, workspacePath, search, cursor string, limit int) (domain.CodexSessionPage, error) {
	var sessions []domain.CodexSession
	if s.listed != nil {
		sessions = append(sessions, s.listed...)
	} else {
		for _, session := range s.sessions {
			sessions = append(sessions, session)
		}
	}
	filtered := sessions[:0]
	for _, session := range sessions {
		if !session.IsCodexCLISession() || (workspacePath != "" && session.WorkspacePath != workspacePath) || (search != "" && !strings.Contains(strings.ToLower(catalogDisplayName(session)), strings.ToLower(search))) {
			continue
		}
		filtered = append(filtered, session)
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].UpdatedAt.After(filtered[j].UpdatedAt) })
	offset := 0
	if cursor != "" {
		var err error
		offset, err = strconv.Atoi(cursor)
		if err != nil || offset < 0 || offset > len(filtered) {
			return domain.CodexSessionPage{}, domain.ErrInvalidInput
		}
	}
	end := min(offset+limit, len(filtered))
	page := domain.CodexSessionPage{Sessions: append([]domain.CodexSession(nil), filtered[offset:end]...)}
	if end < len(filtered) {
		page.NextCursor = strconv.Itoa(end)
	}
	return page, nil
}

func TestCatalogDisplayNameSanitizesPreviewFallback(t *testing.T) {
	if got := catalogDisplayName(domain.CodexSession{Title: " Codex title ", Preview: "ignored fallback"}); got != "Codex title" {
		t.Fatalf("display name=%q, want Codex title", got)
	}

	got := catalogDisplayName(domain.CodexSession{Preview: "  Read\tfixture\napi_key=fixture-secret " + strings.Repeat("x", 200)})
	if strings.ContainsAny(got, "\t\r\n") || strings.Contains(got, "fixture-secret") || len([]rune(got)) > maxCatalogPreviewRunes {
		t.Fatalf("preview was not sanitized and bounded: %q (runes=%d)", got, len([]rune(got)))
	}
	if !strings.HasPrefix(got, "Read fixture api_key=[REDACTED]") {
		t.Fatalf("unexpected sanitized preview: %q", got)
	}
	if got := catalogDisplayName(domain.CodexSession{}); got != "Untitled session" {
		t.Fatalf("empty metadata display name=%q", got)
	}
}

func TestLocalSessionListIsWorkspaceScopedAndDoesNotExposeNonCLIThreads(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	home := t.TempDir()
	workspaces := NewWorkspaceService(store, nil, home, "")
	workspaceA := registerForTest(t, workspaces, home, "project-a")
	workspaceB := registerForTest(t, workspaces, home, "project-b")
	now := time.Now().UTC()
	catalog := &codexSessionCatalogStub{listed: []domain.CodexSession{
		{ID: "cli-a", WorkspacePath: workspaceA.RootPath, Source: "cli", Title: "Fixture", Status: "notLoaded", UpdatedAt: now},
		{ID: "terminal-a", WorkspacePath: workspaceA.RootPath, Source: "vscode", Originator: "codex-tui", Status: "idle", UpdatedAt: now},
		{ID: "editor-a", WorkspacePath: workspaceA.RootPath, Source: "vscode", Originator: "codex-vscode", Status: "idle", UpdatedAt: now},
		{ID: "cli-b", WorkspacePath: workspaceB.RootPath, Source: "cli", Status: "idle", UpdatedAt: now},
	}}
	sessions := NewSessionService(store, workspaces, catalog, nil)
	values, err := sessions.ListLocal(context.Background(), workspaceA.ID, "tester")
	if err != nil || len(values) != 2 || values[0].ID != "cli-a" || values[1].ID != "terminal-a" {
		t.Fatalf("local session list=%+v err=%v", values, err)
	}
	if _, err := sessions.ListLocal(context.Background(), workspaceA.ID, ""); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("unauthenticated local listing error=%v", err)
	}
}

func TestGlobalCatalogTrustPaginationSwitchingAndRevocation(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "db")
	store, err := sqlite.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	logger := audit.New(store)
	workspaces := NewWorkspaceService(store, logger, home, "")
	workspaceA := registerForTest(t, workspaces, home, "project-a")
	workspaceB := registerForTest(t, workspaces, home, "project-b")
	now := time.Now().UTC().Truncate(time.Second)
	catalog := &codexSessionCatalogStub{sessions: map[string]domain.CodexSession{
		"cli-a-new":      {ID: "cli-a-new", WorkspacePath: workspaceA.RootPath, Source: "exec", Originator: "codex_exec", Title: "A newest", Preview: "ignored preview", Status: "notLoaded", CreatedAt: now.Add(-time.Hour), UpdatedAt: now},
		"cli-a-old":      {ID: "cli-a-old", WorkspacePath: workspaceA.RootPath, Source: "cli", Preview: "A older fallback", Status: "idle", CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-time.Minute)},
		"cli-b":          {ID: "cli-b", WorkspacePath: workspaceB.RootPath, Source: "cli", Title: "B session", Status: "notLoaded", CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now.Add(-2 * time.Minute)},
		"cli-other":      {ID: "cli-other", WorkspacePath: t.TempDir(), Source: "cli", Title: "Unregistered project", Status: "notLoaded", CreatedAt: now.Add(-4 * time.Hour), UpdatedAt: now.Add(-3 * time.Minute)},
		"active-session": {ID: "active-session", WorkspacePath: workspaceA.RootPath, Source: "cli", Title: "Active", Status: "active", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-4 * time.Minute)},
	}}
	sessions := NewSessionService(store, workspaces, catalog, logger)
	ctx := authorizedContext("fedcba9876543210fedcba9876543210")
	if _, err := sessions.ListCatalog(ctx, "", "", "", 10); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("global catalog before owner grant error=%v, want not authorized", err)
	}
	if err := sessions.SetCatalogAccessLocal(context.Background(), true, "local-owner"); err != nil {
		t.Fatal(err)
	}
	if err := sessions.SetWorkspaceTrustLocal(context.Background(), workspaceA.ID, true, "local-owner"); err != nil {
		t.Fatal(err)
	}
	if err := sessions.SetWorkspaceTrustLocal(context.Background(), workspaceB.ID, true, "local-owner"); err != nil {
		t.Fatal(err)
	}
	page, err := sessions.ListCatalog(ctx, "", "", "", 2)
	if err != nil || len(page.Sessions) != 2 || page.NextCursor == "" {
		t.Fatalf("first global page=%+v err=%v", page, err)
	}
	if _, err := sessions.ListCatalog(ctx, workspaceA.ID, "", page.NextCursor, 2); !errors.Is(err, domain.ErrInvalidInput) {
		t.Fatalf("cursor was reusable with a different workspace filter: %v", err)
	}
	if page.Sessions[0].ThreadID != "cli-a-new" || page.Sessions[1].ThreadID != "cli-a-old" {
		t.Fatalf("global page not sorted by recent activity: %+v", page.Sessions)
	}
	if page.Sessions[0].DisplayName != "A newest" || page.Sessions[1].DisplayName != "A older fallback" {
		t.Fatalf("catalog display names did not prefer title then preview: %+v", page.Sessions)
	}
	firstSearchPage, err := sessions.ListCatalog(ctx, workspaceA.ID, "A older fallback", "", 1)
	if err != nil || len(firstSearchPage.Sessions) != 0 || firstSearchPage.NextCursor == "" {
		t.Fatalf("search must filter the bounded first metadata page and preserve its cursor: page=%+v err=%v", firstSearchPage, err)
	}
	secondSearchPage, err := sessions.ListCatalog(ctx, workspaceA.ID, "A older fallback", firstSearchPage.NextCursor, 1)
	if err != nil || len(secondSearchPage.Sessions) != 1 || secondSearchPage.Sessions[0].ThreadID != "cli-a-old" {
		t.Fatalf("search did not find a preview-backed display name on the next page: page=%+v err=%v", secondSearchPage, err)
	}
	page2, err := sessions.ListCatalog(ctx, "", "", page.NextCursor, 2)
	if err != nil || len(page2.Sessions) != 2 {
		t.Fatalf("second global page=%+v err=%v", page2, err)
	}
	if page2.Sessions[0].ThreadID != "cli-b" || page2.Sessions[0].ConnectionStatus != "trusted" || !page2.Sessions[0].CanResume {
		t.Fatalf("cross-workspace catalog entry missing trust metadata: %+v", page2.Sessions[0])
	}
	if page2.Sessions[1].ThreadID != "cli-other" || page2.Sessions[1].WorkspaceID != "" || page2.Sessions[1].ConnectionStatus != "workspace_approval_required" || page2.Sessions[1].CanResume {
		t.Fatalf("unapproved workspace was resumable or exposed an owner: %+v", page2.Sessions[1])
	}
	filtered, err := sessions.ListCatalog(ctx, workspaceA.ID, "older fallback", "", 10)
	if err != nil || len(filtered.Sessions) != 1 || filtered.Sessions[0].ThreadID != "cli-a-old" {
		t.Fatalf("workspace/search filtering result=%+v err=%v", filtered, err)
	}
	active, err := sessions.ListCatalog(ctx, workspaceA.ID, "Active", "", 10)
	if err != nil || len(active.Sessions) != 1 || active.Sessions[0].CanResume {
		t.Fatalf("active session was reported resumable: %+v err=%v", active, err)
	}
	if err := sessions.AuthorizeResume(ctx, workspaceA.ID, "cli-a-new"); err != nil {
		t.Fatalf("trusted session was not selectable: %v", err)
	}
	if err := sessions.AuthorizeResume(ctx, workspaceA.ID, "cli-a-old"); err != nil {
		t.Fatalf("second session switch failed: %v", err)
	}
	if err := sessions.AuthorizeResume(ctx, workspaceB.ID, "cli-a-new"); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("cross-workspace thread access error=%v, want unauthorized", err)
	}
	if err := sessions.AuthorizeResume(ctx, workspaceA.ID, "active-session"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("active session resume error=%v, want conflict", err)
	}
	if _, err := store.GetThread(ctx, "active-session"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("active session became bound before conflict: %v", err)
	}
	if err := sessions.SetWorkspaceTrustLocal(context.Background(), workspaceA.ID, false, "local-owner"); err != nil {
		t.Fatal(err)
	}
	if err := sessions.AuthorizeResume(ctx, workspaceA.ID, "cli-a-new"); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("revoked workspace trust still allowed resume: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = sqlite.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	workspaces = NewWorkspaceService(store, nil, home, "")
	workspaceB, err = workspaces.GetLocal(ctx, workspaceB.ID)
	if err != nil || !workspaceB.SessionTrusted {
		t.Fatalf("workspace trust did not persist across restart: workspace=%+v err=%v", workspaceB, err)
	}
	settings, err := store.SessionCatalogEnabled(ctx)
	if err != nil || !settings {
		t.Fatalf("global catalog owner grant did not persist across restart: enabled=%v err=%v", settings, err)
	}
}

func TestTrustedRootsDiscoverSwitchPersistAndRevokeSessions(t *testing.T) {
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	home := filepath.Join(base, "home")
	rootA := filepath.Join(base, "projects")
	rootB := filepath.Join(base, "other-projects")
	repoA := filepath.Join(rootA, "repo-a")
	repoA2 := filepath.Join(rootA, "repo-a2")
	repoB := filepath.Join(rootB, "repo-b")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{home, filepath.Join(repoA, ".git"), filepath.Join(repoA, "nested"), repoA2, filepath.Join(repoB, ".git"), filepath.Join(outside, ".git"), filepath.Join(rootA, "ambiguous")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repoA2, ".git"), []byte("gitdir: ../repo-a/.git/worktrees/a2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(rootA, "escape")); err != nil {
		t.Fatal(err)
	}

	store, err := sqlite.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	logger := audit.New(store)
	workspaces := NewWorkspaceService(store, logger, home, "", store)
	roots := NewTrustedWorkspaceRootService(store, workspaces, logger, home, "")
	now := time.Now().UTC()
	catalog := &codexSessionCatalogStub{listed: []domain.CodexSession{
		{ID: "root-a-one", WorkspacePath: filepath.Join(repoA, "nested"), Source: "cli", Status: "notLoaded", CreatedAt: now.Add(-time.Hour), UpdatedAt: now},
		{ID: "root-a-two", WorkspacePath: repoA2, Source: "cli", Status: "idle", CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-time.Minute)},
		{ID: "root-b-one", WorkspacePath: repoB, Source: "cli", Status: "notLoaded", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-2 * time.Minute)},
		{ID: "root-a-parent", WorkspacePath: rootA, Source: "cli", Status: "idle", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-6 * time.Minute)},
		{ID: "root-b-parent", WorkspacePath: rootB, Source: "cli", Status: "idle", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-7 * time.Minute)},
		{ID: "root-a-active", WorkspacePath: repoA, Source: "cli", Status: "active", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-3 * time.Minute)},
		{ID: "root-a-ambiguous", WorkspacePath: filepath.Join(rootA, "ambiguous"), Source: "cli", Status: "idle", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-4 * time.Minute)},
		{ID: "outside-session", WorkspacePath: outside, Source: "cli", Status: "idle", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-5 * time.Minute)},
		{ID: "symlink-escape", WorkspacePath: filepath.Join(rootA, "escape"), Source: "cli", Status: "idle", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-6 * time.Minute)},
	}}
	sessions := NewSessionService(store, workspaces, catalog, logger, roots)
	ctx := authorizedContext("0123456789abcdef0123456789abcdef")
	if _, err := sessions.ListCatalog(ctx, "", "", "", 25); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("catalog exposed sessions before global approval: %v", err)
	}
	if err := sessions.SetCatalogAccessLocal(context.Background(), true, "owner"); err != nil {
		t.Fatal(err)
	}
	rootRequestA, err := roots.AddLocal(context.Background(), rootA, "owner")
	if err != nil {
		t.Fatal(err)
	}
	rootRequestB, err := roots.AddLocal(context.Background(), rootB, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := roots.ApproveLocal(context.Background(), rootRequestA.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := roots.ApproveLocal(context.Background(), rootRequestB.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	page, err := sessions.ListCatalog(ctx, "", "", "", 25)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]domain.SessionCatalogEntry)
	for _, entry := range page.Sessions {
		byID[entry.ThreadID] = entry
	}
	if len(byID) != 7 {
		t.Fatalf("catalog returned %d sessions, want 7 trusted or ambiguous sessions: %+v", len(byID), page.Sessions)
	}
	for _, id := range []string{"outside-session", "symlink-escape"} {
		if _, ok := byID[id]; ok {
			t.Fatalf("untrusted session %q leaked into catalog", id)
		}
	}
	if !byID["root-a-one"].CanResume || !byID["root-a-two"].CanResume || !byID["root-b-one"].CanResume || byID["root-a-active"].CanResume {
		t.Fatalf("resume eligibility incorrect: %+v", byID)
	}
	if byID["root-a-one"].WorkspaceID != byID["root-a-active"].WorkspaceID || byID["root-a-one"].WorkspaceID == byID["root-a-two"].WorkspaceID || byID["root-a-one"].WorkspaceID == byID["root-b-one"].WorkspaceID {
		t.Fatalf("workspace mapping should group sessions per repository and isolate roots: %+v", byID)
	}
	if byID["root-a-ambiguous"].ConnectionStatus != "project_selection_required" || byID["root-a-ambiguous"].CanResume {
		t.Fatalf("ambiguous boundary not held for explicit approval: %+v", byID["root-a-ambiguous"])
	}
	if !byID["root-a-parent"].CanResume || !byID["root-b-parent"].CanResume || byID["root-a-parent"].ConnectionStatus != "trusted_root" || byID["root-b-parent"].ConnectionStatus != "trusted_root" {
		t.Fatalf("sessions started at approved trusted roots should resume root-scoped: project=%+v bri=%+v", byID["root-a-parent"], byID["root-b-parent"])
	}
	workspaceA := byID["root-a-one"].WorkspaceID
	workspaceA2 := byID["root-a-two"].WorkspaceID
	workspaceB := byID["root-b-one"].WorkspaceID
	rootWorkspaceA := logicalWorkspaceID(rootRequestA.RootPath)
	rootWorkspaceB := logicalWorkspaceID(rootRequestB.RootPath)
	if byID["root-a-parent"].WorkspaceID != rootWorkspaceA || byID["root-b-parent"].WorkspaceID != rootWorkspaceB || rootWorkspaceA == rootWorkspaceB {
		t.Fatalf("parent-root sessions were not mapped to isolated trusted-root workspaces: project=%+v bri=%+v", byID["root-a-parent"], byID["root-b-parent"])
	}
	filtered, err := sessions.ListCatalog(ctx, workspaceA, "", "", 25)
	if err != nil || len(filtered.Sessions) != 2 || filtered.Sessions[0].WorkspaceID != workspaceA || filtered.Sessions[1].WorkspaceID != workspaceA {
		t.Fatalf("root-derived workspace filter=%+v err=%v", filtered, err)
	}
	if err := sessions.AuthorizeResume(ctx, workspaceA, "root-a-one"); err != nil {
		t.Fatalf("resume first root A session: %v", err)
	}
	if err := sessions.AuthorizeResume(ctx, workspaceA2, "root-a-two"); err != nil {
		t.Fatalf("switch to second repository in root A: %v", err)
	}
	if err := sessions.AuthorizeResume(ctx, workspaceB, "root-b-one"); err != nil {
		t.Fatalf("switch to root B: %v", err)
	}
	if err := sessions.AuthorizeResume(ctx, rootWorkspaceA, "root-a-parent"); err != nil {
		t.Fatalf("resume session started at trusted root A: %v", err)
	}
	if err := sessions.AuthorizeResume(ctx, rootWorkspaceB, "root-b-parent"); err != nil {
		t.Fatalf("resume session started at trusted root B: %v", err)
	}
	if err := sessions.AuthorizeResume(ctx, rootWorkspaceA, "root-b-parent"); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("root A session crossed into root B: %v", err)
	}
	if err := sessions.AuthorizeResume(ctx, workspaceA, "root-b-one"); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("cross-root resume error=%v", err)
	}
	threadA, err := store.GetThread(ctx, "root-a-one")
	if err != nil || threadA.Source != domain.ThreadSourceCodexCLIRoot || threadA.TrustedRootID != rootRequestA.ID {
		t.Fatalf("root-bound thread=%+v err=%v", threadA, err)
	}
	workspace, err := workspaces.Get(ctx, workspaceA)
	if err != nil || !workspace.RootSessionOnly || workspace.WriteEnabled || workspace.SessionTrusted {
		t.Fatalf("root-derived workspace policy=%+v err=%v", workspace, err)
	}
	queue := &taskQueueStub{}
	tasks := NewTaskService(store, store, workspaces, NewApprovalService(store, workspaces), queue, allowTaskExecution{}, logger, 20_000, 20, sessions)
	if _, err := tasks.Start(ctx, workspaceA, "This cannot create a new thread.", ""); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("root-only workspace started a new task: %v", err)
	}
	rootWorkspace, err := workspaces.Get(ctx, rootWorkspaceA)
	if err != nil || rootWorkspace.RootPath != rootRequestA.RootPath || rootWorkspace.WriteEnabled {
		t.Fatalf("root-scoped workspace was not read-only: workspace=%+v err=%v", rootWorkspace, err)
	}
	if err := workspaces.SetWriteEnabledLocal(context.Background(), rootWorkspaceA, true, "owner"); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("local owner enabled writes for an entire trusted root: %v", err)
	}
	if _, _, err := tasks.StartWrite(ctx, rootWorkspaceA, "Write across the trusted root.", "root-a-parent"); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("root-scoped write task was not denied: %v", err)
	}
	readTask, err := tasks.Start(ctx, workspaceA, "Continue the selected read-only analysis.", "root-a-one")
	if err != nil || readTask.ExecutionMode != domain.ExecutionReadOnly || len(queue.jobs) != 1 {
		t.Fatalf("trusted session read task=%+v queued=%d err=%v", readTask, len(queue.jobs), err)
	}
	if _, _, err := tasks.StartWrite(ctx, workspaceA, "Write something.", "root-a-one"); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("root trust enabled workspace write without local opt-in: %v", err)
	}
	if err := workspaces.SetWriteEnabledLocal(context.Background(), workspaceA, true, "owner"); err != nil {
		t.Fatalf("local write opt-in failed: %v", err)
	}
	writeTask, writeApproval, err := tasks.StartWrite(ctx, workspaceA, "Write something.", "root-a-one")
	if err != nil || writeTask.Status != domain.TaskWaitingApproval || writeApproval.Status != domain.ApprovalPending || len(queue.jobs) != 1 {
		t.Fatalf("separate per-task write approval missing: task=%+v approval=%+v queued=%d err=%v", writeTask, writeApproval, len(queue.jobs), err)
	}
	if err := roots.SetStatusLocal(context.Background(), rootRequestA.ID, domain.TrustedRootDisabled, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := sessions.AuthorizeResume(ctx, workspaceA, "root-a-one"); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("disabled root still resumed thread: %v", err)
	}
	if err := roots.SetStatusLocal(context.Background(), rootRequestA.ID, domain.TrustedRootApproved, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = sqlite.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	workspaces = NewWorkspaceService(store, nil, home, "", store)
	roots = NewTrustedWorkspaceRootService(store, workspaces, nil, home, "")
	sessions = NewSessionService(store, workspaces, catalog, nil, roots)
	if err := sessions.AuthorizeResume(ctx, workspaceA, "root-a-one"); err != nil {
		t.Fatalf("root trust did not survive restart: %v", err)
	}
	if err := roots.SetStatusLocal(context.Background(), rootRequestA.ID, domain.TrustedRootRevoked, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := sessions.AuthorizeResume(ctx, workspaceA, "root-a-one"); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("revoked root still resumed thread: %v", err)
	}
}

func TestBridgeOwnedResumeRevalidatesWorkspaceAndRuntimeStatus(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	home := t.TempDir()
	workspaces := NewWorkspaceService(store, nil, home, "")
	workspace := registerForTest(t, workspaces, home, "project-a")
	now := time.Now().UTC().Truncate(time.Second)
	for _, id := range []string{"bridge-idle", "bridge-active", "bridge-error", "bridge-other-path"} {
		if err := store.SaveThread(context.Background(), domain.Thread{ID: id, WorkspaceID: workspace.ID, Source: domain.ThreadSourceBridge, SessionPath: workspace.RootPath, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	catalog := &codexSessionCatalogStub{sessions: map[string]domain.CodexSession{
		"bridge-idle":       {ID: "bridge-idle", WorkspacePath: workspace.RootPath, Source: "appServer", Status: "notLoaded", CreatedAt: now, UpdatedAt: now},
		"bridge-active":     {ID: "bridge-active", WorkspacePath: workspace.RootPath, Source: "appServer", Status: "active", CreatedAt: now, UpdatedAt: now},
		"bridge-error":      {ID: "bridge-error", WorkspacePath: workspace.RootPath, Source: "appServer", Status: "systemError", CreatedAt: now, UpdatedAt: now},
		"bridge-other-path": {ID: "bridge-other-path", WorkspacePath: t.TempDir(), Source: "appServer", Status: "idle", CreatedAt: now, UpdatedAt: now},
	}}
	sessions := NewSessionService(store, workspaces, catalog, nil)
	ctx := authorizedContext("fedcba9876543210fedcba9876543210")
	if err := sessions.AuthorizeResume(ctx, workspace.ID, "bridge-idle"); err != nil {
		t.Fatalf("idle Bridge thread should remain resumable: %v", err)
	}
	for _, id := range []string{"bridge-active", "bridge-error"} {
		if err := sessions.AuthorizeResume(ctx, workspace.ID, id); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("runtime status %q should reject resume, got %v", id, err)
		}
	}
	if err := sessions.AuthorizeResume(ctx, workspace.ID, "bridge-other-path"); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("Bridge thread outside workspace was accepted: %v", err)
	}
}

func TestTrustedSessionWriteStillRequiresPerTaskApproval(t *testing.T) {
	store, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	home := t.TempDir()
	workspaces := NewWorkspaceService(store, nil, home, "")
	workspace := registerForTest(t, workspaces, home, "project-a")
	if err := workspaces.SetSessionTrustedLocal(context.Background(), workspace.ID, true, "owner"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	catalog := &codexSessionCatalogStub{sessions: map[string]domain.CodexSession{
		"trusted-write": {ID: "trusted-write", WorkspacePath: workspace.RootPath, Source: "cli", Status: "notLoaded", CreatedAt: now.Add(-time.Hour), UpdatedAt: now},
	}}
	sessions := NewSessionService(store, workspaces, catalog, nil)
	ctx := authorizedContext("fedcba9876543210fedcba9876543210")
	if err := sessions.AuthorizeResume(ctx, workspace.ID, "trusted-write"); err != nil {
		t.Fatal(err)
	}
	queue := &taskQueueStub{}
	tasks := NewTaskService(store, store, workspaces, NewApprovalService(store, workspaces), queue, allowTaskExecution{}, nil, 20_000, 20)
	if _, _, err := tasks.StartWrite(ctx, workspace.ID, "Change fixture", "trusted-write"); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("workspace trust enabled writes without workspace write opt-in: %v", err)
	}
	if err := workspaces.SetWriteEnabledLocal(context.Background(), workspace.ID, true, "owner"); err != nil {
		t.Fatal(err)
	}
	task, approval, err := tasks.StartWrite(ctx, workspace.ID, "Change fixture", "trusted-write")
	if err != nil || task.Status != domain.TaskWaitingApproval || approval.Status != domain.ApprovalPending || len(queue.jobs) != 0 {
		t.Fatalf("trusted session bypassed per-task write approval: task=%+v approval=%+v jobs=%d err=%v", task, approval, len(queue.jobs), err)
	}
}

func (s *codexSessionCatalogStub) ReadSessionMetadata(_ context.Context, threadID string) (domain.CodexSession, error) {
	for _, session := range s.listed {
		if session.ID == threadID {
			return session, nil
		}
	}
	session, ok := s.sessions[threadID]
	if !ok {
		return domain.CodexSession{}, domain.ErrNotFound
	}
	return session, nil
}

func TestLegacyParentSessionResumesRootScopedWithOptionalProjectBinding(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	rootPath := filepath.Join(base, "trusted-parent")
	repoA := filepath.Join(rootPath, "repo-a")
	repoB := filepath.Join(rootPath, "repo-b")
	for _, path := range []string{home, filepath.Join(repoA, ".git"), filepath.Join(repoB, ".git")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	store, err := sqlite.Open(filepath.Join(base, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	logger := audit.New(store)
	workspaces := NewWorkspaceService(store, logger, home, "", store)
	roots := NewTrustedWorkspaceRootService(store, workspaces, logger, home, "")
	rootRequest, err := roots.AddLocal(context.Background(), rootPath, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := roots.ApproveLocal(context.Background(), rootRequest.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	catalog := &codexSessionCatalogStub{sessions: map[string]domain.CodexSession{
		"legacy-a":      {ID: "legacy-a", WorkspacePath: rootPath, Source: "cli", Title: "A", Status: "notLoaded", CreatedAt: now.Add(-time.Hour), UpdatedAt: now},
		"legacy-b":      {ID: "legacy-b", WorkspacePath: rootPath, Source: "cli", Title: "B", Status: "idle", CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-time.Minute)},
		"legacy-active": {ID: "legacy-active", WorkspacePath: rootPath, Source: "cli", Title: "Active", Status: "active", CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now.Add(-2 * time.Minute)},
	}}
	sessions := NewSessionService(store, workspaces, catalog, logger, roots)
	currentNow := now
	sessions.now = func() time.Time { return currentNow }
	ctx := authorizedContext("0123456789abcdef0123456789abcdef")
	if err := sessions.SetCatalogAccessLocal(context.Background(), true, "owner"); err != nil {
		t.Fatal(err)
	}
	page, err := sessions.ListCatalog(ctx, "", "", "", 10)
	byID := make(map[string]domain.SessionCatalogEntry, len(page.Sessions))
	for _, entry := range page.Sessions {
		byID[entry.ThreadID] = entry
	}
	if err != nil || len(page.Sessions) != 3 || byID["legacy-a"].ConnectionStatus != "trusted_root" || !byID["legacy-a"].CanResume || byID["legacy-b"].ConnectionStatus != "trusted_root" || !byID["legacy-b"].CanResume || byID["legacy-active"].CanResume {
		t.Fatalf("parent-directory sessions should resume at trusted-root scope while active sessions remain blocked: page=%+v err=%v", page, err)
	}
	if _, _, err := sessions.ListProjectChoices(ctx, "legacy-active", 0, 10); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("active CLI session exposed bindable project choices: %v", err)
	}
	choices, next, err := sessions.ListProjectChoices(ctx, "legacy-a", 0, 1)
	if err != nil || len(choices) != 1 || next == nil || choices[0].RelativePath == repoA || filepath.IsAbs(choices[0].RelativePath) {
		t.Fatalf("project choices must be bounded and root-relative: choices=%+v next=%v err=%v", choices, next, err)
	}
	otherPage, _, err := sessions.ListProjectChoices(ctx, "legacy-a", *next, 1)
	if err != nil || len(otherPage) != 1 || otherPage[0].ProjectID == choices[0].ProjectID {
		t.Fatalf("project choice pagination failed: choices=%+v err=%v", otherPage, err)
	}
	if _, err := sessions.RequestProjectBinding(ctx, "legacy-a", strings.Repeat("0", 32)); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("unlisted project ID was accepted: %v", err)
	}
	request, err := sessions.RequestProjectBinding(ctx, "legacy-a", choices[0].ProjectID)
	if err != nil || request.Status != domain.SessionProjectBindingPending {
		t.Fatalf("project binding request=%+v err=%v", request, err)
	}
	if err := sessions.AuthorizeResume(ctx, logicalWorkspaceID(repoA), "legacy-a"); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("pending project binding granted resume: %v", err)
	}
	if _, err := sessions.ApproveProjectBindingLocal(context.Background(), request.ID, ""); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("remote or empty actor approved project binding: %v", err)
	}
	stale, err := sessions.RequestProjectBinding(ctx, "legacy-b", otherPage[0].ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	catalog.sessions["legacy-b"] = func() domain.CodexSession {
		value := catalog.sessions["legacy-b"]
		value.UpdatedAt = value.UpdatedAt.Add(time.Minute)
		return value
	}()
	if _, err := sessions.ApproveProjectBindingLocal(context.Background(), stale.ID, "owner"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("changed session metadata was approved: %v", err)
	}
	if err := sessions.RejectProjectBindingLocal(context.Background(), stale.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	catalog.sessions["legacy-b"] = domain.CodexSession{ID: "legacy-b", WorkspacePath: rootPath, Source: "cli", Title: "B", Status: "idle", CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-time.Minute)}
	expired, err := sessions.RequestProjectBinding(ctx, "legacy-b", otherPage[0].ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	currentNow = now.Add(sessionProjectBindingTTL + time.Second)
	if _, err := sessions.ApproveProjectBindingLocal(context.Background(), expired.ID, "owner"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expired project binding was approved: %v", err)
	}
	canonicalRepoB, _ := filepath.EvalSymlinks(repoB)
	if _, exists, err := workspaces.FindByRootPathLocal(context.Background(), canonicalRepoB); err != nil || exists {
		t.Fatalf("expired request created a workspace before approval: exists=%t err=%v", exists, err)
	}
	currentNow = now
	binding, err := sessions.ApproveProjectBindingLocal(context.Background(), request.ID, "owner")
	if err != nil || binding.Status != domain.SessionProjectBindingApproved || binding.WorkspaceID == "" {
		t.Fatalf("local project binding approval=%+v err=%v", binding, err)
	}
	workspace, err := workspaces.Get(ctx, binding.WorkspaceID)
	canonicalRepoA, _ := filepath.EvalSymlinks(repoA)
	if err != nil || workspace.RootPath != canonicalRepoA || !workspace.RootSessionOnly || workspace.WriteEnabled || workspace.SessionTrusted {
		t.Fatalf("approved binding widened workspace or enabled writes: %+v err=%v", workspace, err)
	}
	thread, err := store.GetThread(ctx, "legacy-a")
	canonicalRootPath, _ := filepath.EvalSymlinks(rootPath)
	if err != nil || thread.Source != domain.ThreadSourceCodexCLIProjectBound || thread.SessionPath != canonicalRootPath || thread.TrustedRootID != rootRequest.ID {
		t.Fatalf("persisted thread binding=%+v err=%v", thread, err)
	}
	page, err = sessions.ListCatalog(ctx, binding.WorkspaceID, "", "", 10)
	if err != nil || len(page.Sessions) != 1 || page.Sessions[0].ConnectionStatus != "project_bound" || !page.Sessions[0].CanResume {
		t.Fatalf("approved binding not visible as resumable: page=%+v err=%v", page, err)
	}
	if err := sessions.AuthorizeResume(ctx, binding.WorkspaceID, "legacy-a"); err != nil {
		t.Fatalf("approved parent session did not resume: %v", err)
	}
	currentSession := catalog.sessions["legacy-a"]
	currentSession.WorkspacePath = canonicalRepoA
	catalog.sessions["legacy-a"] = currentSession
	if err := sessions.AuthorizeResume(ctx, binding.WorkspaceID, "legacy-a"); err != nil {
		t.Fatalf("Codex session reporting the explicitly bound project CWD was rejected: %v", err)
	}
	currentSession.WorkspacePath = canonicalRepoB
	catalog.sessions["legacy-a"] = currentSession
	if err := sessions.AuthorizeResume(ctx, binding.WorkspaceID, "legacy-a"); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("bound thread accepted a different repository CWD: %v", err)
	}
	currentSession.WorkspacePath = canonicalRootPath
	catalog.sessions["legacy-a"] = currentSession
	if err := sessions.AuthorizeResume(ctx, logicalWorkspaceID(repoB), "legacy-a"); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("bound thread crossed into another repository: %v", err)
	}
	tasks := NewTaskService(store, store, workspaces, NewApprovalService(store, workspaces), &taskQueueStub{}, allowTaskExecution{}, logger, 20_000, 20, sessions)
	if _, _, err := tasks.StartWrite(ctx, binding.WorkspaceID, "Change code", "legacy-a"); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("project binding granted write permission: %v", err)
	}
	if _, err := sessions.ApproveProjectBindingLocal(context.Background(), request.ID, "owner"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("project approval replay was accepted: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = sqlite.Open(filepath.Join(base, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	logger = audit.New(store)
	workspaces = NewWorkspaceService(store, logger, home, "", store)
	roots = NewTrustedWorkspaceRootService(store, workspaces, logger, home, "")
	sessions = NewSessionService(store, workspaces, catalog, logger, roots)
	sessions.now = func() time.Time { return currentNow }
	if err := sessions.AuthorizeResume(ctx, binding.WorkspaceID, "legacy-a"); err != nil {
		t.Fatalf("approved project binding did not survive SQLite restart: %v", err)
	}
	thread, err = store.GetThread(ctx, "legacy-a")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = workspaces.Get(ctx, binding.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.RevokeProjectBindingLocal(context.Background(), "legacy-a", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := sessions.AuthorizeResume(ctx, binding.WorkspaceID, "legacy-a"); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("revoked project binding still resumed: %v", err)
	}
	if err := roots.ValidateThread(ctx, thread, workspace); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("revoked binding still passed worker thread validation: %v", err)
	}
}

func TestAttachSessionRequiresIndependentLocalApprovalAndKeepsWriteGate(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	home := t.TempDir()
	logger := audit.New(store)
	workspaces := NewWorkspaceService(store, logger, home, "")
	workspace := registerForTest(t, workspaces, home, "project-a")
	workspaceB := registerForTest(t, workspaces, home, "project-b")
	now := time.Now().UTC().Truncate(time.Second)
	catalog := &codexSessionCatalogStub{sessions: map[string]domain.CodexSession{
		"cli-thread-a": {ID: "cli-thread-a", WorkspacePath: workspace.RootPath, Source: "vscode", Originator: "codex-tui", Title: "fixture session", Status: "notLoaded", CreatedAt: now.Add(-time.Hour), UpdatedAt: now},
	}}
	sessions := NewSessionService(store, workspaces, catalog, logger)
	ctx := authorizedContext("fedcba9876543210fedcba9876543210")

	request, err := sessions.RequestAttach(ctx, workspace.ID, "cli-thread-a")
	if err != nil {
		t.Fatal(err)
	}
	if request.Status != domain.SessionAttachPending || request.ThreadID != "cli-thread-a" {
		t.Fatalf("bad attach request: %+v", request)
	}
	if err := store.EnsureThreadWorkspace(context.Background(), workspace.ID, "cli-thread-a"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("pending session became visible before local approval: %v", err)
	}
	if _, err := sessions.ApproveLocal(context.Background(), request.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	thread, err := store.GetThread(context.Background(), "cli-thread-a")
	if err != nil {
		t.Fatal(err)
	}
	if thread.WorkspaceID != workspace.ID || thread.Source != domain.ThreadSourceCodexCLI || thread.SessionPath != workspace.RootPath {
		t.Fatalf("attached thread lost its workspace/source binding: %+v", thread)
	}
	if _, err := sessions.ApproveLocal(context.Background(), request.ID, "tester"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("approval replay error=%v, want conflict", err)
	}

	queue := &taskQueueStub{}
	tasks := NewTaskService(store, store, workspaces, NewApprovalService(store, workspaces), queue, allowTaskExecution{}, logger, 20_000, 20)
	listed, err := tasks.ListThreads(ctx, workspace.ID)
	if err != nil || len(listed) != 1 || listed[0].ThreadID != "cli-thread-a" || listed[0].Source != "codex-cli" {
		t.Fatalf("attached session not discoverable through MCP thread list: %+v err=%v", listed, err)
	}
	continued, err := tasks.Start(ctx, workspace.ID, "Continue read-only analysis", "cli-thread-a")
	if err != nil || continued.ThreadID != "cli-thread-a" || len(queue.jobs) != 1 {
		t.Fatalf("attached session did not continue through owned workspace: task=%+v err=%v", continued, err)
	}
	if _, err := tasks.Start(ctx, workspaceB.ID, "Cross workspace", "cli-thread-a"); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("cross-workspace resume error=%v, want not authorized", err)
	}
	if err := workspaces.SetWriteEnabledLocal(context.Background(), workspace.ID, true, "tester"); err != nil {
		t.Fatal(err)
	}
	writeTask, writeApproval, err := tasks.StartWrite(ctx, workspace.ID, "Change fixture", "cli-thread-a")
	if err != nil || writeTask.Status != domain.TaskWaitingApproval || writeApproval.Status != domain.ApprovalPending || len(queue.jobs) != 1 {
		t.Fatalf("attached thread bypassed per-task write approval: task=%+v approval=%+v jobs=%d err=%v", writeTask, writeApproval, len(queue.jobs), err)
	}
}

func TestAttachRejectsUnapprovedCrossWorkspaceActiveAndStaleSessions(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	home := t.TempDir()
	workspaces := NewWorkspaceService(store, nil, home, "")
	workspaceA := registerForTest(t, workspaces, home, "project-a")
	workspaceB := registerForTest(t, workspaces, home, "project-b")
	link := filepath.Join(home, "alias-project-b")
	if err := os.Symlink(workspaceB.RootPath, link); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	catalog := &codexSessionCatalogStub{sessions: map[string]domain.CodexSession{
		"wrong-root":     {ID: "wrong-root", WorkspacePath: workspaceB.RootPath, Source: "cli", Status: "idle", CreatedAt: now, UpdatedAt: now},
		"symlink-escape": {ID: "symlink-escape", WorkspacePath: link, Source: "cli", Status: "notLoaded", CreatedAt: now, UpdatedAt: now},
		"path-escape":    {ID: "path-escape", WorkspacePath: filepath.Join(workspaceA.RootPath, "..", "project-b"), Source: "cli", Status: "notLoaded", CreatedAt: now, UpdatedAt: now},
		"active":         {ID: "active", WorkspacePath: workspaceA.RootPath, Source: "cli", Status: "active", CreatedAt: now, UpdatedAt: now},
		"editor":         {ID: "editor", WorkspacePath: workspaceA.RootPath, Source: "vscode", Originator: "codex-vscode", Status: "idle", CreatedAt: now, UpdatedAt: now},
	}}
	sessions := NewSessionService(store, workspaces, catalog, nil)
	ctx := authorizedContext("fedcba9876543210fedcba9876543210")
	for _, test := range []struct{ workspaceID, threadID string }{
		{workspaceA.ID, "wrong-root"},
		{workspaceA.ID, "symlink-escape"},
		{workspaceA.ID, "path-escape"},
		{workspaceA.ID, "active"},
		{workspaceA.ID, "editor"},
	} {
		if _, err := sessions.RequestAttach(ctx, test.workspaceID, test.threadID); !errors.Is(err, domain.ErrNotAuthorized) {
			t.Errorf("RequestAttach(%q) error=%v, want not authorized", test.threadID, err)
		}
	}
	if _, err := sessions.RequestAttach(context.Background(), workspaceA.ID, "active"); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("unauthorized caller error=%v, want not authorized", err)
	}
	if _, err := sessions.RequestAttach(ctx, "unapproved", "active"); err == nil {
		t.Fatal("unapproved workspace accepted a session attach request")
	}

	valid := domain.CodexSession{ID: "stale", WorkspacePath: workspaceA.RootPath, Source: "cli", Status: "notLoaded", CreatedAt: now.Add(-time.Hour), UpdatedAt: now}
	catalog.sessions[valid.ID] = valid
	request, err := sessions.RequestAttach(ctx, workspaceA.ID, valid.ID)
	if err != nil {
		t.Fatal(err)
	}
	changed := valid
	changed.UpdatedAt = now.Add(time.Second)
	catalog.sessions[valid.ID] = changed
	if _, err := sessions.ApproveLocal(context.Background(), request.ID, "tester"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("changed session was attached: %v", err)
	}
}

func TestAttachRequestExpiresAndCannotBeReplayed(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	home := t.TempDir()
	workspaces := NewWorkspaceService(store, nil, home, "")
	workspace := registerForTest(t, workspaces, home, "project-a")
	now := time.Now().UTC().Truncate(time.Second)
	catalog := &codexSessionCatalogStub{sessions: map[string]domain.CodexSession{
		"cli-thread": {ID: "cli-thread", WorkspacePath: workspace.RootPath, Source: "cli", Status: "notLoaded", CreatedAt: now.Add(-time.Hour), UpdatedAt: now},
	}}
	sessions := NewSessionService(store, workspaces, catalog, nil)
	sessions.now = func() time.Time { return now }
	ctx := authorizedContext("fedcba9876543210fedcba9876543210")
	request, err := sessions.RequestAttach(ctx, workspace.ID, "cli-thread")
	if err != nil {
		t.Fatal(err)
	}
	sessions.now = func() time.Time { return request.ExpiresAt.Add(time.Second) }
	if _, err := sessions.ApproveLocal(context.Background(), request.ID, "tester"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expired approval error=%v, want conflict", err)
	}
	if err := store.EnsureThreadWorkspace(context.Background(), workspace.ID, "cli-thread"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expired session attached: %v", err)
	}
}

func TestAttachApprovalRejectsReplacedWorkspaceRoot(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	home := t.TempDir()
	workspaces := NewWorkspaceService(store, nil, home, "")
	workspace := registerForTest(t, workspaces, home, "project-a")
	now := time.Now().UTC().Truncate(time.Second)
	catalog := &codexSessionCatalogStub{sessions: map[string]domain.CodexSession{
		"cli-thread": {ID: "cli-thread", WorkspacePath: workspace.RootPath, Source: "cli", Status: "notLoaded", CreatedAt: now.Add(-time.Hour), UpdatedAt: now},
	}}
	sessions := NewSessionService(store, workspaces, catalog, nil)
	ctx := authorizedContext("fedcba9876543210fedcba9876543210")
	request, err := sessions.RequestAttach(ctx, workspace.ID, "cli-thread")
	if err != nil {
		t.Fatal(err)
	}
	oldPath := workspace.RootPath + ".old"
	if err := os.Rename(workspace.RootPath, oldPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(workspace.RootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.ApproveLocal(context.Background(), request.ID, "tester"); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("replaced workspace was approved: %v", err)
	}
	if err := store.EnsureThreadWorkspace(context.Background(), workspace.ID, "cli-thread"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("session attached to replaced workspace: %v", err)
	}
}
