package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zazhedho/codex-chatgpt-bridge/internal/domain"
)

func (s *Store) InsertApproval(ctx context.Context, a domain.Approval, taskID string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO approvals(id, workspace_id, task_id, operation, target, parameters_json, parameters_hash, artifact_hash, status, expires_at, created_at)
		VALUES(?, NULLIF(?,''), NULLIF(?,''), ?, ?, ?, ?, ?, ?, ?, ?)`, a.ID, a.WorkspaceID, taskID, a.OperationType, a.TargetResource, a.ParametersJSON, a.ParametersHash, a.ArtifactHash, a.Status, a.ExpiresAt.UnixMilli(), a.CreatedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("create approval: %w", err)
	}
	return s.secureFiles()
}

func (s *Store) CreateWriteTaskApproval(ctx context.Context, task domain.Task, approval domain.Approval, maxPending int) error {
	if task.Status != domain.TaskWaitingApproval || task.ExecutionMode != domain.ExecutionWorkspaceWrite || task.WorkspaceID == "" || task.ID == "" || approval.OperationType != "codex.workspace-write" || approval.TargetResource != task.WorkspaceID || approval.WorkspaceID != task.WorkspaceID || approval.Status != domain.ApprovalPending || maxPending < 1 {
		return domain.ErrInvalidInput
	}
	progress, err := json.Marshal(task.Progress)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var enabled bool
	if err := tx.QueryRowContext(ctx, `SELECT write_enabled FROM workspaces WHERE id=?`, task.WorkspaceID).Scan(&enabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		}
		return err
	}
	if !enabled {
		return domain.ErrNotAuthorized
	}
	if task.ThreadID != "" {
		var owner string
		if err := tx.QueryRowContext(ctx, `SELECT workspace_id FROM threads WHERE id=?`, task.ThreadID).Scan(&owner); err != nil || owner != task.WorkspaceID {
			return domain.ErrNotAuthorized
		}
	}
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE execution_mode='workspace-write' AND status='waiting_approval'`).Scan(&pending); err != nil {
		return err
	}
	if pending >= maxPending {
		return domain.ErrQueueFull
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tasks(id, workspace_id, execution_mode, thread_id, turn_id, status, prompt_hash, progress_json, result, diff, error, output_expired, created_at, updated_at)
		VALUES (?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		task.ID, task.WorkspaceID, task.ExecutionMode, task.ThreadID, task.TurnID, task.Status, task.PromptHash, string(progress), task.Result, task.Diff, task.Error, task.OutputExpired, task.CreatedAt.UnixMilli(), task.UpdatedAt.UnixMilli()); err != nil {
		return fmt.Errorf("create write task: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO approvals(id, workspace_id, task_id, operation, target, parameters_json, parameters_hash, artifact_hash, status, expires_at, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, approval.ID, approval.WorkspaceID, task.ID, approval.OperationType, approval.TargetResource, approval.ParametersJSON, approval.ParametersHash, approval.ArtifactHash, approval.Status, approval.ExpiresAt.UnixMilli(), approval.CreatedAt.UnixMilli()); err != nil {
		return fmt.Errorf("create write task approval: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.secureFiles()
}

func (s *Store) ApproveApproval(ctx context.Context, id, actor string, now time.Time) (domain.Approval, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Approval{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE approvals SET status='approved', approved_by=?, decided_at=? WHERE id=? AND status='pending' AND expires_at>?`, actor, now.UnixMilli(), id, now.UnixMilli())
	if err != nil {
		return domain.Approval{}, err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		if _, err := tx.ExecContext(ctx, `UPDATE approvals SET status='expired', parameters_json=CASE WHEN operation='codex.workspace-write' THEN '{}' ELSE parameters_json END, decided_at=? WHERE id=? AND status='pending' AND expires_at<=?`, now.UnixMilli(), id, now.UnixMilli()); err != nil {
			return domain.Approval{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status='failed', error='Write task approval expired', updated_at=? WHERE status='waiting_approval' AND id=(SELECT task_id FROM approvals WHERE id=? AND operation='codex.workspace-write' AND status='expired')`, now.UnixMilli(), id); err != nil {
			return domain.Approval{}, err
		}
		if err := tx.Commit(); err != nil {
			return domain.Approval{}, err
		}
		if err := s.secureFiles(); err != nil {
			return domain.Approval{}, err
		}
		return domain.Approval{}, domain.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return domain.Approval{}, err
	}
	if err := s.secureFiles(); err != nil {
		return domain.Approval{}, err
	}
	return s.GetApproval(ctx, id)
}

func (s *Store) RejectApproval(ctx context.Context, id string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE approvals SET status='rejected', parameters_json=CASE WHEN operation='codex.workspace-write' THEN '{}' ELSE parameters_json END, decided_at=? WHERE id=? AND status='pending' AND expires_at>?`, now.UnixMilli(), id, now.UnixMilli())
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return domain.ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status='cancelled', error='Write task approval was rejected', updated_at=? WHERE id=(SELECT task_id FROM approvals WHERE id=? AND operation='codex.workspace-write') AND status='waiting_approval'`, now.UnixMilli(), id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE approvals SET status='expired', parameters_json=CASE WHEN operation='codex.workspace-write' THEN '{}' ELSE parameters_json END, decided_at=? WHERE status='pending' AND expires_at<=?`, now.UnixMilli(), now.UnixMilli()); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status='failed', error='Write task approval expired', updated_at=? WHERE status='waiting_approval' AND id IN (SELECT task_id FROM approvals WHERE operation='codex.workspace-write' AND status='expired')`, now.UnixMilli()); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if err := s.secureFiles(); err != nil {
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

func (s *Store) ListPendingApprovalsPage(ctx context.Context, now time.Time, limit, offset int) ([]domain.Approval, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, COALESCE(workspace_id,''), operation, target, parameters_json, parameters_hash, NULLIF(artifact_hash,''), status, COALESCE(approved_by,''), created_at, expires_at, consumed_at FROM approvals WHERE status='pending' AND expires_at>? ORDER BY created_at, id LIMIT ? OFFSET ?`, now.UnixMilli(), limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	values := make([]domain.Approval, 0, limit)
	for rows.Next() {
		if len(values) == limit {
			return values, true, nil
		}
		value, err := scanApproval(rows)
		if err != nil {
			return nil, false, err
		}
		values = append(values, value)
	}
	return values, false, rows.Err()
}

func (s *Store) GetApproval(ctx context.Context, id string) (domain.Approval, error) {
	return scanApproval(s.db.QueryRowContext(ctx, `SELECT id, COALESCE(workspace_id,''), operation, target, parameters_json, parameters_hash, NULLIF(artifact_hash,''), status, COALESCE(approved_by,''), created_at, expires_at, consumed_at FROM approvals WHERE id=?`, id))
}

func (s *Store) ClaimApprovedWriteTask(ctx context.Context, now time.Time) (domain.ApprovedWriteTask, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ApprovedWriteTask{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE approvals SET status='expired', parameters_json='{}', decided_at=? WHERE operation='codex.workspace-write' AND status IN ('pending','approved') AND expires_at<=?`, now.UnixMilli(), now.UnixMilli()); err != nil {
		return domain.ApprovedWriteTask{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status='failed', error='Write task approval expired', updated_at=? WHERE status='waiting_approval' AND id IN (SELECT task_id FROM approvals WHERE operation='codex.workspace-write' AND status='expired')`, now.UnixMilli()); err != nil {
		return domain.ApprovedWriteTask{}, err
	}
	var approvalID, approvalWorkspace, taskID, target, parametersJSON, parametersHash, artifactHash string
	var taskWorkspace, mode, threadID, promptHash, taskStatus string
	var rootPath, gitRepository string
	var rootDevice, rootInode uint64
	var created int64
	var writeEnabled, threadOwned bool
	err = tx.QueryRowContext(ctx, `SELECT a.id, COALESCE(a.workspace_id,''), COALESCE(a.task_id,''), a.target, a.parameters_json, a.parameters_hash, a.artifact_hash,
		t.workspace_id, t.execution_mode, COALESCE(t.thread_id,''), t.prompt_hash, t.status,
		w.root_path, w.root_device, w.root_inode, w.created_at, w.git_repository, w.write_enabled,
		CASE WHEN t.thread_id IS NULL OR th.workspace_id=t.workspace_id THEN 1 ELSE 0 END
		FROM approvals a JOIN tasks t ON t.id=a.task_id JOIN workspaces w ON w.id=t.workspace_id LEFT JOIN threads th ON th.id=t.thread_id
		WHERE a.operation='codex.workspace-write' AND a.status='approved' AND a.expires_at>? AND t.status='waiting_approval'
		ORDER BY a.created_at, a.id LIMIT 1`, now.UnixMilli()).
		Scan(&approvalID, &approvalWorkspace, &taskID, &target, &parametersJSON, &parametersHash, &artifactHash,
			&taskWorkspace, &mode, &threadID, &promptHash, &taskStatus,
			&rootPath, &rootDevice, &rootInode, &created, &gitRepository, &writeEnabled, &threadOwned)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return domain.ApprovedWriteTask{}, err
		}
		return domain.ApprovedWriteTask{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.ApprovedWriteTask{}, err
	}
	var parameters domain.WriteTaskApprovalParameters
	parameterDigest := sha256.Sum256([]byte(parametersJSON))
	if err := json.Unmarshal([]byte(parametersJSON), &parameters); err != nil || hex.EncodeToString(parameterDigest[:]) != parametersHash || approvalWorkspace != taskWorkspace || target != taskWorkspace || taskID == "" || taskStatus != string(domain.TaskWaitingApproval) || mode != string(domain.ExecutionWorkspaceWrite) || artifactHash != "" || !writeEnabled || !threadOwned || parameters.TaskID != taskID || parameters.WorkspaceID != taskWorkspace || parameters.ExecutionMode != domain.ExecutionWorkspaceWrite || parameters.ThreadID != threadID || parameters.Prompt == "" || hashPrompt(parameters.Prompt) != promptHash {
		if _, updateErr := tx.ExecContext(ctx, `UPDATE approvals SET status='rejected', parameters_json='{}', decided_at=? WHERE id=? AND status='approved'`, now.UnixMilli(), approvalID); updateErr != nil {
			return domain.ApprovedWriteTask{}, updateErr
		}
		if _, updateErr := tx.ExecContext(ctx, `UPDATE tasks SET status='failed', error='Write task approval binding failed', updated_at=? WHERE id=? AND status='waiting_approval'`, now.UnixMilli(), taskID); updateErr != nil {
			return domain.ApprovedWriteTask{}, updateErr
		}
		if err := tx.Commit(); err != nil {
			return domain.ApprovedWriteTask{}, err
		}
		return domain.ApprovedWriteTask{}, domain.ErrNotAuthorized
	}
	result, err := tx.ExecContext(ctx, `UPDATE approvals SET status='consumed', parameters_json='{}', consumed_at=? WHERE id=? AND status='approved' AND expires_at>?`, now.UnixMilli(), approvalID, now.UnixMilli())
	if err != nil {
		return domain.ApprovedWriteTask{}, err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return domain.ApprovedWriteTask{}, domain.ErrConflict
	}
	result, err = tx.ExecContext(ctx, `UPDATE tasks SET status='queued', error=NULL, updated_at=? WHERE id=? AND status='waiting_approval' AND execution_mode='workspace-write'`, now.UnixMilli(), taskID)
	if err != nil {
		return domain.ApprovedWriteTask{}, err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return domain.ApprovedWriteTask{}, domain.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return domain.ApprovedWriteTask{}, err
	}
	if err := s.secureFiles(); err != nil {
		return domain.ApprovedWriteTask{}, err
	}
	return domain.ApprovedWriteTask{TaskID: taskID, Workspace: domain.Workspace{ID: taskWorkspace, RootPath: rootPath, RootDevice: rootDevice, RootInode: rootInode, CreatedAt: time.UnixMilli(created).UTC(), GitRepository: gitRepository, WriteEnabled: true}, Prompt: parameters.Prompt, ThreadID: threadID}, nil
}

func (s *Store) CancelWriteTask(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status, mode string
	if err := tx.QueryRowContext(ctx, `SELECT status, execution_mode FROM tasks WHERE id=?`, id).Scan(&status, &mode); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		}
		return err
	}
	if status != string(domain.TaskWaitingApproval) || mode != string(domain.ExecutionWorkspaceWrite) {
		return domain.ErrConflict
	}
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `UPDATE approvals SET status='rejected', parameters_json='{}', decided_at=? WHERE task_id=? AND operation='codex.workspace-write' AND status IN ('pending','approved')`, now, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status='cancelled', error='Cancelled before approval', updated_at=? WHERE id=? AND status='waiting_approval'`, now, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.secureFiles()
}

func hashPrompt(prompt string) string {
	digest := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(digest[:])
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
