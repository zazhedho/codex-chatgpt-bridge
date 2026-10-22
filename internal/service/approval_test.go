package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zazhedho/codex-chatgpt-bridge/internal/domain"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/repository/sqlite"
)

func TestApprovalBindsParametersAndArtifactAndConsumesOnce(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	workspaces := NewWorkspaceService(store, nil, "", "")
	approvals := NewApprovalService(store, workspaces)
	input := ApprovalInput{OperationType: "adapter.run", TargetResource: "fixture", Parameters: map[string]any{"b": 2, "a": 1}, Artifact: []byte("plan-v1"), HasArtifact: true}
	created, err := approvals.CreatePending(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(created.ParametersHash) != 64 || len(created.ArtifactHash) != 64 {
		t.Fatalf("missing SHA-256 binding: %+v", created)
	}
	if _, err := approvals.ApproveLocal(context.Background(), created.ID, ApprovalActor{Channel: "mcp", Subject: "remote"}); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("remote approval error = %v", err)
	}
	approved, err := approvals.ApproveLocal(context.Background(), created.ID, ApprovalActor{Channel: "local", Subject: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if approved.ApprovedBy != "operator" {
		t.Fatalf("approved by %q", approved.ApprovedBy)
	}
	if _, err := approvals.ConsumeOnce(context.Background(), created.ID, map[string]any{"a": 1, "b": 2}, []byte("plan-v1"), true); err != nil {
		t.Fatalf("canonical parameter ordering failed: %v", err)
	}
	if _, err := approvals.ConsumeOnce(context.Background(), created.ID, input.Parameters, input.Artifact, true); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate consume error = %v", err)
	}
}

func TestWriteApprovalExpiryAndPromptTamperingFailClosed(t *testing.T) {
	for _, scenario := range []string{"expired", "modified-prompt"} {
		t.Run(scenario, func(t *testing.T) {
			dataDir := t.TempDir()
			store, err := sqlite.Open(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			home := t.TempDir()
			workspaces := NewWorkspaceService(store, nil, home, "")
			workspace := registerForTest(t, workspaces, home, "project-a")
			if err := workspaces.SetWriteEnabledLocal(context.Background(), workspace.ID, true, "operator"); err != nil {
				t.Fatal(err)
			}
			prompt := "change fixture from old to new"
			digest := sha256.Sum256([]byte(prompt))
			now := time.Now().UTC()
			task := domain.Task{ID: "00000000-0000-4000-8000-000000000002", WorkspaceID: workspace.ID, ExecutionMode: domain.ExecutionWorkspaceWrite, Status: domain.TaskWaitingApproval, PromptHash: hex.EncodeToString(digest[:]), Progress: []domain.ProgressEvent{}, CreatedAt: now, UpdatedAt: now}
			approvals := NewApprovalService(store, workspaces)
			approval, err := approvals.CreateWriteTaskApproval(context.Background(), task, prompt, 4)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := approvals.ApproveLocal(context.Background(), approval.ID, ApprovalActor{Channel: "local", Subject: "operator"}); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", filepath.Join(dataDir, "bridge.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			switch scenario {
			case "expired":
				if _, err := db.Exec(`UPDATE approvals SET expires_at=? WHERE id=?`, now.Add(-time.Second).UnixMilli(), approval.ID); err != nil {
					t.Fatal(err)
				}
			case "modified-prompt":
				modified := strings.Replace(approval.ParametersJSON, prompt, "different prompt", 1)
				if _, err := db.Exec(`UPDATE approvals SET parameters_json=? WHERE id=?`, modified, approval.ID); err != nil {
					t.Fatal(err)
				}
			}
			_, claimErr := store.ClaimApprovedWriteTask(context.Background(), now)
			if scenario == "expired" && !errors.Is(claimErr, domain.ErrNotFound) {
				t.Fatalf("expired approval claim error=%v", claimErr)
			}
			if scenario == "modified-prompt" && !errors.Is(claimErr, domain.ErrNotAuthorized) {
				t.Fatalf("modified prompt claim error=%v", claimErr)
			}
			storedTask, err := store.GetTask(context.Background(), task.ID)
			if err != nil {
				t.Fatal(err)
			}
			storedApproval, err := store.GetApproval(context.Background(), approval.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantApproval := domain.ApprovalExpired
			if scenario == "modified-prompt" {
				wantApproval = domain.ApprovalRejected
			}
			if storedTask.Status != domain.TaskFailed || storedApproval.Status != wantApproval || storedApproval.ParametersJSON != "{}" {
				t.Fatalf("unsafe write request remained: task=%s approval=%s parameters=%s", storedTask.Status, storedApproval.Status, storedApproval.ParametersJSON)
			}
		})
	}
}

func TestExpiredWriteApprovalCannotRetainPromptOrTask(t *testing.T) {
	dataDir := t.TempDir()
	store, err := sqlite.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	home := t.TempDir()
	workspaces := NewWorkspaceService(store, nil, home, "")
	workspace := registerForTest(t, workspaces, home, "project-a")
	if err := workspaces.SetWriteEnabledLocal(context.Background(), workspace.ID, true, "operator"); err != nil {
		t.Fatal(err)
	}
	approvals := NewApprovalService(store, workspaces)
	prompt := "change this disposable fixture"
	promptHash := sha256.Sum256([]byte(prompt))
	now := time.Now().UTC()
	task := domain.Task{ID: "00000000-0000-4000-8000-000000000003", WorkspaceID: workspace.ID, ExecutionMode: domain.ExecutionWorkspaceWrite, Status: domain.TaskWaitingApproval, PromptHash: hex.EncodeToString(promptHash[:]), Progress: []domain.ProgressEvent{}, CreatedAt: now, UpdatedAt: now}
	approval, err := approvals.CreateWriteTaskApproval(context.Background(), task, prompt, 4)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "bridge.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE approvals SET expires_at=? WHERE id=?`, now.Add(-time.Second).UnixMilli(), approval.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := approvals.ApproveLocal(context.Background(), approval.ID, ApprovalActor{Channel: "local", Subject: "operator"}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expired local approval error=%v", err)
	}
	storedApproval, err := store.GetApproval(context.Background(), approval.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedTask, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedApproval.Status != domain.ApprovalExpired || storedApproval.ParametersJSON != "{}" || storedTask.Status != domain.TaskFailed {
		t.Fatalf("expired approval retained work: approval=%s parameters=%s task=%s", storedApproval.Status, storedApproval.ParametersJSON, storedTask.Status)
	}
}

func TestApprovalRejectsModifiedArtifactAndExpires(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	approvals := NewApprovalService(store, NewWorkspaceService(store, nil, "", ""))
	now := time.UnixMilli(1000).UTC()
	approvals.now = func() time.Time { return now }
	created, err := approvals.CreatePending(context.Background(), ApprovalInput{OperationType: "adapter.run", TargetResource: "fixture", Parameters: map[string]any{"destroy": false}, Artifact: []byte("plan-v1"), HasArtifact: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := approvals.ApproveLocal(context.Background(), created.ID, ApprovalActor{Channel: "local", Subject: "operator"}); err != nil {
		t.Fatal(err)
	}
	if _, err := approvals.ConsumeOnce(context.Background(), created.ID, map[string]any{"destroy": false}, []byte("plan-v2"), true); !errors.Is(err, domain.ErrNotAuthorized) {
		t.Fatalf("modified artifact error = %v", err)
	}
	value, err := approvals.GetLocal(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if value.Status != domain.ApprovalRejected {
		t.Fatalf("modified artifact status = %s", value.Status)
	}

	expired, err := approvals.CreatePending(context.Background(), ApprovalInput{OperationType: "adapter.run", TargetResource: "fixture", Parameters: map[string]any{}, ExpiresIn: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := approvals.ApproveLocal(context.Background(), expired.ID, ApprovalActor{Channel: "local", Subject: "operator"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	if _, err := approvals.ConsumeOnce(context.Background(), expired.ID, map[string]any{}, nil, false); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expired approval error = %v", err)
	}
	value, err = approvals.GetLocal(context.Background(), expired.ID)
	if err != nil {
		t.Fatal(err)
	}
	if value.Status != domain.ApprovalExpired {
		t.Fatalf("expired status = %s", value.Status)
	}
}

func TestApprovalDetailsRedactSensitiveParameterValues(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	approvals := NewApprovalService(store, NewWorkspaceService(store, nil, "", ""))
	created, err := approvals.CreatePending(context.Background(), ApprovalInput{OperationType: "adapter.run", TargetResource: "fixture", Parameters: map[string]any{"apiToken": "super-secret-value", "environment": "test"}})
	if err != nil {
		t.Fatal(err)
	}
	value, err := approvals.GetLocal(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if value.ParametersJSON == "" || value.ParametersJSON == created.ParametersJSON {
		t.Fatalf("parameters were not redacted: %s", value.ParametersJSON)
	}
	if strings.Contains(value.ParametersJSON, "super-secret-value") {
		t.Fatal("approval details leaked a secret value")
	}
}
