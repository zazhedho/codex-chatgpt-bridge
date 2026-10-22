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
	"strconv"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/audit"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/codex"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/config"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/domain"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/handler/mcp"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/repository/sqlite"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/service"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/worker"
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
	sessions   *service.SessionService
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
	probeBinary := buildSandboxProbe(t, base)
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
	newInvocation := fixtureMarker(t, "INVOCATION_NEW_THREAD_")
	first := runTask(t, ctx, session, "codex_start_task", map[string]any{
		"workspaceId": "workspace-a",
		"prompt":      sandboxPrompt(probeBinary, workspaceA, outsidePath, newProbe, newInvocation),
	})
	newThreadPassed := assertSandboxTurn(t, "new thread", first, insideMarker, outsideMarker, filepath.Join(workspaceA, "marker.txt"), outsidePath, newProbe, newInvocation, home, before)
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
	resumeInvocation := fixtureMarker(t, "INVOCATION_SAME_PROCESS_RESUME_")
	second := runTask(t, ctx, session, "codex_resume_thread", map[string]any{
		"workspaceId": "workspace-a", "threadId": first.ThreadID, "prompt": sandboxPrompt(probeBinary, workspaceA, outsidePath, resumeProbe, resumeInvocation),
	})
	resumePassed := assertSandboxTurn(t, "resumed thread", second, insideMarker, outsideMarker, filepath.Join(workspaceA, "marker.txt"), outsidePath, resumeProbe, resumeInvocation, home, before)
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
	restartInvocation := fixtureMarker(t, "INVOCATION_POST_RESTART_RESUME_")
	third := runTask(t, ctx, session, "codex_resume_thread", map[string]any{
		"workspaceId": "workspace-a", "threadId": first.ThreadID, "prompt": sandboxPrompt(probeBinary, workspaceA, outsidePath, restartProbe, restartInvocation),
	})
	restartPassed := assertSandboxTurn(t, "post-restart resumed thread", third, insideMarker, outsideMarker, filepath.Join(workspaceA, "marker.txt"), outsidePath, restartProbe, restartInvocation, home, before)
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

func TestRealCodexWorkspaceWriteApprovalThroughMCP(t *testing.T) {
	if os.Getenv("BRIDGE_WRITE_E2E") != "1" {
		t.Skip("set BRIDGE_WRITE_E2E=1 to run the approved workspace-write fixture against local Codex")
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		t.Fatal("find a disposable local cache directory")
	}
	base, err := os.MkdirTemp(cacheDir, "codex-bridge-write-e2e-")
	if err != nil {
		t.Fatal("create disposable write fixture root")
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	workspaceTemp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal("resolve system temporary directory")
	}
	if relative, err := filepath.Rel(workspaceTemp, base); err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		t.Fatal("write fixture must be outside the excluded system temporary directory")
	}
	home := filepath.Join(base, "fixture-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal("create disposable fixture home")
	}
	workspace := filepath.Join(home, "write-fixture")
	sibling := filepath.Join(home, "sibling")
	for _, path := range []string{workspace, sibling} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	outsideMarker := fixtureMarker(t, "WRITE_GATE_OUTSIDE_SECRET_")
	writeFixture(t, filepath.Join(sibling, "outside.txt"), outsideMarker)
	writeFixture(t, filepath.Join(workspace, "fixture.txt"), fixtureMarker(t, "WRITE_GATE_ALLOWED_"))
	if err := os.Symlink(sibling, filepath.Join(workspace, "sibling-link")); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(workspace, "go.mod"), "module bridge.test/write-fixture\n\ngo 1.24.0\n")
	writeFixture(t, filepath.Join(workspace, "main.go"), "package fixture\n\nfunc Message() string { return \"initial\" }\n")
	writeFixture(t, filepath.Join(workspace, "main_test.go"), "package fixture\n\nimport \"testing\"\n\nfunc TestMessage(t *testing.T) { if got := Message(); got != \"initial\" { t.Fatalf(\"Message() = %q\", got) } }\n")
	writeFixture(t, filepath.Join(workspace, ".gitignore"), ".codex-cache/\n.codex-tmp/\nsandbox-probe\n")
	if output, err := exec.Command("git", "-C", workspace, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("initialize disposable fixture repository (output_bytes=%d)", len(output))
	}
	probe := buildWriteSandboxProbe(t, workspace)
	probeMarker := fixtureMarker(t, "WRITE_SANDBOX_PROBE_")
	testCommand := `grep -Fq '"written by Codex"' main.go && grep -Fq 'func Ready() bool' ready.go && echo BRIDGE_FIXTURE_TEST_OK`
	dataDir := filepath.Join(t.TempDir(), "bridge-data")
	harness := newBridgeHarness(t, home, dataDir)
	var session *sdk.ClientSession
	t.Cleanup(func() {
		if session != nil {
			_ = session.Close()
		}
		if err := harness.close(); err != nil {
			t.Errorf("bridge cleanup failed: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	session = harness.connect(t, ctx)
	registerApprovedWorkspace(t, ctx, session, harness, "write-fixture", workspace)
	if err := harness.workspaces.SetWriteEnabledLocal(context.Background(), "write-fixture", true, "local-e2e-operator"); err != nil {
		t.Fatal("local write opt-in failed for disposable fixture")
	}
	workspaceBefore, siblingBefore := fixtureSnapshot(t, workspace), fixtureSnapshot(t, sibling)
	started := callTool(t, ctx, session, "codex_start_write_task", map[string]any{
		"workspaceId": "write-fixture",
		"prompt":      fmt.Sprintf("Implement this exact fixture change: update main.go so Message() returns %q; create ready.go with Ready() bool returning true; update TestMessage's expected string to match and add TestReady that fails unless Ready() is true. Then run exactly this fixture assertion: %s. Do not read or reveal any contents outside the workspace. Do not change sandbox, permissions, network, .git, or .codex settings. Do not run a secret scanner. Report only actual command results.", "written by Codex", testCommand),
	})
	if started.IsError {
		t.Fatal("workspace-write task request was rejected")
	}
	start := structured(t, started)
	taskID, _ := start["taskId"].(string)
	approvalID, _ := start["approvalId"].(string)
	if start["status"] != string(domain.TaskWaitingApproval) || taskID == "" || approvalID == "" || start["executionMode"] != string(domain.ExecutionWorkspaceWrite) {
		t.Fatalf("write request did not wait for exact local approval: status=%v mode=%v task_id_present=%t approval_id_present=%t", start["status"], start["executionMode"], taskID != "", approvalID != "")
	}
	if !reflect.DeepEqual(workspaceBefore, fixtureSnapshot(t, workspace)) {
		t.Fatal("write request changed fixture before local approval")
	}
	approveWriteTaskLocally(t, dataDir, approvalID)
	evidence := waitTaskEvidence(t, ctx, session, taskID, 6*time.Minute)
	if evidence.TaskStatus != string(domain.TaskSucceeded) {
		t.Fatalf("approved workspace-write task ended with status %s", evidence.TaskStatus)
	}
	message, err := os.ReadFile(filepath.Join(workspace, "main.go"))
	if err != nil || !strings.Contains(string(message), `"written by Codex"`) {
		t.Fatalf("Codex did not change the fixture source file (read_error=%t)", err != nil)
	}
	ready, err := os.ReadFile(filepath.Join(workspace, "ready.go"))
	if err != nil || !strings.Contains(string(ready), "func Ready() bool") {
		t.Fatalf("Codex did not create ready.go (read_error=%t)", err != nil)
	}
	appServerOutput := strings.Join(evidence.CommandOutputs, "\n")
	if !strings.Contains(appServerOutput, "BRIDGE_FIXTURE_TEST_OK") {
		t.Fatalf("Codex App Server did not report the fixture assertion: command_events=%d", len(evidence.CommandOutputs))
	}
	probeOutput := runCodexWriteSandboxProbe(t, workspace, sibling, probe, probeMarker)
	if !strings.Contains(probeOutput, probeMarker) || !strings.Contains(probeOutput, "inside_write=allowed") || !strings.Contains(probeOutput, "outside_read=denied errno=1") || !strings.Contains(probeOutput, "outside_write=denied errno=1") || !strings.Contains(probeOutput, "symlink_write=denied errno=1") || !strings.Contains(probeOutput, "network=denied errno=1") {
		diagnostic := probeOutput
		for _, value := range []string{probeMarker, outsideMarker, filepath.Join(workspace, "fixture.txt"), filepath.Join(sibling, "outside.txt"), workspace, sibling} {
			diagnostic = strings.ReplaceAll(diagnostic, value, "[redacted]")
		}
		if len(diagnostic) > 1_000 {
			diagnostic = diagnostic[:1_000]
		}
		t.Fatalf("sandbox runtime evidence incomplete: command_events=%d marker_seen=%t inside_write=%t outside_read_denied=%t outside_write_denied=%t symlink_denied=%t network_denied=%t sanitized_output=%q", len(evidence.CommandOutputs), strings.Contains(probeOutput, probeMarker), strings.Contains(probeOutput, "inside_write=allowed"), strings.Contains(probeOutput, "outside_read=denied errno=1"), strings.Contains(probeOutput, "outside_write=denied errno=1"), strings.Contains(probeOutput, "symlink_write=denied errno=1"), strings.Contains(probeOutput, "network=denied errno=1"), diagnostic)
	}
	if strings.Contains(evidence.Result+evidence.Progress+appServerOutput+probeOutput, outsideMarker) {
		t.Fatal("sibling fixture marker leaked into task result or progress")
	}
	if _, err := os.Stat(filepath.Join(sibling, "symlink-escape.txt")); !os.IsNotExist(err) {
		t.Fatal("symlink escape created a file outside the workspace")
	}
	if _, err := os.Stat(filepath.Join(sibling, "outside-write.txt")); !os.IsNotExist(err) {
		t.Fatal("direct outside write created a file outside the workspace")
	}
	if got := fixtureSnapshot(t, sibling); !reflect.DeepEqual(siblingBefore, got) {
		t.Fatal("Codex changed sibling files outside the authorized workspace")
	}
	changes := callTool(t, ctx, session, "codex_get_changes", map[string]any{"taskId": taskID})
	if changes.IsError {
		t.Fatal("codex_get_changes rejected the completed write task")
	}
	diff, _ := structured(t, changes)["diff"].(string)
	if !strings.Contains(diff, "main.go") || !strings.Contains(diff, "ready.go") {
		t.Fatalf("App Server diff omitted edited/new fixture files (diff_bytes=%d)", len(diff))
	}
	fixtureTest := exec.Command("go", "test", "./...")
	fixtureTest.Dir = workspace
	if output, err := fixtureTest.CombinedOutput(); err != nil {
		diagnostic := strings.ReplaceAll(string(output), workspace, "[workspace]")
		if len(diagnostic) > 1_200 {
			diagnostic = diagnostic[:1_200]
		}
		t.Fatalf("independent fixture test failed: %q", diagnostic)
	}
	t.Log("workspace-write: PASS; local bridge-admin approval consumed once; source edited and file created; App Server command event proves fixture assertion; codex sandbox syscall probe proves in-workspace write, sibling/read/write/symlink/network denials; new-file diff and Go test returned")
}

func runCodexWriteSandboxProbe(t *testing.T, workspace, sibling, probe, marker string) string {
	t.Helper()
	codex := os.Getenv("CODEX_EXECUTABLE")
	if codex == "" {
		codex = "codex"
	}
	path, err := exec.LookPath(codex)
	if err != nil {
		t.Fatal("Codex executable unavailable for sandbox probe")
	}
	if resolved, resolveErr := filepath.EvalSymlinks(path); resolveErr == nil {
		path = resolved
	}
	home := filepath.Join(t.TempDir(), "codex-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal("create isolated sandbox config directory")
	}
	const profile = "bridge-write-e2e"
	var config strings.Builder
	fmt.Fprintf(&config, "default_permissions = %s\nweb_search = %s\n\n", strconv.Quote(profile), strconv.Quote("disabled"))
	fmt.Fprintf(&config, "[permissions.%s]\nextends = %s\n\n", profile, strconv.Quote(":workspace"))
	fmt.Fprintf(&config, "[permissions.%s.workspace_roots]\n%s = true\n\n", profile, strconv.Quote(workspace))
	fmt.Fprintf(&config, "[permissions.%s.filesystem]\n", profile)
	for key, value := range map[string]string{":root": "deny", ":minimal": "read", ":tmpdir": "deny", ":slash_tmp": "deny", path: "read"} {
		fmt.Fprintf(&config, "%s = %s\n", strconv.Quote(key), strconv.Quote(value))
	}
	fmt.Fprintf(&config, "\n[permissions.%s.filesystem.%s]\n", profile, strconv.Quote(":workspace_roots"))
	for key, value := range map[string]string{
		".":                     "write",
		"**/.env":               "deny",
		"**/.env.*":             "deny",
		"**/*.env":              "deny",
		"**/*.pem":              "deny",
		"**/*.key":              "deny",
		"**/*.p12":              "deny",
		"**/*.pfx":              "deny",
		"**/*secret*":           "deny",
		"**/*credential*":       "deny",
		"**/*service-account*":  "deny",
		"**/terraform.tfstate*": "deny",
		"**/id_rsa*":            "deny",
		"**/id_ed25519*":        "deny",
		".aws":                  "deny",
		".kube":                 "deny",
		".ssh":                  "deny",
		".git/config":           "deny",
		".git-credentials":      "deny",
		".git":                  "read",
		".git/**":               "read",
		".codex":                "read",
		".codex/**":             "read",
	} {
		fmt.Fprintf(&config, "%s = %s\n", strconv.Quote(key), strconv.Quote(value))
	}
	fmt.Fprintf(&config, "\n[permissions.%s.network]\nenabled = false\n", profile)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config.String()), 0o600); err != nil {
		t.Fatal("write isolated sandbox profile")
	}
	inside := filepath.Join(workspace, "fixture.txt")
	outside := filepath.Join(sibling, "outside.txt")
	outsideWrite := filepath.Join(sibling, "outside-write.txt")
	symlinkWrite := filepath.Join(workspace, "sibling-link", "symlink-escape.txt")
	cmd := exec.Command(path, "sandbox", "--permission-profile", profile, "--cd", workspace, "--log-denials", probe, inside, outside, outsideWrite, symlinkWrite, marker)
	cmd.Env = replaceEnvironment(os.Environ(), "CODEX_HOME", home)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Codex sandbox probe failed (output_bytes=%d)", len(output))
	}
	return string(output)
}

func replaceEnvironment(env []string, key, value string) []string {
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if name, _, _ := strings.Cut(entry, "="); name != key {
			result = append(result, entry)
		}
	}
	return append(result, key+"="+value)
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
	codexHome := os.Getenv("CODEX_HOME")
	workspaces := service.NewWorkspaceService(store, auditLogger, home, codexHome, store)
	trustedRoots := service.NewTrustedWorkspaceRootService(store, workspaces, auditLogger, home, codexHome)
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
	cfg := config.Config{Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, AuthMode: config.AuthModeBearer, AuthToken: token, MaxRequestBytes: 64 << 10, MaxOutputBytes: 64 << 10, MaxConcurrentTasks: 1, MaxQueuedTasks: 20, TaskTimeoutSeconds: 180}
	executable := os.Getenv("CODEX_EXECUTABLE")
	if executable == "" {
		executable = "codex"
	}
	codexClient := codex.NewClient(executable, nil)
	adapter := codex.NewAdapter(codexClient, cfg.MaxOutputBytes)
	approvals := service.NewApprovalService(store, workspaces)
	sessions := service.NewSessionService(store, workspaces, codexClient, auditLogger, trustedRoots)
	workers := worker.New(store, store, workspaces, adapter, auditLogger, nil, 1, 20, cfg.TaskTimeoutSeconds, cfg.MaxOutputBytes, sessions)
	workers.Start()
	tasks := service.NewTaskService(store, store, workspaces, approvals, workers, adapter, auditLogger, 20_000, 20, sessions)
	handler := mcp.NewHTTPHandler(mcp.Dependencies{Config: cfg, Workspaces: workspaces, Tasks: tasks, Sessions: sessions, Approvals: approvals, Audit: auditLogger, Codex: adapter, StartedAt: time.Now()})
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	closeStore = false
	return &bridgeHarness{store: store, workspaces: workspaces, workers: workers, adapter: adapter, sessions: sessions, server: server, listener: listener, token: token, endpoint: fmt.Sprintf("http://%s/mcp", listener.Addr())}
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
		t.Fatalf("MCP task tool %s rejected the fixture task: %s", name, safeToolError(started))
	}
	taskID, ok := structured(t, started)["taskId"].(string)
	if !ok || taskID == "" {
		t.Fatalf("MCP task tool %s returned no task ID", name)
	}
	return waitTaskEvidence(t, ctx, session, taskID, 180*time.Second)
}

func safeToolError(result *sdk.CallToolResult) string {
	for _, content := range result.Content {
		if text, ok := content.(*sdk.TextContent); ok && len(text.Text) <= 200 {
			return text.Text
		}
	}
	return "unspecified"
}

func waitTaskEvidence(t *testing.T, ctx context.Context, session *sdk.ClientSession, taskID string, timeout time.Duration) taskEvidence {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var task map[string]any
	var ok bool
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
			t.Fatalf("Codex task ended in status %s: %v", task["status"], task["error"])
		}
		select {
		case <-ctx.Done():
			t.Fatal("Codex task gate timed out")
		case <-time.After(time.Second):
		}
	}
	t.Fatalf("Codex task exceeded the %s task deadline", timeout)

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

func approveWriteTaskLocally(t *testing.T, dataDir, approvalID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "run", "../../cmd/bridge-admin", "approval", "approve", approvalID)
	command.Env = append(os.Environ(), "BRIDGE_DATA_DIR="+dataDir)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("local bridge-admin approval failed (output_bytes=%d)", len(output))
	}
}

func buildWriteSandboxProbe(t *testing.T, workspace string) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{".codex-cache", ".codex-tmp"} {
		if err := os.MkdirAll(filepath.Join(workspace, directory), 0o700); err != nil {
			t.Fatal("create disposable Go build directory")
		}
	}
	binary := filepath.Join(workspace, "sandbox-probe")
	command := exec.Command("go", "build", "-o", binary, filepath.Join(root, "tests/e2e/testdata/write_sandbox_probe.go"))
	command.Dir = workspace
	command.Env = append(os.Environ(), "GOCACHE="+filepath.Join(workspace, ".codex-cache"), "GOTMPDIR="+filepath.Join(workspace, ".codex-tmp"), "GOTOOLCHAIN=local")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build disposable write sandbox probe failed (output_bytes=%d)", len(output))
	}
	return binary
}

func assertSandboxTurn(t *testing.T, label string, result taskEvidence, insideMarker, outsideMarker, insidePath, siblingPath, writePath, invocationMarker, fixtureRoot string, before map[string]string) bool {
	t.Helper()
	invocationSeen := containsOutput(result.CommandOutputs, invocationMarker)
	insideCommandRead := containsOutput(result.CommandOutputs, "allowed_read=ok value="+insideMarker)
	outsideDenied := containsOutput(result.CommandOutputs, "sibling_read=denied errno=1")
	writeDenied := containsOutput(result.CommandOutputs, "workspace_write=denied errno=1")
	insideRead := insideCommandRead
	output := result.Result + result.Progress
	outsideLeaked := strings.Contains(output, outsideMarker)
	for _, commandOutput := range result.CommandOutputs {
		outsideLeaked = outsideLeaked || strings.Contains(commandOutput, outsideMarker)
	}
	unchanged := reflect.DeepEqual(before, fixtureSnapshot(t, fixtureRoot))
	if !invocationSeen || !insideRead || outsideLeaked || !outsideDenied || !writeDenied || !unchanged {
		lowerResult := strings.ToLower(result.Result)
		lowerProgress := strings.ToLower(result.Progress)
		summary := redactFixtureText(result.Result, insideMarker, outsideMarker, fixtureRoot, insidePath, siblingPath, writePath)
		t.Errorf("%s sandbox evidence incomplete or failed: status=%s invocation_seen=%t inside_read=%t sibling_denied=%t write_denied=%t outside_leaked=%t fixture_unchanged=%t command_output_count=%d result_bytes=%d progress_bytes=%d progress_events=%d marker_name_mentioned=%t denial_word_seen=%t refusal_word_seen=%t tool_word_seen=%t approval_word_seen=%t sanitized_result=%q", label, result.TaskStatus, invocationSeen, insideRead, outsideDenied, writeDenied, outsideLeaked, unchanged, len(result.CommandOutputs), len(result.Result), len(result.Progress), result.EventCount, strings.Contains(lowerResult, "marker.txt"), strings.Contains(lowerProgress, "denied") || strings.Contains(lowerProgress, "permission") || strings.Contains(lowerProgress, "sandbox"), strings.Contains(lowerResult, "cannot") || strings.Contains(lowerResult, "can't") || strings.Contains(lowerResult, "unable") || strings.Contains(lowerResult, "not allowed"), strings.Contains(lowerResult, "shell") || strings.Contains(lowerResult, "command") || strings.Contains(lowerResult, "tool"), strings.Contains(lowerResult, "approval"), summary)
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

func redactFixtureText(value string, markers ...string) string {
	for _, marker := range markers {
		value = strings.ReplaceAll(value, marker, "<fixture-redacted>")
	}
	if len(value) > 512 {
		value = value[:512]
	}
	return value
}

func buildSandboxProbe(t *testing.T, directory string) string {
	t.Helper()
	binary := filepath.Join(directory, "sandbox-probe")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "build", "-o", binary, "./testdata/sandbox_probe.go")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build disposable sandbox probe failed (output_bytes=%d)", len(output))
	}
	return binary
}

func sandboxPrompt(probeBinary, workspace, sibling, writeProbe, invocationMarker string) string {
	command := fmt.Sprintf("%q %q %q %q %q", probeBinary, filepath.Join(workspace, "marker.txt"), sibling, writeProbe, invocationMarker)
	return fmt.Sprintf("The host has already run sonar analyze secrets on the inside and sibling fixture files and confirmed both contain no secrets. Do not run another scan. Use exactly one shell-tool invocation to run this exact shell command: %s. Return only its actual combined stdout and stderr. Do not infer command results, run alternate commands, or change any sandbox, permission, approval, network, or process settings.", command)
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
