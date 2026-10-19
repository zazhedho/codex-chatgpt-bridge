package mcp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/codex-chatgpt-bridge/internal/audit"
	"example.com/codex-chatgpt-bridge/internal/config"
	"example.com/codex-chatgpt-bridge/internal/domain"
	"example.com/codex-chatgpt-bridge/internal/repository/sqlite"
	"example.com/codex-chatgpt-bridge/internal/security"
	"example.com/codex-chatgpt-bridge/internal/service"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type queueStub struct {
	mu    sync.Mutex
	jobs  []service.ExecutionJob
	store *sqlite.Store
}

func (q *queueStub) Submit(_ context.Context, job service.ExecutionJob) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.jobs = append(q.jobs, job)
	return nil
}
func (q *queueStub) Cancel(_ context.Context, id string) error {
	return q.store.SetTaskStatus(context.Background(), id, domain.TaskCancelled, "Cancelled before execution")
}

type codexStatusStub struct{}

func (codexStatusStub) Connected() bool { return false }

type allowTaskExecution struct{}

func (allowTaskExecution) AuthorizeExecution(context.Context, domain.Workspace) error { return nil }

type bearerTransport struct {
	base  http.RoundTripper
	token string
}

func (t bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}

func TestAllMCPContractsThroughAuthenticatedHTTP(t *testing.T) {
	dataDir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dataDir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	logger := audit.New(store)
	home := t.TempDir()
	workspaces := service.NewWorkspaceService(store, logger, home, "")
	queue := &queueStub{store: store}
	tasks := service.NewTaskService(store, store, workspaces, queue, allowTaskExecution{}, logger, 20_000)
	approvals := service.NewApprovalService(store, workspaces)
	token := strings.Repeat("t", 48)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	cfg := config.Config{Host: "127.0.0.1", Port: port, AuthToken: token, MaxRequestBytes: 64 << 10}
	handler := NewHTTPHandler(Dependencies{Config: cfg, Workspaces: workspaces, Tasks: tasks, Approvals: approvals, Audit: logger, Codex: codexStatusStub{}, StartedAt: time.Now()})
	httpServer := &http.Server{Handler: handler}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/mcp", port)
	client := sdk.NewClient(&sdk.Implementation{Name: "contract-test", Version: "0.0.0"}, nil)
	transport := &sdk.StreamableClientTransport{Endpoint: endpoint, HTTPClient: &http.Client{Transport: bearerTransport{base: http.DefaultTransport, token: token}, Timeout: 10 * time.Second}, DisableStandaloneSSE: true, MaxRetries: -1}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	gotNames := make([]string, 0, len(tools.Tools))
	byName := make(map[string]*sdk.Tool)
	for _, tool := range tools.Tools {
		gotNames = append(gotNames, tool.Name)
		byName[tool.Name] = tool
	}
	sort.Strings(gotNames)
	wantNames := []string{"approval_get_details", "approval_list_pending", "bridge_get_capabilities", "bridge_get_status", "codex_cancel_task", "codex_get_changes", "codex_get_progress", "codex_get_result", "codex_get_task", "codex_list_threads", "codex_resume_thread", "codex_start_task", "workspace_get", "workspace_list", "workspace_register"}
	if fmt.Sprint(gotNames) != fmt.Sprint(wantNames) {
		t.Fatalf("tools = %v, want %v", gotNames, wantNames)
	}
	if byName["workspace_list"].Annotations == nil || !byName["workspace_list"].Annotations.ReadOnlyHint {
		t.Fatal("workspace_list is missing read-only annotation")
	}
	if byName["codex_cancel_task"].Annotations == nil || byName["codex_cancel_task"].Annotations.ReadOnlyHint || byName["codex_cancel_task"].Annotations.DestructiveHint == nil || !*byName["codex_cancel_task"].Annotations.DestructiveHint {
		t.Fatal("codex_cancel_task annotation is unsafe")
	}
	if byName["codex_start_task"].InputSchema == nil {
		t.Fatal("codex_start_task has no input schema")
	}

	call := func(name string, input any) *sdk.CallToolResult {
		t.Helper()
		result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: input})
		if err != nil {
			t.Fatalf("tool %s protocol error: %v", name, err)
		}
		return result
	}
	structured := func(result *sdk.CallToolResult) map[string]any {
		t.Helper()
		value, ok := result.StructuredContent.(map[string]any)
		if !ok {
			t.Fatalf("structured content has type %T", result.StructuredContent)
		}
		return value
	}
	if result := call("workspace_list", map[string]any{}); result.IsError {
		t.Fatalf("workspace_list: %v", result.Content)
	}
	if result := call("workspace_get", map[string]any{"workspaceId": "bad"}); !result.IsError {
		t.Fatal("invalid workspace ID accepted")
	}
	if result := call("codex_get_task", map[string]any{"taskId": "bad"}); !result.IsError {
		t.Fatal("invalid task ID accepted")
	}

	workspacePath := filepath.Join(home, "project-a")
	if err := os.Mkdir(workspacePath, 0o700); err != nil {
		t.Fatal(err)
	}
	registered := call("workspace_register", map[string]any{"workspaceId": "project-a", "path": workspacePath})
	if registered.IsError {
		t.Fatalf("workspace_register: %s", contentText(registered))
	}
	requestID := structured(registered)["requestId"].(string)
	if _, err := workspaces.ApproveRegistration(context.Background(), requestID, "test-local-user"); err != nil {
		t.Fatal(err)
	}
	if result := call("workspace_get", map[string]any{"workspaceId": "project-a"}); result.IsError {
		t.Fatalf("workspace_get: %v", result.Content)
	}
	started := call("codex_start_task", map[string]any{"workspaceId": "project-a", "prompt": "Review the package structure."})
	if started.IsError {
		t.Fatalf("codex_start_task: %v", started.Content)
	}
	taskID := structured(started)["taskId"].(string)
	for _, name := range []string{"codex_get_task", "codex_get_progress", "codex_get_result", "codex_get_changes"} {
		inputKey := "taskId"
		result := call(name, map[string]any{inputKey: taskID})
		if result.IsError {
			t.Errorf("%s: %v", name, result.Content)
		}
	}
	threadID := "bridge-owned-thread"
	if err := store.SaveThread(ctx, domain.Thread{ID: threadID, WorkspaceID: "project-a", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTaskThread(ctx, taskID, threadID, "turn-a"); err != nil {
		t.Fatal(err)
	}
	if result := call("codex_list_threads", map[string]any{"workspaceId": "project-a"}); result.IsError {
		t.Fatalf("codex_list_threads: %v", result.Content)
	}
	resumed := call("codex_resume_thread", map[string]any{"workspaceId": "project-a", "threadId": threadID, "prompt": "Continue the analysis."})
	if resumed.IsError {
		t.Fatalf("codex_resume_thread: %v", resumed.Content)
	}
	if result := call("codex_cancel_task", map[string]any{"taskId": taskID}); result.IsError {
		t.Fatalf("codex_cancel_task: %v", result.Content)
	}
	secondPath := filepath.Join(home, "project-b")
	if err := os.Mkdir(secondPath, 0o700); err != nil {
		t.Fatal(err)
	}
	pending := call("workspace_register", map[string]any{"workspaceId": "project-b", "path": secondPath})
	if pending.IsError {
		t.Fatalf("second workspace request: %v", pending.Content)
	}
	secondRequestID := structured(pending)["requestId"].(string)
	if result := call("approval_list_pending", map[string]any{}); result.IsError {
		t.Fatalf("approval_list_pending: %v", result.Content)
	}
	if result := call("approval_get_details", map[string]any{"approvalId": secondRequestID}); result.IsError {
		t.Fatalf("approval_get_details: %v", result.Content)
	}
	if result := call("bridge_get_status", map[string]any{}); result.IsError {
		t.Fatalf("bridge_get_status: %v", result.Content)
	}
	if result := call("bridge_get_capabilities", map[string]any{}); result.IsError {
		t.Fatalf("bridge_get_capabilities: %v", result.Content)
	}
	auditRows, err := store.AuditEventCount(ctx)
	if err != nil || auditRows == 0 {
		t.Fatalf("audit rows=%d err=%v", auditRows, err)
	}
}

func contentText(result *sdk.CallToolResult) string {
	var text []string
	for _, content := range result.Content {
		if value, ok := content.(*sdk.TextContent); ok {
			text = append(text, value.Text)
		}
	}
	return strings.Join(text, " | ")
}

func TestMCPRejectsMissingBearerAndBadHost(t *testing.T) {
	token := strings.Repeat("z", 40)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler := security.AuthMiddlewareForHost(token, "127.0.0.1", 8787, next)
	for _, test := range []struct {
		name, host, auth string
		want             int
	}{
		{"missing token", "127.0.0.1:8787", "", http.StatusUnauthorized},
		{"bad token", "127.0.0.1:8787", "Bearer wrong", http.StatusUnauthorized},
		{"bad host", "attacker.example:8787", "Bearer " + token, http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/mcp", nil)
			req.Host = test.host
			req.Header.Set("Authorization", test.auth)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != test.want {
				t.Fatalf("status=%d want=%d", rec.Code, test.want)
			}
			if strings.Contains(rec.Body.String(), token) {
				t.Fatal("auth error leaked the bearer token")
			}
		})
	}
}
