package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"example.com/codex-chatgpt-bridge/internal/audit"
	"example.com/codex-chatgpt-bridge/internal/config"
	"example.com/codex-chatgpt-bridge/internal/domain"
	"example.com/codex-chatgpt-bridge/internal/security"
	"example.com/codex-chatgpt-bridge/internal/service"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type CodexStatus interface{ Connected() bool }

type Dependencies struct {
	Config     config.Config
	Workspaces *service.WorkspaceService
	Tasks      *service.TaskService
	Approvals  *service.ApprovalService
	Audit      *audit.Logger
	Codex      CodexStatus
	StartedAt  time.Time
	Logger     *slog.Logger
}

type Empty struct{}
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
	ThreadID  string            `json:"threadId"`
	TaskID    string            `json:"taskId"`
	Status    domain.TaskStatus `json:"status"`
	UpdatedAt int64             `json:"updatedAt"`
}
type ThreadListOutput struct {
	Threads []ThreadSummaryDTO `json:"threads"`
}
type TaskStartOutput struct {
	TaskID    string            `json:"taskId"`
	Status    domain.TaskStatus `json:"status"`
	ThreadID  *string           `json:"threadId"`
	CreatedAt int64             `json:"createdAt"`
}
type TaskDTO struct {
	ID            string                 `json:"id"`
	WorkspaceID   string                 `json:"workspaceId"`
	ThreadID      *string                `json:"threadId"`
	TurnID        *string                `json:"turnId"`
	Status        domain.TaskStatus      `json:"status"`
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
	WorkspaceRegistrations []RegistrationApprovalDTO `json:"workspaceRegistrations"`
	OperationApprovals     []OperationApprovalDTO    `json:"operationApprovals"`
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
	CodexNetwork        string `json:"codexNetwork"`
	CodexMCPServers     string `json:"codexMcpServers"`
	PrivilegedExecution string `json:"privilegedExecution"`
	SecureTunnel        string `json:"secureTunnel"`
	ChatGPTConnection   string `json:"chatgptConnection"`
}

var taskIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

func NewServer(deps Dependencies) *mcp.Server {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "codex-chatgpt-bridge", Title: "Codex ChatGPT Bridge", Version: "0.2.0"}, &mcp.ServerOptions{Logger: logger, KeepAlive: 30 * time.Second})

	register(server, deps, "workspace_list", "List explicitly registered workspaces.", true, false, true, func(ctx context.Context, _ Empty) (WorkspaceListOutput, error) {
		values, err := deps.Workspaces.List(ctx)
		if err != nil {
			return WorkspaceListOutput{}, err
		}
		out := WorkspaceListOutput{Workspaces: make([]WorkspaceDTO, 0, len(values))}
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
	register(server, deps, "codex_list_threads", "List threads created and owned by this bridge for the selected workspace.", true, false, true, func(ctx context.Context, in WorkspaceIDInput) (ThreadListOutput, error) {
		values, err := deps.Tasks.ListThreads(ctx, in.WorkspaceID)
		if err != nil {
			return ThreadListOutput{}, err
		}
		out := ThreadListOutput{Threads: make([]ThreadSummaryDTO, 0, len(values))}
		for _, item := range values {
			out.Threads = append(out.Threads, ThreadSummaryDTO{ThreadID: item.ThreadID, TaskID: item.TaskID, Status: item.Status, UpdatedAt: item.UpdatedAt})
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
	register(server, deps, "codex_resume_thread", "Continue a thread previously created by this bridge in the same approved workspace.", false, false, false, func(ctx context.Context, in TaskResumeInput) (TaskStartOutput, error) {
		if len(in.Prompt) > 64<<10 || len(in.ThreadID) == 0 || len(in.ThreadID) > 256 {
			return TaskStartOutput{}, domain.ErrInvalidInput
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
	register(server, deps, "approval_list_pending", "List pending workspace registration and operation approval details. Approvals remain local.", true, false, true, func(ctx context.Context, _ Empty) (ApprovalListOutput, error) {
		requests, approvals, err := deps.Approvals.ListPending(ctx)
		if err != nil {
			return ApprovalListOutput{}, err
		}
		out := ApprovalListOutput{WorkspaceRegistrations: make([]RegistrationApprovalDTO, 0, len(requests)), OperationApprovals: make([]OperationApprovalDTO, 0, len(approvals))}
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
		return CapabilitiesOutput{MCPTransport: "streamable-http", CodexTransport: "stdio-jsonl", Workspaces: "explicit-registration", CodexFilesystem: "workspace-confined-read-only; App Server >= 0.138.0", CodexNetwork: "sandboxed-command-network-disabled", CodexMCPServers: "disabled", PrivilegedExecution: "mock-only", SecureTunnel: "not-configured", ChatGPTConnection: "not-tested"}, nil
	})
	return server
}

func NewHTTPHandler(deps Dependencies) http.Handler {
	server := NewServer(deps)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{JSONResponse: true, SessionTimeout: 30 * time.Minute, MaxRequestBodyBytes: deps.Config.MaxRequestBytes})
	mux := http.NewServeMux()
	mux.Handle("/mcp", security.AuthMiddlewareForHost(deps.Config.AuthToken, deps.Config.Host, deps.Config.Port, mcpHandler))
	mux.Handle("/healthz", security.AuthMiddlewareForHost(deps.Config.AuthToken, deps.Config.Host, deps.Config.Port, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	default:
		return "internal"
	}
}

func auditIDs(value any) (string, string) {
	switch input := value.(type) {
	case WorkspaceIDInput:
		return input.WorkspaceID, ""
	case WorkspaceRegisterInput:
		return input.WorkspaceID, ""
	case TaskStartInput:
		return input.WorkspaceID, ""
	case TaskResumeInput:
		return input.WorkspaceID, ""
	case TaskIDInput:
		return "", input.TaskID
	default:
		return "", ""
	}
}

func workspaceDTO(value domain.Workspace) WorkspaceDTO {
	git := optionalString(value.GitRepository)
	return WorkspaceDTO{ID: value.ID, RootPath: value.RootPath, AllowedDirectories: []string{value.RootPath}, DefaultModel: nil, SandboxPermissions: SandboxPermissionsDTO{Filesystem: "read-only", Network: false}, AllowedOperations: []string{"analysis"}, ApprovalRequirements: []string{"workspace-registration"}, MaxConcurrentTasks: 1, TimeoutMS: 20 * 60 * 1000, GitRepository: git, CreatedAt: value.CreatedAt.UnixMilli()}
}

func taskStartDTO(task domain.Task) TaskStartOutput {
	return TaskStartOutput{TaskID: task.ID, Status: task.Status, ThreadID: optionalString(task.ThreadID), CreatedAt: task.CreatedAt.UnixMilli()}
}
func taskDTO(task domain.Task) TaskDTO {
	return TaskDTO{ID: task.ID, WorkspaceID: task.WorkspaceID, ThreadID: optionalString(task.ThreadID), TurnID: optionalString(task.TurnID), Status: task.Status, Progress: task.Progress, Result: task.Result, Diff: task.Diff, Error: task.Error, OutputExpired: task.OutputExpired, CreatedAt: task.CreatedAt.UnixMilli(), UpdatedAt: task.UpdatedAt.UnixMilli()}
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
	case WorkspaceRegisterInput:
		return objectSchema(map[string]any{"workspaceId": stringSchema("Requested workspace identifier", 2, 63, `^[a-z][a-z0-9-]{1,62}$`), "path": stringSchema("Absolute local directory path", 1, 4096, "")}, "workspaceId", "path")
	case TaskStartInput:
		return objectSchema(map[string]any{"workspaceId": stringSchema("Approved workspace identifier", 2, 63, `^[a-z][a-z0-9-]{1,62}$`), "prompt": stringSchema("Task prompt", 1, 20000, "")}, "workspaceId", "prompt")
	case TaskResumeInput:
		return objectSchema(map[string]any{"workspaceId": stringSchema("Approved workspace identifier", 2, 63, `^[a-z][a-z0-9-]{1,62}$`), "threadId": stringSchema("Bridge-owned thread identifier", 1, 256, ""), "prompt": stringSchema("Task prompt", 1, 20000, "")}, "workspaceId", "threadId", "prompt")
	case TaskIDInput:
		return objectSchema(map[string]any{"taskId": stringSchema("Task UUID", 36, 36, taskIDPattern.String())}, "taskId")
	case ApprovalIDInput:
		return objectSchema(map[string]any{"approvalId": stringSchema("Approval request UUID", 36, 36, taskIDPattern.String())}, "approvalId")
	default:
		return nil
	}
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}

func stringSchema(description string, minimum, maximum int, pattern string) map[string]any {
	schema := map[string]any{"type": "string", "description": description, "minLength": minimum, "maxLength": maximum}
	if pattern != "" {
		schema["pattern"] = pattern
	}
	return schema
}
