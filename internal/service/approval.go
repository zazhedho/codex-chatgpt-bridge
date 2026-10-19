package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"example.com/codex-chatgpt-bridge/internal/domain"
	"example.com/codex-chatgpt-bridge/internal/security"
)

type ApprovalRepository interface {
	ListPendingApprovals(context.Context, time.Time) ([]domain.Approval, error)
	GetApproval(context.Context, string) (domain.Approval, error)
	InsertApproval(context.Context, domain.Approval, string) error
	ApproveApproval(context.Context, string, string, time.Time) (domain.Approval, error)
	RejectApproval(context.Context, string, time.Time) error
	ConsumeApproval(context.Context, string, string, string, time.Time) (domain.Approval, error)
}

type ApprovalService struct {
	repo       ApprovalRepository
	workspaces *WorkspaceService
	now        func() time.Time
}

func NewApprovalService(repo ApprovalRepository, workspaces *WorkspaceService) *ApprovalService {
	return &ApprovalService{repo: repo, workspaces: workspaces, now: time.Now}
}

type ApprovalInput struct {
	WorkspaceID    string
	TaskID         string
	OperationType  string
	TargetResource string
	Parameters     any
	Artifact       []byte
	HasArtifact    bool
	ExpiresIn      time.Duration
}

type ApprovalActor struct{ Channel, Subject string }

// CreatePending records an internal operation request; it does not execute the operation.
func (s *ApprovalService) CreatePending(ctx context.Context, input ApprovalInput) (domain.Approval, error) {
	if input.OperationType == "" || len(input.OperationType) > 128 || input.TargetResource == "" || len(input.TargetResource) > 4096 {
		return domain.Approval{}, domain.ErrInvalidInput
	}
	parameters, err := json.Marshal(input.Parameters)
	if err != nil || len(parameters) > 64<<10 {
		return domain.Approval{}, domain.ErrInvalidInput
	}
	created := s.now().UTC()
	expiresIn := input.ExpiresIn
	if expiresIn == 0 {
		expiresIn = 5 * time.Minute
	}
	if expiresIn < time.Second || expiresIn > 24*time.Hour {
		return domain.Approval{}, domain.ErrInvalidInput
	}
	id, err := newID()
	if err != nil {
		return domain.Approval{}, err
	}
	parameterHash := sha256.Sum256(parameters)
	artifactHash := ""
	if input.HasArtifact {
		digest := sha256.Sum256(input.Artifact)
		artifactHash = hex.EncodeToString(digest[:])
	}
	approval := domain.Approval{ID: id, WorkspaceID: input.WorkspaceID, OperationType: input.OperationType, TargetResource: input.TargetResource, ParametersJSON: string(parameters), ParametersHash: hex.EncodeToString(parameterHash[:]), ArtifactHash: artifactHash, Status: domain.ApprovalPending, CreatedAt: created, ExpiresAt: created.Add(expiresIn)}
	if err := s.repo.InsertApproval(ctx, approval, input.TaskID); err != nil {
		return domain.Approval{}, err
	}
	return approval, nil
}

func (s *ApprovalService) ApproveLocal(ctx context.Context, id string, actor ApprovalActor) (domain.Approval, error) {
	if actor.Channel != "local" || strings.TrimSpace(actor.Subject) == "" {
		return domain.Approval{}, domain.ErrNotAuthorized
	}
	return s.repo.ApproveApproval(ctx, id, actor.Subject, s.now().UTC())
}

func (s *ApprovalService) RejectLocal(ctx context.Context, id string, actor ApprovalActor) error {
	if actor.Channel != "local" || strings.TrimSpace(actor.Subject) == "" {
		return domain.ErrNotAuthorized
	}
	return s.repo.RejectApproval(ctx, id, s.now().UTC())
}

func (s *ApprovalService) ConsumeOnce(ctx context.Context, id string, parameters any, artifact []byte, hasArtifact bool) (domain.Approval, error) {
	encoded, err := json.Marshal(parameters)
	if err != nil {
		return domain.Approval{}, domain.ErrInvalidInput
	}
	parameterHash := sha256.Sum256(encoded)
	artifactHash := ""
	if hasArtifact {
		digest := sha256.Sum256(artifact)
		artifactHash = hex.EncodeToString(digest[:])
	}
	return s.repo.ConsumeApproval(ctx, id, hex.EncodeToString(parameterHash[:]), artifactHash, s.now().UTC())
}

func (s *ApprovalService) ListPending(ctx context.Context) ([]domain.WorkspaceRequest, []domain.Approval, error) {
	if _, ok := security.Authorize(ctx); !ok {
		return nil, nil, domain.ErrNotAuthorized
	}
	requests, err := s.workspaces.PendingRequests(ctx)
	if err != nil {
		return nil, nil, err
	}
	approvals, err := s.repo.ListPendingApprovals(ctx, s.now())
	if err != nil {
		return nil, nil, err
	}
	for i := range approvals {
		approvals[i].ParametersJSON = redactJSON(approvals[i].ParametersJSON)
	}
	return requests, approvals, nil
}

func (s *ApprovalService) ListPendingLocal(ctx context.Context) ([]domain.Approval, error) {
	values, err := s.repo.ListPendingApprovals(ctx, s.now())
	if err != nil {
		return nil, err
	}
	for i := range values {
		values[i].ParametersJSON = redactJSON(values[i].ParametersJSON)
	}
	return values, nil
}

func (s *ApprovalService) GetLocal(ctx context.Context, id string) (domain.Approval, error) {
	approval, err := s.repo.GetApproval(ctx, id)
	if err != nil {
		return domain.Approval{}, err
	}
	approval.ParametersJSON = redactJSON(approval.ParametersJSON)
	return approval, nil
}

func (s *ApprovalService) Get(ctx context.Context, id string) (map[string]any, error) {
	if _, ok := security.Authorize(ctx); !ok {
		return nil, domain.ErrNotAuthorized
	}
	if requests, err := s.workspaces.PendingRequests(ctx); err != nil {
		return nil, err
	} else {
		for _, request := range requests {
			if request.ID == id {
				return map[string]any{"id": request.ID, "type": "workspace_registration", "workspaceId": request.WorkspaceID, "rootPath": request.RootPath, "status": request.Status, "expiresAt": request.ExpiresAt.UnixMilli()}, nil
			}
		}
	}
	approval, err := s.repo.GetApproval(ctx, id)
	if err != nil {
		return nil, err
	}
	if approval.WorkspaceID != "" {
		if _, err := s.workspaces.Get(ctx, approval.WorkspaceID); err != nil {
			return nil, domain.ErrNotAuthorized
		}
	}
	var parameters any
	if err := json.Unmarshal([]byte(redactJSON(approval.ParametersJSON)), &parameters); err != nil {
		return nil, domain.ErrConflict
	}
	return map[string]any{"id": approval.ID, "workspaceId": nullString(approval.WorkspaceID), "operationType": security.RedactSecrets(approval.OperationType), "targetResource": security.RedactSecrets(approval.TargetResource), "parameters": parameters, "parametersHash": approval.ParametersHash, "artifactHash": nullString(approval.ArtifactHash), "status": approval.Status, "approvedBy": nullString(approval.ApprovedBy), "createdAt": approval.CreatedAt.UnixMilli(), "expiresAt": approval.ExpiresAt.UnixMilli(), "consumedAt": nullableMillis(approval.ConsumedAt)}, nil
}

func redactJSON(raw string) string {
	var value any
	if json.Unmarshal([]byte(raw), &value) != nil {
		return `"[REDACTED]"`
	}
	var walk func(any) any
	walk = func(value any) any {
		switch current := value.(type) {
		case map[string]any:
			for key, child := range current {
				if strings.Contains(strings.ToLower(key), "secret") || strings.Contains(strings.ToLower(key), "token") || strings.Contains(strings.ToLower(key), "password") || strings.Contains(strings.ToLower(key), "credential") || strings.Contains(strings.ToLower(key), "key") {
					current[key] = "[REDACTED]"
				} else {
					current[key] = walk(child)
				}
			}
			return current
		case []any:
			for i := range current {
				current[i] = walk(current[i])
			}
			return current
		case string:
			return security.RedactSecrets(current)
		default:
			return current
		}
	}
	encoded, err := json.Marshal(walk(value))
	if err != nil {
		return `"[REDACTED]"`
	}
	return string(encoded)
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func nullableMillis(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UnixMilli()
}
