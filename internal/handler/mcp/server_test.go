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
	readOnlyTools := map[string]bool{
		"workspace_list": true, "workspace_get": true, "codex_list_threads": true,
		"codex_get_task": true, "codex_get_progress": true, "codex_get_result": true,
		"codex_get_changes": true, "approval_list_pending": true, "approval_get_details": true,
		"bridge_get_status": true, "bridge_get_capabilities": true,
	}
	for name, tool := range byName {
		_, readOnly := readOnlyTools[name]
		if tool.Annotations == nil || tool.Annotations.ReadOnlyHint != readOnly || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
			t.Errorf("%s has incorrect read-only or open-world annotation", name)
			continue
		}
		if !readOnly {
			wantDestructive := name == "codex_cancel_task"
			if tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint != wantDestructive {
				t.Errorf("%s has incorrect destructive annotation", name)
			}
		}
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
	thirdPath := filepath.Join(home, "project-c")
	if err := os.Mkdir(thirdPath, 0o700); err != nil {
		t.Fatal(err)
	}
	thirdRequest, err := workspaces.RequestRegistrationLocal(ctx, "project-c", thirdPath, "test-local-user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspaces.ApproveRegistration(ctx, thirdRequest.ID, "test-local-user"); err != nil {
		t.Fatal(err)
	}
	workspacePage := call("workspace_list", map[string]any{"limit": 1, "offset": 0})
	if workspacePage.IsError {
		t.Fatalf("workspace_list page: %v", workspacePage.Content)
	}
	workspacePageData := structured(workspacePage)
	if values, ok := workspacePageData["workspaces"].([]any); !ok || len(values) != 1 {
		t.Fatalf("workspace page returned %v rows, want 1", lenOrInvalid(values, ok))
	}
	if workspacePageData["nextOffset"] != float64(1) {
		t.Fatalf("workspace page nextOffset = %v, want 1", workspacePageData["nextOffset"])
	}
	workspacePage = call("workspace_list", map[string]any{"limit": 1, "offset": 1})
	if workspacePage.IsError {
		t.Fatalf("workspace_list second page: %v", workspacePage.Content)
	}
	workspacePageData = structured(workspacePage)
	if values, ok := workspacePageData["workspaces"].([]any); !ok || len(values) != 1 {
		t.Fatalf("workspace second page returned %v rows, want 1", lenOrInvalid(values, ok))
	}
	if _, ok := workspacePageData["nextOffset"]; ok {
		t.Fatalf("workspace second page unexpectedly has nextOffset %v", workspacePageData["nextOffset"])
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
	for _, id := range []string{"project-d", "project-e"} {
		path := filepath.Join(home, id)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if result := call("workspace_register", map[string]any{"workspaceId": id, "path": path}); result.IsError {
			t.Fatalf("pending workspace %s: %v", id, result.Content)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := approvals.CreatePending(ctx, service.ApprovalInput{OperationType: "fixture.inspect", TargetResource: "fixture", Parameters: map[string]any{"index": i}}); err != nil {
			t.Fatal(err)
		}
	}
	if result := call("approval_list_pending", map[string]any{}); result.IsError {
		t.Fatalf("approval_list_pending: %v", result.Content)
	}
	approvalPage := call("approval_list_pending", map[string]any{"limit": 1, "offset": 0})
	if approvalPage.IsError {
		t.Fatalf("approval_list_pending page: %v", approvalPage.Content)
	}
	approvalPageData := structured(approvalPage)
	for _, key := range []string{"workspaceRegistrations", "operationApprovals"} {
		if values, ok := approvalPageData[key].([]any); !ok || len(values) != 1 {
			t.Fatalf("%s page returned %v rows, want 1", key, lenOrInvalid(values, ok))
		}
	}
	if approvalPageData["workspaceRegistrationsNextOffset"] != float64(1) || approvalPageData["operationApprovalsNextOffset"] != float64(1) {
		t.Fatalf("approval page offsets = %v / %v, want 1 / 1", approvalPageData["workspaceRegistrationsNextOffset"], approvalPageData["operationApprovalsNextOffset"])
	}
	approvalPage = call("approval_list_pending", map[string]any{"limit": 1, "offset": 1})
	if approvalPage.IsError {
		t.Fatalf("approval_list_pending second page: %v", approvalPage.Content)
	}
	approvalPageData = structured(approvalPage)
	for _, key := range []string{"workspaceRegistrations", "operationApprovals"} {
		if values, ok := approvalPageData[key].([]any); !ok || len(values) != 1 {
			t.Fatalf("%s second page returned %v rows, want 1", key, lenOrInvalid(values, ok))
		}
	}
	if approvalPageData["workspaceRegistrationsNextOffset"] != float64(2) {
		t.Fatalf("workspace registration nextOffset = %v, want 2", approvalPageData["workspaceRegistrationsNextOffset"])
	}
	if _, ok := approvalPageData["operationApprovalsNextOffset"]; ok {
		t.Fatalf("operation approvals second page unexpectedly has nextOffset %v", approvalPageData["operationApprovalsNextOffset"])
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
	largeStarted := call("codex_start_task", map[string]any{"workspaceId": "project-a", "prompt": "Return a bounded result."})
	if largeStarted.IsError {
		t.Fatalf("start output-limit task: %v", largeStarted.Content)
	}
	largeTaskID := structured(largeStarted)["taskId"].(string)
	if err := store.SetTaskStatus(ctx, largeTaskID, domain.TaskRunning, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishTask(ctx, largeTaskID, domain.TaskSucceeded, strings.Repeat("x", (1<<20)+1), "", ""); err != nil {
		t.Fatal(err)
	}
	if result := call("codex_get_result", map[string]any{"taskId": largeTaskID}); !result.IsError {
		t.Fatal("MCP returned a result larger than the bridge output limit")
	}
	auditRows, err := store.AuditEventCount(ctx)
	if err != nil || auditRows == 0 {
		t.Fatalf("audit rows=%d err=%v", auditRows, err)
	}
}

func lenOrInvalid(values []any, ok bool) any {
	if !ok {
		return "invalid"
	}
	return len(values)
}

func TestNewHTTPHandlerProtectsMCPDiscoveryAndHealth(t *testing.T) {
	const token = "local-test-token-that-is-long-enough-123456"
	handler := NewHTTPHandler(Dependencies{Config: config.Config{Host: "127.0.0.1", Port: 8787, AuthToken: token, MaxRequestBytes: 64 << 10}})
	for _, test := range []struct {
		name, method, path, body string
	}{
		{"MCP initialize", http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`},
		{"MCP tools discovery", http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`},
		{"health", http.MethodGet, "/healthz", ""},
	} {
		for _, auth := range []struct{ name, value string }{{"missing", ""}, {"invalid", "Bearer invalid-token-value"}} {
			t.Run(test.name+"/"+auth.name, func(t *testing.T) {
				req := httptest.NewRequest(test.method, "http://127.0.0.1:8787"+test.path, strings.NewReader(test.body))
				if auth.value != "" {
					req.Header.Set("Authorization", auth.value)
				}
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("%s request to %s returned %d, want 401", auth.name, test.path, rec.Code)
				}
				if strings.Contains(rec.Body.String(), token) {
					t.Fatal("authentication response exposed the bearer token")
				}
			})
		}
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/healthz", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("authenticated health returned %d, want 200", response.Code)
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
