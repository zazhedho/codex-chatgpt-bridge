package domain

import "time"

const (
	ThreadSourceBridge               = "bridge"
	ThreadSourceCodexCLI             = "codex-cli"
	ThreadSourceCodexCLITrusted      = "codex-cli-trusted"
	ThreadSourceCodexCLIRoot         = "codex-cli-root-trusted"
	ThreadSourceCodexCLIProjectBound = "codex-cli-project-bound"

	SessionAttachPending  = "pending"
	SessionAttachApproved = "approved"
	SessionAttachRejected = "rejected"
	SessionAttachExpired  = "expired"
)

type CodexSession struct {
	ID            string    `json:"threadId"`
	WorkspacePath string    `json:"workspacePath"`
	Source        string    `json:"source"`
	Originator    string    `json:"originator,omitempty"`
	Title         string    `json:"title,omitempty"`
	Preview       string    `json:"-"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

func (s CodexSession) IsCodexCLISession() bool {
	return s.Source == "cli" || (s.Source == "vscode" && s.Originator == "codex-tui") || (s.Source == "exec" && s.Originator == "codex_exec")
}

type SessionAttachRequest struct {
	ID               string
	ThreadID         string
	WorkspaceID      string
	WorkspacePath    string
	RootDevice       uint64
	RootInode        uint64
	SessionUpdatedAt time.Time
	Status           string
	CreatedAt        time.Time
	ExpiresAt        time.Time
	DecidedAt        time.Time
	ApprovedBy       string
}

type SessionAttachDetails struct {
	Request SessionAttachRequest
	Session CodexSession
}

type CodexSessionPage struct {
	Sessions   []CodexSession
	NextCursor string
}

const (
	TrustedRootPending  = "pending"
	TrustedRootApproved = "approved"
	TrustedRootDisabled = "disabled"
	TrustedRootRevoked  = "revoked"
	TrustedRootExpired  = "expired"
)

type TrustedWorkspaceRoot struct {
	ID         string    `json:"id"`
	RootPath   string    `json:"rootPath"`
	RootDevice uint64    `json:"-"`
	RootInode  uint64    `json:"-"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"createdAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
	DecidedAt  time.Time `json:"decidedAt,omitempty"`
	ApprovedBy string    `json:"approvedBy,omitempty"`
}

type SessionCatalogEntry struct {
	ThreadID         string    `json:"threadId"`
	DisplayName      string    `json:"displayName"`
	WorkspaceID      string    `json:"workspaceId,omitempty"`
	Project          string    `json:"project"`
	Status           string    `json:"status"`
	UpdatedAt        time.Time `json:"updatedAt"`
	ConnectionStatus string    `json:"connectionStatus"`
	CanResume        bool      `json:"canResume"`
}

type SessionProjectChoice struct {
	ProjectID    string `json:"projectId"`
	Name         string `json:"name"`
	RelativePath string `json:"relativePath"`
}

type SessionProjectBinding struct {
	ID               string
	ThreadID         string
	ProjectID        string
	WorkspaceID      string
	TrustedRootID    string
	TrustedRootPath  string
	RootDevice       uint64
	RootInode        uint64
	SessionPath      string
	SessionDevice    uint64
	SessionInode     uint64
	SessionCreatedAt time.Time
	SessionUpdatedAt time.Time
	ProjectPath      string
	ProjectDevice    uint64
	ProjectInode     uint64
	Status           string
	RequestedBy      string
	ApprovedBy       string
	CreatedAt        time.Time
	ExpiresAt        time.Time
	DecidedAt        time.Time
}

const (
	SessionProjectBindingPending  = "pending"
	SessionProjectBindingApproved = "approved"
	SessionProjectBindingRejected = "rejected"
	SessionProjectBindingExpired  = "expired"
	SessionProjectBindingRevoked  = "revoked"
)

type SessionCatalogPage struct {
	Sessions   []SessionCatalogEntry `json:"sessions"`
	NextCursor string                `json:"nextCursor,omitempty"`
}
