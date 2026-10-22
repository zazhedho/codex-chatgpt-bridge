package domain

import "time"

type Thread struct {
	ID            string
	WorkspaceID   string
	LastTaskID    string
	Source        string
	SessionPath   string
	TrustedRootID string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}
