package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/audit"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/config"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/domain"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/repository/sqlite"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/security"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/service"
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

type sessionCatalogContractStub struct {
	session domain.CodexSession
	bridge  domain.CodexSession
}

func (s *sessionCatalogContractStub) ListCLISessions(_ context.Context, workspacePath string) ([]domain.CodexSession, error) {
	if s.session.WorkspacePath == workspacePath && s.session.Source == "cli" {
		return []domain.CodexSession{s.session}, nil
	}
	return nil, nil
}
func (s *sessionCatalogContractStub) ListCLISessionsPage(_ context.Context, workspacePath, search, cursor string, limit int) (domain.CodexSessionPage, error) {
	displayName := s.session.Title
	if strings.TrimSpace(displayName) == "" {
		displayName = s.session.Preview
	}
	if (workspacePath == "" || s.session.WorkspacePath == workspacePath) && s.session.Source == "cli" && (search == "" || strings.Contains(strings.ToLower(displayName), strings.ToLower(search))) && cursor == "" {
		return domain.CodexSessionPage{Sessions: []domain.CodexSession{s.session}}, nil
	}
	return domain.CodexSessionPage{}, nil
}
func (s *sessionCatalogContractStub) ReadSessionMetadata(_ context.Context, threadID string) (domain.CodexSession, error) {
	for _, session := range []domain.CodexSession{s.session, s.bridge} {
		if session.ID == threadID {
			return session, nil
		}
	}
	return domain.CodexSession{}, domain.ErrNotFound
}

type allowTaskExecution struct{}

func (allowTaskExecution) AuthorizeExecution(context.Context, domain.Workspace) error { return nil }

type bearerTransport struct {
	base      http.RoundTripper
	token     string
	assertion string
	host      string
}

func (t bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	if t.token != "" {
		clone.Header.Set("Authorization", "Bearer "+t.token)
	}
	if t.assertion != "" {
		clone.Header.Set("Cf-Access-Jwt-Assertion", t.assertion)
	}
	if t.host != "" {
		clone.Host = t.host
	}
	return t.base.RoundTrip(clone)
}

type accessVerifierStub struct{}

func (accessVerifierStub) Verify(_ context.Context, assertion string) (string, error) {
	if assertion != "synthetic-access-assertion" {
		return "", errors.New("invalid assertion")
	}
	return "zaqi@example.com", nil
}

func TestModernServerDiscoveryAndToolsList(t *testing.T) {
	handler := NewHTTPHandler(Dependencies{
		Config:             config.Config{Host: "127.0.0.1", Port: 8787, AuthMode: config.AuthModeCloudflareAccess, MaxRequestBytes: 64 << 10},
		CloudflareVerifier: accessVerifierStub{},
	})
	request := func(method, id, version, host, origin string) *httptest.ResponseRecorder {
		t.Helper()
		payload, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"method":  method,
			"params": map[string]any{"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion":    version,
				"io.modelcontextprotocol/clientCapabilities": map[string]any{},
				"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "ChatGPT", "version": "discovery-test"},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/mcp", strings.NewReader(string(payload)))
		if host == "" {
			host = "127.0.0.1:8787"
		}
		req.Host = host
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", version)
		req.Header.Set("Mcp-Method", method)
		req.Header.Set("Cf-Access-Jwt-Assertion", "synthetic-access-assertion")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	assertServerInfo := func(t *testing.T, metadata map[string]json.RawMessage) {
		t.Helper()
		var info struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		if err := json.Unmarshal(metadata["io.modelcontextprotocol/serverInfo"], &info); err != nil {
			t.Fatalf("serverInfo missing or invalid: %v", err)
		}
		if info.Name != "codex-chatgpt-bridge" || info.Version == "" {
			t.Fatalf("serverInfo = %+v", info)
		}
	}

	discovery := request("server/discover", "discover-1", "2026-07-28", "", "")
	if discovery.Code != http.StatusOK || !strings.HasPrefix(discovery.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("discover response status=%d content-type=%q body=%s", discovery.Code, discovery.Header().Get("Content-Type"), discovery.Body.String())
	}
	var discovered struct {
		JSONRPC string `json:"jsonrpc"`
		ID      any    `json:"id"`
		Result  struct {
			ResultType        string                     `json:"resultType"`
			SupportedVersions []string                   `json:"supportedVersions"`
			Capabilities      map[string]json.RawMessage `json:"capabilities"`
			Meta              map[string]json.RawMessage `json:"_meta"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(discovery.Body.Bytes(), &discovered); err != nil {
		t.Fatalf("decode discover response: %v; body=%s", err, discovery.Body.String())
	}
	if discovered.JSONRPC != "2.0" || discovered.ID != "discover-1" || discovered.Error != nil {
		t.Fatalf("invalid discover envelope: %+v", discovered)
	}
	if discovered.Result.ResultType != "complete" || !slices.Contains(discovered.Result.SupportedVersions, "2026-07-28") {
		t.Fatalf("discover did not negotiate 2026-07-28: resultType=%q supported=%v body=%s", discovered.Result.ResultType, discovered.Result.SupportedVersions, discovery.Body.String())
	}
	if _, ok := discovered.Result.Capabilities["tools"]; !ok {
		t.Fatal("discover capabilities omit tools")
	}
	assertServerInfo(t, discovered.Result.Meta)
	t.Logf("server/discover: HTTP %d %s id=%v resultType=%s supportedVersions=%v toolsCapability=true serverInfo=%s", discovery.Code, discovery.Header().Get("Content-Type"), discovered.ID, discovered.Result.ResultType, discovered.Result.SupportedVersions, discovered.Result.Meta["io.modelcontextprotocol/serverInfo"])

	tools := request("tools/list", "tools-1", "2026-07-28", "", "")
	if tools.Code != http.StatusOK || !strings.HasPrefix(tools.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("tools/list response status=%d content-type=%q body=%s", tools.Code, tools.Header().Get("Content-Type"), tools.Body.String())
	}
	var listed struct {
		JSONRPC string `json:"jsonrpc"`
		ID      any    `json:"id"`
		Result  struct {
			Tools []struct {
				Name         string          `json:"name"`
				Description  string          `json:"description"`
				InputSchema  json.RawMessage `json:"inputSchema"`
				OutputSchema json.RawMessage `json:"outputSchema"`
				Annotations  json.RawMessage `json:"annotations"`
				Meta         json.RawMessage `json:"_meta"`
			} `json:"tools"`
			Meta map[string]json.RawMessage `json:"_meta"`
		} `json:"result"`
	}
	if err := json.Unmarshal(tools.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode tools/list response: %v; body=%s", err, tools.Body.String())
	}
	if listed.JSONRPC != "2.0" || listed.ID != "tools-1" || len(listed.Result.Tools) != 20 {
		t.Fatalf("tools/list envelope or count invalid: jsonrpc=%q id=%v tools=%d", listed.JSONRPC, listed.ID, len(listed.Result.Tools))
	}
	toolNames := make(map[string]bool, len(listed.Result.Tools))
	for _, tool := range listed.Result.Tools {
		if tool.Name == "" || toolNames[tool.Name] {
			t.Fatalf("empty or duplicate tool name %q", tool.Name)
		}
		toolNames[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("tool %q has no description", tool.Name)
		}
		if len(tool.Annotations) > 0 && !json.Valid(tool.Annotations) {
			t.Errorf("tool %q has malformed annotations", tool.Name)
		}
		if len(tool.Meta) > 0 && !json.Valid(tool.Meta) {
			t.Errorf("tool %q has malformed _meta", tool.Name)
		}
		assertToolJSONSchema(t, tool.Name+" inputSchema", tool.InputSchema, true)
		if len(tool.OutputSchema) > 0 {
			assertToolJSONSchema(t, tool.Name+" outputSchema", tool.OutputSchema, false)
		}
		if tool.Name == "codex_list_local_sessions" {
			var inputSchema map[string]json.RawMessage
			if err := json.Unmarshal(tool.InputSchema, &inputSchema); err != nil {
				t.Fatalf("decode local session input schema: %v", err)
			}
			if _, exists := inputSchema["required"]; exists {
				t.Errorf("catalog parameters are all optional; omit required from inputSchema")
			}
		}
		if tool.Name == "codex_list_local_sessions" && len(tool.OutputSchema) > 0 {
			var schema jsonschema.Schema
			if err := json.Unmarshal(tool.OutputSchema, &schema); err != nil {
				t.Fatalf("decode local session output schema: %v", err)
			}
			resolved, err := schema.Resolve(nil)
			if err != nil {
				t.Fatalf("resolve local session output schema: %v", err)
			}
			output, err := json.Marshal(SessionCatalogOutput{Sessions: []SessionCatalogEntryDTO{{
				ThreadID: "fixture-thread", DisplayName: "Fixture session", Project: "fixture-project", Status: "idle",
				UpdatedAt: time.Now().UnixMilli(), ConnectionStatus: "trusted", CanResume: true,
			}}})
			if err != nil {
				t.Fatal(err)
			}
			var value any
			if err := json.Unmarshal(output, &value); err != nil {
				t.Fatal(err)
			}
			if err := resolved.Validate(value); err != nil {
				t.Errorf("local session output schema rejects actual empty-page response: %v", err)
			}
		}
		if tool.Name == "codex_list_session_projects" && len(tool.OutputSchema) > 0 {
			assertOutputSchemaAccepts(t, tool.OutputSchema, SessionProjectsOutput{Projects: []SessionProjectChoiceDTO{{ProjectID: "0123456789abcdef0123456789abcdef", Name: "fixture", RelativePath: "fixture"}}})
		}
		if tool.Name == "codex_request_session_project_binding" && len(tool.OutputSchema) > 0 {
			assertOutputSchemaAccepts(t, tool.OutputSchema, SessionProjectBindingOutput{RequestID: "fixture-request", ThreadID: "fixture-thread", ProjectID: "0123456789abcdef0123456789abcdef", Status: domain.SessionProjectBindingPending, ExpiresAt: time.Now().UnixMilli()})
		}
	}
	assertServerInfo(t, listed.Result.Meta)
	t.Logf("tools/list: HTTP %d %s id=%v tools=%d serverInfo=%s", tools.Code, tools.Header().Get("Content-Type"), listed.ID, len(listed.Result.Tools), listed.Result.Meta["io.modelcontextprotocol/serverInfo"])

	invalidVersion := request("tools/list", "invalid-1", "2025-01-01", "", "")
	if invalidVersion.Code != http.StatusBadRequest {
		t.Fatalf("unsupported protocol version accepted: %s", invalidVersion.Body.String())
	}
	if wrongHost := request("server/discover", "wrong-host", "2026-07-28", "attacker.example:8787", ""); wrongHost.Code != http.StatusForbidden {
		t.Fatalf("modern discovery accepted invalid Host: status=%d", wrongHost.Code)
	}
	if wrongOrigin := request("server/discover", "wrong-origin", "2026-07-28", "", "https://attacker.example"); wrongOrigin.Code != http.StatusForbidden {
		t.Fatalf("modern discovery accepted invalid Origin: status=%d", wrongOrigin.Code)
	}
}

func assertOutputSchemaAccepts(t *testing.T, raw json.RawMessage, output any) {
	t.Helper()
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	if err := resolved.Validate(value); err != nil {
		t.Errorf("serialized tool output violates schema: %v", err)
	}
}

func assertToolJSONSchema(t *testing.T, name string, raw json.RawMessage, requireObject bool) {
	t.Helper()
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("%s is invalid JSON Schema: %v", name, err)
	}
	if _, err := schema.Resolve(nil); err != nil {
		t.Fatalf("%s cannot be resolved: %v", name, err)
	}
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	if requireObject {
		object, ok := root.(map[string]any)
		if !ok || object["type"] != "object" {
			t.Fatalf("%s root must be a JSON Schema object", name)
		}
	}

	var visit func(any, string)
	visit = func(value any, path string) {
		t.Helper()
		if _, ok := value.(bool); ok {
			return
		}
		object, ok := value.(map[string]any)
		if !ok {
			t.Errorf("%s %s must be a schema object or boolean", name, path)
			return
		}
		if rawType, exists := object["type"]; exists {
			validType := func(value any) bool {
				switch value {
				case "object", "array", "string", "integer", "number", "boolean", "null":
					return true
				default:
					return false
				}
			}
			switch types := rawType.(type) {
			case string:
				if !validType(types) {
					t.Errorf("%s %s has invalid type %q", name, path, types)
				}
			case []any:
				for _, item := range types {
					if !validType(item) {
						t.Errorf("%s %s has invalid union type %v", name, path, item)
					}
				}
			default:
				t.Errorf("%s %s type must be a string or string array", name, path)
			}
		}
		properties, hasProperties := object["properties"].(map[string]any)
		if rawRequired, exists := object["required"]; exists {
			required, ok := rawRequired.([]any)
			if !ok {
				serialized, _ := json.Marshal(object)
				t.Errorf("%s %s required must be an array; schema=%s", name, path, serialized)
			} else {
				for _, item := range required {
					property, ok := item.(string)
					if !ok || !hasProperties || properties[property] == nil {
						t.Errorf("%s %s required entry %v has no matching property", name, path, item)
					}
				}
			}
		}
		if object["type"] == "array" {
			if _, ok := object["items"]; !ok {
				t.Errorf("%s %s array has no items schema", name, path)
			}
		}
		for _, key := range []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"} {
			if values, ok := object[key].(map[string]any); ok {
				for child, childSchema := range values {
					visit(childSchema, path+"."+key+"."+child)
				}
			} else if _, exists := object[key]; exists {
				t.Errorf("%s %s %s must be an object", name, path, key)
			}
		}
		for _, key := range []string{"items", "contains", "not", "if", "then", "else", "propertyNames", "unevaluatedProperties", "unevaluatedItems", "contentSchema", "additionalProperties"} {
			if child, exists := object[key]; exists {
				visit(child, path+"."+key)
			}
		}
		for _, key := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
			if children, exists := object[key]; exists {
				array, ok := children.([]any)
				if !ok {
					t.Errorf("%s %s %s must be an array", name, path, key)
					continue
				}
				for i, child := range array {
					visit(child, path+"."+key+"["+strconv.Itoa(i)+"]")
				}
			}
		}
	}
	visit(root, "#")
}

func TestLegacyMCPInitializeAndToolsListRemainSupported(t *testing.T) {
	handler := NewHTTPHandler(Dependencies{
		Config:             config.Config{Host: "127.0.0.1", Port: 8787, AuthMode: config.AuthModeCloudflareAccess, MaxRequestBytes: 64 << 10},
		CloudflareVerifier: accessVerifierStub{},
	})
	post := func(body, protocolVersion, sessionID string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/mcp", strings.NewReader(body))
		req.Host = "127.0.0.1:8787"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Cf-Access-Jwt-Assertion", "synthetic-access-assertion")
		if protocolVersion != "" {
			req.Header.Set("MCP-Protocol-Version", protocolVersion)
		}
		if sessionID != "" {
			req.Header.Set("Mcp-Session-Id", sessionID)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	initialized := post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"legacy-test","version":"1"}}}`, "", "")
	if initialized.Code != http.StatusOK {
		t.Fatalf("legacy initialize status=%d body=%s", initialized.Code, initialized.Body.String())
	}
	sessionID := initialized.Header().Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("legacy initialize did not return a session ID")
	}
	if response := post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`, "2025-11-25", sessionID); response.Code != http.StatusAccepted {
		t.Fatalf("legacy initialized notification status=%d body=%s", response.Code, response.Body.String())
	}
	listed := post(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, "2025-11-25", sessionID)
	if listed.Code != http.StatusOK {
		t.Fatalf("legacy tools/list status=%d body=%s", listed.Code, listed.Body.String())
	}
	var response struct {
		Result struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &response); err != nil || len(response.Result.Tools) != 20 {
		t.Fatalf("legacy tools/list invalid: tools=%d err=%v body=%s", len(response.Result.Tools), err, listed.Body.String())
	}
	closeRequest := httptest.NewRequest(http.MethodDelete, "http://127.0.0.1:8787/mcp", nil)
	closeRequest.Host = "127.0.0.1:8787"
	closeRequest.Header.Set("Cf-Access-Jwt-Assertion", "synthetic-access-assertion")
	closeRequest.Header.Set("MCP-Protocol-Version", "2025-11-25")
	closeRequest.Header.Set("Mcp-Session-Id", sessionID)
	closed := httptest.NewRecorder()
	handler.ServeHTTP(closed, closeRequest)
	if closed.Code != http.StatusNoContent {
		t.Fatalf("legacy session close status=%d body=%s", closed.Code, closed.Body.String())
	}
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
	catalog := &sessionCatalogContractStub{}
	sessions := service.NewSessionService(store, workspaces, catalog, logger)
	queue := &queueStub{store: store}
	tasks := service.NewTaskService(store, store, workspaces, service.NewApprovalService(store, workspaces), queue, allowTaskExecution{}, logger, 20_000, 20)
	approvals := service.NewApprovalService(store, workspaces)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	cfg := config.Config{Host: "127.0.0.1", Port: port, AuthMode: config.AuthModeCloudflareAccess, MaxRequestBytes: 64 << 10}
	handler := NewHTTPHandler(Dependencies{Config: cfg, CloudflareVerifier: accessVerifierStub{}, Workspaces: workspaces, Tasks: tasks, Sessions: sessions, Approvals: approvals, Audit: logger, Codex: codexStatusStub{}, StartedAt: time.Now()})
	httpServer := &http.Server{Handler: handler}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/mcp", port)
	client := sdk.NewClient(&sdk.Implementation{Name: "contract-test", Version: "0.0.0"}, nil)
	transport := &sdk.StreamableClientTransport{Endpoint: endpoint, HTTPClient: &http.Client{Transport: bearerTransport{base: http.DefaultTransport, assertion: "synthetic-access-assertion", host: net.JoinHostPort(cfg.Host, fmt.Sprint(cfg.Port))}, Timeout: 10 * time.Second}, DisableStandaloneSSE: true, MaxRetries: -1}
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
	wantNames := []string{"approval_get_details", "approval_list_pending", "bridge_get_capabilities", "bridge_get_status", "codex_cancel_task", "codex_get_changes", "codex_get_progress", "codex_get_result", "codex_get_task", "codex_list_local_sessions", "codex_list_session_projects", "codex_list_threads", "codex_request_session_attach", "codex_request_session_project_binding", "codex_resume_thread", "codex_start_task", "codex_start_write_task", "workspace_get", "workspace_list", "workspace_register"}
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
		"workspace_list": true, "workspace_get": true, "codex_list_threads": true, "codex_list_local_sessions": true, "codex_list_session_projects": true,
		"codex_get_task": true, "codex_get_progress": true, "codex_get_result": true,
		"codex_get_changes": true, "approval_list_pending": true, "approval_get_details": true,
		"bridge_get_status": true, "bridge_get_capabilities": true,
	}
	if byName["codex_start_write_task"].InputSchema == nil || byName["codex_start_write_task"].Annotations == nil || byName["codex_start_write_task"].Annotations.ReadOnlyHint {
		t.Fatal("codex_start_write_task is missing a mutating tool contract")
	}
	for name, tool := range byName {
		_, readOnly := readOnlyTools[name]
		if tool.Annotations == nil || tool.Annotations.ReadOnlyHint != readOnly || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
			t.Errorf("%s has incorrect read-only or open-world annotation", name)
			continue
		}
		if !readOnly {
			wantDestructive := name == "codex_cancel_task" || name == "codex_start_write_task"
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
	if result := call("codex_list_local_sessions", map[string]any{}); !result.IsError {
		t.Fatal("global session catalog was available before local owner approval")
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
	if err := sessions.SetCatalogAccessLocal(context.Background(), true, "test-local-user"); err != nil {
		t.Fatal(err)
	}
	if err := sessions.SetWorkspaceTrustLocal(context.Background(), "project-a", true, "test-local-user"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	approvedWorkspace, err := workspaces.GetLocal(ctx, "project-a")
	if err != nil {
		t.Fatalf("fixture workspace unavailable: err=%v", err)
	}
	catalog.session = domain.CodexSession{ID: "cli-session-fixture", WorkspacePath: approvedWorkspace.RootPath, Source: "cli", Title: "fixture session", Status: "notLoaded", CreatedAt: now.Add(-time.Hour), UpdatedAt: now}
	catalogSessions, err := catalog.ListCLISessions(ctx, approvedWorkspace.RootPath)
	if err != nil || len(catalogSessions) != 1 || catalogSessions[0].ID != catalog.session.ID {
		t.Fatalf("fixture session not present in local CLI catalog: sessions=%+v err=%v", catalogSessions, err)
	}
	attach := call("codex_request_session_attach", map[string]any{"workspaceId": "project-a", "threadId": catalog.session.ID})
	if attach.IsError || structured(attach)["status"] != domain.SessionAttachPending {
		t.Fatalf("codex_request_session_attach did not return a pending request: error=%t content=%s structured=%v", attach.IsError, contentText(attach), attach.StructuredContent)
	}
	if result := call("codex_list_threads", map[string]any{"workspaceId": "project-a"}); result.IsError || strings.Contains(contentText(result), catalog.session.ID) {
		t.Fatalf("unapproved session leaked through MCP thread list: %v", result.Content)
	}
	requestID = structured(attach)["requestId"].(string)
	if _, err := sessions.ApproveLocal(context.Background(), requestID, "test-local-user"); err != nil {
		t.Fatal(err)
	}
	listedAttached := call("codex_list_threads", map[string]any{"workspaceId": "project-a"})
	if listedAttached.IsError || !strings.Contains(contentText(listedAttached), catalog.session.ID) {
		t.Fatalf("approved CLI session not exposed through MCP thread list: %v", listedAttached.Content)
	}
	globalSessions := call("codex_list_local_sessions", map[string]any{"workspaceId": "project-a", "limit": 10})
	if globalSessions.IsError || !strings.Contains(contentText(globalSessions), catalog.session.ID) || !strings.Contains(contentText(globalSessions), "trusted") {
		t.Fatalf("trusted global session catalog was not exposed: %v", globalSessions.Content)
	}
	globalRows, _ := structured(globalSessions)["sessions"].([]any)
	if len(globalRows) != 1 {
		t.Fatalf("catalog returned %d rows, want one", len(globalRows))
	}
	firstGlobalRow, ok := globalRows[0].(map[string]any)
	if !ok || firstGlobalRow["displayName"] != "fixture session" {
		t.Fatalf("Codex thread name was not exposed as displayName: %v", globalSessions.StructuredContent)
	}
	catalog.session.Title = ""
	catalog.session.Preview = "fixture\tpreview api_key=fixture-secret"
	previewSessions := call("codex_list_local_sessions", map[string]any{"workspaceId": "project-a", "search": "preview", "limit": 10})
	previewRows, _ := structured(previewSessions)["sessions"].([]any)
	if len(previewRows) != 1 {
		t.Fatalf("preview search returned %d rows, want one", len(previewRows))
	}
	previewRow, ok := previewRows[0].(map[string]any)
	if previewSessions.IsError || !ok || previewRow["displayName"] != "fixture preview api_key=[REDACTED]" || strings.Contains(contentText(previewSessions), "fixture-secret") {
		t.Fatalf("sanitized preview fallback was not searchable: %v", previewSessions.StructuredContent)
	}
	resumeAttached := call("codex_resume_thread", map[string]any{"workspaceId": "project-a", "threadId": catalog.session.ID, "prompt": "Continue read-only analysis."})
	if resumeAttached.IsError {
		t.Fatalf("attached CLI session could not be resumed through MCP: %v", resumeAttached.Content)
	}
	if result := call("workspace_get", map[string]any{"workspaceId": "project-a"}); result.IsError {
		t.Fatalf("workspace_get: %v", result.Content)
	}
	if result := call("codex_start_task", map[string]any{
		"workspaceId": "project-a",
		"prompt":      "Inspect the disposable fixture.",
		"sandbox":     "danger-full-access",
	}); !result.IsError {
		t.Fatal("codex_start_task accepted caller-supplied sandbox override")
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
	catalog.bridge = domain.CodexSession{ID: threadID, WorkspacePath: approvedWorkspace.RootPath, Source: "appServer", Status: "notLoaded", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
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
		t.Fatalf("codex_resume_thread: %s", contentText(resumed))
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
	if result := call("codex_start_write_task", map[string]any{"workspaceId": "project-a", "prompt": "change the fixture"}); !result.IsError {
		t.Fatal("write task was accepted while the workspace remained read-only")
	}
	if err := workspaces.SetWriteEnabledLocal(context.Background(), "project-a", true, "test-local-user"); err != nil {
		t.Fatal(err)
	}
	queue.mu.Lock()
	queuedBeforeWrite := len(queue.jobs)
	queue.mu.Unlock()
	writeStarted := call("codex_start_write_task", map[string]any{"workspaceId": "project-a", "prompt": "change the fixture"})
	if writeStarted.IsError {
		t.Fatalf("codex_start_write_task: %s", contentText(writeStarted))
	}
	writeOutput := structured(writeStarted)
	writeTaskID, _ := writeOutput["taskId"].(string)
	approvalID, _ := writeOutput["approvalId"].(string)
	if writeOutput["status"] != string(domain.TaskWaitingApproval) || writeOutput["executionMode"] != string(domain.ExecutionWorkspaceWrite) || !validTaskID(writeTaskID) || !validTaskID(approvalID) {
		t.Fatalf("write request did not return a bound pending approval: %#v", writeOutput)
	}
	if taskResult := call("codex_get_task", map[string]any{"taskId": writeTaskID}); taskResult.IsError || structured(taskResult)["task"].(map[string]any)["status"] != string(domain.TaskWaitingApproval) {
		t.Fatalf("write task did not persist pending state: %s", contentText(taskResult))
	}
	queue.mu.Lock()
	queueSize := len(queue.jobs)
	queue.mu.Unlock()
	if queueSize != queuedBeforeWrite {
		t.Fatalf("write request was queued before approval; queued jobs=%d", queueSize)
	}
	if details := call("approval_get_details", map[string]any{"approvalId": approvalID}); details.IsError || !strings.Contains(contentText(details), "change the fixture") {
		t.Fatalf("local approval details do not show the exact task: %s", contentText(details))
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
	handler := NewHTTPHandler(Dependencies{Config: config.Config{Host: "127.0.0.1", Port: 8787, AuthMode: config.AuthModeBearer, AuthToken: token, MaxRequestBytes: 64 << 10}})
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

func TestBearerModeAllowsAuthenticatedMCPToolInvocation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	token := strings.Repeat("b", 48)
	handler := NewHTTPHandler(Dependencies{Config: config.Config{Host: "127.0.0.1", Port: port, AuthMode: config.AuthModeBearer, AuthToken: token, MaxRequestBytes: 64 << 10}})
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	client := sdk.NewClient(&sdk.Implementation{Name: "bearer-test", Version: "0.0.0"}, nil)
	transport := &sdk.StreamableClientTransport{Endpoint: fmt.Sprintf("http://127.0.0.1:%d/mcp", port), HTTPClient: &http.Client{Transport: bearerTransport{base: http.DefaultTransport, token: token}, Timeout: 5 * time.Second}, DisableStandaloneSSE: true, MaxRetries: -1}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "bridge_get_capabilities", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatalf("authenticated bearer tool call result=%v err=%v", result, err)
	}
}

func TestCloudflareHTTPAuthHasNoBearerFallbackAndRejectsWrongHost(t *testing.T) {
	handler := NewHTTPHandler(Dependencies{
		Config:             config.Config{Host: "127.0.0.1", Port: 8787, AuthMode: config.AuthModeCloudflareAccess},
		CloudflareVerifier: accessVerifierStub{},
	})
	for _, request := range []struct {
		name   string
		host   string
		header string
		want   int
	}{
		{name: "missing assertion", host: "127.0.0.1:8787", want: http.StatusUnauthorized},
		{name: "bearer does not fall back", host: "127.0.0.1:8787", header: "Bearer local-test-token-that-is-long-enough-123456", want: http.StatusUnauthorized},
		{name: "invalid assertion", host: "127.0.0.1:8787", header: "invalid", want: http.StatusUnauthorized},
		{name: "wrong host", host: "attacker.example.test", header: "synthetic-access-assertion", want: http.StatusForbidden},
	} {
		t.Run(request.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/healthz", nil)
			req.Host = request.host
			if strings.HasPrefix(request.header, "Bearer ") {
				req.Header.Set("Authorization", request.header)
			} else if request.header != "" {
				req.Header.Set("Cf-Access-Jwt-Assertion", request.header)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != request.want {
				t.Fatalf("status=%d, want %d", response.Code, request.want)
			}
		})
	}
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`,
	} {
		for _, auth := range []string{"", "Bearer local-test-token-that-is-long-enough-123456"} {
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/mcp", strings.NewReader(body))
			req.Host = "127.0.0.1:8787"
			req.Header.Set("Content-Type", "application/json")
			if auth != "" {
				req.Header.Set("Authorization", auth)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated MCP request method=%s status=%d, want 401", body, response.Code)
			}
		}
	}

	withoutVerifier := NewHTTPHandler(Dependencies{Config: config.Config{Host: "127.0.0.1", Port: 8787, AuthMode: config.AuthModeCloudflareAccess}})
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/healthz", nil)
	response := httptest.NewRecorder()
	withoutVerifier.ServeHTTP(response, req)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing verifier status=%d, want 503", response.Code)
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
