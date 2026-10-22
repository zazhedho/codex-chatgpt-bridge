package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/domain"
)

func TestTrustedRootScopedSessionReadsAcrossRepositoriesReadOnly(t *testing.T) {
	if os.Getenv("BRIDGE_ROOT_SCOPED_E2E") != "1" {
		t.Skip("set BRIDGE_ROOT_SCOPED_E2E=1 to run root-scoped session and sandbox E2E")
	}
	base := t.TempDir()
	codexPath, cliEnv, provider := setupDeterministicCodexCLI(t, base)
	workspaceRoot := filepath.Join(base, "workspaces")
	rootA := filepath.Join(workspaceRoot, "root-a")
	rootB := filepath.Join(workspaceRoot, "root-b")
	repoOne, repoTwo := filepath.Join(rootA, "repo-one"), filepath.Join(rootA, "repo-two")
	repoB := filepath.Join(rootB, "repo-b")
	for _, path := range []string{repoOne, repoTwo, repoB} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal("create disposable repository")
		}
		if output, err := exec.Command("git", "-C", path, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("initialize disposable repository (output_bytes=%d)", len(output))
		}
	}
	firstMarker := fixtureMarker(t, "ROOT_SCOPE_REPO_ONE_")
	secondMarker := fixtureMarker(t, "ROOT_SCOPE_REPO_TWO_")
	otherRootMarker := fixtureMarker(t, "ROOT_SCOPE_OTHER_ROOT_")
	contextA := fixtureMarker(t, "ROOT_SCOPE_CONTEXT_A_")
	contextB := fixtureMarker(t, "ROOT_SCOPE_CONTEXT_B_")
	writeFixture(t, filepath.Join(repoOne, "fixture.txt"), firstMarker)
	writeFixture(t, filepath.Join(repoTwo, "fixture.txt"), secondMarker)
	writeFixture(t, filepath.Join(repoB, "fixture.txt"), otherRootMarker)
	if err := os.Symlink(repoB, filepath.Join(repoOne, "outside-link")); err != nil {
		t.Fatal("create disposable symlink escape")
	}
	threadA := createDisposableCLISession(t, codexPath, cliEnv, rootA, contextA)
	time.Sleep(1100 * time.Millisecond)
	threadB := createDisposableCLISession(t, codexPath, cliEnv, rootB, contextB)
	if threadA == threadB {
		t.Fatal("Codex CLI reused a thread ID for separate trusted roots")
	}
	probe := filepath.Join(rootA, "root-scoped-probe")
	build := exec.Command("go", "build", "-o", probe, "./testdata/root_scoped_probe.go")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build read-only fixture probe (output_bytes=%d)", len(output))
	}

	dataDir := filepath.Join(base, "bridge-data")
	approveRoot := func(path string) string {
		t.Helper()
		output := runBridgeAdmin(t, dataDir, "trusted-root", "add", path)
		var request struct {
			RequestID string `json:"requestId"`
		}
		if err := json.Unmarshal(output, &request); err != nil || request.RequestID == "" {
			t.Fatal("trusted-root request returned no request ID")
		}
		runBridgeAdmin(t, dataDir, "trusted-root", "approve", request.RequestID)
		return request.RequestID
	}
	rootAID, rootBID := approveRoot(rootA), approveRoot(rootB)
	runBridgeAdmin(t, dataDir, "codex-session", "catalog", "enable")
	harness := newBridgeHarness(t, workspaceRoot, dataDir)
	var session *sdk.ClientSession
	t.Cleanup(func() {
		if session != nil {
			_ = session.Close()
		}
		if err := harness.close(); err != nil {
			t.Errorf("bridge cleanup failed: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	session = harness.connect(t, ctx)
	catalog := callTool(t, ctx, session, "codex_list_local_sessions", map[string]any{"limit": 50})
	if catalog.IsError {
		t.Fatal("trusted-root session catalog failed")
	}
	rows, _ := structured(t, catalog)["sessions"].([]any)
	entries := make(map[string]map[string]any, len(rows))
	for _, row := range rows {
		entry := catalogRow(t, row)
		id, _ := entry["threadId"].(string)
		entries[id] = entry
	}
	entryA, okA := entries[threadA]
	entryB, okB := entries[threadB]
	if !okA || !okB || entryA["canResume"] != true || entryB["canResume"] != true || entryA["connectionStatus"] != "trusted_root" || entryB["connectionStatus"] != "trusted_root" {
		t.Fatalf("parent-root sessions were not resumable without project binding (root_a=%t root_b=%t)", okA, okB)
	}
	workspaceA, _ := entryA["workspaceId"].(string)
	workspaceB, _ := entryB["workspaceId"].(string)
	if workspaceA == "" || workspaceB == "" || workspaceA == workspaceB {
		t.Fatal("trusted roots did not receive distinct root-scoped workspaces")
	}
	if crossRoot := callTool(t, ctx, session, "codex_resume_thread", map[string]any{"workspaceId": workspaceA, "threadId": threadB, "prompt": "Cross-root resume must be denied."}); !crossRoot.IsError {
		t.Fatal("thread from root B resumed through root A")
	}

	for _, test := range []struct {
		workspaceID string
		threadID    string
		marker      string
	}{{workspaceA, threadA, contextA}, {workspaceB, threadB, contextB}} {
		resumed := runTask(t, ctx, session, "codex_resume_thread", map[string]any{
			"workspaceId": test.workspaceID,
			"threadId":    test.threadID,
			"prompt":      "Recall the exact marker from the original Terminal session. Do not read files or use tools. Return only that marker.",
		})
		if resumed.TaskStatus != string(domain.TaskSucceeded) || resumed.ThreadID != test.threadID || !strings.Contains(resumed.Result, test.marker) {
			t.Fatalf("root-scoped resume did not preserve its original context (status=%s thread_match=%t marker_match=%t)", resumed.TaskStatus, resumed.ThreadID == test.threadID, strings.Contains(resumed.Result, test.marker))
		}
	}

	before := fixtureSnapshot(t, rootA)
	invocation := fixtureMarker(t, "ROOT_SCOPE_PROBE_RUN_")
	writePath := filepath.Join(repoOne, "must-not-be-created.txt")
	command := fmt.Sprintf("%q %q %q %q %q %q %q", probe, filepath.Join(repoOne, "fixture.txt"), filepath.Join(repoTwo, "fixture.txt"), filepath.Join(repoB, "fixture.txt"), filepath.Join(repoOne, "outside-link", "fixture.txt"), writePath, invocation)
	provider.queueProbe(invocation, command)
	prompt := "Execute the deterministic fixture probe ROOT_SCOPE_PROBE:" + invocation + ". Return its actual command output."
	probeRun := runTask(t, ctx, session, "codex_resume_thread", map[string]any{"workspaceId": workspaceA, "threadId": threadA, "prompt": prompt})
	output := strings.Join(probeRun.CommandOutputs, "\n")
	if probeRun.TaskStatus != string(domain.TaskSucceeded) || probeRun.ThreadID != threadA ||
		!strings.Contains(output, "invocation="+invocation) || !strings.Contains(output, "repo_one=ok value="+firstMarker) || !strings.Contains(output, "repo_two=ok value="+secondMarker) ||
		!strings.Contains(output, "other_root=denied") || !strings.Contains(output, "symlink=denied") || !strings.Contains(output, "write=denied") ||
		strings.Contains(output+probeRun.Result+probeRun.Progress, otherRootMarker) {
		t.Fatalf("root-scoped sandbox evidence incomplete (status=%s command_events=%d invocation=%t repo_one=%t repo_two=%t other_root_denied=%t symlink_denied=%t write_denied=%t outside_marker_leaked=%t)",
			probeRun.TaskStatus, len(probeRun.CommandOutputs), strings.Contains(output, "invocation="+invocation), strings.Contains(output, "repo_one=ok value="+firstMarker), strings.Contains(output, "repo_two=ok value="+secondMarker),
			strings.Contains(output, "other_root=denied"), strings.Contains(output, "symlink=denied"), strings.Contains(output, "write=denied"), strings.Contains(output+probeRun.Result+probeRun.Progress, otherRootMarker))
	}
	if !strings.Contains(probeRun.Result+probeRun.Progress, "effective sandbox") && !strings.Contains(output, "invocation="+invocation) {
		t.Fatal("root-scoped task has no App Server runtime command evidence")
	}
	if !mapsEqual(before, fixtureSnapshot(t, rootA)) {
		t.Fatal("read-only root-scoped task modified its trusted root")
	}
	if _, err := os.Stat(writePath); !os.IsNotExist(err) {
		t.Fatal("read-only probe created a file in a repository")
	}
	if err := session.Close(); err != nil {
		t.Fatal("close MCP session before restart")
	}
	session = nil
	if err := harness.close(); err != nil {
		t.Fatal("graceful root-session Bridge/App Server restart")
	}
	harness = newBridgeHarness(t, workspaceRoot, dataDir)
	session = harness.connect(t, ctx)
	restarted := runTask(t, ctx, session, "codex_resume_thread", map[string]any{
		"workspaceId": workspaceA,
		"threadId":    threadA,
		"prompt":      "Recall the exact marker from before the Bridge restart. Do not read files or use tools. Return only that marker.",
	})
	if restarted.TaskStatus != string(domain.TaskSucceeded) || restarted.ThreadID != threadA || !strings.Contains(restarted.Result, contextA) {
		t.Fatalf("root-scoped context or trust did not survive Bridge/App Server restart (status=%s thread_match=%t marker_match=%t)", restarted.TaskStatus, restarted.ThreadID == threadA, strings.Contains(restarted.Result, contextA))
	}
	if err := harness.workspaces.SetWriteEnabledLocal(context.Background(), workspaceA, true, "local-e2e-owner"); err == nil {
		t.Fatal("trusted root was made writable through bridge-admin")
	}
	if err := harness.store.SetWorkspaceWriteEnabled(ctx, workspaceA, true); err != nil {
		t.Fatal("could not prepare stale root-write state for the defense-in-depth check")
	}
	if write := callTool(t, ctx, session, "codex_start_write_task", map[string]any{"workspaceId": workspaceA, "threadId": threadA, "prompt": "Stale root-wide write state must still be denied."}); !write.IsError {
		t.Fatal("stale root-wide write flag bypassed the root-scope policy")
	}
	if err := harness.store.SetWorkspaceWriteEnabled(ctx, workspaceA, false); err != nil {
		t.Fatal("could not restore the disposable root workspace to read-only")
	}
	if write := callTool(t, ctx, session, "codex_start_write_task", map[string]any{"workspaceId": workspaceA, "threadId": threadA, "prompt": "Write a fixture file in this trusted root."}); !write.IsError {
		t.Fatal("root-scoped write request was accepted")
	}
	runBridgeAdmin(t, dataDir, "trusted-root", "revoke", rootBID)
	if revoked := callTool(t, ctx, session, "codex_resume_thread", map[string]any{"workspaceId": workspaceB, "threadId": threadB, "prompt": "Revoked root must deny resume."}); !revoked.IsError {
		t.Fatal("revoked root B remained resumable")
	}
	if rootAID == rootBID {
		t.Fatal("disposable root approvals were not independent")
	}
	if !provider.sawExecCommand() || provider.error() != "" {
		t.Fatal("deterministic provider did not submit the expected App Server command invocation")
	}
	t.Log("BRIDGE_ROOT_SCOPED_SESSION_E2E_OK: disposable Codex CLI sessions and real App Server used a loopback Responses stub; root A read two repositories; root B and symlink reads plus workspace write denied by App Server sandbox; context switching preserved; root write remained unapproved")
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}
