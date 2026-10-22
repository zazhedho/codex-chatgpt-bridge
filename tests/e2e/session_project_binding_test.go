package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/domain"
)

func TestParentDirectorySessionsRequireProjectBindingAndResumeAcrossRestart(t *testing.T) {
	if os.Getenv("BRIDGE_SESSION_PROJECT_BINDING_E2E") != "1" {
		t.Skip("set BRIDGE_SESSION_PROJECT_BINDING_E2E=1 to run disposable Codex CLI project-binding E2E")
	}
	base := t.TempDir()
	codexPath, cliEnv := setupDisposableCodexCLI(t, base)
	workspaceRoot := filepath.Join(base, "workspaces")
	rootA, rootB := filepath.Join(workspaceRoot, "Project"), filepath.Join(workspaceRoot, "BRI")
	repoA, repoB := filepath.Join(rootA, "app-a"), filepath.Join(rootB, "app-b")
	for _, path := range []string{rootA, rootB, repoA, repoB} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal("create disposable parent fixture")
		}
	}
	for _, repo := range []string{repoA, repoB} {
		command := exec.Command("git", "-C", repo, "init", "-q")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("initialize disposable repository (output_bytes=%d)", len(output))
		}
	}
	markerA := fixtureMarker(t, "PARENT_SESSION_CONTEXT_A_")
	markerB := fixtureMarker(t, "PARENT_SESSION_CONTEXT_B_")
	threadA := createDisposableCLISession(t, codexPath, cliEnv, rootA, markerA)
	time.Sleep(1100 * time.Millisecond)
	threadB := createDisposableCLISession(t, codexPath, cliEnv, rootB, markerB)
	if threadA == threadB {
		t.Fatal("Codex CLI reused a thread ID for separate parent-directory fixtures")
	}

	dataDir := filepath.Join(base, "bridge-data")
	approveRoot := func(path string) {
		t.Helper()
		output := runBridgeAdmin(t, dataDir, "trusted-root", "add", path)
		var request struct {
			RequestID string `json:"requestId"`
		}
		if err := json.Unmarshal(output, &request); err != nil || request.RequestID == "" {
			t.Fatal("local root registration returned no request ID")
		}
		runBridgeAdmin(t, dataDir, "trusted-root", "approve", request.RequestID)
	}
	approveRoot(rootA)
	approveRoot(rootB)
	runBridgeAdmin(t, dataDir, "codex-session", "catalog", "enable")

	harness := newBridgeHarness(t, workspaceRoot, dataDir)
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

	catalog := callTool(t, ctx, session, "codex_list_local_sessions", map[string]any{"limit": 50})
	if catalog.IsError {
		t.Fatal("parent-directory sessions were not listed")
	}
	rows, _ := structured(t, catalog)["sessions"].([]any)
	byThread := make(map[string]map[string]any, len(rows))
	for _, row := range rows {
		entry := catalogRow(t, row)
		id, _ := entry["threadId"].(string)
		byThread[id] = entry
	}
	for _, threadID := range []string{threadA, threadB} {
		entry := byThread[threadID]
		if entry == nil || entry["connectionStatus"] != "project_selection_required" || entry["canResume"] != false {
			t.Fatalf("parent-directory session was not held for explicit project selection: found=%t", entry != nil)
		}
	}

	choiceFor := func(threadID, wantRelativePath string) string {
		t.Helper()
		result := callTool(t, ctx, session, "codex_list_session_projects", map[string]any{"threadId": threadID, "limit": 10})
		if result.IsError {
			t.Fatal("approved-root repository choices were not available")
		}
		projects, _ := structured(t, result)["projects"].([]any)
		if len(projects) != 1 {
			t.Fatalf("fixture returned %d repository choices, want one", len(projects))
		}
		project := catalogRow(t, projects[0])
		if project["relativePath"] != wantRelativePath || project["name"] != filepath.Base(wantRelativePath) {
			t.Fatalf("repository choice was not root-relative and deterministic: %v", project)
		}
		id, _ := project["projectId"].(string)
		if len(id) != 32 {
			t.Fatal("repository choice did not return an opaque project ID")
		}
		return id
	}
	projectAID := choiceFor(threadA, "app-a")
	projectBID := choiceFor(threadB, "app-b")
	if cross := callTool(t, ctx, session, "codex_request_session_project_binding", map[string]any{"threadId": threadB, "projectId": projectAID}); !cross.IsError {
		t.Fatal("project choice from another trusted root was accepted")
	}
	requestFor := func(threadID, projectID string) string {
		t.Helper()
		result := callTool(t, ctx, session, "codex_request_session_project_binding", map[string]any{"threadId": threadID, "projectId": projectID})
		if result.IsError {
			t.Fatal("project binding request failed")
		}
		requestID, _ := structured(t, result)["requestId"].(string)
		if requestID == "" {
			t.Fatal("project binding request returned no request ID")
		}
		return requestID
	}
	requestA := requestFor(threadA, projectAID)
	requestB := requestFor(threadB, projectBID)
	var pending []domain.SessionProjectBinding
	if err := json.Unmarshal(runBridgeAdmin(t, dataDir, "codex-session", "project-binding", "pending"), &pending); err != nil || len(pending) != 2 {
		t.Fatal("bridge-admin did not list both pending project binding requests")
	}
	var shown domain.SessionProjectBinding
	if err := json.Unmarshal(runBridgeAdmin(t, dataDir, "codex-session", "project-binding", "show", requestA), &shown); err != nil || shown.ID != requestA {
		t.Fatal("bridge-admin could not show pending project binding details")
	}
	runBridgeAdmin(t, dataDir, "codex-session", "project-binding", "approve", requestA)
	runBridgeAdmin(t, dataDir, "codex-session", "project-binding", "approve", requestB)
	var approved []domain.SessionProjectBinding
	if err := json.Unmarshal(runBridgeAdmin(t, dataDir, "codex-session", "project-binding", "list"), &approved); err != nil || len(approved) != 2 {
		t.Fatal("bridge-admin did not list both approved project bindings")
	}

	verifyBoundCatalog := func() map[string]map[string]any {
		t.Helper()
		result := callTool(t, ctx, session, "codex_list_local_sessions", map[string]any{"limit": 50})
		if result.IsError {
			t.Fatal("catalog failed after local project approvals")
		}
		values, _ := structured(t, result)["sessions"].([]any)
		entries := make(map[string]map[string]any, len(values))
		for _, value := range values {
			entry := catalogRow(t, value)
			id, _ := entry["threadId"].(string)
			entries[id] = entry
		}
		for _, id := range []string{threadA, threadB} {
			if entries[id]["connectionStatus"] != "project_bound" || entries[id]["canResume"] != true {
				t.Fatalf("locally approved session not resumable after binding: found=%t", entries[id] != nil)
			}
		}
		return entries
	}
	entries := verifyBoundCatalog()
	for _, test := range []struct {
		threadID, marker, otherMarker string
	}{{threadA, markerA, markerB}, {threadB, markerB, markerA}, {threadA, markerA, markerB}} {
		entry := entries[test.threadID]
		workspaceID, _ := entry["workspaceId"].(string)
		workspace := callTool(t, ctx, session, "workspace_get", map[string]any{"workspaceId": workspaceID})
		if workspace.IsError {
			t.Fatal("bound logical project workspace was unavailable")
		}
		workspaceInfo, _ := structured(t, workspace)["workspace"].(map[string]any)
		permissions, _ := workspaceInfo["sandboxPermissions"].(map[string]any)
		if workspaceInfo["writeEnabled"] != false || permissions["filesystem"] != "read-only" || permissions["network"] != false {
			t.Fatal("project binding changed read-only or network sandbox defaults")
		}
		if write := callTool(t, ctx, session, "codex_start_write_task", map[string]any{"workspaceId": workspaceID, "threadId": test.threadID, "prompt": "This fixture write must be denied."}); !write.IsError {
			t.Fatal("project binding enabled a write task without separate local workspace permission")
		}
		listed := callTool(t, ctx, session, "codex_list_threads", map[string]any{"workspaceId": workspaceID})
		if listed.IsError {
			t.Fatalf("project-bound thread could not pass resume authorization: %s", safeToolError(listed))
		}
		listedThread := false
		threads, _ := structured(t, listed)["threads"].([]any)
		for _, value := range threads {
			if catalogRow(t, value)["threadId"] == test.threadID {
				listedThread = true
			}
		}
		if !listedThread {
			t.Fatal("project-bound thread did not appear in authorized workspace thread listing")
		}
		thread, err := harness.store.GetThread(ctx, test.threadID)
		if err != nil || thread.Source != domain.ThreadSourceCodexCLIProjectBound || thread.TrustedRootID == "" || thread.WorkspaceID != workspaceID {
			t.Fatalf("persisted project binding did not satisfy task-thread authorization: thread_loaded=%t source_ok=%t root_bound=%t workspace_matches=%t", err == nil, thread.Source == domain.ThreadSourceCodexCLIProjectBound, thread.TrustedRootID != "", thread.WorkspaceID == workspaceID)
		}
		started := callTool(t, ctx, session, "codex_resume_thread", map[string]any{
			"workspaceId": workspaceID,
			"threadId":    test.threadID,
			"prompt":      "Return only the exact harmless marker remembered from this same Terminal Codex session. Do not read or change files and do not invoke tools.",
		})
		if started.IsError {
			active, queued, _ := harness.store.TaskStats(ctx)
			t.Fatalf("MCP resume was rejected: %s (persisted_active=%d persisted_queued=%d)", safeToolError(started), active, queued)
		}
		taskID, _ := structured(t, started)["taskId"].(string)
		if taskID == "" {
			t.Fatal("MCP resume returned no task identifier")
		}
		evidence := waitTaskEvidence(t, ctx, session, taskID, 180*time.Second)
		if evidence.TaskStatus != string(domain.TaskSucceeded) || evidence.ThreadID != test.threadID || !strings.Contains(evidence.Result, test.marker) || strings.Contains(evidence.Result, test.otherMarker) {
			t.Fatalf("parent-directory resume lost or crossed context (status=%s same_thread=%t expected_marker=%t wrong_marker=%t)", evidence.TaskStatus, evidence.ThreadID == test.threadID, strings.Contains(evidence.Result, test.marker), strings.Contains(evidence.Result, test.otherMarker))
		}
	}
	for _, repo := range []string{repoA, repoB} {
		status := exec.Command("git", "-C", repo, "status", "--porcelain", "--untracked-files=all")
		if output, err := status.CombinedOutput(); err != nil || len(strings.TrimSpace(string(output))) != 0 {
			t.Fatal("read-only context resume changed disposable repository files")
		}
	}

	if err := session.Close(); err != nil {
		t.Fatal("close first MCP client session")
	}
	session = nil
	if err := harness.close(); err != nil {
		t.Fatal("gracefully stop isolated bridge before restart")
	}
	harness = nil
	harness = newBridgeHarness(t, workspaceRoot, dataDir)
	session = harness.connect(t, ctx)
	entries = verifyBoundCatalog()
	resume := runTask(t, ctx, session, "codex_resume_thread", map[string]any{
		"workspaceId": entries[threadB]["workspaceId"], "threadId": threadB,
		"prompt": "Return only the exact harmless marker remembered from this same Terminal Codex session. Do not read or change files and do not invoke tools.",
	})
	if resume.TaskStatus != string(domain.TaskSucceeded) || resume.ThreadID != threadB || !strings.Contains(resume.Result, markerB) || strings.Contains(resume.Result, markerA) {
		t.Fatal("persistent project binding did not preserve session context after Bridge/App Server restart")
	}
	runBridgeAdmin(t, dataDir, "codex-session", "project-binding", "revoke", threadA)
	if denied := callTool(t, ctx, session, "codex_resume_thread", map[string]any{"workspaceId": entries[threadA]["workspaceId"], "threadId": threadA, "prompt": "This revoked binding must be denied."}); !denied.IsError {
		t.Fatal("revoked project binding remained resumable")
	}
	t.Log("BRIDGE_PARENT_SESSION_PROJECT_BINDING_E2E_OK: disposable CLI sessions from two parent roots, local one-time bindings, explicit thread switching, per-thread context retained across Bridge/App Server restart, read-only workspaces unchanged")
}
