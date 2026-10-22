package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/audit"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/config"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/domain"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/security"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/service"
)

type CodexStatus interface{ Connected() bool }

type Dependencies struct {
	Config             config.Config
	Workspaces         *service.WorkspaceService
	Tasks              *service.TaskService
	Sessions           *service.SessionService
	Approvals          *service.ApprovalService
	Audit              *audit.Logger
	Codex              CodexStatus
	CloudflareVerifier security.AccessAssertionVerifier
	StartedAt          time.Time
	Logger             *slog.Logger
}

type Empty struct{}
type PageInput struct {
	Limit  *int `json:"limit,omitempty" jsonschema:"Optional page size from 1 to 10; defaults to 10"`
	Offset *int `json:"offset,omitempty" jsonschema:"Optional stable offset; defaults to 0"`
}

func (in PageInput) values() (int, int) {
	limit, offset := 0, 0
	if in.Limit != nil {
		limit = *in.Limit
	}
	if in.Offset != nil {
		offset = *in.Offset
	}
	return limit, offset
}

type WorkspaceIDInput struct {
	WorkspaceID string `json:"workspaceId" jsonschema:"Registered workspace ID"`
}
type WorkspaceRegisterInput struct {
	WorkspaceID string `json:"workspaceId"`
	Path        string `json:"path"`
}
type TaskStartInput struct {
	WorkspaceID string `json:"workspaceId"`
	Prompt      string `json:"prompt"`
}
type TaskResumeInput struct {
	WorkspaceID string `json:"workspaceId"`
	ThreadID    string `json:"threadId"`
	Prompt      string `json:"prompt"`
}
type SessionAttachInput struct {
	WorkspaceID string `json:"workspaceId" jsonschema:"Approved workspace ID matching the Codex CLI session working directory"`
	ThreadID    string `json:"threadId" jsonschema:"Codex CLI thread ID from local bridge-admin session discovery"`
}
type SessionCatalogInput struct {
	WorkspaceID string `json:"workspaceId,omitempty" jsonschema:"Optional approved workspace filter"`
	Search      string `json:"search,omitempty" jsonschema:"Optional case-insensitive search of each page's displayed session name"`
	Limit       *int   `json:"limit,omitempty" jsonschema:"Optional page size from 1 to 50; defaults to 25"`
	Cursor      string `json:"cursor,omitempty" jsonschema:"Opaque App Server pagination cursor from the previous page"`
}
type SessionProjectsInput struct {
	ThreadID string `json:"threadId" jsonschema:"Codex CLI thread ID requiring project selection"`
	Limit    *int   `json:"limit,omitempty" jsonschema:"Optional page size from 1 to 50; defaults to 25"`
	Offset   *int   `json:"offset,omitempty" jsonschema:"Optional stable choice offset; defaults to 0"`
}
type SessionProjectBindingInput struct {
	ThreadID  string `json:"threadId" jsonschema:"Codex CLI thread ID requiring project binding"`
	ProjectID string `json:"projectId" jsonschema:"Opaque project ID returned by codex_list_session_projects"`
}
type TaskWriteInput struct {
	WorkspaceID string `json:"workspaceId"`
	Prompt      string `json:"prompt"`
	ThreadID    string `json:"threadId,omitempty"`
}
type TaskIDInput struct {
	TaskID string `json:"taskId"`
}
type ApprovalIDInput struct {
	ApprovalID string `json:"approvalId"`
}

type WorkspaceDTO struct {
	ID                   string                `json:"id"`
	RootPath             string                `json:"rootPath"`
	AllowedDirectories   []string              `json:"allowedDirectories"`
	DefaultModel         *string               `json:"defaultModel"`
	SandboxPermissions   SandboxPermissionsDTO `json:"sandboxPermissions"`
	AllowedOperations    []string              `json:"allowedOperations"`
	ApprovalRequirements []string              `json:"approvalRequirements"`
	WriteEnabled         bool                  `json:"writeEnabled"`
	SessionTrusted       bool                  `json:"sessionTrusted"`
	MaxConcurrentTasks   int                   `json:"maxConcurrentTasks"`
	TimeoutMS            int64                 `json:"timeoutMs"`
	GitRepository        *string               `json:"gitRepository"`
	CreatedAt            int64                 `json:"createdAt"`
}
type SandboxPermissionsDTO struct {
	Filesystem string `json:"filesystem"`
	Network    bool   `json:"network"`
}
type WorkspaceListOutput struct {
	Workspaces []WorkspaceDTO `json:"workspaces"`
	NextOffset *int           `json:"nextOffset,omitempty"`
}
type WorkspaceOutput struct {
	Workspace WorkspaceDTO `json:"workspace"`
}
type RegistrationOutput struct {
	RequestID   string `json:"requestId"`
	WorkspaceID string `json:"workspaceId"`
	Status      string `json:"status"`
	ExpiresAt   int64  `json:"expiresAt"`
}
type ThreadSummaryDTO struct {
	ThreadID  string `json:"threadId"`
	TaskID    string `json:"taskId,omitempty"`
	Status    string `json:"status"`
	Source    string `json:"source,omitempty"`
	UpdatedAt int64  `json:"updatedAt"`
}
type ThreadListOutput struct {
	Threads []ThreadSummaryDTO `json:"threads"`
}
type SessionAttachOutput struct {
	RequestID   string `json:"requestId"`
	ThreadID    string `json:"threadId"`
	WorkspaceID string `json:"workspaceId"`
	Status      string `json:"status"`
	ExpiresAt   int64  `json:"expiresAt"`
}
type SessionProjectChoiceDTO struct {
	ProjectID    string `json:"projectId"`
	Name         string `json:"name"`
	RelativePath string `json:"relativePath"`
}
type SessionProjectsOutput struct {
	Projects   []SessionProjectChoiceDTO `json:"projects"`
	NextOffset *int                      `json:"nextOffset,omitempty"`
}
type SessionProjectBindingOutput struct {
	RequestID string `json:"requestId"`
	ThreadID  string `json:"threadId"`
	ProjectID string `json:"projectId"`
	Status    string `json:"status"`
	ExpiresAt int64  `json:"expiresAt"`
}
type SessionCatalogEntryDTO struct {
	ThreadID         string `json:"threadId"`
	DisplayName      string `json:"displayName"`
	WorkspaceID      string `json:"workspaceId,omitempty"`
	Project          string `json:"project"`
	Status           string `json:"status"`
	UpdatedAt        int64  `json:"updatedAt"`
	ConnectionStatus string `json:"connectionStatus"`
	CanResume        bool   `json:"canResume"`
}
type SessionCatalogOutput struct {
	Sessions   []SessionCatalogEntryDTO `json:"sessions"`
	NextCursor string                   `json:"nextCursor,omitempty"`
}
type TaskStartOutput struct {
	TaskID        string               `json:"taskId"`
	Status        domain.TaskStatus    `json:"status"`
	ThreadID      *string              `json:"threadId"`
	ExecutionMode domain.ExecutionMode `json:"executionMode,omitempty"`
	ApprovalID    string               `json:"approvalId,omitempty"`
	CreatedAt     int64                `json:"createdAt"`
}
type TaskDTO struct {
	ID            string                 `json:"id"`
	WorkspaceID   string                 `json:"workspaceId"`
	ThreadID      *string                `json:"threadId"`
	TurnID        *string                `json:"turnId"`
	Status        domain.TaskStatus      `json:"status"`
	ExecutionMode domain.ExecutionMode   `json:"executionMode"`
	Progress      []domain.ProgressEvent `json:"progress"`
	Result        *string                `json:"result"`
	Diff          *string                `json:"diff"`
	Error         *string                `json:"error"`
	OutputExpired bool                   `json:"outputExpired"`
	CreatedAt     int64                  `json:"createdAt"`
	UpdatedAt     int64                  `json:"updatedAt"`
}
type TaskOutput struct {
	Task TaskDTO `json:"task"`
}
type ProgressOutput struct {
	TaskID        string                 `json:"taskId"`
	Status        domain.TaskStatus      `json:"status"`
	Events        []domain.ProgressEvent `json:"events"`
	OutputExpired bool                   `json:"outputExpired"`
}
type ResultOutput struct {
	TaskID        string            `json:"taskId"`
	Status        domain.TaskStatus `json:"status"`
	Result        *string           `json:"result"`
	Error         *string           `json:"error"`
	OutputExpired bool              `json:"outputExpired"`
}
type ChangesOutput struct {
	TaskID        string            `json:"taskId"`
	Status        domain.TaskStatus `json:"status"`
	Diff          *string           `json:"diff"`
	OutputExpired bool              `json:"outputExpired"`
}
type CancelOutput struct {
	TaskID string            `json:"taskId"`
	Status domain.TaskStatus `json:"status"`
}
type RegistrationApprovalDTO struct {
	RequestID   string `json:"requestId"`
	WorkspaceID string `json:"workspaceId"`
	RootPath    string `json:"rootPath"`
	ExpiresAt   int64  `json:"expiresAt"`
}
type OperationApprovalDTO struct {
	ID             string                `json:"id"`
	WorkspaceID    *string               `json:"workspaceId"`
	OperationType  string                `json:"operationType"`
	TargetResource string                `json:"targetResource"`
	Parameters     any                   `json:"parameters"`
	ParametersHash string                `json:"parametersHash"`
	ArtifactHash   *string               `json:"artifactHash"`
	Status         domain.ApprovalStatus `json:"status"`
	ExpiresAt      int64                 `json:"expiresAt"`
}
type ApprovalListOutput struct {
	WorkspaceRegistrations           []RegistrationApprovalDTO `json:"workspaceRegistrations"`
	WorkspaceRegistrationsNextOffset *int                      `json:"workspaceRegistrationsNextOffset,omitempty"`
	OperationApprovals               []OperationApprovalDTO    `json:"operationApprovals"`
	OperationApprovalsNextOffset     *int                      `json:"operationApprovalsNextOffset,omitempty"`
}
type ApprovalDetailsOutput struct {
	Details map[string]any `json:"details"`
}
type StatusOutput struct {
	Status         string `json:"status"`
	UptimeSeconds  int64  `json:"uptimeSeconds"`
	CodexAppServer string `json:"codexAppServer"`
	ActiveTasks    int    `json:"activeTasks"`
	QueuedTasks    int    `json:"queuedTasks"`
}
type CapabilitiesOutput struct {
	MCPTransport        string `json:"mcpTransport"`
	CodexTransport      string `json:"codexTransport"`
	Workspaces          string `json:"workspaces"`
	CodexFilesystem     string `json:"codexFilesystem"`
	WorkspaceWrite      string `json:"workspaceWrite"`
	CodexNetwork        string `json:"codexNetwork"`
	CodexMCPServers     string `json:"codexMcpServers"`
	PrivilegedExecution string `json:"privilegedExecution"`
	SecureTunnel        string `json:"secureTunnel"`
	ChatGPTConnection   string `json:"chatgptConnection"`
}

var taskIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

const maxToolResponseBytes = 1 << 20
const firstStatelessProtocolVersion = "2026-07-28"

func NewServer(deps Dependencies) *mcp.Server {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "codex-chatgpt-bridge", Title: "Codex ChatGPT Bridge", Version: "0.2.0"}, &mcp.ServerOptions{Logger: logger, KeepAlive: 30 * time.Second})

	register(server, deps, "workspace_list", "List approved workspaces using stable offset pages of up to 10 entries. Omitted limit defaults to 10; use nextOffset to continue.", true, false, true, func(ctx context.Context, in PageInput) (WorkspaceListOutput, error) {
		limit, offset := in.values()
		values, nextOffset, err := deps.Workspaces.ListPage(ctx, limit, offset)
		if err != nil {
			return WorkspaceListOutput{}, err
		}
		out := WorkspaceListOutput{Workspaces: make([]WorkspaceDTO, 0, len(values)), NextOffset: nextOffset}
		for _, item := range values {
			out.Workspaces = append(out.Workspaces, workspaceDTO(item))
		}
		return out, nil
	})
	register(server, deps, "workspace_get", "Get one approved workspace and revalidate its filesystem identity.", true, false, true, func(ctx context.Context, in WorkspaceIDInput) (WorkspaceOutput, error) {
		value, err := deps.Workspaces.Get(ctx, in.WorkspaceID)
		return WorkspaceOutput{Workspace: workspaceDTO(value)}, err
	})
	register(server, deps, "workspace_register", "Request workspace access. The request remains pending until approved through the local bridge-admin command.", false, false, false, func(ctx context.Context, in WorkspaceRegisterInput) (RegistrationOutput, error) {
		if len(in.Path) > 4096 {
			return RegistrationOutput{}, domain.ErrInvalidInput
		}
		request, err := deps.Workspaces.RequestRegistration(ctx, in.WorkspaceID, in.Path)
		if err != nil {
			return RegistrationOutput{}, err
		}
		return RegistrationOutput{RequestID: request.ID, WorkspaceID: request.WorkspaceID, Status: request.Status, ExpiresAt: request.ExpiresAt.UnixMilli()}, nil
	})
	register(server, deps, "codex_list_threads", "List bridge-created threads and Codex CLI threads explicitly attached to this approved workspace. Unattached CLI sessions are never listed here.", true, false, true, func(ctx context.Context, in WorkspaceIDInput) (ThreadListOutput, error) {
		values, err := deps.Tasks.ListThreads(ctx, in.WorkspaceID)
		if err != nil {
			return ThreadListOutput{}, err
		}
		out := ThreadListOutput{Threads: make([]ThreadSummaryDTO, 0, len(values))}
		for _, item := range values {
			out.Threads = append(out.Threads, ThreadSummaryDTO{ThreadID: item.ThreadID, TaskID: item.TaskID, Status: item.Status, Source: item.Source, UpdatedAt: item.UpdatedAt})
		}
		return out, nil
	})
	register(server, deps, "codex_list_local_sessions", "List one metadata-only page of local Codex CLI sessions sorted by recent activity. Global catalog access must first be granted locally; each result includes the Codex thread name or a sanitized, bounded thread preview. connectionStatus trusted_root means the session can resume read-only across repositories beneath exactly one approved root; trusted means repository-scoped. Unapproved locations cannot be resumed and conversation history is never returned.", true, false, true, func(ctx context.Context, in SessionCatalogInput) (SessionCatalogOutput, error) {
		if deps.Sessions == nil {
			return SessionCatalogOutput{}, domain.ErrServiceUnavailable
		}
		if len(in.WorkspaceID) > 63 || len(in.Search) > 200 || len(in.Cursor) > 8192 {
			return SessionCatalogOutput{}, domain.ErrInvalidInput
		}
		limit := 0
		if in.Limit != nil {
			limit = *in.Limit
		}
		page, err := deps.Sessions.ListCatalog(ctx, in.WorkspaceID, in.Search, in.Cursor, limit)
		if err != nil {
			return SessionCatalogOutput{}, err
		}
		out := SessionCatalogOutput{Sessions: make([]SessionCatalogEntryDTO, 0, len(page.Sessions)), NextCursor: page.NextCursor}
		for _, item := range page.Sessions {
			out.Sessions = append(out.Sessions, SessionCatalogEntryDTO{ThreadID: item.ThreadID, DisplayName: item.DisplayName, WorkspaceID: item.WorkspaceID, Project: item.Project, Status: item.Status, UpdatedAt: item.UpdatedAt.UnixMilli(), ConnectionStatus: item.ConnectionStatus, CanResume: item.CanResume})
		}
		return out, nil
	})
	register(server, deps, "codex_list_session_projects", "List bounded, root-relative Git repository choices for a legacy Codex CLI session marked project_selection_required. Selecting a project only creates a local owner approval request; it does not grant access or enable writes. No conversation history or absolute paths are returned.", true, false, true, func(ctx context.Context, in SessionProjectsInput) (SessionProjectsOutput, error) {
		if deps.Sessions == nil || len(in.ThreadID) == 0 || len(in.ThreadID) > 256 {
			return SessionProjectsOutput{}, domain.ErrInvalidInput
		}
		limit, offset := 25, 0
		if in.Limit != nil {
			limit = *in.Limit
		}
		if in.Offset != nil {
			offset = *in.Offset
		}
		choices, next, err := deps.Sessions.ListProjectChoices(ctx, in.ThreadID, offset, limit)
		if err != nil {
			return SessionProjectsOutput{}, err
		}
		out := SessionProjectsOutput{Projects: make([]SessionProjectChoiceDTO, 0, len(choices)), NextOffset: next}
		for _, choice := range choices {
			out.Projects = append(out.Projects, SessionProjectChoiceDTO{ProjectID: choice.ProjectID, Name: choice.Name, RelativePath: choice.RelativePath})
		}
		return out, nil
	})
	register(server, deps, "codex_start_task", "Start an asynchronous read-only Codex task in an approved workspace.", false, false, false, func(ctx context.Context, in TaskStartInput) (TaskStartOutput, error) {
		if len(in.Prompt) > 64<<10 {
			return TaskStartOutput{}, domain.ErrInvalidInput
		}
		task, err := deps.Tasks.Start(ctx, in.WorkspaceID, in.Prompt, "")
		if err != nil {
			return TaskStartOutput{}, err
		}
		return taskStartDTO(task), nil
	})
	register(server, deps, "codex_start_write_task", "Request a source-changing Codex task. It remains pending until this exact prompt is approved once through local bridge-admin; execution uses the approved workspace-write sandbox with network disabled.", false, true, false, func(ctx context.Context, in TaskWriteInput) (TaskStartOutput, error) {
		if len(in.Prompt) > 64<<10 || len(in.ThreadID) > 256 {
			return TaskStartOutput{}, domain.ErrInvalidInput
		}
		if in.ThreadID != "" {
			if deps.Sessions == nil {
				return TaskStartOutput{}, domain.ErrServiceUnavailable
			}
			if err := deps.Sessions.AuthorizeResume(ctx, in.WorkspaceID, in.ThreadID); err != nil {
				return TaskStartOutput{}, err
			}
		}
		task, approval, err := deps.Tasks.StartWrite(ctx, in.WorkspaceID, in.Prompt, in.ThreadID)
		if err != nil {
			return TaskStartOutput{}, err
		}
		output := taskStartDTO(task)
		output.ApprovalID = approval.ID
		return output, nil
	})
	register(server, deps, "codex_request_session_attach", "Request local one-time approval to attach a Codex CLI session from an untrusted workspace. Trusted workspaces can resume sessions without per-session attach approval.", false, false, false, func(ctx context.Context, in SessionAttachInput) (SessionAttachOutput, error) {
		if len(in.ThreadID) == 0 || len(in.ThreadID) > 256 {
			return SessionAttachOutput{}, domain.ErrInvalidInput
		}
		if deps.Sessions == nil {
			return SessionAttachOutput{}, domain.ErrServiceUnavailable
		}
		request, err := deps.Sessions.RequestAttach(ctx, in.WorkspaceID, in.ThreadID)
		if err != nil {
			return SessionAttachOutput{}, err
		}
		return SessionAttachOutput{RequestID: request.ID, ThreadID: request.ThreadID, WorkspaceID: request.WorkspaceID, Status: request.Status, ExpiresAt: request.ExpiresAt.UnixMilli()}, nil
	})
	register(server, deps, "codex_request_session_project_binding", "Request local owner approval to bind a legacy Codex CLI thread to one repository selected from codex_list_session_projects. Approval is tied to the thread and filesystem identities, expires, and grants read-only session resume only; write approval remains per task.", false, false, false, func(ctx context.Context, in SessionProjectBindingInput) (SessionProjectBindingOutput, error) {
		if deps.Sessions == nil || len(in.ThreadID) == 0 || len(in.ThreadID) > 256 || len(in.ProjectID) != 32 {
			return SessionProjectBindingOutput{}, domain.ErrInvalidInput
		}
		binding, err := deps.Sessions.RequestProjectBinding(ctx, in.ThreadID, in.ProjectID)
		if err != nil {
			return SessionProjectBindingOutput{}, err
		}
		return SessionProjectBindingOutput{RequestID: binding.ID, ThreadID: binding.ThreadID, ProjectID: binding.ProjectID, Status: binding.Status, ExpiresAt: binding.ExpiresAt.UnixMilli()}, nil
	})
	register(server, deps, "codex_resume_thread", "Continue one explicit bridge-owned, individually attached, or trusted-root CLI thread. Workspace and thread IDs are required; repository sessions stay repository-scoped and sessions started at a trusted root get read-only access only within that root. The current sandbox is reapplied on every resume; root-scoped writes are denied and repository writes still require separate per-task approval.", false, false, false, func(ctx context.Context, in TaskResumeInput) (TaskStartOutput, error) {
		if len(in.Prompt) > 64<<10 || len(in.ThreadID) == 0 || len(in.ThreadID) > 256 {
			return TaskStartOutput{}, domain.ErrInvalidInput
		}
		if deps.Sessions == nil {
			return TaskStartOutput{}, domain.ErrServiceUnavailable
		}
		if err := deps.Sessions.AuthorizeResume(ctx, in.WorkspaceID, in.ThreadID); err != nil {
			return TaskStartOutput{}, err
		}
		task, err := deps.Tasks.Start(ctx, in.WorkspaceID, in.Prompt, in.ThreadID)
		if err != nil {
			return TaskStartOutput{}, err
		}
		return taskStartDTO(task), nil
	})
	register(server, deps, "codex_get_task", "Get status and bounded task outputs.", true, false, true, func(ctx context.Context, in TaskIDInput) (TaskOutput, error) {
		if !validTaskID(in.TaskID) {
			return TaskOutput{}, domain.ErrInvalidInput
		}
		task, err := deps.Tasks.Get(ctx, in.TaskID)
		return TaskOutput{Task: taskDTO(task)}, err
	})
	register(server, deps, "codex_get_progress", "Get recent progress events for a task.", true, false, true, func(ctx context.Context, in TaskIDInput) (ProgressOutput, error) {
		if !validTaskID(in.TaskID) {
			return ProgressOutput{}, domain.ErrInvalidInput
		}
		task, err := deps.Tasks.GetProgress(ctx, in.TaskID)
		if err != nil {
			return ProgressOutput{}, err
		}
		return ProgressOutput{TaskID: task.ID, Status: task.Status, Events: task.Progress, OutputExpired: task.OutputExpired}, nil
	})
	register(server, deps, "codex_get_result", "Get the final result or safe error for a task.", true, false, true, func(ctx context.Context, in TaskIDInput) (ResultOutput, error) {
		if !validTaskID(in.TaskID) {
			return ResultOutput{}, domain.ErrInvalidInput
		}
		task, err := deps.Tasks.GetResult(ctx, in.TaskID)
		if err != nil {
			return ResultOutput{}, err
		}
		return ResultOutput{TaskID: task.ID, Status: task.Status, Result: task.Result, Error: task.Error, OutputExpired: task.OutputExpired}, nil
	})
	register(server, deps, "codex_get_changes", "Get the bounded diff reported by a task.", true, false, true, func(ctx context.Context, in TaskIDInput) (ChangesOutput, error) {
		if !validTaskID(in.TaskID) {
			return ChangesOutput{}, domain.ErrInvalidInput
		}
		task, err := deps.Tasks.GetChanges(ctx, in.TaskID)
		if err != nil {
			return ChangesOutput{}, err
		}
		return ChangesOutput{TaskID: task.ID, Status: task.Status, Diff: task.Diff, OutputExpired: task.OutputExpired}, nil
	})
	register(server, deps, "codex_cancel_task", "Cancel a queued or running task.", false, true, false, func(ctx context.Context, in TaskIDInput) (CancelOutput, error) {
		if !validTaskID(in.TaskID) {
			return CancelOutput{}, domain.ErrInvalidInput
		}
		task, err := deps.Tasks.Cancel(ctx, in.TaskID)
		return CancelOutput{TaskID: task.ID, Status: task.Status}, err
	})
	register(server, deps, "approval_list_pending", "List pending workspace registrations and operation approvals with separate nextOffset values. Each list returns up to 10 entries; approvals remain local.", true, false, true, func(ctx context.Context, in PageInput) (ApprovalListOutput, error) {
		limit, offset := in.values()
		requests, requestsNext, approvals, approvalsNext, err := deps.Approvals.ListPendingPage(ctx, limit, offset)
		if err != nil {
			return ApprovalListOutput{}, err
		}
		out := ApprovalListOutput{WorkspaceRegistrations: make([]RegistrationApprovalDTO, 0, len(requests)), WorkspaceRegistrationsNextOffset: requestsNext, OperationApprovals: make([]OperationApprovalDTO, 0, len(approvals)), OperationApprovalsNextOffset: approvalsNext}
		for _, request := range requests {
			out.WorkspaceRegistrations = append(out.WorkspaceRegistrations, RegistrationApprovalDTO{RequestID: request.ID, WorkspaceID: request.WorkspaceID, RootPath: request.RootPath, ExpiresAt: request.ExpiresAt.UnixMilli()})
		}
		for _, item := range approvals {
			var params any
			_ = jsonUnmarshalString(item.ParametersJSON, &params)
			out.OperationApprovals = append(out.OperationApprovals, OperationApprovalDTO{ID: item.ID, WorkspaceID: optionalString(item.WorkspaceID), OperationType: security.RedactSecrets(item.OperationType), TargetResource: security.RedactSecrets(item.TargetResource), Parameters: params, ParametersHash: item.ParametersHash, ArtifactHash: optionalString(item.ArtifactHash), Status: item.Status, ExpiresAt: item.ExpiresAt.UnixMilli()})
		}
		return out, nil
	})
	register(server, deps, "approval_get_details", "Get a pending approval request with secret-like values redacted. Approval must be performed locally.", true, false, true, func(ctx context.Context, in ApprovalIDInput) (ApprovalDetailsOutput, error) {
		if !validTaskID(in.ApprovalID) {
			return ApprovalDetailsOutput{}, domain.ErrInvalidInput
		}
		details, err := deps.Approvals.Get(ctx, in.ApprovalID)
		return ApprovalDetailsOutput{Details: details}, err
	})
	register(server, deps, "bridge_get_status", "Get authenticated bridge, App Server, and worker status.", true, false, true, func(ctx context.Context, _ Empty) (StatusOutput, error) {
		if _, ok := security.Authorize(ctx); !ok {
			return StatusOutput{}, domain.ErrNotAuthorized
		}
		active, queued, err := deps.Tasks.Stats(ctx)
		if err != nil {
			return StatusOutput{}, err
		}
		connected := "disconnected"
		if deps.Codex != nil && deps.Codex.Connected() {
			connected = "connected"
		}
		return StatusOutput{Status: "ok", UptimeSeconds: int64(time.Since(deps.StartedAt).Seconds()), CodexAppServer: connected, ActiveTasks: active, QueuedTasks: queued}, nil
	})
	register(server, deps, "bridge_get_capabilities", "List supported local bridge capabilities and external connection state.", true, false, true, func(ctx context.Context, _ Empty) (CapabilitiesOutput, error) {
		if _, ok := security.Authorize(ctx); !ok {
			return CapabilitiesOutput{}, domain.ErrNotAuthorized
		}
		return CapabilitiesOutput{MCPTransport: "streamable-http", CodexTransport: "stdio-jsonl", Workspaces: "explicit-registration", CodexFilesystem: "read-only-by-default", WorkspaceWrite: "workspace-write after local per-task approval", CodexNetwork: "sandboxed-command-network-disabled", CodexMCPServers: "disabled", PrivilegedExecution: "mock-only", SecureTunnel: "not-configured", ChatGPTConnection: "not-tested"}, nil
	})
	return server
}

func NewHTTPHandler(deps Dependencies) http.Handler {
	server := NewServer(deps)
	newStreamableHandler := func(stateless bool) http.Handler {
		return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{JSONResponse: true, Stateless: stateless, SessionTimeout: 30 * time.Minute, MaxRequestBodyBytes: deps.Config.MaxRequestBytes})
	}
	legacyMCPHandler := newStreamableHandler(false)
	statelessMCPHandler := newStreamableHandler(true)
	mcpHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// MCP protocol versions are ISO dates; 2026-07-28 introduced stateless per-request metadata.
		if r.Header.Get("MCP-Protocol-Version") >= firstStatelessProtocolVersion {
			statelessMCPHandler.ServeHTTP(w, r)
			return
		}
		legacyMCPHandler.ServeHTTP(w, r)
	})
	mux := http.NewServeMux()
	var authenticate func(http.Handler) http.Handler
	switch deps.Config.AuthMode {
	case config.AuthModeBearer:
		authenticate = func(next http.Handler) http.Handler {
			return security.AuthMiddlewareForHost(deps.Config.AuthToken, deps.Config.Host, deps.Config.Port, next)
		}
	case config.AuthModeCloudflareAccess:
		authenticate = func(next http.Handler) http.Handler {
			return security.CloudflareAccessMiddlewareForHost(deps.CloudflareVerifier, net.JoinHostPort(deps.Config.Host, strconv.Itoa(deps.Config.Port)), next)
		}
	default:
		authenticate = func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "Authentication unavailable", http.StatusServiceUnavailable)
			})
		}
	}
	mux.Handle("/mcp", authenticate(mcpHandler))
	mux.Handle("/healthz", authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})))
	return http.NewCrossOriginProtection().Handler(mux)
}

func register[In any, Out any](server *mcp.Server, deps Dependencies, name, description string, readOnly, destructive, idempotent bool, handler func(context.Context, In) (Out, error)) {
	annotations := &mcp.ToolAnnotations{ReadOnlyHint: readOnly, IdempotentHint: idempotent, OpenWorldHint: boolPtr(false)}
	if !readOnly {
		annotations.DestructiveHint = boolPtr(destructive)
	}
	mcp.AddTool(server, &mcp.Tool{Name: name, Title: name, Description: description, Annotations: annotations, InputSchema: inputSchema[In]()}, func(ctx context.Context, _ *mcp.CallToolRequest, input In) (*mcp.CallToolResult, Out, error) {
		principal, ok := security.Authorize(ctx)
		if !ok {
			var zero Out
			return toolError(domain.ErrNotAuthorized), zero, nil
		}
		workspaceID, taskID := auditIDs(input)
		if deps.Audit != nil {
			if err := deps.Audit.Record(ctx, audit.Event{Operation: "mcp.tool." + name, Principal: principal, WorkspaceID: workspaceID, TaskID: taskID, Outcome: "requested"}); err != nil {
				var zero Out
				return toolError(domain.ErrServiceUnavailable), zero, nil
			}
		}
		output, err := handler(ctx, input)
		if err == nil {
			encoded, marshalErr := json.Marshal(output)
			switch {
			case marshalErr != nil:
				err = domain.ErrServiceUnavailable
			case len(encoded) > maxToolResponseBytes:
				err = domain.ErrResponseTooLarge
			}
		}
		if err != nil {
			if deps.Audit != nil {
				_ = deps.Audit.Record(ctx, audit.Event{Operation: "mcp.tool." + name, Principal: principal, WorkspaceID: workspaceID, TaskID: taskID, Outcome: "failed", ErrorClass: errorClass(err)})
			}
			var zero Out
			return toolError(err), zero, nil
		}
		if deps.Audit != nil {
			_ = deps.Audit.Record(ctx, audit.Event{Operation: "mcp.tool." + name, Principal: principal, WorkspaceID: workspaceID, TaskID: taskID, Outcome: "success"})
		}
		return nil, output, nil
	})
}

func toolError(err error) *mcp.CallToolResult {
	text := "Bridge operation failed"
	switch {
	case errors.Is(err, domain.ErrNotAuthorized):
		text = "Resource is not authorized"
	case errors.Is(err, domain.ErrNotFound):
		text = "Resource not found"
	case errors.Is(err, domain.ErrInvalidInput):
		text = "Invalid input"
	case errors.Is(err, domain.ErrConflict):
		text = "Resource state conflict"
	case errors.Is(err, domain.ErrQueueFull):
		text = "Task queue is full"
	case errors.Is(err, domain.ErrServiceUnavailable):
		text = "Bridge service is unavailable"
	case errors.Is(err, domain.ErrResponseTooLarge):
		text = "Response exceeds bridge output limit"
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: true}
}

func errorClass(err error) string {
	switch {
	case errors.Is(err, domain.ErrNotAuthorized):
		return "unauthorized"
	case errors.Is(err, domain.ErrNotFound):
		return "not_found"
	case errors.Is(err, domain.ErrInvalidInput):
		return "invalid_input"
	case errors.Is(err, domain.ErrConflict):
		return "conflict"
	case errors.Is(err, domain.ErrQueueFull):
		return "queue_full"
	case errors.Is(err, domain.ErrServiceUnavailable):
		return "unavailable"
	case errors.Is(err, domain.ErrResponseTooLarge):
		return "response_too_large"
	default:
		return "internal"
	}
}

func auditIDs(value any) (string, string) {
	switch input := value.(type) {
	case SessionCatalogInput:
		return input.WorkspaceID, ""
	case WorkspaceIDInput:
		return input.WorkspaceID, ""
	case WorkspaceRegisterInput:
		return input.WorkspaceID, ""
	case TaskStartInput:
		return input.WorkspaceID, ""
	case TaskResumeInput:
		return input.WorkspaceID, ""
	case TaskWriteInput:
		return input.WorkspaceID, ""
	case TaskIDInput:
		return "", input.TaskID
	default:
		return "", ""
	}
}

func workspaceDTO(value domain.Workspace) WorkspaceDTO {
	git := optionalString(value.GitRepository)
	filesystem, operations, approvals := "read-only", []string{"analysis"}, []string{"workspace-registration"}
	if value.WriteEnabled {
		filesystem = "workspace-write-after-approval"
		operations = append(operations, "workspace-write")
		approvals = append(approvals, "write-task")
	}
	return WorkspaceDTO{ID: value.ID, RootPath: value.RootPath, AllowedDirectories: []string{value.RootPath}, DefaultModel: nil, SandboxPermissions: SandboxPermissionsDTO{Filesystem: filesystem, Network: false}, AllowedOperations: operations, ApprovalRequirements: approvals, WriteEnabled: value.WriteEnabled, SessionTrusted: value.SessionTrusted, MaxConcurrentTasks: 1, TimeoutMS: 20 * 60 * 1000, GitRepository: git, CreatedAt: value.CreatedAt.UnixMilli()}
}

func taskStartDTO(task domain.Task) TaskStartOutput {
	return TaskStartOutput{TaskID: task.ID, Status: task.Status, ThreadID: optionalString(task.ThreadID), ExecutionMode: task.ExecutionMode, CreatedAt: task.CreatedAt.UnixMilli()}
}
func taskDTO(task domain.Task) TaskDTO {
	return TaskDTO{ID: task.ID, WorkspaceID: task.WorkspaceID, ThreadID: optionalString(task.ThreadID), TurnID: optionalString(task.TurnID), Status: task.Status, ExecutionMode: task.ExecutionMode, Progress: task.Progress, Result: task.Result, Diff: task.Diff, Error: task.Error, OutputExpired: task.OutputExpired, CreatedAt: task.CreatedAt.UnixMilli(), UpdatedAt: task.UpdatedAt.UnixMilli()}
}
func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
func boolPtr(value bool) *bool      { return &value }
func validTaskID(value string) bool { return taskIDPattern.MatchString(value) }
func jsonUnmarshalString(value string, output any) error {
	return json.Unmarshal([]byte(value), output)
}

func inputSchema[In any]() any {
	switch any(*new(In)).(type) {
	case Empty:
		return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
	case WorkspaceIDInput:
		return objectSchema(map[string]any{"workspaceId": stringSchema("Registered workspace identifier", 2, 63, `^[a-z][a-z0-9-]{1,62}$`)}, "workspaceId")
	case SessionCatalogInput:
		return objectSchema(map[string]any{
			"workspaceId": stringSchema("Optional approved workspace ID filter", 0, 63, `^[a-z][a-z0-9-]{1,62}$`),
			"search":      stringSchema("Optional Codex thread title search", 0, 200, ""),
			"limit":       map[string]any{"type": "integer", "minimum": 1, "maximum": 50},
			"cursor":      stringSchema("Opaque App Server cursor from the previous page", 0, 8192, ""),
		})
	case WorkspaceRegisterInput:
		return objectSchema(map[string]any{"workspaceId": stringSchema("Requested workspace identifier", 2, 63, `^[a-z][a-z0-9-]{1,62}$`), "path": stringSchema("Absolute local directory path", 1, 4096, "")}, "workspaceId", "path")
	case TaskStartInput:
		return objectSchema(map[string]any{"workspaceId": stringSchema("Approved workspace identifier", 2, 63, `^[a-z][a-z0-9-]{1,62}$`), "prompt": stringSchema("Task prompt", 1, 20000, "")}, "workspaceId", "prompt")
	case TaskResumeInput:
		return objectSchema(map[string]any{"workspaceId": stringSchema("Approved workspace identifier", 2, 63, `^[a-z][a-z0-9-]{1,62}$`), "threadId": stringSchema("Bridge-owned thread identifier", 1, 256, ""), "prompt": stringSchema("Task prompt", 1, 20000, "")}, "workspaceId", "threadId", "prompt")
	case TaskWriteInput:
		return objectSchema(map[string]any{"workspaceId": stringSchema("Locally write-enabled workspace identifier", 2, 63, `^[a-z][a-z0-9-]{1,62}$`), "prompt": stringSchema("Task prompt requiring local one-time approval", 1, 20000, ""), "threadId": stringSchema("Optional bridge-owned thread identifier", 0, 256, "")}, "workspaceId", "prompt")
	case TaskIDInput:
		return objectSchema(map[string]any{"taskId": stringSchema("Task UUID", 36, 36, taskIDPattern.String())}, "taskId")
	case ApprovalIDInput:
		return objectSchema(map[string]any{"approvalId": stringSchema("Approval request UUID", 36, 36, taskIDPattern.String())}, "approvalId")
	default:
		return nil
	}
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func stringSchema(description string, minimum, maximum int, pattern string) map[string]any {
	schema := map[string]any{"type": "string", "description": description, "minLength": minimum, "maxLength": maximum}
	if pattern != "" {
		schema["pattern"] = pattern
	}
	return schema
}
