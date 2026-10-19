package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"example.com/codex-chatgpt-bridge/internal/audit"
	"example.com/codex-chatgpt-bridge/internal/domain"
	"example.com/codex-chatgpt-bridge/internal/security"
)

type TaskRepository interface {
	CreateTask(context.Context, domain.Task) error
	GetTask(context.Context, string) (domain.Task, error)
	ListTasksForThread(context.Context, string, string) ([]domain.Task, error)
	SetTaskThread(context.Context, string, string, string) error
	SetTaskStatus(context.Context, string, domain.TaskStatus, string) error
	FinishTask(context.Context, string, domain.TaskStatus, string, string, string) error
	AppendProgress(context.Context, string, string, time.Time) error
	TaskStats(context.Context) (int, int, error)
}

type ThreadRepository interface {
	SaveThread(context.Context, domain.Thread) error
	ListThreads(context.Context, string) ([]domain.Thread, error)
	EnsureThreadWorkspace(context.Context, string, string) error
}

type ExecutionJob struct {
	TaskID    string
	Workspace domain.Workspace
	Prompt    string
	ThreadID  string
}

type TaskQueue interface {
	Submit(context.Context, ExecutionJob) error
	Cancel(context.Context, string) error
}

type TaskExecutionGate interface {
	AuthorizeExecution(context.Context, domain.Workspace) error
}

type TaskService struct {
	tasks      TaskRepository
	threads    ThreadRepository
	workspaces *WorkspaceService
	queue      TaskQueue
	gate       TaskExecutionGate
	audit      *audit.Logger
	maxPrompt  int
}

func NewTaskService(tasks TaskRepository, threads ThreadRepository, workspaces *WorkspaceService, queue TaskQueue, gate TaskExecutionGate, auditLogger *audit.Logger, maxPrompt int) *TaskService {
	if maxPrompt <= 0 {
		maxPrompt = 20_000
	}
	return &TaskService{tasks: tasks, threads: threads, workspaces: workspaces, queue: queue, gate: gate, audit: auditLogger, maxPrompt: maxPrompt}
}

func (s *TaskService) Start(ctx context.Context, workspaceID, prompt, threadID string) (domain.Task, error) {
	if _, ok := security.Authorize(ctx); !ok {
		return domain.Task{}, domain.ErrNotAuthorized
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" || utf8.RuneCountInString(prompt) > s.maxPrompt {
		return domain.Task{}, domain.ErrInvalidInput
	}
	workspace, err := s.workspaces.Get(ctx, workspaceID)
	if err != nil {
		return domain.Task{}, err
	}
	if s.gate == nil {
		return domain.Task{}, domain.ErrServiceUnavailable
	}
	if err := s.gate.AuthorizeExecution(ctx, workspace); err != nil {
		return domain.Task{}, err
	}
	if threadID != "" {
		if err := s.threads.EnsureThreadWorkspace(ctx, workspaceID, threadID); err != nil {
			return domain.Task{}, domain.ErrNotAuthorized
		}
	}
	id, err := newID()
	if err != nil {
		return domain.Task{}, err
	}
	now := time.Now().UTC()
	hash := sha256.Sum256([]byte(prompt))
	task := domain.Task{ID: id, WorkspaceID: workspace.ID, ThreadID: threadID, Status: domain.TaskQueued, PromptHash: hex.EncodeToString(hash[:]), Progress: []domain.ProgressEvent{}, CreatedAt: now, UpdatedAt: now}
	if err := s.tasks.CreateTask(ctx, task); err != nil {
		return domain.Task{}, err
	}
	if s.audit != nil {
		if err := s.audit.Record(ctx, audit.Event{Operation: "task.queued", Principal: "mcp-bearer", WorkspaceID: workspace.ID, TaskID: id, Outcome: "requested"}); err != nil {
			_ = s.tasks.SetTaskStatus(context.Background(), id, domain.TaskFailed, "Task was not started because audit persistence failed")
			return domain.Task{}, domain.ErrServiceUnavailable
		}
	}
	if err := s.queue.Submit(ctx, ExecutionJob{TaskID: id, Workspace: workspace, Prompt: prompt, ThreadID: threadID}); err != nil {
		_ = s.tasks.SetTaskStatus(context.Background(), id, domain.TaskFailed, "Task could not be queued")
		return domain.Task{}, err
	}
	return s.tasks.GetTask(ctx, id)
}

func (s *TaskService) Get(ctx context.Context, id string) (domain.Task, error) {
	if _, ok := security.Authorize(ctx); !ok {
		return domain.Task{}, domain.ErrNotAuthorized
	}
	task, err := s.tasks.GetTask(ctx, id)
	if err != nil {
		return domain.Task{}, err
	}
	if _, err := s.workspaces.Get(ctx, task.WorkspaceID); err != nil {
		return domain.Task{}, domain.ErrNotAuthorized
	}
	return scrubTask(task), nil
}

func (s *TaskService) GetProgress(ctx context.Context, id string) (domain.Task, error) {
	return s.Get(ctx, id)
}
func (s *TaskService) GetResult(ctx context.Context, id string) (domain.Task, error) {
	return s.Get(ctx, id)
}
func (s *TaskService) GetChanges(ctx context.Context, id string) (domain.Task, error) {
	return s.Get(ctx, id)
}

func (s *TaskService) Cancel(ctx context.Context, id string) (domain.Task, error) {
	if _, ok := security.Authorize(ctx); !ok {
		return domain.Task{}, domain.ErrNotAuthorized
	}
	task, err := s.Get(ctx, id)
	if err != nil {
		return domain.Task{}, err
	}
	if task.Status == domain.TaskCancelled {
		return task, nil
	}
	if task.Status.Terminal() {
		return domain.Task{}, domain.ErrConflict
	}
	if err := s.queue.Cancel(ctx, id); err != nil {
		return domain.Task{}, err
	}
	if s.audit != nil {
		_ = s.audit.Record(ctx, audit.Event{Operation: "task.cancel_requested", Principal: "mcp-bearer", WorkspaceID: task.WorkspaceID, TaskID: id, Outcome: "success"})
	}
	return s.Get(ctx, id)
}

func (s *TaskService) ListThreads(ctx context.Context, workspaceID string) ([]ThreadSummary, error) {
	if _, ok := security.Authorize(ctx); !ok {
		return nil, domain.ErrNotAuthorized
	}
	if _, err := s.workspaces.Get(ctx, workspaceID); err != nil {
		return nil, err
	}
	threads, err := s.threads.ListThreads(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	result := make([]ThreadSummary, 0, len(threads))
	for _, thread := range threads {
		tasks, err := s.tasks.ListTasksForThread(ctx, workspaceID, thread.ID)
		if err != nil {
			return nil, err
		}
		if len(tasks) == 0 {
			continue
		}
		result = append(result, ThreadSummary{ThreadID: thread.ID, TaskID: tasks[0].ID, Status: tasks[0].Status, UpdatedAt: thread.UpdatedAt.UnixMilli()})
	}
	return result, nil
}

func (s *TaskService) Stats(ctx context.Context) (int, int, error) { return s.tasks.TaskStats(ctx) }

func scrubTask(task domain.Task) domain.Task {
	for i := range task.Progress {
		task.Progress[i].Text = security.RedactSecrets(task.Progress[i].Text)
	}
	if task.Result != nil {
		value := security.RedactSecrets(*task.Result)
		task.Result = &value
	}
	if task.Diff != nil {
		value := security.RedactSecrets(*task.Diff)
		task.Diff = &value
	}
	if task.Error != nil {
		value := security.RedactSecrets(*task.Error)
		task.Error = &value
	}
	if task.OutputExpired {
		task.Progress = []domain.ProgressEvent{}
		task.Result = nil
		task.Diff = nil
		task.Error = nil
	}
	return task
}

type ThreadSummary struct {
	ThreadID  string
	TaskID    string
	Status    domain.TaskStatus
	UpdatedAt int64
}

func IsNotFound(err error) bool { return errors.Is(err, domain.ErrNotFound) }
