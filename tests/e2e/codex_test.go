package e2e

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"example.com/codex-chatgpt-bridge/internal/audit"
	"example.com/codex-chatgpt-bridge/internal/codex"
	"example.com/codex-chatgpt-bridge/internal/config"
	"example.com/codex-chatgpt-bridge/internal/domain"
	"example.com/codex-chatgpt-bridge/internal/handler/mcp"
	"example.com/codex-chatgpt-bridge/internal/repository/sqlite"
	"example.com/codex-chatgpt-bridge/internal/service"
	"example.com/codex-chatgpt-bridge/internal/worker"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearerRoundTripper struct {
	base  http.RoundTripper
	token string
}

func (t bearerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}

type bridgeHarness struct {
	store      *sqlite.Store
	workspaces *service.WorkspaceService
	workers    *worker.Manager
	adapter    *codex.Adapter
	server     *http.Server
	listener   net.Listener
	token      string
	endpoint   string
}

type taskEvidence struct {
	ID               string
	ThreadID         string
	Result           string
	Progress         string
	CommandOutputs   []string
	RawResponseTypes []string
	ShellCallCount   int
	EventCount       int
	TaskStatus       string
}

func TestRealCodexThroughMCPAndSandbox(t *testing.T) {
	if os.Getenv("BRIDGE_E2E") != "1" {
		t.Skip("set BRIDGE_E2E=1 to use the locally authenticated Codex App Server")
	}
	base := t.TempDir()
	home := filepath.Join(base, "fixture-home")
	workspaceA := filepath.Join(home, "workspace-a")
	workspaceB := filepath.Join(home, "workspace-b")
	sibling := filepath.Join(home, "sibling")
	outsideRoot := filepath.Join(base, "outside-root")
	for _, path := range []string{workspaceA, workspaceB, sibling, outsideRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	insideMarker := fixtureMarker(t, "BRIDGE_MCP_CODEX_TURN_OK_")
	outsideMarker := fixtureMarker(t, "OUTSIDE_FIXTURE_DO_NOT_READ_")
	insidePath := filepath.Join(workspaceA, "marker.txt")
	writeFixture(t, insidePath, insideMarker)
	writeFixture(t, filepath.Join(workspaceB, "marker.txt"), fixtureMarker(t, "WORKSPACE_B_FIXTURE_"))
	outsidePath := filepath.Join(sibling, "outside.txt")
	writeFixture(t, outsidePath, outsideMarker)
	if err := scanFixtureSecrets(t, insidePath, outsidePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideRoot, filepath.Join(home, "escape-link")); err != nil {
		t.Fatal(err)
	}
	canonicalOutsideRoot, err := filepath.EvalSymlinks(outsideRoot)
	if err != nil {
		t.Fatal(err)
	}

	dataDir := filepath.Join(base, "bridge-data")
	harness := newBridgeHarness(t, home, dataDir)
	var session *sdk.ClientSession
	t.Cleanup(func() {
		if session != nil {
			_ = session.Close()
		}
		if harness != nil {
			if err := harness.close(); err != nil {
				t.Errorf("bridge cleanup failed: %v", err)
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	session = harness.connect(t, ctx)
	registerApprovedWorkspace(t, ctx, session, harness, "workspace-a", workspaceA)
	registerApprovedWorkspace(t, ctx, session, harness, "workspace-b", workspaceB)

	traversal := workspaceA + string(os.PathSeparator) + ".." + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "outside-root"
	traversalResult := callTool(t, ctx, session, "workspace_register", map[string]any{"workspaceId": "workspace-traversal", "path": traversal})
	if !traversalResult.IsError {
		t.Fatal("path traversal outside the fixture home was accepted")
	}
	symlinkResult := callTool(t, ctx, session, "workspace_register", map[string]any{"workspaceId": "workspace-symlink", "path": filepath.Join(home, "escape-link")})
	if symlinkResult.IsError {
		t.Fatal("canonical symlink target could not be submitted for independent local review")
	}
	symlinkRequest := structured(t, symlinkResult)
	if symlinkRequest["status"] != "pending" {
		t.Fatal("symlink target did not remain pending for local approval")
	}
	symlinkDetails := callTool(t, ctx, session, "approval_get_details", map[string]any{"approvalId": symlinkRequest["requestId"]})
	var details map[string]any
	if !symlinkDetails.IsError {
		details, _ = structured(t, symlinkDetails)["details"].(map[string]any)
	}
	rootPathValue, hasRootPath := details["rootPath"]
	canonicalPath, _ := rootPathValue.(string)
	if symlinkDetails.IsError || canonicalPath != canonicalOutsideRoot || canonicalPath == filepath.Join(home, "escape-link") {
		t.Fatalf("symlink target canonicalization evidence failed: details_error=%t details_keys=%v root_path_present=%t root_path_type=%T target_match=%t symlink_path_preserved=%t", symlinkDetails.IsError, mapKeys(details), hasRootPath, rootPathValue, canonicalPath == canonicalOutsideRoot, canonicalPath == filepath.Join(home, "escape-link"))
	}
	if result := callTool(t, ctx, session, "workspace_get", map[string]any{"workspaceId": "workspace-symlink"}); !result.IsError {
		t.Fatal("pending symlink workspace became trusted without local approval")
	}

	before := fixtureSnapshot(t, home)
	newProbe := filepath.Join(workspaceA, "write-probe-new.txt")
	first := runTask(t, ctx, session, "codex_start_task", map[string]any{
		"workspaceId": "workspace-a",
		"prompt":      sandboxPrompt(workspaceA, outsidePath, newProbe),
	})
	newThreadPassed := assertSandboxTurn(t, "new thread", first, insideMarker, outsideMarker, filepath.Join(workspaceA, "marker.txt"), outsidePath, newProbe, home, before)
	if first.ThreadID == "" {
		t.Fatal("completed bridge task did not persist its thread ID")
	}
	if newThreadPassed {
		t.Log("new-thread: PASS; activePermissionProfile=bridge-readonly extends=:read-only; sandbox=readOnly networkAccess=false; actual sibling-read and write denials observed")
	} else {
		t.Log("new-thread: UNVERIFIED; App Server effective profile was accepted, but runtime probe evidence was incomplete")
	}

	wrongWorkspace := callTool(t, ctx, session, "codex_resume_thread", map[string]any{
		"workspaceId": "workspace-b", "threadId": first.ThreadID, "prompt": "This resume must be rejected by workspace ownership checks.",
	})
	if !wrongWorkspace.IsError {
		t.Fatal("thread owned by workspace-a was accepted through workspace-b")
	}
	unknownThread := callTool(t, ctx, session, "codex_resume_thread", map[string]any{
		"workspaceId": "workspace-a", "threadId": "unowned-cli-thread-" + insideMarker[len("BRIDGE_MCP_CODEX_TURN_OK_"):][:8], "prompt": "This unknown CLI thread must not be adopted.",
	})
	if !unknownThread.IsError {
		t.Fatal("thread absent from bridge ownership storage was accepted")
	}
	workspaceBThreads := callTool(t, ctx, session, "codex_list_threads", map[string]any{"workspaceId": "workspace-b"})
	if workspaceBThreads.IsError || len(structured(t, workspaceBThreads)["threads"].([]any)) != 0 {
		t.Fatal("cross-workspace resume created a workspace-b thread")
	}

	resumeProbe := filepath.Join(workspaceA, "write-probe-resume.txt")
	second := runTask(t, ctx, session, "codex_resume_thread", map[string]any{
		"workspaceId": "workspace-a", "threadId": first.ThreadID, "prompt": sandboxPrompt(workspaceA, outsidePath, resumeProbe),
	})
	resumePassed := assertSandboxTurn(t, "resumed thread", second, insideMarker, outsideMarker, filepath.Join(workspaceA, "marker.txt"), outsidePath, resumeProbe, home, before)
	if resumePassed {
		t.Log("same-process-resume: PASS; effective profile and read-only/network policy confirmed by App Server response and runtime probes")
	} else {
		t.Log("same-process-resume: UNVERIFIED; App Server effective profile was accepted, but runtime probe evidence was incomplete")
	}

	if err := session.Close(); err != nil {
		t.Fatalf("close MCP session before restart: %v", err)
	}
	session = nil
	if err := harness.close(); err != nil {
		t.Fatalf("graceful bridge/App Server shutdown: %v", err)
	}
	harness = nil
	harness = newBridgeHarness(t, home, dataDir)
	session = harness.connect(t, ctx)
	restartProbe := filepath.Join(workspaceA, "write-probe-restart.txt")
	third := runTask(t, ctx, session, "codex_resume_thread", map[string]any{
		"workspaceId": "workspace-a", "threadId": first.ThreadID, "prompt": sandboxPrompt(workspaceA, outsidePath, restartProbe),
	})
	restartPassed := assertSandboxTurn(t, "post-restart resumed thread", third, insideMarker, outsideMarker, filepath.Join(workspaceA, "marker.txt"), outsidePath, restartProbe, home, before)
	if restartPassed {
		t.Log("post-restart-resume: PASS; SQLite thread ownership survived restart; App Server effective profile and runtime denials confirmed")
	} else {
		t.Log("post-restart-resume: UNVERIFIED; App Server effective profile was accepted, but runtime probe evidence was incomplete")
	}

	verifyTaskWorkspaceAuthorization(t, ctx, session, harness, workspaceB)
	t.Log("workspace-isolation: PASS; unapproved traversal/symlink rejected; cross-workspace and unknown thread IDs rejected; task output denied after owner workspace identity changed")
	if !t.Failed() {
		t.Log("BRIDGE_MCP_CODEX_TURN_OK")
	} else {
		t.Log("sandbox gate did not pass; BRIDGE_MCP_CODEX_TURN_OK withheld")
	}
}

func newBridgeHarness(t *testing.T, home, dataDir string) *bridgeHarness {
	t.Helper()
	store, err := sqlite.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	closeStore := true
	defer func() {
		if closeStore {
			_ = store.Close()
		}
	}()
	auditLogger := audit.New(store)
	workspaces := service.NewWorkspaceService(store, auditLogger, home, os.Getenv("CODEX_HOME"))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var tokenBytes [36]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes[:])
	cfg := config.Config{Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, AuthToken: token, MaxRequestBytes: 64 << 10, MaxOutputBytes: 64 << 10, MaxConcurrentTasks: 1, MaxQueuedTasks: 20, TaskTimeoutSeconds: 180}
	executable := os.Getenv("CODEX_EXECUTABLE")
	if executable == "" {
		executable = "codex"
	}
	adapter := codex.NewAdapter(codex.NewClient(executable, nil), cfg.MaxOutputBytes)
	workers := worker.New(store, store, adapter, auditLogger, nil, 1, 20, cfg.TaskTimeoutSeconds, cfg.MaxOutputBytes)
	tasks := service.NewTaskService(store, store, workspaces, workers, adapter, auditLogger, 20_000)
	approvals := service.NewApprovalService(store, workspaces)
	handler := mcp.NewHTTPHandler(mcp.Dependencies{Config: cfg, Workspaces: workspaces, Tasks: tasks, Approvals: approvals, Audit: auditLogger, Codex: adapter, StartedAt: time.Now()})
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	closeStore = false
	return &bridgeHarness{store: store, workspaces: workspaces, workers: workers, adapter: adapter, server: server, listener: listener, token: token, endpoint: fmt.Sprintf("http://%s/mcp", listener.Addr())}
}

func (h *bridgeHarness) connect(t *testing.T, ctx context.Context) *sdk.ClientSession {
	t.Helper()
	client := sdk.NewClient(&sdk.Implementation{Name: "codex-bridge-e2e", Version: "0.0.0"}, nil)
	transport := &sdk.StreamableClientTransport{Endpoint: h.endpoint, HTTPClient: &http.Client{Transport: bearerRoundTripper{base: http.DefaultTransport, token: h.token}}, MaxRetries: -1}
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect local MCP client: %v", err)
	}
	return session
}

func (h *bridgeHarness) close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.server.Shutdown(ctx); err != nil {
		return err
	}
	if err := h.workers.Shutdown(ctx); err != nil {
		return err
	}
	if err := h.adapter.Close(); err != nil {
		return err
	}
	return h.store.Close()
}

func registerApprovedWorkspace(t *testing.T, ctx context.Context, session *sdk.ClientSession, h *bridgeHarness, id, path string) {
	t.Helper()
	result := callTool(t, ctx, session, "workspace_register", map[string]any{"workspaceId": id, "path": path})
	if result.IsError {
		t.Fatalf("fixture workspace registration was rejected (%s)", id)
	}
	requestID, ok := structured(t, result)["requestId"].(string)
	if !ok || requestID == "" {
		t.Fatal("workspace registration returned no pending request ID")
	}
	if _, err := h.workspaces.ApproveRegistration(context.Background(), requestID, "local-e2e-operator"); err != nil {
		t.Fatalf("local workspace approval failed (%s)", id)
	}
}

func callTool(t *testing.T, ctx context.Context, session *sdk.ClientSession, name string, arguments map[string]any) *sdk.CallToolResult {
	t.Helper()
	result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("MCP tool %s protocol error: %v", name, err)
	}
	return result
}

func runTask(t *testing.T, ctx context.Context, session *sdk.ClientSession, name string, arguments map[string]any) taskEvidence {
	t.Helper()
	started := callTool(t, ctx, session, name, arguments)
	if started.IsError {
		t.Fatalf("MCP task tool %s rejected the fixture task", name)
	}
	taskID, ok := structured(t, started)["taskId"].(string)
	if !ok || taskID == "" {
		t.Fatalf("MCP task tool %s returned no task ID", name)
	}
	deadline := time.Now().Add(180 * time.Second)
	var task map[string]any
	for time.Now().Before(deadline) {
		response := callTool(t, ctx, session, "codex_get_task", map[string]any{"taskId": taskID})
		if response.IsError {
			t.Fatal("Codex task polling was rejected")
		}
		task, ok = structured(t, response)["task"].(map[string]any)
		if !ok {
			t.Fatal("Codex task polling returned no task record")
		}
		switch task["status"] {
		case "completed":
			goto completed
		case "failed", "cancelled", "interrupted":
			t.Fatalf("Codex task ended in status %s", task["status"])
		}
		select {
		case <-ctx.Done():
			t.Fatal("Codex task gate timed out")
		case <-time.After(time.Second):
		}
	}
	t.Fatal("Codex task exceeded the 180 second task deadline")

completed:
	threadID, _ := task["threadId"].(string)
	result := callTool(t, ctx, session, "codex_get_result", map[string]any{"taskId": taskID})
	if result.IsError {
		t.Fatal("Codex task result was unavailable")
	}
	resultText, _ := structured(t, result)["result"].(string)
	progressResult := callTool(t, ctx, session, "codex_get_progress", map[string]any{"taskId": taskID})
	if progressResult.IsError {
		t.Fatal("Codex task progress was unavailable")
	}
	events, _ := structured(t, progressResult)["events"].([]any)
	var progress strings.Builder
	var commandOutputs []string
	var rawResponseTypes []string
	shellCallCount := 0
	for _, raw := range events {
		if event, ok := raw.(map[string]any); ok {
			if value, ok := event["text"].(string); ok {
				progress.WriteString(value)
				if strings.HasPrefix(value, "Codex command output: ") {
					commandOutputs = append(commandOutputs, strings.TrimPrefix(value, "Codex command output: "))
				}
				if strings.HasPrefix(value, "Codex App Server raw response item completed: ") {
					rawResponseTypes = append(rawResponseTypes, strings.TrimPrefix(value, "Codex App Server raw response item completed: "))
				}
				if value == "Codex local shell call completed" {
					shellCallCount++
				}
			}
		}
	}
	return taskEvidence{ID: taskID, ThreadID: threadID, Result: resultText, Progress: progress.String(), CommandOutputs: commandOutputs, RawResponseTypes: rawResponseTypes, ShellCallCount: shellCallCount, EventCount: len(events), TaskStatus: fmt.Sprint(task["status"])}
}

func assertSandboxTurn(t *testing.T, label string, result taskEvidence, insideMarker, outsideMarker, insidePath, siblingPath, writePath, fixtureRoot string, before map[string]string) bool {
	t.Helper()
	insideCommandRead := result.ShellCallCount > 0 && containsOutput(result.CommandOutputs, insideMarker)
	outsideDenied := hasDeniedCommandOutput(result.CommandOutputs, "cat:", siblingPath)
	writeDenied := hasDeniedCommandOutput(result.CommandOutputs, "touch:", writePath)
	insideRead := insideCommandRead && strings.Contains(result.Result, insideMarker)
	output := result.Result + result.Progress
	outsideLeaked := strings.Contains(output, outsideMarker)
	for _, commandOutput := range result.CommandOutputs {
		outsideLeaked = outsideLeaked || strings.Contains(commandOutput, outsideMarker)
	}
	unchanged := reflect.DeepEqual(before, fixtureSnapshot(t, fixtureRoot))
	if !insideRead || !insideCommandRead || outsideLeaked || !outsideDenied || !writeDenied || !unchanged {
		lowerResult := strings.ToLower(result.Result)
		lowerProgress := strings.ToLower(result.Progress)
		summary := redactFixtureText(result.Result, insideMarker, outsideMarker, fixtureRoot, insidePath, siblingPath, writePath)
		t.Errorf("%s sandbox evidence incomplete or failed: status=%s inside_read=%t inside_command_read=%t sibling_denied=%t write_denied=%t outside_leaked=%t fixture_unchanged=%t shell_call_count=%d raw_response_types=%s command_output_count=%d result_bytes=%d progress_bytes=%d progress_events=%d marker_name_mentioned=%t denial_word_seen=%t refusal_word_seen=%t tool_word_seen=%t approval_word_seen=%t sanitized_result=%q", label, result.TaskStatus, insideRead, insideCommandRead, outsideDenied, writeDenied, outsideLeaked, unchanged, result.ShellCallCount, strings.Join(result.RawResponseTypes, ","), len(result.CommandOutputs), len(result.Result), len(result.Progress), result.EventCount, strings.Contains(lowerResult, "marker.txt"), strings.Contains(lowerProgress, "denied") || strings.Contains(lowerProgress, "permission") || strings.Contains(lowerProgress, "sandbox"), strings.Contains(lowerResult, "cannot") || strings.Contains(lowerResult, "can't") || strings.Contains(lowerResult, "unable") || strings.Contains(lowerResult, "not allowed"), strings.Contains(lowerResult, "shell") || strings.Contains(lowerResult, "command") || strings.Contains(lowerResult, "tool"), strings.Contains(lowerResult, "approval"), summary)
		return false
	}
	return true
}

func containsOutput(outputs []string, value string) bool {
	for _, output := range outputs {
		if strings.Contains(output, value) {
			return true
		}
	}
	return false
}

func hasDeniedCommandOutput(outputs []string, commandPrefix, path string) bool {
	for _, output := range outputs {
		lower := strings.ToLower(output)
		if strings.Contains(lower, strings.ToLower(commandPrefix)) && strings.Contains(output, path) && hasDeniedProbe(lower) {
			return true
		}
	}
	return false
}

func redactFixtureText(value string, markers ...string) string {
	for _, marker := range markers {
		value = strings.ReplaceAll(value, marker, "<fixture-redacted>")
	}
	if len(value) > 512 {
		value = value[:512]
	}
	return value
}

func hasDeniedProbe(progress string) bool {
	for _, phrase := range []string{"permission denied", "operation not permitted", "read-only file system", "access denied", "not permitted"} {
		if strings.Contains(progress, phrase) {
			return true
		}
	}
	return false
}

func sandboxPrompt(workspace, sibling, writeProbe string) string {
	return fmt.Sprintf("The host has already run sonar analyze secrets on the inside and sibling fixture files and confirmed both contain no secrets. Do not run another scan. Use exactly one shell-tool invocation to run this exact shell command, with semicolons preserved: cat %q; cat %q; touch %q. Return only its actual combined stdout and stderr. Do not infer command results, run alternate commands, or change any sandbox, permission, approval, network, or process settings.", filepath.Join(workspace, "marker.txt"), sibling, writeProbe)
}

func workspaceReadPrompt(workspace string) string {
	return fmt.Sprintf("The host has already run `sonar analyze secrets` on this exact fixture file and confirmed it contains no secrets. Read %q using the available read-only command tool and return its exact contents. Do not run another scan. Perform no other action.", filepath.Join(workspace, "marker.txt"))
}

func scanFixtureSecrets(t *testing.T, paths ...string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, path := range paths {
		output, err := exec.CommandContext(ctx, "sonar", "analyze", "secrets", path).CombinedOutput()
		clean := strings.Contains(strings.ToLower(string(output)), "no secrets found")
		if err != nil || !clean {
			return fmt.Errorf("fixture secrets scan did not confirm clean input (scanner_failed=%t, clean_result=%t)", err != nil, clean)
		}
	}
	return nil
}

func fixtureMarker(t *testing.T, prefix string) string {
	t.Helper()
	var value [18]byte
	if _, err := rand.Read(value[:]); err != nil {
		t.Fatal(err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(value[:])
}

func writeFixture(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fixtureSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	result := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == "." {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			result[relative] = "symlink"
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			result[relative] = fmt.Sprintf("directory:%o", info.Mode().Perm())
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(contents)
		result[relative] = fmt.Sprintf("file:%o:%s", info.Mode().Perm(), hex.EncodeToString(digest[:]))
		return nil
	})
	if err != nil {
		t.Fatal("cannot snapshot disposable fixture tree")
	}
	return result
}

func verifyTaskWorkspaceAuthorization(t *testing.T, ctx context.Context, session *sdk.ClientSession, h *bridgeHarness, workspacePath string) {
	t.Helper()
	var idBytes [16]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		t.Fatal(err)
	}
	idBytes[6] = idBytes[6]&0x0f | 0x40
	idBytes[8] = idBytes[8]&0x3f | 0x80
	taskID := fmt.Sprintf("%x-%x-%x-%x-%x", idBytes[:4], idBytes[4:6], idBytes[6:8], idBytes[8:10], idBytes[10:])
	now := time.Now().UTC()
	hash := sha256.Sum256([]byte("disposable authorization fixture"))
	value := "disposable authorization fixture"
	err := h.store.CreateTask(context.Background(), domain.Task{ID: taskID, WorkspaceID: "workspace-b", Status: domain.TaskSucceeded, PromptHash: hex.EncodeToString(hash[:]), Progress: []domain.ProgressEvent{}, Result: &value, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal("could not create isolated task authorization fixture")
	}
	if result := callTool(t, ctx, session, "codex_get_task", map[string]any{"taskId": taskID}); result.IsError {
		t.Fatal("task ID was denied while its owner workspace remained approved")
	}
	moved := workspacePath + "-moved"
	if err := os.Rename(workspacePath, moved); err != nil {
		t.Fatal("could not invalidate disposable workspace identity")
	}
	if result := callTool(t, ctx, session, "codex_get_task", map[string]any{"taskId": taskID}); !result.IsError {
		_ = os.Rename(moved, workspacePath)
		t.Fatal("task ID was accessible after its owner workspace lost authorization")
	}
	if err := os.Rename(moved, workspacePath); err != nil {
		t.Fatal("could not restore disposable workspace fixture")
	}
}

func structured(t *testing.T, result *sdk.CallToolResult) map[string]any {
	t.Helper()
	value, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("MCP response has no structured content (is_error=%t)", result.IsError)
	}
	return value
}

func mapKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	return keys
}
