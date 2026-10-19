package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"example.com/codex-chatgpt-bridge/internal/domain"
	modernsqlite "modernc.org/sqlite"
)

func (s *Store) ListWorkspaces(ctx context.Context) ([]domain.Workspace, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, root_path, root_device, root_inode, created_at, git_repository FROM workspaces ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.Workspace
	for rows.Next() {
		var w domain.Workspace
		var created int64
		if err := rows.Scan(&w.ID, &w.RootPath, &w.RootDevice, &w.RootInode, &created, &w.GitRepository); err != nil {
			return nil, err
		}
		w.CreatedAt = time.UnixMilli(created).UTC()
		result = append(result, w)
	}
	return result, rows.Err()
}

func (s *Store) GetWorkspace(ctx context.Context, id string) (domain.Workspace, error) {
	var w domain.Workspace
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT id, root_path, root_device, root_inode, created_at, git_repository FROM workspaces WHERE id = ?`, id).
		Scan(&w.ID, &w.RootPath, &w.RootDevice, &w.RootInode, &created, &w.GitRepository)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Workspace{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Workspace{}, err
	}
	w.CreatedAt = time.UnixMilli(created).UTC()
	return w, nil
}

func (s *Store) InsertWorkspaceRequest(ctx context.Context, request domain.WorkspaceRequest) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO workspace_requests(id, workspace_id, root_path, status, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		request.ID, request.WorkspaceID, request.RootPath, request.Status, request.CreatedAt.UnixMilli(), request.ExpiresAt.UnixMilli())
	if err != nil {
		var sqliteError *modernsqlite.Error
		if errors.As(err, &sqliteError) && sqliteError.Code()&0xff == 19 {
			return domain.ErrConflict
		}
		return fmt.Errorf("insert workspace request: %w", err)
	}
	return s.secureFiles()
}

func (s *Store) ListPendingWorkspaceRequests(ctx context.Context, now time.Time) ([]domain.WorkspaceRequest, error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE workspace_requests SET status='expired' WHERE status='pending' AND expires_at <= ?`, now.UnixMilli()); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, workspace_id, root_path, status, created_at, expires_at FROM workspace_requests WHERE status='pending' ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var requests []domain.WorkspaceRequest
	for rows.Next() {
		var r domain.WorkspaceRequest
		var created, expires int64
		if err := rows.Scan(&r.ID, &r.WorkspaceID, &r.RootPath, &r.Status, &created, &expires); err != nil {
			return nil, err
		}
		r.CreatedAt, r.ExpiresAt = time.UnixMilli(created).UTC(), time.UnixMilli(expires).UTC()
		requests = append(requests, r)
	}
	return requests, rows.Err()
}

func (s *Store) GetWorkspaceRequest(ctx context.Context, id string) (domain.WorkspaceRequest, error) {
	var r domain.WorkspaceRequest
	var created, expires int64
	err := s.db.QueryRowContext(ctx, `SELECT id, workspace_id, root_path, status, created_at, expires_at FROM workspace_requests WHERE id=?`, id).
		Scan(&r.ID, &r.WorkspaceID, &r.RootPath, &r.Status, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WorkspaceRequest{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.WorkspaceRequest{}, err
	}
	r.CreatedAt, r.ExpiresAt = time.UnixMilli(created).UTC(), time.UnixMilli(expires).UTC()
	return r, nil
}

func (s *Store) ApproveWorkspaceRequest(ctx context.Context, requestID string, workspace domain.Workspace) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	var expires int64
	err = tx.QueryRowContext(ctx, `SELECT status, expires_at FROM workspace_requests WHERE id=?`, requestID).Scan(&status, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return err
	}
	if status != "pending" || expires <= time.Now().UnixMilli() {
		return domain.ErrConflict
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO workspaces(id, root_path, root_device, root_inode, created_at, git_repository) VALUES (?, ?, ?, ?, ?, ?)`,
		workspace.ID, workspace.RootPath, workspace.RootDevice, workspace.RootInode, workspace.CreatedAt.UnixMilli(), workspace.GitRepository)
	if err != nil {
		return fmt.Errorf("register workspace: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE workspace_requests SET status='approved' WHERE id=? AND status='pending'`, requestID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return domain.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.secureFiles()
}

func (s *Store) SetWorkspaceRequestStatus(ctx context.Context, id, status string) error {
	if status != "rejected" && status != "expired" {
		return domain.ErrInvalidInput
	}
	result, err := s.db.ExecContext(ctx, `UPDATE workspace_requests SET status=? WHERE id=? AND status='pending'`, status, id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return domain.ErrConflict
	}
	return s.secureFiles()
}
