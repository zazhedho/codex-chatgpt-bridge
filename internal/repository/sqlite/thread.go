package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/zazhedho/codex-chatgpt-bridge/internal/domain"
)

func (s *Store) SaveThread(ctx context.Context, thread domain.Thread) error {
	createdAt := thread.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	updatedAt := thread.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}
	source := thread.Source
	if source == "" {
		source = domain.ThreadSourceBridge
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO threads(id, workspace_id, created_at, title, source, session_path, updated_at, trusted_root_id)
		VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET updated_at=MAX(threads.updated_at, excluded.updated_at)
		WHERE threads.workspace_id=excluded.workspace_id`,
		thread.ID, thread.WorkspaceID, createdAt.UnixMilli(), "", source, thread.SessionPath, updatedAt.UnixMilli(), thread.TrustedRootID)
	if err != nil {
		return fmt.Errorf("save thread: %w", err)
	}
	var owner string
	if err := s.db.QueryRowContext(ctx, `SELECT workspace_id FROM threads WHERE id=?`, thread.ID).Scan(&owner); err != nil {
		return err
	}
	if owner != thread.WorkspaceID {
		return domain.ErrNotAuthorized
	}
	return s.secureFiles()
}

func (s *Store) ListThreads(ctx context.Context, workspaceID string) ([]domain.Thread, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT th.id, th.workspace_id, th.source, th.session_path, th.trusted_root_id, th.created_at,
		CASE WHEN th.source=? THEN th.updated_at ELSE COALESCE(MAX(t.updated_at), th.updated_at) END
		FROM threads th LEFT JOIN tasks t ON t.thread_id=th.id AND t.workspace_id=th.workspace_id
		WHERE th.workspace_id=? GROUP BY th.id ORDER BY 6 DESC LIMIT 500`, domain.ThreadSourceCodexCLI, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var threads []domain.Thread
	for rows.Next() {
		var th domain.Thread
		var created, updated int64
		if err := rows.Scan(&th.ID, &th.WorkspaceID, &th.Source, &th.SessionPath, &th.TrustedRootID, &created, &updated); err != nil {
			return nil, err
		}
		th.CreatedAt, th.UpdatedAt = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
		threads = append(threads, th)
	}
	return threads, rows.Err()
}

func (s *Store) GetThread(ctx context.Context, threadID string) (domain.Thread, error) {
	var thread domain.Thread
	var created, updated int64
	err := s.db.QueryRowContext(ctx, `SELECT id, workspace_id, source, session_path, trusted_root_id, created_at, updated_at FROM threads WHERE id=?`, threadID).
		Scan(&thread.ID, &thread.WorkspaceID, &thread.Source, &thread.SessionPath, &thread.TrustedRootID, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Thread{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Thread{}, err
	}
	thread.CreatedAt, thread.UpdatedAt = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
	return thread, nil
}

func (s *Store) EnsureThreadWorkspace(ctx context.Context, workspaceID, threadID string) error {
	var owner string
	err := s.db.QueryRowContext(ctx, `SELECT workspace_id FROM threads WHERE id=?`, threadID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return err
	}
	if owner != workspaceID {
		return domain.ErrNotAuthorized
	}
	return nil
}
