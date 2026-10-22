package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/zazhedho/codex-chatgpt-bridge/internal/domain"
)

const maxPendingSessionAttachments = 20

func (s *Store) CreateSessionAttachRequest(ctx context.Context, request domain.SessionAttachRequest) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE session_attach_requests SET status='expired', decided_at=? WHERE status='pending' AND expires_at<=?`, request.CreatedAt.UnixMilli(), request.CreatedAt.UnixMilli()); err != nil {
		return err
	}
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_attach_requests WHERE status='pending'`).Scan(&pending); err != nil {
		return err
	}
	if pending >= maxPendingSessionAttachments {
		return domain.ErrConflict
	}
	var attached int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM threads WHERE id=?`, request.ThreadID).Scan(&attached); err != nil {
		return err
	}
	if attached != 0 {
		return domain.ErrConflict
	}
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO session_attach_requests
		(id, thread_id, workspace_id, workspace_path, root_device, root_inode, session_updated_at, status, created_at, expires_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, request.ID, request.ThreadID, request.WorkspaceID, request.WorkspacePath,
		request.RootDevice, request.RootInode, request.SessionUpdatedAt.UnixMilli(), request.Status,
		request.CreatedAt.UnixMilli(), request.ExpiresAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("create session attach request: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if inserted != 1 {
		return domain.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.secureFiles()
}

func (s *Store) GetSessionAttachRequest(ctx context.Context, requestID string) (domain.SessionAttachRequest, error) {
	return scanSessionAttachRequest(s.db.QueryRowContext(ctx, `SELECT id, thread_id, workspace_id, workspace_path, root_device, root_inode,
		session_updated_at, status, created_at, expires_at, COALESCE(decided_at, 0), COALESCE(approved_by, '')
		FROM session_attach_requests WHERE id=?`, requestID))
}

func (s *Store) ExpireSessionAttachRequests(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE session_attach_requests SET status='expired', decided_at=? WHERE status='pending' AND expires_at<=?`, now.UnixMilli(), now.UnixMilli())
	return err
}

func (s *Store) ListPendingSessionAttachRequests(ctx context.Context, now time.Time) ([]domain.SessionAttachRequest, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, thread_id, workspace_id, workspace_path, root_device, root_inode,
		session_updated_at, status, created_at, expires_at, COALESCE(decided_at, 0), COALESCE(approved_by, '')
		FROM session_attach_requests WHERE status='pending' AND expires_at>? ORDER BY created_at LIMIT ?`, now.UnixMilli(), maxPendingSessionAttachments)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var requests []domain.SessionAttachRequest
	for rows.Next() {
		request, err := scanSessionAttachRequest(rows)
		if err != nil {
			return nil, err
		}
		requests = append(requests, request)
	}
	return requests, rows.Err()
}

func (s *Store) ApproveSessionAttachRequest(ctx context.Context, requestID, actor string, thread domain.Thread, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	request, err := scanSessionAttachRequest(tx.QueryRowContext(ctx, `SELECT id, thread_id, workspace_id, workspace_path, root_device, root_inode,
		session_updated_at, status, created_at, expires_at, COALESCE(decided_at, 0), COALESCE(approved_by, '')
		FROM session_attach_requests WHERE id=?`, requestID))
	if err != nil {
		return err
	}
	if request.Status != domain.SessionAttachPending || !now.Before(request.ExpiresAt) {
		if request.Status == domain.SessionAttachPending {
			_, _ = tx.ExecContext(ctx, `UPDATE session_attach_requests SET status='expired', decided_at=? WHERE id=? AND status='pending'`, now.UnixMilli(), requestID)
		}
		return domain.ErrConflict
	}
	if actor == "" || thread.ID != request.ThreadID || thread.WorkspaceID != request.WorkspaceID || thread.Source != domain.ThreadSourceCodexCLI || thread.SessionPath != request.WorkspacePath || thread.UpdatedAt.UnixMilli() != request.SessionUpdatedAt.UnixMilli() {
		return domain.ErrNotAuthorized
	}
	var rootPath string
	var rootDevice, rootInode uint64
	if err := tx.QueryRowContext(ctx, `SELECT root_path, root_device, root_inode FROM workspaces WHERE id=?`, request.WorkspaceID).Scan(&rootPath, &rootDevice, &rootInode); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		}
		return err
	}
	if rootPath != request.WorkspacePath || rootDevice != request.RootDevice || rootInode != request.RootInode {
		return domain.ErrNotAuthorized
	}
	result, err := tx.ExecContext(ctx, `UPDATE session_attach_requests SET status='approved', approved_by=?, decided_at=? WHERE id=? AND status='pending' AND expires_at>?`, actor, now.UnixMilli(), requestID, now.UnixMilli())
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return domain.ErrConflict
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO threads(id, workspace_id, created_at, title, source, session_path, updated_at)
		VALUES(?,?,?,'',?,?,?)`, thread.ID, thread.WorkspaceID, thread.CreatedAt.UnixMilli(), thread.Source, thread.SessionPath, thread.UpdatedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("attach Codex thread: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.secureFiles()
}

func (s *Store) RejectSessionAttachRequest(ctx context.Context, requestID, actor string, now time.Time) error {
	if actor == "" {
		return domain.ErrNotAuthorized
	}
	result, err := s.db.ExecContext(ctx, `UPDATE session_attach_requests SET status='rejected', decided_at=? WHERE id=? AND status='pending' AND expires_at>?`, now.UnixMilli(), requestID, now.UnixMilli())
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return domain.ErrConflict
	}
	return s.secureFiles()
}

type sessionAttachScanner interface{ Scan(...any) error }

func scanSessionAttachRequest(scanner sessionAttachScanner) (domain.SessionAttachRequest, error) {
	var request domain.SessionAttachRequest
	var device, inode uint64
	var sessionUpdatedAt, created, expires, decided int64
	err := scanner.Scan(&request.ID, &request.ThreadID, &request.WorkspaceID, &request.WorkspacePath, &device, &inode,
		&sessionUpdatedAt, &request.Status, &created, &expires, &decided, &request.ApprovedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.SessionAttachRequest{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.SessionAttachRequest{}, err
	}
	request.RootDevice, request.RootInode = device, inode
	request.SessionUpdatedAt = time.UnixMilli(sessionUpdatedAt).UTC()
	request.CreatedAt, request.ExpiresAt = time.UnixMilli(created).UTC(), time.UnixMilli(expires).UTC()
	if decided > 0 {
		request.DecidedAt = time.UnixMilli(decided).UTC()
	}
	return request, nil
}
