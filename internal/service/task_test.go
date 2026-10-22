package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zazhedho/codex-chatgpt-bridge/internal/audit"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/domain"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/repository/sqlite"
)

type taskQueueStub struct {
	mu        sync.Mutex
	jobs      []ExecutionJob
	submitErr error
}

type allowTaskExecution struct{}

func (allowTaskExecution) AuthorizeExecution(context.Context, domain.Workspace) error { return nil }

func (q *taskQueueStub) Submit(_ context.Context, job ExecutionJob) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.submitErr != nil {
		return q.submitErr
	}
	q.jobs = append(q.jobs, job)
	return nil
}
func (q *taskQueueStub) Cancel(context.Context, string) error { return nil }

func TestTaskServiceAuthorizesWorkspaceAndThreadOwnership(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	home := t.TempDir()
	auditLogger := audit.New(store)
	workspaces := NewWorkspaceService(store, auditLogger, home, "")
	ctx := authorizedContext("fedcba9876543210fedcba9876543210")
	workspaceA := registerForTest(t, workspaces, home, "project-a")
	workspaceB := registerForTest(t, workspaces, home, "project-b")
	if err := store.SaveThread(ctx, domain.Thread{ID: "thread-a", WorkspaceID: workspaceA.ID, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	queue := &taskQueueStub{}
	approvals := NewApprovalService(store, workspaces)
	tasks := NewTaskService(store, store, workspaces, approvals, queue, allowTaskExecution{}, auditLogger, 20_000, 20)
	if _, err := tasks.Start(context.Background(), workspaceA.ID, "Analyze project", ""); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("unauthorized task error = %v", err)
	}
	if _, err := tasks.Start(ctx, workspaceB.ID, "Analyze project", "thread-a"); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("cross-workspace thread error = %v", err)
	}
	task, err := tasks.Start(ctx, workspaceA.ID, "  Analyze project  ", "")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != domain.TaskQueued || task.PromptHash == "" {
		t.Fatalf("bad queued task: %+v", task)
	}
	queue.mu.Lock()
	count := len(queue.jobs)
	queue.mu.Unlock()
	if count != 1 {
		t.Fatalf("submitted jobs=%d", count)
	}
}

func TestWriteTaskWaitsForLocalApprovalAndWorkspaceOptIn(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	home := t.TempDir()
	logger := audit.New(store)
	workspaces := NewWorkspaceService(store, logger, home, "")
	workspace := registerForTest(t, workspaces, home, "project-a")
	queue := &taskQueueStub{}
	approvals := NewApprovalService(store, workspaces)
	tasks := NewTaskService(store, store, workspaces, approvals, queue, allowTaskExecution{}, logger, 20_000, 20)
	ctx := authorizedContext("fedcba9876543210fedcba9876543210")

	if _, _, err := tasks.StartWrite(ctx, workspace.ID, "change a fixture", ""); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("read-only workspace accepted a write task: %v", err)
	}
	if err := workspaces.SetWriteEnabledLocal(context.Background(), workspace.ID, true, "operator"); err != nil {
		t.Fatal(err)
	}
	task, approval, err := tasks.StartWrite(ctx, workspace.ID, "change a fixture", "")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != domain.TaskWaitingApproval || task.ExecutionMode != domain.ExecutionWorkspaceWrite {
		t.Fatalf("write task was not held for approval: %+v", task)
	}
	if approval.Status != domain.ApprovalPending || approval.WorkspaceID != workspace.ID || approval.OperationType != "codex.workspace-write" {
		t.Fatalf("write approval does not bind operation and workspace: %+v", approval)
	}
	if !strings.Contains(approval.ParametersJSON, "change a fixture") || !strings.Contains(approval.ParametersJSON, task.ID) {
		t.Fatal("local approval does not contain the exact task parameters")
	}
	if len(queue.jobs) != 0 {
		t.Fatal("write task was queued before local approval")
	}
}

func TestDisablingWorkspaceWriteCancelsPendingTaskAndApproval(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	home := t.TempDir()
	logger := audit.New(store)
	workspaces := NewWorkspaceService(store, logger, home, "")
	workspace := registerForTest(t, workspaces, home, "project-a")
	if err := workspaces.SetWriteEnabledLocal(context.Background(), workspace.ID, true, "operator"); err != nil {
		t.Fatal(err)
	}
	approvals := NewApprovalService(store, workspaces)
	tasks := NewTaskService(store, store, workspaces, approvals, &taskQueueStub{}, allowTaskExecution{}, logger, 20_000, 20)
	ctx := authorizedContext("fedcba9876543210fedcba9876543210")
	task, approval, err := tasks.StartWrite(ctx, workspace.ID, "change fixture", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := workspaces.SetWriteEnabledLocal(context.Background(), workspace.ID, false, "operator"); err != nil {
		t.Fatal(err)
	}
	storedTask, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedApproval, err := approvals.GetLocal(context.Background(), approval.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedTask.Status != domain.TaskCancelled || storedApproval.Status != domain.ApprovalRejected || storedApproval.ParametersJSON != "{}" {
		t.Fatalf("write disable left pending work: task=%s approval=%s parameters=%s", storedTask.Status, storedApproval.Status, storedApproval.ParametersJSON)
	}
}

func TestTaskResultIsRedactedAndOutputExpiryIsVisible(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	home := t.TempDir()
	workspaces := NewWorkspaceService(store, audit.New(store), home, "")
	workspace := registerForTest(t, workspaces, home, "project-a")
	queue := &taskQueueStub{}
	tasks := NewTaskService(store, store, workspaces, NewApprovalService(store, workspaces), queue, allowTaskExecution{}, audit.New(store), 20_000, 20)
	ctx := authorizedContext("fedcba9876543210fedcba9876543210")
	task, err := tasks.Start(ctx, workspace.ID, "Review", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetTaskStatus(ctx, task.ID, domain.TaskRunning, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishTask(ctx, task.ID, domain.TaskSucceeded, "apiToken: secret-value-which-must-not-leak", "", ""); err != nil {
		t.Fatal(err)
	}
	result, err := tasks.GetResult(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Result == nil || strings.Contains(*result.Result, "secret-value-which-must-not-leak") {
		t.Fatalf("secret escaped redaction: %+v", result.Result)
	}
	if !strings.Contains(*result.Result, "[REDACTED]") {
		t.Fatalf("redaction marker missing: %s", *result.Result)
	}
	oldID, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-48 * time.Hour)
	if err := store.CreateTask(ctx, domain.Task{ID: oldID, WorkspaceID: workspace.ID, Status: domain.TaskSucceeded, PromptHash: "fixture", Progress: []domain.ProgressEvent{{At: old.UnixMilli(), Text: "old output"}}, Result: stringPointer("old result"), CreatedAt: old, UpdatedAt: old}); err != nil {
		t.Fatal(err)
	}
	runningID, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTask(ctx, domain.Task{ID: runningID, WorkspaceID: workspace.ID, Status: domain.TaskRunning, PromptHash: "fixture", Progress: []domain.ProgressEvent{{At: old.UnixMilli(), Text: "active output"}}, Result: stringPointer("active result"), CreatedAt: old, UpdatedAt: old}); err != nil {
		t.Fatal(err)
	}
	recentID, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTask(ctx, domain.Task{ID: recentID, WorkspaceID: workspace.ID, Status: domain.TaskSucceeded, PromptHash: "fixture", Progress: []domain.ProgressEvent{{At: old.UnixMilli(), Text: "recent output"}}, Result: stringPointer("recent result"), CreatedAt: old, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := store.ExpireTaskOutputs(ctx, 1); err != nil {
		t.Fatal(err)
	}
	expired, err := tasks.Get(ctx, oldID)
	if err != nil {
		t.Fatal(err)
	}
	if !expired.OutputExpired || expired.Result != nil || expired.Progress == nil {
		t.Fatalf("expired output state is unclear: %+v", expired)
	}
	for _, id := range []string{runningID, recentID} {
		preserved, err := tasks.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if preserved.OutputExpired || preserved.Result == nil {
			t.Fatalf("active or recently completed output expired: task=%s value=%+v", id, preserved)
		}
	}
}

func TestTaskExecutionFailsClosedWithoutVerifiedReadIsolation(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	home := t.TempDir()
	logger := audit.New(store)
	workspaces := NewWorkspaceService(store, logger, home, "")
	workspace := registerForTest(t, workspaces, home, "project-a")
	queue := &taskQueueStub{}
	tasks := NewTaskService(store, store, workspaces, NewApprovalService(store, workspaces), queue, nil, logger, 20_000, 20)
	ctx := authorizedContext("fedcba9876543210fedcba9876543210")
	if _, err := tasks.Start(ctx, workspace.ID, "Read the project", ""); !errors.Is(err, domain.ErrServiceUnavailable) {
		t.Fatalf("task start error=%v, want service unavailable", err)
	}
	active, queued, err := store.TaskStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if active != 0 || queued != 0 || len(queue.jobs) != 0 {
		t.Fatalf("disabled task was persisted or queued: active=%d queued=%d jobs=%d", active, queued, len(queue.jobs))
	}
}

func stringPointer(value string) *string { return &value }

func registerForTest(t *testing.T, workspaces *WorkspaceService, home, id string) domain.Workspace {
	t.Helper()
	root := filepath.Join(home, id)
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	request, err := workspaces.RequestRegistrationLocal(context.Background(), id, root, "operator")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := workspaces.ApproveRegistration(context.Background(), request.ID, "operator")
	if err != nil {
		t.Fatal(err)
	}
	return workspace
}
