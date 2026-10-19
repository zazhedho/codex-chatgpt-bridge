package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"example.com/codex-chatgpt-bridge/internal/audit"
	"example.com/codex-chatgpt-bridge/internal/codex"
	"example.com/codex-chatgpt-bridge/internal/domain"
	"example.com/codex-chatgpt-bridge/internal/security"
	"example.com/codex-chatgpt-bridge/internal/service"
)

type Repository interface {
	SetTaskStatus(context.Context, string, domain.TaskStatus, string) error
	FinishTask(context.Context, string, domain.TaskStatus, string, string, string) error
	AppendProgress(context.Context, string, string, time.Time) error
	SetTaskThread(context.Context, string, string, string) error
	GetTask(context.Context, string) (domain.Task, error)
}

type Threads interface {
	SaveThread(context.Context, domain.Thread) error
}

type activeTask struct {
	cancel context.CancelFunc
	done   chan struct{}
	reason string
}

type Manager struct {
	mu            sync.Mutex
	queue         []service.ExecutionJob
	active        map[string]*activeTask
	maxConcurrent int
	maxQueued     int
	timeout       time.Duration
	maxOutput     int
	stopping      bool
	repo          Repository
	threads       Threads
	gateway       codex.Gateway
	audit         *audit.Logger
	logger        *slog.Logger
}

func New(repo Repository, threads Threads, gateway codex.Gateway, auditLogger *audit.Logger, logger *slog.Logger, maxConcurrent, maxQueued, timeoutSeconds, maxOutput int) *Manager {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	if maxQueued < 1 {
		maxQueued = 20
	}
	if timeoutSeconds < 1 {
		timeoutSeconds = 1200
	}
	if maxOutput < 1 {
		maxOutput = 64_000
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{repo: repo, threads: threads, gateway: gateway, audit: auditLogger, logger: logger, maxConcurrent: maxConcurrent, maxQueued: maxQueued, timeout: time.Duration(timeoutSeconds) * time.Second, maxOutput: maxOutput, active: make(map[string]*activeTask)}
}

func (m *Manager) Submit(ctx context.Context, job service.ExecutionJob) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopping {
		return domain.ErrServiceUnavailable
	}
	if len(m.queue) >= m.maxQueued {
		return domain.ErrQueueFull
	}
	m.queue = append(m.queue, job)
	m.pumpLocked()
	return nil
}

func (m *Manager) Cancel(ctx context.Context, id string) error {
	m.mu.Lock()
	for i, job := range m.queue {
		if job.TaskID == id {
			m.queue = append(m.queue[:i], m.queue[i+1:]...)
			m.mu.Unlock()
			return m.repo.SetTaskStatus(context.Background(), id, domain.TaskCancelled, "Cancelled before execution")
		}
	}
	active := m.active[id]
	if active == nil {
		m.mu.Unlock()
		return domain.ErrConflict
	}
	if active.reason == "" {
		active.reason = "cancelled"
	}
	cancel, done := active.cancel, active.done
	m.mu.Unlock()
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(4 * time.Second):
		return nil
	}
}

func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.stopping = true
	queued := m.queue
	m.queue = nil
	active := make([]*activeTask, 0, len(m.active))
	for _, task := range m.active {
		if task.reason == "" {
			task.reason = "shutdown"
		}
		active = append(active, task)
		task.cancel()
	}
	m.mu.Unlock()
	for _, job := range queued {
		_ = m.repo.SetTaskStatus(context.Background(), job.TaskID, domain.TaskInterrupted, "Bridge stopped before execution")
	}
	for _, task := range active {
		select {
		case <-task.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (m *Manager) Stats() (active, queued int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.active), len(m.queue)
}

func (m *Manager) pumpLocked() {
	for !m.stopping && len(m.active) < m.maxConcurrent && len(m.queue) > 0 {
		job := m.queue[0]
		m.queue = m.queue[1:]
		ctx, cancel := context.WithTimeout(context.Background(), m.timeout)
		task := &activeTask{cancel: cancel, done: make(chan struct{})}
		m.active[job.TaskID] = task
		go m.execute(ctx, job, task)
	}
}

func (m *Manager) execute(ctx context.Context, job service.ExecutionJob, active *activeTask) {
	defer func() {
		active.cancel()
		close(active.done)
		m.mu.Lock()
		delete(m.active, job.TaskID)
		m.pumpLocked()
		m.mu.Unlock()
	}()
	if err := m.repo.SetTaskStatus(context.Background(), job.TaskID, domain.TaskRunning, ""); err != nil {
		m.logger.Error("failed to mark task running", "task_id", job.TaskID)
		return
	}
	if m.audit != nil {
		_ = m.audit.Record(context.Background(), audit.Event{Operation: "task.started", Principal: "worker", WorkspaceID: job.Workspace.ID, TaskID: job.TaskID, Outcome: "success"})
	}
	result, err := m.gateway.Run(ctx, codex.TaskRequest{
		TaskID: job.TaskID, Workspace: job.Workspace, Prompt: job.Prompt, ThreadID: job.ThreadID,
		OnProgress: func(text string) {
			if saveErr := m.repo.AppendProgress(context.Background(), job.TaskID, security.RedactSecrets(text), time.Now().UTC()); saveErr != nil {
				m.logger.Warn("could not persist task progress", "task_id", job.TaskID)
			}
		},
		OnThread: func(threadID, turnID string) error {
			if err := m.threads.SaveThread(context.Background(), domain.Thread{ID: threadID, WorkspaceID: job.Workspace.ID, CreatedAt: time.Now().UTC()}); err != nil {
				return err
			}
			return m.repo.SetTaskThread(context.Background(), job.TaskID, threadID, turnID)
		},
	})
	if ctx.Err() != nil {
		m.finishInterrupted(ctx, job, active)
		return
	}
	if err != nil {
		message := safeError(err)
		_ = m.repo.FinishTask(context.Background(), job.TaskID, domain.TaskFailed, "", "", message)
		m.record(job, "task.failed")
		return
	}
	if result.Status == domain.TaskSucceeded {
		_ = m.repo.FinishTask(context.Background(), job.TaskID, domain.TaskSucceeded, boundSafe(result.FinalText, m.maxOutput), boundSafe(result.Diff, m.maxOutput), "")
		m.record(job, "task.completed")
		return
	}
	if result.Status != domain.TaskFailed && result.Status != domain.TaskInterrupted {
		result.Status = domain.TaskFailed
	}
	_ = m.repo.FinishTask(context.Background(), job.TaskID, result.Status, "", boundSafe(result.Diff, m.maxOutput), boundSafe(result.Error, 2_000))
	m.record(job, "task."+string(result.Status))
}

func (m *Manager) finishInterrupted(ctx context.Context, job service.ExecutionJob, active *activeTask) {
	m.mu.Lock()
	reason := active.reason
	m.mu.Unlock()
	status, message := domain.TaskFailed, "Task timed out"
	if reason == "cancelled" {
		status, message = domain.TaskCancelled, "Cancelled by request"
	}
	if reason == "shutdown" {
		status, message = domain.TaskInterrupted, "Bridge stopped while task was running"
	}
	if errors.Is(ctx.Err(), context.Canceled) && reason == "" {
		status, message = domain.TaskInterrupted, "Task interrupted"
	}
	_ = m.repo.FinishTask(context.Background(), job.TaskID, status, "", "", message)
	m.record(job, "task."+string(status))
}

func (m *Manager) record(job service.ExecutionJob, operation string) {
	if m.audit == nil {
		return
	}
	if err := m.audit.Record(context.Background(), audit.Event{Operation: operation, Principal: "worker", WorkspaceID: job.Workspace.ID, TaskID: job.TaskID, Outcome: "success"}); err != nil {
		m.logger.Error("task audit write failed", "task_id", job.TaskID)
	}
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	return security.RedactSecrets(err.Error())
}
func boundSafe(value string, max int) string {
	if value == "" {
		return ""
	}
	value = security.RedactSecrets(value)
	if len(value) > max {
		value = value[:max]
	}
	return value
}
