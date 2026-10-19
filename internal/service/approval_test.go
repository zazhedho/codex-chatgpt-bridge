package service

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"example.com/codex-chatgpt-bridge/internal/domain"
	"example.com/codex-chatgpt-bridge/internal/repository/sqlite"
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
