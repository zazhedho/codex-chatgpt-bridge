package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/zazhedho/codex-chatgpt-bridge/internal/domain"
)

const maxPendingSessionProjectBindings = 20

func (s *Store) CreateSessionProjectBinding(ctx context.Context, binding domain.SessionProjectBinding) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE session_project_bindings SET status='expired', decided_at=? WHERE status='pending' AND expires_at<=?`, binding.CreatedAt.UnixMilli(), binding.CreatedAt.UnixMilli()); err != nil {
		return err
	}
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_project_bindings WHERE status='pending'`).Scan(&pending); err != nil {
		return err
	}
	if pending >= maxPendingSessionProjectBindings {
		return domain.ErrConflict
	}
	var attached int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM threads WHERE id=?`, binding.ThreadID).Scan(&attached); err != nil {
		return err
	}
	if attached != 0 {
		return domain.ErrConflict
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO session_project_bindings
		(id, thread_id, project_id, trusted_root_id, trusted_root_path, root_device, root_inode,
		 session_path, session_device, session_inode, session_created_at, session_updated_at,
		 project_path, project_device, project_inode, status, requested_by, created_at, expires_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, binding.ID, binding.ThreadID, binding.ProjectID, binding.TrustedRootID,
		binding.TrustedRootPath, binding.RootDevice, binding.RootInode, binding.SessionPath, binding.SessionDevice, binding.SessionInode,
		binding.SessionCreatedAt.UnixMilli(), binding.SessionUpdatedAt.UnixMilli(), binding.ProjectPath, binding.ProjectDevice,
		binding.ProjectInode, binding.Status, binding.RequestedBy, binding.CreatedAt.UnixMilli(), binding.ExpiresAt.UnixMilli())
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.secureFiles()
}

func (s *Store) GetSessionProjectBinding(ctx context.Context, id string) (domain.SessionProjectBinding, error) {
	return scanSessionProjectBinding(s.db.QueryRowContext(ctx, sessionProjectBindingSelect+` WHERE id=?`, id))
}

func (s *Store) GetApprovedSessionProjectBinding(ctx context.Context, threadID string) (domain.SessionProjectBinding, error) {
	return scanSessionProjectBinding(s.db.QueryRowContext(ctx, sessionProjectBindingSelect+` WHERE thread_id=? AND status='approved' ORDER BY created_at DESC LIMIT 1`, threadID))
}

func (s *Store) ListPendingSessionProjectBindings(ctx context.Context, now time.Time) ([]domain.SessionProjectBinding, error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE session_project_bindings SET status='expired', decided_at=? WHERE status='pending' AND expires_at<=?`, now.UnixMilli(), now.UnixMilli()); err != nil {
		return nil, err
	}
	return listSessionProjectBindings(ctx, s.db, ` WHERE status='pending' AND expires_at>? ORDER BY created_at LIMIT ?`, now.UnixMilli(), maxPendingSessionProjectBindings)
}

func (s *Store) ListApprovedSessionProjectBindings(ctx context.Context) ([]domain.SessionProjectBinding, error) {
	return listSessionProjectBindings(ctx, s.db, ` WHERE status='approved' ORDER BY decided_at DESC LIMIT ?`, 200)
}

func (s *Store) ApproveSessionProjectBinding(ctx context.Context, id, actor string, workspace domain.Workspace, thread domain.Thread, now time.Time) (domain.Workspace, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Workspace{}, err
	}
	defer tx.Rollback()
	binding, err := scanSessionProjectBinding(tx.QueryRowContext(ctx, sessionProjectBindingSelect+` WHERE id=?`, id))
	if err != nil {
		return domain.Workspace{}, err
	}
	if binding.Status != domain.SessionProjectBindingPending || !now.Before(binding.ExpiresAt) {
		if binding.Status == domain.SessionProjectBindingPending {
			_, _ = tx.ExecContext(ctx, `UPDATE session_project_bindings SET status='expired', decided_at=? WHERE id=? AND status='pending'`, now.UnixMilli(), id)
		}
		return domain.Workspace{}, domain.ErrConflict
	}
	if actor == "" || thread.ID != binding.ThreadID || thread.WorkspaceID != workspace.ID || thread.Source != domain.ThreadSourceCodexCLIProjectBound ||
		thread.SessionPath != binding.SessionPath || thread.TrustedRootID != binding.TrustedRootID || workspace.RootPath != binding.ProjectPath ||
		workspace.RootDevice != binding.ProjectDevice || workspace.RootInode != binding.ProjectInode {
		return domain.Workspace{}, domain.ErrNotAuthorized
	}
	var rootPath, rootStatus string
	var rootDevice, rootInode uint64
	if err := tx.QueryRowContext(ctx, `SELECT root_path, root_device, root_inode, status FROM trusted_workspace_roots WHERE id=?`, binding.TrustedRootID).Scan(&rootPath, &rootDevice, &rootInode, &rootStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Workspace{}, domain.ErrNotFound
		}
		return domain.Workspace{}, err
	}
	if rootStatus != domain.TrustedRootApproved || rootPath != binding.TrustedRootPath || rootDevice != binding.RootDevice || rootInode != binding.RootInode {
		return domain.Workspace{}, domain.ErrNotAuthorized
	}
	var dbWorkspace domain.Workspace
	var created int64
	insertedWorkspace := false
	err = tx.QueryRowContext(ctx, `SELECT id, root_path, root_device, root_inode, created_at, git_repository, write_enabled, session_trusted, COALESCE(trusted_root_id,''), root_session_only FROM workspaces WHERE root_path=?`, workspace.RootPath).
		Scan(&dbWorkspace.ID, &dbWorkspace.RootPath, &dbWorkspace.RootDevice, &dbWorkspace.RootInode, &created, &dbWorkspace.GitRepository, &dbWorkspace.WriteEnabled, &dbWorkspace.SessionTrusted, &dbWorkspace.TrustedRootID, &dbWorkspace.RootSessionOnly)
	if errors.Is(err, sql.ErrNoRows) {
		if workspace.ID == "" || workspace.RootPath != binding.ProjectPath || workspace.RootDevice != binding.ProjectDevice || workspace.RootInode != binding.ProjectInode || workspace.TrustedRootID != binding.TrustedRootID {
			return domain.Workspace{}, domain.ErrNotAuthorized
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO workspaces(id, root_path, root_device, root_inode, created_at, git_repository, write_enabled, session_trusted, trusted_root_id, root_session_only) VALUES(?,?,?,?,?,?,0,0,?,1)`,
			workspace.ID, workspace.RootPath, workspace.RootDevice, workspace.RootInode, now.UnixMilli(), workspace.GitRepository, binding.TrustedRootID); err != nil {
			return domain.Workspace{}, fmt.Errorf("create read-only bound project workspace: %w", err)
		}
		dbWorkspace = workspace
		dbWorkspace.CreatedAt = now
		insertedWorkspace = true
		err = nil
	}
	if err != nil {
		return domain.Workspace{}, err
	}
	if !insertedWorkspace {
		dbWorkspace.CreatedAt = time.UnixMilli(created).UTC()
	}
	if dbWorkspace.RootPath != binding.ProjectPath || dbWorkspace.RootDevice != binding.ProjectDevice || dbWorkspace.RootInode != binding.ProjectInode ||
		(dbWorkspace.TrustedRootID != "" && dbWorkspace.TrustedRootID != binding.TrustedRootID) {
		return domain.Workspace{}, domain.ErrNotAuthorized
	}
	result, err := tx.ExecContext(ctx, `UPDATE session_project_bindings SET status='approved', workspace_id=?, approved_by=?, decided_at=? WHERE id=? AND status='pending' AND expires_at>?`, dbWorkspace.ID, actor, now.UnixMilli(), id, now.UnixMilli())
	if err != nil {
		return domain.Workspace{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return domain.Workspace{}, err
	}
	if changed != 1 {
		return domain.Workspace{}, domain.ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO threads(id, workspace_id, created_at, title, source, session_path, updated_at, trusted_root_id) VALUES(?,?,?,'',?,?,?,?)`,
		thread.ID, dbWorkspace.ID, thread.CreatedAt.UnixMilli(), thread.Source, thread.SessionPath, thread.UpdatedAt.UnixMilli(), thread.TrustedRootID); err != nil {
		return domain.Workspace{}, fmt.Errorf("bind Codex CLI thread to selected project: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.Workspace{}, err
	}
	if err := s.secureFiles(); err != nil {
		return domain.Workspace{}, err
	}
	return dbWorkspace, nil
}

func (s *Store) RejectSessionProjectBinding(ctx context.Context, id, actor string, now time.Time) error {
	if actor == "" {
		return domain.ErrNotAuthorized
	}
	result, err := s.db.ExecContext(ctx, `UPDATE session_project_bindings SET status='rejected', decided_at=? WHERE id=? AND status='pending' AND expires_at>?`, now.UnixMilli(), id, now.UnixMilli())
	if err != nil {
		return err
	}
	return requireOneChanged(result)
}

func (s *Store) RevokeSessionProjectBinding(ctx context.Context, threadID, actor string, now time.Time) error {
	if actor == "" {
		return domain.ErrNotAuthorized
	}
	result, err := s.db.ExecContext(ctx, `UPDATE session_project_bindings SET status='revoked', decided_at=? WHERE thread_id=? AND status='approved'`, now.UnixMilli(), threadID)
	if err != nil {
		return err
	}
	if err := requireOneChanged(result); err != nil {
		return err
	}
	return s.secureFiles()
}

const sessionProjectBindingSelect = `SELECT id, thread_id, project_id, workspace_id, trusted_root_id, trusted_root_path, root_device, root_inode,
	session_path, session_device, session_inode, session_created_at, session_updated_at, project_path, project_device, project_inode,
	status, requested_by, COALESCE(approved_by,''), created_at, expires_at, COALESCE(decided_at,0) FROM session_project_bindings`

type sessionProjectBindingQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func listSessionProjectBindings(ctx context.Context, queryer sessionProjectBindingQueryer, suffix string, args ...any) ([]domain.SessionProjectBinding, error) {
	rows, err := queryer.QueryContext(ctx, sessionProjectBindingSelect+suffix, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var bindings []domain.SessionProjectBinding
	for rows.Next() {
		binding, err := scanSessionProjectBinding(rows)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, binding)
	}
	return bindings, rows.Err()
}

type sessionProjectBindingScanner interface{ Scan(...any) error }

func scanSessionProjectBinding(scanner sessionProjectBindingScanner) (domain.SessionProjectBinding, error) {
	var binding domain.SessionProjectBinding
	var sessionCreated, sessionUpdated, created, expires, decided int64
	err := scanner.Scan(&binding.ID, &binding.ThreadID, &binding.ProjectID, &binding.WorkspaceID, &binding.TrustedRootID, &binding.TrustedRootPath,
		&binding.RootDevice, &binding.RootInode, &binding.SessionPath, &binding.SessionDevice, &binding.SessionInode, &sessionCreated,
		&sessionUpdated, &binding.ProjectPath, &binding.ProjectDevice, &binding.ProjectInode, &binding.Status, &binding.RequestedBy,
		&binding.ApprovedBy, &created, &expires, &decided)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.SessionProjectBinding{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.SessionProjectBinding{}, err
	}
	binding.SessionCreatedAt, binding.SessionUpdatedAt = time.UnixMilli(sessionCreated).UTC(), time.UnixMilli(sessionUpdated).UTC()
	binding.CreatedAt, binding.ExpiresAt = time.UnixMilli(created).UTC(), time.UnixMilli(expires).UTC()
	if decided > 0 {
		binding.DecidedAt = time.UnixMilli(decided).UTC()
	}
	return binding, nil
}

func requireOneChanged(result sql.Result) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return domain.ErrConflict
	}
	return nil
}
