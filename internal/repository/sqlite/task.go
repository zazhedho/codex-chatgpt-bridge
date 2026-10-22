package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zazhedho/codex-chatgpt-bridge/internal/domain"
)

func (s *Store) CreateTask(ctx context.Context, task domain.Task) error {
	progress, err := json.Marshal(task.Progress)
	if err != nil {
		return err
	}
	mode := task.ExecutionMode
	if mode == "" {
		mode = domain.ExecutionReadOnly
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO tasks(id, workspace_id, execution_mode, thread_id, turn_id, status, prompt_hash, progress_json, result, diff, error, output_expired, created_at, updated_at)
		VALUES (?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		task.ID, task.WorkspaceID, mode, task.ThreadID, task.TurnID, task.Status, task.PromptHash, string(progress), task.Result, task.Diff, task.Error, task.OutputExpired, task.CreatedAt.UnixMilli(), task.UpdatedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("create task: %w", err)
	}
	return s.secureFiles()
}

func (s *Store) GetTask(ctx context.Context, id string) (domain.Task, error) {
	return scanTask(s.db.QueryRowContext(ctx, `SELECT id, workspace_id, execution_mode, COALESCE(thread_id,''), turn_id, status, prompt_hash, progress_json, result, diff, error, output_expired, created_at, updated_at FROM tasks WHERE id=?`, id))
}

func scanTask(row *sql.Row) (domain.Task, error) {
	var task domain.Task
	var thread, turn sql.NullString
	var status, mode string
	var progress string
	var expired bool
	var created, updated int64
	err := row.Scan(&task.ID, &task.WorkspaceID, &mode, &thread, &turn, &status, &task.PromptHash, &progress, &task.Result, &task.Diff, &task.Error, &expired, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Task{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Task{}, err
	}
	task.ThreadID, task.TurnID, task.ExecutionMode = thread.String, turn.String, domain.ExecutionMode(mode)
	task.Status, task.OutputExpired = domain.TaskStatus(status), expired
	if err := json.Unmarshal([]byte(progress), &task.Progress); err != nil {
		return domain.Task{}, fmt.Errorf("decode stored progress: %w", err)
	}
	task.CreatedAt, task.UpdatedAt = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
	return task, nil
}

func (s *Store) ListTasksForThread(ctx context.Context, workspaceID, threadID string) ([]domain.Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, workspace_id, execution_mode, COALESCE(thread_id,''), turn_id, status, prompt_hash, progress_json, result, diff, error, output_expired, created_at, updated_at FROM tasks WHERE workspace_id=? AND thread_id=? ORDER BY created_at DESC`, workspaceID, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []domain.Task
	for rows.Next() {
		var t domain.Task
		var thread, turn sql.NullString
		var status, mode, progress string
		var expired bool
		var created, updated int64
		if err := rows.Scan(&t.ID, &t.WorkspaceID, &mode, &thread, &turn, &status, &t.PromptHash, &progress, &t.Result, &t.Diff, &t.Error, &expired, &created, &updated); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(progress), &t.Progress); err != nil {
			return nil, err
		}
		t.ThreadID, t.TurnID, t.Status, t.ExecutionMode, t.OutputExpired = thread.String, turn.String, domain.TaskStatus(status), domain.ExecutionMode(mode), expired
		t.CreatedAt, t.UpdatedAt = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

func (s *Store) SetTaskThread(ctx context.Context, id, threadID, turnID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE tasks SET thread_id=?, turn_id=CASE WHEN ?='' THEN turn_id ELSE ? END, updated_at=? WHERE id=?`, threadID, turnID, turnID, time.Now().UnixMilli(), id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return domain.ErrNotFound
	}
	return s.secureFiles()
}

func (s *Store) SetTaskStatus(ctx context.Context, id string, next domain.TaskStatus, message string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT id, workspace_id, execution_mode, COALESCE(thread_id,''), turn_id, status, prompt_hash, progress_json, result, diff, error, output_expired, created_at, updated_at FROM tasks WHERE id=?`, id))
	if err != nil {
		return err
	}
	if err := task.Transition(next); err != nil {
		return err
	}
	if message == "" {
		message = errorOrEmpty(task.Error)
	}
	_, err = tx.ExecContext(ctx, `UPDATE tasks SET status=?, error=NULLIF(?,''), updated_at=? WHERE id=?`, task.Status, message, task.UpdatedAt.UnixMilli(), id)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.secureFiles()
}

func (s *Store) FinishTask(ctx context.Context, id string, status domain.TaskStatus, result, diff, message string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT id, workspace_id, execution_mode, COALESCE(thread_id,''), turn_id, status, prompt_hash, progress_json, result, diff, error, output_expired, created_at, updated_at FROM tasks WHERE id=?`, id))
	if err != nil {
		return err
	}
	if err := task.Transition(status); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE tasks SET status=?, result=?, diff=?, error=?, updated_at=? WHERE id=?`, status, nullIfEmpty(result), nullIfEmpty(diff), nullIfEmpty(message), task.UpdatedAt.UnixMilli(), id)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.secureFiles()
}

func (s *Store) AppendProgress(ctx context.Context, id, text string, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT id, workspace_id, execution_mode, COALESCE(thread_id,''), turn_id, status, prompt_hash, progress_json, result, diff, error, output_expired, created_at, updated_at FROM tasks WHERE id=?`, id))
	if err != nil {
		return err
	}
	if task.OutputExpired {
		return domain.ErrConflict
	}
	if len(text) > 2000 {
		text = text[:2000]
	}
	task.Progress = append(task.Progress, domain.ProgressEvent{At: at.UnixMilli(), Text: text})
	if len(task.Progress) > 100 {
		task.Progress = task.Progress[len(task.Progress)-100:]
	}
	encoded, err := json.Marshal(task.Progress)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE tasks SET progress_json=?, updated_at=? WHERE id=?`, string(encoded), at.UnixMilli(), id)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.secureFiles()
}

func (s *Store) RecoverTasks(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET status='interrupted', error='Bridge restarted while task was active', updated_at=? WHERE status IN ('queued','running')`, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	return s.secureFiles()
}

func (s *Store) ExpireTaskOutputs(ctx context.Context, retentionDays int) error {
	if retentionDays < 0 {
		return domain.ErrInvalidInput
	}
	if retentionDays == 0 {
		return nil
	}
	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour).UnixMilli()
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET progress_json='[]', result=NULL, diff=NULL, error=NULL, output_expired=1 WHERE updated_at < ? AND status IN ('completed','failed','cancelled','interrupted') AND output_expired=0`, cutoff)
	if err != nil {
		return err
	}
	return s.secureFiles()
}

func (s *Store) TaskStats(ctx context.Context) (active, queued int, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM tasks WHERE status IN ('queued','running','waiting_approval') GROUP BY status`)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return 0, 0, err
		}
		if status == "queued" {
			queued = count
		} else {
			active += count
		}
	}
	return active, queued, rows.Err()
}

func errorOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
