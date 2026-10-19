package domain

import "time"

type Thread struct {
	ID          string
	WorkspaceID string
	LastTaskID  string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
