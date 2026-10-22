package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/zazhedho/codex-chatgpt-bridge/internal/domain"
	modernsqlite "modernc.org/sqlite"
)

func (s *Store) CreateTrustedRoot(ctx context.Context, root domain.TrustedWorkspaceRoot) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO trusted_workspace_roots(id, root_path, root_device, root_inode, status, created_at, expires_at)
		VALUES(?,?,?,?,?,?,?)`, root.ID, root.RootPath, root.RootDevice, root.RootInode, root.Status, root.CreatedAt.UnixMilli(), root.ExpiresAt.UnixMilli())
	if err != nil {
		var sqliteError *modernsqlite.Error
		if errors.As(err, &sqliteError) && sqliteError.Code()&0xff == 19 {
			return domain.ErrConflict
		}
		return err
	}
	return s.secureFiles()
}

func (s *Store) GetTrustedRoot(ctx context.Context, id string) (domain.TrustedWorkspaceRoot, error) {
	var root domain.TrustedWorkspaceRoot
	var created, expires, decided sql.NullInt64
	var approvedBy sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id, root_path, root_device, root_inode, status, created_at, expires_at, decided_at, approved_by
		FROM trusted_workspace_roots WHERE id=?`, id).Scan(&root.ID, &root.RootPath, &root.RootDevice, &root.RootInode, &root.Status, &created, &expires, &decided, &approvedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.TrustedWorkspaceRoot{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.TrustedWorkspaceRoot{}, err
	}
	root.CreatedAt = time.UnixMilli(created.Int64).UTC()
	root.ExpiresAt = time.UnixMilli(expires.Int64).UTC()
	root.ApprovedBy = approvedBy.String
	if decided.Valid {
		root.DecidedAt = time.UnixMilli(decided.Int64).UTC()
	}
	return root, nil
}

func (s *Store) ListTrustedRoots(ctx context.Context, now time.Time) ([]domain.TrustedWorkspaceRoot, error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE trusted_workspace_roots SET status='expired' WHERE status='pending' AND expires_at<=?`, now.UnixMilli()); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, root_path, root_device, root_inode, status, created_at, expires_at, decided_at, approved_by
		FROM trusted_workspace_roots ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var roots []domain.TrustedWorkspaceRoot
	for rows.Next() {
		var root domain.TrustedWorkspaceRoot
		var created, expires, decided sql.NullInt64
		var approvedBy sql.NullString
		if err := rows.Scan(&root.ID, &root.RootPath, &root.RootDevice, &root.RootInode, &root.Status, &created, &expires, &decided, &approvedBy); err != nil {
			return nil, err
		}
		root.CreatedAt, root.ExpiresAt = time.UnixMilli(created.Int64).UTC(), time.UnixMilli(expires.Int64).UTC()
		root.ApprovedBy = approvedBy.String
		if decided.Valid {
			root.DecidedAt = time.UnixMilli(decided.Int64).UTC()
		}
		roots = append(roots, root)
	}
	return roots, rows.Err()
}

func (s *Store) ApproveTrustedRoot(ctx context.Context, expected domain.TrustedWorkspaceRoot, actor string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var root domain.TrustedWorkspaceRoot
	var created, expires int64
	err = tx.QueryRowContext(ctx, `SELECT id, root_path, root_device, root_inode, status, created_at, expires_at
		FROM trusted_workspace_roots WHERE id=?`, expected.ID).Scan(&root.ID, &root.RootPath, &root.RootDevice, &root.RootInode, &root.Status, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return err
	}
	root.CreatedAt, root.ExpiresAt = time.UnixMilli(created).UTC(), time.UnixMilli(expires).UTC()
	if root.Status != domain.TrustedRootPending || !now.Before(root.ExpiresAt) {
		if root.Status == domain.TrustedRootPending {
			if _, err := tx.ExecContext(ctx, `UPDATE trusted_workspace_roots SET status='expired' WHERE id=? AND status='pending'`, root.ID); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
		}
		return domain.ErrConflict
	}
	if root.RootPath != expected.RootPath || root.RootDevice != expected.RootDevice || root.RootInode != expected.RootInode {
		return domain.ErrConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE trusted_workspace_roots SET status='approved', decided_at=?, approved_by=? WHERE id=? AND status='pending' AND expires_at>?`, now.UnixMilli(), actor, root.ID, now.UnixMilli())
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return domain.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.secureFiles()
}

func (s *Store) SetTrustedRootStatus(ctx context.Context, id, expected, status string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE trusted_workspace_roots SET status=?, decided_at=? WHERE id=? AND status=?`, status, now.UnixMilli(), id, expected)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return domain.ErrConflict
	}
	return s.secureFiles()
}

func (s *Store) EnsureTrustedWorkspace(ctx context.Context, workspace domain.Workspace) (domain.Workspace, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Workspace{}, err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM trusted_workspace_roots WHERE id=?`, workspace.TrustedRootID).Scan(&status); errors.Is(err, sql.ErrNoRows) {
		return domain.Workspace{}, domain.ErrNotFound
	} else if err != nil {
		return domain.Workspace{}, err
	}
	if status != domain.TrustedRootApproved {
		return domain.Workspace{}, domain.ErrNotAuthorized
	}
	existing, err := scanWorkspace(tx.QueryRowContext(ctx, `SELECT id, root_path, root_device, root_inode, created_at, git_repository, write_enabled, session_trusted, COALESCE(trusted_root_id,''), root_session_only
		FROM workspaces WHERE root_path=?`, workspace.RootPath))
	if err == nil {
		if existing.RootDevice != workspace.RootDevice || existing.RootInode != workspace.RootInode {
			return domain.Workspace{}, domain.ErrConflict
		}
		if existing.TrustedRootID != "" && existing.TrustedRootID != workspace.TrustedRootID {
			var oldStatus string
			oldErr := tx.QueryRowContext(ctx, `SELECT status FROM trusted_workspace_roots WHERE id=?`, existing.TrustedRootID).Scan(&oldStatus)
			if !existing.RootSessionOnly || oldErr != nil || (oldStatus != domain.TrustedRootDisabled && oldStatus != domain.TrustedRootRevoked && oldStatus != domain.TrustedRootExpired) {
				return domain.Workspace{}, domain.ErrConflict
			}
			if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET trusted_root_id=? WHERE id=? AND trusted_root_id=?`, workspace.TrustedRootID, existing.ID, existing.TrustedRootID); err != nil {
				return domain.Workspace{}, err
			}
			existing.TrustedRootID = workspace.TrustedRootID
			if err := tx.Commit(); err != nil {
				return domain.Workspace{}, err
			}
			if err := s.secureFiles(); err != nil {
				return domain.Workspace{}, err
			}
			return existing, nil
		}
		return existing, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return domain.Workspace{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workspaces(id, root_path, root_device, root_inode, created_at, git_repository, write_enabled, session_trusted, trusted_root_id, root_session_only)
		VALUES(?,?,?,?,?,?,0,0,?,1)`, workspace.ID, workspace.RootPath, workspace.RootDevice, workspace.RootInode, workspace.CreatedAt.UnixMilli(), workspace.GitRepository, workspace.TrustedRootID); err != nil {
		var sqliteError *modernsqlite.Error
		if errors.As(err, &sqliteError) && sqliteError.Code()&0xff == 19 {
			return domain.Workspace{}, domain.ErrConflict
		}
		return domain.Workspace{}, fmt.Errorf("create trusted logical workspace: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.Workspace{}, err
	}
	if err := s.secureFiles(); err != nil {
		return domain.Workspace{}, err
	}
	return workspace, nil
}

func scanWorkspace(row interface{ Scan(...any) error }) (domain.Workspace, error) {
	var workspace domain.Workspace
	var created int64
	err := row.Scan(&workspace.ID, &workspace.RootPath, &workspace.RootDevice, &workspace.RootInode, &created, &workspace.GitRepository, &workspace.WriteEnabled, &workspace.SessionTrusted, &workspace.TrustedRootID, &workspace.RootSessionOnly)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Workspace{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Workspace{}, err
	}
	workspace.CreatedAt = time.UnixMilli(created).UTC()
	return workspace, nil
}
