package domain

import "time"

type Workspace struct {
	ID              string
	RootPath        string
	RootDevice      uint64
	RootInode       uint64
	CreatedAt       time.Time
	GitRepository   string
	WriteEnabled    bool
	SessionTrusted  bool
	TrustedRootID   string
	RootSessionOnly bool
}

type WorkspaceRequest struct {
	ID          string
	WorkspaceID string
	RootPath    string
	Status      string
	ExpiresAt   time.Time
	CreatedAt   time.Time
}
