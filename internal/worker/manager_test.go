package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zazhedho/codex-chatgpt-bridge/internal/audit"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/codex"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/domain"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/repository/sqlite"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/service"
)

type gatewayStub struct {
	started chan string
	modes   chan domain.ExecutionMode
	release chan struct{}
	runErr  error
}

type trustedThreadValidatorStub struct{ allowed bool }

func (v *trustedThreadValidatorStub) ValidateThread(context.Context, domain.Thread, domain.Workspace) error {
	if !v.allowed {
		return domain.ErrNotAuthorized
	}
	return nil
}

func (g *gatewayStub) Connected() bool { return true }
func (g *gatewayStub) Close() error    { return nil }
func (g *gatewayStub) Run(ctx context.Context, request codex.TaskRequest) (codex.TaskResult, error) {
	g.started <- request.TaskID
	if g.modes != nil {
		g.modes <- request.ExecutionMode
	}
	if g.runErr != nil {
		return codex.TaskResult{}, g.runErr
	}
	select {
	case <-ctx.Done():
		return codex.TaskResult{}, ctx.Err()
	case <-g.release:
		return codex.TaskResult{ThreadID: "thread-" + request.TaskID, TurnID: "turn-" + request.TaskID, Status: domain.TaskSucceeded, FinalText: "done", Diff: ""}, nil
	}
}

type workerFixture struct {
	store      *sqlite.Store
	manager    *Manager
	workspace  domain.Workspace
	workspaces *service.WorkspaceService
	gateway    *gatewayStub
}

func setupWorker(t *testing.T, concurrent, queued int, timeout int, runErr error) *workerFixture {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	root := filepath.Join(home, "project")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	workspaces := service.NewWorkspaceService(store, nil, home, "")
	request, err := workspaces.RequestRegistrationLocal(context.Background(), "project-a", root, "tester")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := workspaces.ApproveRegistration(context.Background(), request.ID, "tester")
	if err != nil {
		t.Fatal(err)
	}
	gateway := &gatewayStub{started: make(chan string, 10), modes: make(chan domain.ExecutionMode, 10), release: make(chan struct{}, 10), runErr: runErr}
	manager := New(store, store, workspaces, gateway, audit.New(store), nil, concurrent, queued, timeout, 1024)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = manager.Shutdown(ctx)
		_ = store.Close()
	})
	return &workerFixture{store: store, manager: manager, workspace: workspace, workspaces: workspaces, gateway: gateway}
}

func workerTask(t *testing.T, f *workerFixture, id string) service.ExecutionJob {
	t.Helper()
	now := time.Now().UTC()
	if err := f.store.CreateTask(context.Background(), domain.Task{ID: id, WorkspaceID: f.workspace.ID, Status: domain.TaskQueued, PromptHash: "hash", Progress: []domain.ProgressEvent{}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	return service.ExecutionJob{TaskID: id, Workspace: f.workspace, Prompt: "inspect"}
}

func TestWorkerBoundsQueueAndCompletesWithoutReplay(t *testing.T) {
	f := setupWorker(t, 1, 1, 30, nil)
	if err := f.manager.Submit(context.Background(), workerTask(t, f, "task-one")); err != nil {
		t.Fatal(err)
	}
	if id := <-f.gateway.started; id != "task-one" {
		t.Fatalf("started %s", id)
	}
	if err := f.manager.Submit(context.Background(), workerTask(t, f, "task-two")); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.Submit(context.Background(), workerTask(t, f, "task-three")); !errors.Is(err, domain.ErrQueueFull) {
		t.Fatalf("saturation error = %v", err)
	}
	active, queued := f.manager.Stats()
	if active != 1 || queued != 1 {
		t.Fatalf("stats active=%d queued=%d", active, queued)
	}
	f.gateway.release <- struct{}{}
	select {
	case id := <-f.gateway.started:
		if id != "task-two" {
			t.Fatalf("second task=%s", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued task did not start")
	}
	f.gateway.release <- struct{}{}
	waitStatus(t, f.store, "task-one", domain.TaskSucceeded)
	waitStatus(t, f.store, "task-two", domain.TaskSucceeded)
	select {
	case id := <-f.gateway.started:
		t.Fatalf("saturated task was run: %s", id)
	default:
	}
}

func TestWorkerCancellationAndShutdownAreTerminalAndIdempotent(t *testing.T) {
	f := setupWorker(t, 1, 2, 30, nil)
	if err := f.manager.Submit(context.Background(), workerTask(t, f, "cancel-me")); err != nil {
		t.Fatal(err)
	}
	<-f.gateway.started
	if err := f.manager.Submit(context.Background(), workerTask(t, f, "queued-interrupted")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := f.manager.Cancel(ctx, "cancel-me"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, f.store, "cancel-me", domain.TaskCancelled)
	if err := f.manager.Cancel(ctx, "cancel-me"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("repeated cancel error=%v", err)
	}
	if id := <-f.gateway.started; id != "queued-interrupted" {
		t.Fatalf("queued task did not start in order: %s", id)
	}
	if err := f.manager.Submit(ctx, workerTask(t, f, "shutdown-me")); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, f.store, "shutdown-me", domain.TaskInterrupted)
	waitStatus(t, f.store, "queued-interrupted", domain.TaskInterrupted)
}

func TestWorkerBackendFailureCannotBecomeSuccess(t *testing.T) {
	f := setupWorker(t, 1, 1, 30, errors.New("backend failed"))
	if err := f.manager.Submit(context.Background(), workerTask(t, f, "fails")); err != nil {
		t.Fatal(err)
	}
	<-f.gateway.started
	waitStatus(t, f.store, "fails", domain.TaskFailed)
	task, err := f.store.GetTask(context.Background(), "fails")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status == domain.TaskSucceeded {
		t.Fatal("backend failure reported as success")
	}
}

func TestWorkerRejectsQueuedTrustedThreadAfterWorkspaceTrustRevocation(t *testing.T) {
	f := setupWorker(t, 1, 2, 30, nil)
	if err := f.workspaces.SetSessionTrustedLocal(context.Background(), f.workspace.ID, true, "tester"); err != nil {
		t.Fatal(err)
	}
	threadID := "trusted-cli-session"
	if err := f.store.SaveThread(context.Background(), domain.Thread{ID: threadID, WorkspaceID: f.workspace.ID, Source: domain.ThreadSourceCodexCLITrusted, SessionPath: f.workspace.RootPath}); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.Submit(context.Background(), workerTask(t, f, "blocks-worker")); err != nil {
		t.Fatal(err)
	}
	if id := <-f.gateway.started; id != "blocks-worker" {
		t.Fatalf("blocker task=%q", id)
	}
	queued := workerTask(t, f, "revoked-session-task")
	queued.ThreadID = threadID
	if err := f.manager.Submit(context.Background(), queued); err != nil {
		t.Fatal(err)
	}
	if err := f.workspaces.SetSessionTrustedLocal(context.Background(), f.workspace.ID, false, "tester"); err != nil {
		t.Fatal(err)
	}
	f.gateway.release <- struct{}{}
	waitStatus(t, f.store, queued.TaskID, domain.TaskFailed)
	select {
	case id := <-f.gateway.started:
		t.Fatalf("revoked trusted session reached Codex: task=%s", id)
	default:
	}
}

func TestWorkerRevalidatesTrustedRootBeforeStartingQueuedResume(t *testing.T) {
	f := setupWorker(t, 1, 2, 30, nil)
	validator := &trustedThreadValidatorStub{allowed: true}
	f.manager.trustedThreads = validator
	threadID := "root-trusted-cli-session"
	if err := f.store.SaveThread(context.Background(), domain.Thread{ID: threadID, WorkspaceID: f.workspace.ID, Source: domain.ThreadSourceCodexCLIRoot, SessionPath: f.workspace.RootPath, TrustedRootID: "approved-root"}); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.Submit(context.Background(), workerTask(t, f, "root-blocker")); err != nil {
		t.Fatal(err)
	}
	if id := <-f.gateway.started; id != "root-blocker" {
		t.Fatalf("blocker task=%q", id)
	}
	queued := workerTask(t, f, "root-revoked-session-task")
	queued.ThreadID = threadID
	if err := f.manager.Submit(context.Background(), queued); err != nil {
		t.Fatal(err)
	}
	validator.allowed = false
	f.gateway.release <- struct{}{}
	waitStatus(t, f.store, queued.TaskID, domain.TaskFailed)
	select {
	case id := <-f.gateway.started:
		t.Fatalf("revoked root session reached Codex: task=%s", id)
	default:
	}
}

func TestWorkerWaitsForLocalWriteApprovalAndConsumesItOnce(t *testing.T) {
	f := setupWorker(t, 1, 2, 30, nil)
	if err := f.workspaces.SetWriteEnabledLocal(context.Background(), f.workspace.ID, true, "tester"); err != nil {
		t.Fatal(err)
	}
	prompt := "change the fixture and run its test"
	hash := sha256.Sum256([]byte(prompt))
	now := time.Now().UTC()
	task := domain.Task{ID: "00000000-0000-4000-8000-000000000001", WorkspaceID: f.workspace.ID, ExecutionMode: domain.ExecutionWorkspaceWrite, Status: domain.TaskWaitingApproval, PromptHash: hex.EncodeToString(hash[:]), Progress: []domain.ProgressEvent{}, CreatedAt: now, UpdatedAt: now}
	approvals := service.NewApprovalService(f.store, f.workspaces)
	approval, err := approvals.CreateWriteTaskApproval(context.Background(), task, prompt, 4)
	if err != nil {
		t.Fatal(err)
	}
	f.manager.Start()
	select {
	case id := <-f.gateway.started:
		t.Fatalf("write task %s ran before local approval", id)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := approvals.ApproveLocal(context.Background(), approval.ID, service.ApprovalActor{Channel: "local", Subject: "tester"}); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-f.gateway.started:
		if id != task.ID {
			t.Fatalf("worker started task %q, want %q", id, task.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("approved write task did not run")
	}
	if mode := <-f.gateway.modes; mode != domain.ExecutionWorkspaceWrite {
		t.Fatalf("worker execution mode=%q", mode)
	}
	f.gateway.release <- struct{}{}
	waitStatus(t, f.store, task.ID, domain.TaskSucceeded)
	stored, err := approvals.GetLocal(context.Background(), approval.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != domain.ApprovalConsumed || stored.ParametersJSON != "{}" {
		t.Fatalf("approval was not consumed and cleared: status=%s parameters=%s", stored.Status, stored.ParametersJSON)
	}
	if _, err := f.store.ClaimApprovedWriteTask(context.Background(), time.Now().UTC()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("consumed approval replay error=%v", err)
	}
}

func TestCancelBeforeApprovalRejectsWriteRequestWithoutStartingCodex(t *testing.T) {
	f := setupWorker(t, 1, 2, 30, nil)
	if err := f.workspaces.SetWriteEnabledLocal(context.Background(), f.workspace.ID, true, "tester"); err != nil {
		t.Fatal(err)
	}
	prompt := "modify the disposable fixture"
	hash := sha256.Sum256([]byte(prompt))
	now := time.Now().UTC()
	task := domain.Task{ID: "00000000-0000-4000-8000-000000000004", WorkspaceID: f.workspace.ID, ExecutionMode: domain.ExecutionWorkspaceWrite, Status: domain.TaskWaitingApproval, PromptHash: hex.EncodeToString(hash[:]), Progress: []domain.ProgressEvent{}, CreatedAt: now, UpdatedAt: now}
	approvals := service.NewApprovalService(f.store, f.workspaces)
	approval, err := approvals.CreateWriteTaskApproval(context.Background(), task, prompt, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.manager.Cancel(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	storedTask, err := f.store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedApproval, err := approvals.GetLocal(context.Background(), approval.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedTask.Status != domain.TaskCancelled || storedApproval.Status != domain.ApprovalRejected || storedApproval.ParametersJSON != "{}" {
		t.Fatalf("cancel left write request active: task=%s approval=%s parameters=%s", storedTask.Status, storedApproval.Status, storedApproval.ParametersJSON)
	}
	select {
	case id := <-f.gateway.started:
		t.Fatalf("cancelled write task started Codex task %s", id)
	default:
	}
}

func waitStatus(t *testing.T, store *sqlite.Store, id string, expected domain.TaskStatus) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		task, err := store.GetTask(context.Background(), id)
		if err == nil && task.Status == expected {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, err := store.GetTask(context.Background(), id)
	t.Fatalf("task %s status=%s err=%v, want %s", id, task.Status, err, expected)
}
