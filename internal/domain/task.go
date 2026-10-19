package domain

import "time"

type TaskStatus string

const (
	TaskQueued          TaskStatus = "queued"
	TaskRunning         TaskStatus = "running"
	TaskWaitingApproval TaskStatus = "waiting_approval"
	TaskSucceeded       TaskStatus = "completed"
	TaskFailed          TaskStatus = "failed"
	TaskCancelled       TaskStatus = "cancelled"
	TaskInterrupted     TaskStatus = "interrupted"
)

type ProgressEvent struct {
	At   int64  `json:"at"`
	Text string `json:"text"`
}

type Task struct {
	ID            string
	WorkspaceID   string
	ThreadID      string
	TurnID        string
	Status        TaskStatus
	PromptHash    string
	Progress      []ProgressEvent
	Result        *string
	Diff          *string
	Error         *string
	OutputExpired bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func (t *Task) Transition(next TaskStatus) error {
	allowed := false
	switch t.Status {
	case TaskQueued:
		allowed = next == TaskRunning || next == TaskFailed || next == TaskCancelled || next == TaskInterrupted
	case TaskRunning:
		allowed = next == TaskWaitingApproval || next == TaskSucceeded || next == TaskFailed || next == TaskCancelled || next == TaskInterrupted
	case TaskWaitingApproval:
		allowed = next == TaskRunning || next == TaskFailed || next == TaskCancelled || next == TaskInterrupted
	}
	if !allowed {
		return ErrInvalidTaskTransition
	}
	t.Status = next
	t.UpdatedAt = time.Now().UTC()
	return nil
}

func (s TaskStatus) Terminal() bool {
	return s == TaskSucceeded || s == TaskFailed || s == TaskCancelled || s == TaskInterrupted
}
