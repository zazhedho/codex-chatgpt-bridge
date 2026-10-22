package audit

import (
	"context"
)

type Event struct {
	Operation   string
	Principal   string
	WorkspaceID string
	TaskID      string
	ResourceID  string
	Outcome     string
	ErrorClass  string
}

type Writer interface {
	AppendAudit(context.Context, Event) error
}

type Logger struct{ writer Writer }

func New(writer Writer) *Logger { return &Logger{writer: writer} }

func (l *Logger) Record(ctx context.Context, event Event) error {
	if l == nil || l.writer == nil {
		return nil
	}
	return l.writer.AppendAudit(ctx, event)
}
