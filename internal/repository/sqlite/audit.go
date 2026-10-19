package sqlite

import (
	"context"
	"fmt"
	"time"

	"example.com/codex-chatgpt-bridge/internal/audit"
)

func (s *Store) AuditEventCount(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events`).Scan(&count)
	return count, err
}

func (s *Store) AppendAudit(ctx context.Context, event audit.Event) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit_events(occurred_at, operation, principal, workspace_id, task_id, outcome, error_class)
		VALUES(?,?,?,?,?,?,?)`, time.Now().UnixMilli(), event.Operation, event.Principal, event.WorkspaceID, event.TaskID, event.Outcome, event.ErrorClass)
	if err != nil {
		return fmt.Errorf("write audit event: %w", err)
	}
	return s.secureFiles()
}
