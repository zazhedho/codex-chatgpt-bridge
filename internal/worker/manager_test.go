package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"example.com/codex-chatgpt-bridge/internal/audit"
	"example.com/codex-chatgpt-bridge/internal/codex"
	"example.com/codex-chatgpt-bridge/internal/domain"
	"example.com/codex-chatgpt-bridge/internal/repository/sqlite"
	"example.com/codex-chatgpt-bridge/internal/service"
)

type gatewayStub struct {
	started chan string
	release chan struct{}
	runErr  error
}

func (g *gatewayStub) Connected() bool { return true }
func (g *gatewayStub) Close() error    { return nil }
func (g *gatewayStub) Run(ctx context.Context, request codex.TaskRequest) (codex.TaskResult, error) {
	g.started <- request.TaskID
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
	store     *sqlite.Store
	manager   *Manager
	workspace domain.Workspace
	gateway   *gatewayStub
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
	gateway := &gatewayStub{started: make(chan string, 10), release: make(chan struct{}, 10), runErr: runErr}
	manager := New(store, store, gateway, audit.New(store), nil, concurrent, queued, timeout, 1024)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = manager.Shutdown(ctx)
		_ = store.Close()
	})
	return &workerFixture{store: store, manager: manager, workspace: workspace, gateway: gateway}
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
