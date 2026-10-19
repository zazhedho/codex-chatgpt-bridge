package domain

import "time"

type ApprovalStatus string

const (
	ApprovalPending  ApprovalStatus = "pending"
	ApprovalApproved ApprovalStatus = "approved"
	ApprovalRejected ApprovalStatus = "rejected"
	ApprovalExpired  ApprovalStatus = "expired"
	ApprovalConsumed ApprovalStatus = "consumed"
)

type Approval struct {
	ID             string
	WorkspaceID    string
	OperationType  string
	TargetResource string
	ParametersJSON string
	ParametersHash string
	ArtifactHash   string
	Status         ApprovalStatus
	ApprovedBy     string
	CreatedAt      time.Time
	ExpiresAt      time.Time
	ConsumedAt     *time.Time
}
