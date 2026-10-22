package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zazhedho/codex-chatgpt-bridge/internal/audit"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/domain"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/security"
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
	GetThread(context.Context, string) (domain.Thread, error)
	EnsureThreadWorkspace(context.Context, string, string) error
}

type ExecutionJob struct {
	TaskID        string
	Workspace     domain.Workspace
	Prompt        string
	ThreadID      string
	ExecutionMode domain.ExecutionMode
}

type TaskQueue interface {
	Submit(context.Context, ExecutionJob) error
	Cancel(context.Context, string) error
}

type TaskExecutionGate interface {
	AuthorizeExecution(context.Context, domain.Workspace) error
}

type TaskThreadAuthorizer interface {
	AuthorizeResume(context.Context, string, string) error
}

type TaskService struct {
	tasks            TaskRepository
	threads          ThreadRepository
	workspaces       *WorkspaceService
	approvals        *ApprovalService
	queue            TaskQueue
	gate             TaskExecutionGate
	audit            *audit.Logger
	maxPrompt        int
	maxPendingWrites int
	threadAuthorizer TaskThreadAuthorizer
}

func NewTaskService(tasks TaskRepository, threads ThreadRepository, workspaces *WorkspaceService, approvals *ApprovalService, queue TaskQueue, gate TaskExecutionGate, auditLogger *audit.Logger, maxPrompt, maxPendingWrites int, threadAuthorizers ...TaskThreadAuthorizer) *TaskService {
	if maxPrompt <= 0 {
		maxPrompt = 20_000
	}
	if maxPendingWrites <= 0 {
		maxPendingWrites = 20
	}
	var threadAuthorizer TaskThreadAuthorizer
	if len(threadAuthorizers) != 0 {
		threadAuthorizer = threadAuthorizers[0]
	}
	return &TaskService{tasks: tasks, threads: threads, workspaces: workspaces, approvals: approvals, queue: queue, gate: gate, audit: auditLogger, maxPrompt: maxPrompt, maxPendingWrites: maxPendingWrites, threadAuthorizer: threadAuthorizer}
}

func (s *TaskService) Start(ctx context.Context, workspaceID, prompt, threadID string) (domain.Task, error) {
	if _, ok := security.Authorize(ctx); !ok {
		return domain.Task{}, domain.ErrNotAuthorized
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" || utf8.RuneCountInString(prompt) > s.maxPrompt {
		return domain.Task{}, domain.ErrInvalidInput
	}
	if threadID != "" && s.threadAuthorizer != nil {
		if err := s.threadAuthorizer.AuthorizeResume(ctx, workspaceID, threadID); err != nil {
			return domain.Task{}, err
		}
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
	if workspace.RootSessionOnly && threadID == "" {
		return domain.Task{}, domain.ErrNotAuthorized
	}
	if threadID != "" {
		thread, err := s.threads.GetThread(ctx, threadID)
		if err != nil || !threadAllowed(workspace, thread) || (thread.Source == domain.ThreadSourceCodexCLIProjectBound && s.threadAuthorizer == nil) {
			return domain.Task{}, domain.ErrNotAuthorized
		}
	}
	id, err := newID()
	if err != nil {
		return domain.Task{}, err
	}
	now := time.Now().UTC()
	hash := sha256.Sum256([]byte(prompt))
	task := domain.Task{ID: id, WorkspaceID: workspace.ID, ExecutionMode: domain.ExecutionReadOnly, ThreadID: threadID, Status: domain.TaskQueued, PromptHash: hex.EncodeToString(hash[:]), Progress: []domain.ProgressEvent{}, CreatedAt: now, UpdatedAt: now}
	if err := s.tasks.CreateTask(ctx, task); err != nil {
		return domain.Task{}, err
	}
	if s.audit != nil {
		if err := s.audit.Record(ctx, audit.Event{Operation: "task.queued", Principal: "mcp-bearer", WorkspaceID: workspace.ID, TaskID: id, Outcome: "requested"}); err != nil {
			_ = s.tasks.SetTaskStatus(context.Background(), id, domain.TaskFailed, "Task was not started because audit persistence failed")
			return domain.Task{}, domain.ErrServiceUnavailable
		}
	}
	if err := s.queue.Submit(ctx, ExecutionJob{TaskID: id, Workspace: workspace, Prompt: prompt, ThreadID: threadID, ExecutionMode: domain.ExecutionReadOnly}); err != nil {
		_ = s.tasks.SetTaskStatus(context.Background(), id, domain.TaskFailed, "Task could not be queued")
		return domain.Task{}, err
	}
	stored, err := s.tasks.GetTask(ctx, id)
	return stored, err
}

func (s *TaskService) StartWrite(ctx context.Context, workspaceID, prompt, threadID string) (domain.Task, domain.Approval, error) {
	if _, ok := security.Authorize(ctx); !ok {
		return domain.Task{}, domain.Approval{}, domain.ErrNotAuthorized
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" || utf8.RuneCountInString(prompt) > s.maxPrompt {
		return domain.Task{}, domain.Approval{}, domain.ErrInvalidInput
	}
	if s.approvals == nil || s.gate == nil {
		return domain.Task{}, domain.Approval{}, domain.ErrServiceUnavailable
	}
	if threadID != "" && s.threadAuthorizer != nil {
		if err := s.threadAuthorizer.AuthorizeResume(ctx, workspaceID, threadID); err != nil {
			return domain.Task{}, domain.Approval{}, err
		}
	}
	workspace, err := s.workspaces.Get(ctx, workspaceID)
	if err != nil {
		return domain.Task{}, domain.Approval{}, err
	}
	if err := s.workspaces.ValidateWriteScope(ctx, workspace); err != nil {
		return domain.Task{}, domain.Approval{}, err
	}
	if !workspace.WriteEnabled {
		return domain.Task{}, domain.Approval{}, domain.ErrNotAuthorized
	}
	if err := s.gate.AuthorizeExecution(ctx, workspace); err != nil {
		return domain.Task{}, domain.Approval{}, err
	}
	if threadID != "" {
		thread, err := s.threads.GetThread(ctx, threadID)
		if err != nil || !threadAllowed(workspace, thread) || (thread.Source == domain.ThreadSourceCodexCLIProjectBound && s.threadAuthorizer == nil) {
			return domain.Task{}, domain.Approval{}, domain.ErrNotAuthorized
		}
	}
	id, err := newID()
	if err != nil {
		return domain.Task{}, domain.Approval{}, err
	}
	now := time.Now().UTC()
	hash := sha256.Sum256([]byte(prompt))
	task := domain.Task{ID: id, WorkspaceID: workspace.ID, ExecutionMode: domain.ExecutionWorkspaceWrite, ThreadID: threadID, Status: domain.TaskWaitingApproval, PromptHash: hex.EncodeToString(hash[:]), Progress: []domain.ProgressEvent{}, CreatedAt: now, UpdatedAt: now}
	approval, err := s.approvals.CreateWriteTaskApproval(ctx, task, prompt, s.maxPendingWrites)
	if err != nil {
		return domain.Task{}, domain.Approval{}, err
	}
	if s.audit != nil {
		if err := s.audit.Record(ctx, audit.Event{Operation: "task.write_requested", Principal: "mcp-bearer", WorkspaceID: workspace.ID, TaskID: id, Outcome: "waiting_approval"}); err != nil {
			_ = s.approvals.RejectLocal(ctx, approval.ID, ApprovalActor{Channel: "local", Subject: "bridge"})
			return domain.Task{}, domain.Approval{}, domain.ErrServiceUnavailable
		}
	}
	stored, err := s.tasks.GetTask(ctx, id)
	return stored, approval, err
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
	workspace, err := s.workspaces.Get(ctx, workspaceID)
	if err != nil {
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
			if thread.Source == domain.ThreadSourceCodexCLIProjectBound {
				if s.threadAuthorizer == nil || s.threadAuthorizer.AuthorizeResume(ctx, workspaceID, thread.ID) != nil {
					continue
				}
			} else if thread.Source != domain.ThreadSourceCodexCLI && !(thread.Source == domain.ThreadSourceCodexCLITrusted && workspace.SessionTrusted) && !(thread.Source == domain.ThreadSourceCodexCLIRoot && workspace.TrustedRootID == thread.TrustedRootID && thread.TrustedRootID != "") {
				continue
			}
			status := "attached"
			if thread.Source == domain.ThreadSourceCodexCLITrusted {
				status = "trusted"
			}
			result = append(result, ThreadSummary{ThreadID: thread.ID, Status: status, Source: thread.Source, UpdatedAt: thread.UpdatedAt.UnixMilli()})
			continue
		}
		result = append(result, ThreadSummary{ThreadID: thread.ID, TaskID: tasks[0].ID, Status: string(tasks[0].Status), Source: thread.Source, UpdatedAt: thread.UpdatedAt.UnixMilli()})
	}
	return result, nil
}

func threadAllowed(workspace domain.Workspace, thread domain.Thread) bool {
	if thread.WorkspaceID != workspace.ID {
		return false
	}
	switch thread.Source {
	case domain.ThreadSourceBridge:
		return true
	case domain.ThreadSourceCodexCLI:
		return thread.SessionPath == workspace.RootPath
	case domain.ThreadSourceCodexCLITrusted:
		return workspace.SessionTrusted && thread.SessionPath == workspace.RootPath
	case domain.ThreadSourceCodexCLIRoot:
		return thread.TrustedRootID != "" && sessionPathWithinWorkspace(thread.SessionPath, workspace.RootPath)
	case domain.ThreadSourceCodexCLIProjectBound:
		return thread.TrustedRootID != ""
	default:
		return false
	}
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
	Status    string
	Source    string
	UpdatedAt int64
}

func IsNotFound(err error) bool { return errors.Is(err, domain.ErrNotFound) }
