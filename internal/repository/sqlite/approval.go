package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"example.com/codex-chatgpt-bridge/internal/domain"
)

func (s *Store) InsertApproval(ctx context.Context, a domain.Approval, taskID string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO approvals(id, workspace_id, task_id, operation, target, parameters_json, parameters_hash, artifact_hash, status, expires_at, created_at)
		VALUES(?, NULLIF(?,''), NULLIF(?,''), ?, ?, ?, ?, ?, ?, ?, ?)`, a.ID, a.WorkspaceID, taskID, a.OperationType, a.TargetResource, a.ParametersJSON, a.ParametersHash, a.ArtifactHash, a.Status, a.ExpiresAt.UnixMilli(), a.CreatedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("create approval: %w", err)
	}
	return s.secureFiles()
}

func (s *Store) ApproveApproval(ctx context.Context, id, actor string, now time.Time) (domain.Approval, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE approvals SET status='approved', approved_by=?, decided_at=? WHERE id=? AND status='pending' AND expires_at>?`, actor, now.UnixMilli(), id, now.UnixMilli())
	if err != nil {
		return domain.Approval{}, err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		_, _ = s.db.ExecContext(ctx, `UPDATE approvals SET status='expired' WHERE id=? AND status='pending' AND expires_at<=?`, id, now.UnixMilli())
		return domain.Approval{}, domain.ErrConflict
	}
	if err := s.secureFiles(); err != nil {
		return domain.Approval{}, err
	}
	return s.GetApproval(ctx, id)
}

func (s *Store) RejectApproval(ctx context.Context, id string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE approvals SET status='rejected', decided_at=? WHERE id=? AND status='pending' AND expires_at>?`, now.UnixMilli(), id, now.UnixMilli())
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return domain.ErrConflict
	}
	return s.secureFiles()
}

func (s *Store) ConsumeApproval(ctx context.Context, id, parametersHash, artifactHash string, now time.Time) (domain.Approval, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Approval{}, err
	}
	defer tx.Rollback()
	a, err := scanApproval(tx.QueryRowContext(ctx, `SELECT id, COALESCE(workspace_id,''), operation, target, parameters_json, parameters_hash, NULLIF(artifact_hash,''), status, COALESCE(approved_by,''), created_at, expires_at, consumed_at FROM approvals WHERE id=?`, id))
	if err != nil {
		return domain.Approval{}, err
	}
	if a.Status == domain.ApprovalConsumed {
		return domain.Approval{}, domain.ErrConflict
	}
	if a.Status != domain.ApprovalApproved {
		return domain.Approval{}, domain.ErrConflict
	}
	if !now.Before(a.ExpiresAt) {
		_, err = tx.ExecContext(ctx, `UPDATE approvals SET status='expired' WHERE id=? AND status='approved'`, id)
		if err != nil {
			return domain.Approval{}, err
		}
		if err := tx.Commit(); err != nil {
			return domain.Approval{}, err
		}
		return domain.Approval{}, domain.ErrConflict
	}
	if a.ParametersHash != parametersHash || a.ArtifactHash != artifactHash {
		_, err = tx.ExecContext(ctx, `UPDATE approvals SET status='rejected' WHERE id=? AND status='approved'`, id)
		if err != nil {
			return domain.Approval{}, err
		}
		if err := tx.Commit(); err != nil {
			return domain.Approval{}, err
		}
		return domain.Approval{}, domain.ErrNotAuthorized
	}
	consumedAt := now.UnixMilli()
	result, err := tx.ExecContext(ctx, `UPDATE approvals SET status='consumed', consumed_at=? WHERE id=? AND status='approved'`, consumedAt, id)
	if err != nil {
		return domain.Approval{}, err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return domain.Approval{}, domain.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return domain.Approval{}, err
	}
	a.Status, a.ConsumedAt = domain.ApprovalConsumed, &now
	return a, nil
}

func (s *Store) ListPendingApprovals(ctx context.Context, now time.Time) ([]domain.Approval, error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE approvals SET status='expired' WHERE status='pending' AND expires_at<=?`, now.UnixMilli()); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, COALESCE(workspace_id,''), operation, target, parameters_json, parameters_hash, NULLIF(artifact_hash,''), status, COALESCE(approved_by,''), created_at, expires_at, consumed_at FROM approvals WHERE status='pending' ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []domain.Approval
	for rows.Next() {
		v, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, rows.Err()
}

func (s *Store) GetApproval(ctx context.Context, id string) (domain.Approval, error) {
	return scanApproval(s.db.QueryRowContext(ctx, `SELECT id, COALESCE(workspace_id,''), operation, target, parameters_json, parameters_hash, NULLIF(artifact_hash,''), status, COALESCE(approved_by,''), created_at, expires_at, consumed_at FROM approvals WHERE id=?`, id))
}

type approvalScanner interface{ Scan(...any) error }

func scanApproval(row approvalScanner) (domain.Approval, error) {
	var a domain.Approval
	var created, expires int64
	var consumed sql.NullInt64
	var artifact sql.NullString
	err := row.Scan(&a.ID, &a.WorkspaceID, &a.OperationType, &a.TargetResource, &a.ParametersJSON, &a.ParametersHash, &artifact, &a.Status, &a.ApprovedBy, &created, &expires, &consumed)
	if err == sql.ErrNoRows {
		return domain.Approval{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Approval{}, err
	}
	a.ArtifactHash = artifact.String
	a.CreatedAt, a.ExpiresAt = time.UnixMilli(created).UTC(), time.UnixMilli(expires).UTC()
	if consumed.Valid {
		value := time.UnixMilli(consumed.Int64).UTC()
		a.ConsumedAt = &value
	}
	return a, nil
}
