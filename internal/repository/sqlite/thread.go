package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"example.com/codex-chatgpt-bridge/internal/domain"
)

func (s *Store) SaveThread(ctx context.Context, thread domain.Thread) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO threads(id, workspace_id, created_at, title) VALUES(?,?,?,?) ON CONFLICT(id) DO NOTHING`,
		thread.ID, thread.WorkspaceID, thread.CreatedAt.UnixMilli(), "")
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
	rows, err := s.db.QueryContext(ctx, `SELECT th.id, th.workspace_id, th.created_at, COALESCE(MAX(t.updated_at), th.created_at)
		FROM threads th LEFT JOIN tasks t ON t.thread_id=th.id AND t.workspace_id=th.workspace_id
		WHERE th.workspace_id=? GROUP BY th.id ORDER BY MAX(t.updated_at) DESC LIMIT 500`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var threads []domain.Thread
	for rows.Next() {
		var th domain.Thread
		var created, updated int64
		if err := rows.Scan(&th.ID, &th.WorkspaceID, &created, &updated); err != nil {
			return nil, err
		}
		th.CreatedAt, th.UpdatedAt = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
		threads = append(threads, th)
	}
	return threads, rows.Err()
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
