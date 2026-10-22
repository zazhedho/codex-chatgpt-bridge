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

	"github.com/zazhedho/codex-chatgpt-bridge/internal/domain"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAttachAndResumeTerminalCodexSession(t *testing.T) {
	if os.Getenv("BRIDGE_ATTACH_E2E") != "1" {
		t.Skip("set BRIDGE_ATTACH_E2E=1 with a disposable Terminal-created Codex CLI session")
	}
	workspacePath := os.Getenv("BRIDGE_ATTACH_E2E_WORKSPACE")
	workspaceID := os.Getenv("BRIDGE_ATTACH_E2E_WORKSPACE_ID")
	threadID := os.Getenv("BRIDGE_ATTACH_E2E_THREAD_ID")
	marker := os.Getenv("BRIDGE_ATTACH_E2E_MARKER")
	dataDir := os.Getenv("BRIDGE_ATTACH_E2E_DATA_DIR")
	if workspacePath == "" || workspaceID == "" || threadID == "" || marker == "" || dataDir == "" {
		t.Fatal("attach E2E requires workspace, workspace ID, thread ID, marker, and isolated data directory")
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		t.Fatal("find local cache directory")
	}
	canonicalWorkspace, err := filepath.EvalSymlinks(workspacePath)
	if err != nil {
		t.Fatal("resolve disposable session workspace")
	}
	relative, err := filepath.Rel(cacheDir, canonicalWorkspace)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) || !strings.HasPrefix(filepath.Base(filepath.Dir(canonicalWorkspace)), "codex-bridge-session-e2e.") {
		t.Fatal("attach E2E workspace must be inside its disposable cache fixture")
	}
	fixture, err := os.ReadFile(filepath.Join(canonicalWorkspace, "fixture.txt"))
	if err != nil || strings.TrimSpace(string(fixture)) != marker {
		t.Fatal("attach E2E fixture marker does not match")
	}
	if info, err := os.Stat(dataDir); err != nil || !info.IsDir() {
		t.Fatal("attach E2E requires an existing isolated bridge data directory")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal("resolve local user home")
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	session = harness.connect(t, ctx)
	workspace, err := harness.workspaces.GetLocal(ctx, workspaceID)
	if err != nil || workspace.RootPath != canonicalWorkspace {
		t.Fatal("disposable workspace is not locally approved in the isolated bridge database")
	}

	localList := runBridgeAdmin(t, dataDir, "codex-session", "list", workspaceID)
	var discovered []domain.CodexSession
	if err := json.Unmarshal(localList, &discovered); err != nil {
		t.Fatal("bridge-admin session discovery did not return metadata")
	}
	found := false
	for _, item := range discovered {
		if item.ID == threadID && item.IsCodexCLISession() && item.WorkspacePath == canonicalWorkspace {
			found = true
		}
	}
	if !found {
		t.Fatal("bridge-admin did not discover the Terminal-created CLI session in its approved workspace")
	}

	before := callTool(t, ctx, session, "codex_list_threads", map[string]any{"workspaceId": workspaceID})
	if before.IsError || mcpThreadListContains(t, before, threadID) {
		t.Fatal("unattached CLI session was visible through MCP")
	}
	unattachedResume := callTool(t, ctx, session, "codex_resume_thread", map[string]any{"workspaceId": workspaceID, "threadId": threadID, "prompt": "This unapproved session must not be resumed."})
	if !unattachedResume.IsError {
		t.Fatal("unattached CLI session was resumable through MCP")
	}

	requested := callTool(t, ctx, session, "codex_request_session_attach", map[string]any{"workspaceId": workspaceID, "threadId": threadID})
	if requested.IsError {
		t.Fatal("MCP session attach request failed")
	}
	requestID, _ := structured(t, requested)["requestId"].(string)
	if requestID == "" || structured(t, requested)["status"] != domain.SessionAttachPending {
		t.Fatal("attach request was not held for local approval")
	}
	requestMetadata := runBridgeAdmin(t, dataDir, "codex-session", "show", requestID)
	if strings.Contains(string(requestMetadata), marker) {
		t.Fatal("bridge-admin attach request exposed conversation/fixture contents")
	}
	runBridgeAdmin(t, dataDir, "codex-session", "approve", requestID)

	listed := callTool(t, ctx, session, "codex_list_threads", map[string]any{"workspaceId": workspaceID})
	if listed.IsError || !mcpThreadListContains(t, listed, threadID) {
		t.Fatal("locally approved CLI session did not become visible through MCP")
	}
	resumed := runTask(t, ctx, session, "codex_resume_thread", map[string]any{
		"workspaceId": workspaceID,
		"threadId":    threadID,
		"prompt":      "From the earlier Terminal conversation, return the exact harmless marker and the planned next code change. Do not read or change files.",
	})
	if resumed.TaskStatus != string(domain.TaskSucceeded) || !strings.Contains(resumed.Result, marker) || !strings.Contains(resumed.Result, "GreetingFor") {
		t.Fatalf("resumed Terminal session did not prove retained context (status=%s marker_seen=%t planned_change_seen=%t)", resumed.TaskStatus, strings.Contains(resumed.Result, marker), strings.Contains(resumed.Result, "GreetingFor"))
	}

	if err := harness.workspaces.SetWriteEnabledLocal(ctx, workspaceID, true, "attach-e2e-operator"); err != nil {
		t.Fatal("local fixture write opt-in failed")
	}
	unchanged, err := os.ReadFile(filepath.Join(canonicalWorkspace, "fixture.txt"))
	if err != nil {
		t.Fatal("read fixture before write-approval check")
	}
	write := callTool(t, ctx, session, "codex_start_write_task", map[string]any{
		"workspaceId": workspaceID,
		"threadId":    threadID,
		"prompt":      "Change fixture.txt. This request must wait for its own local write approval.",
	})
	if write.IsError || structured(t, write)["status"] != string(domain.TaskWaitingApproval) {
		t.Fatal("attached session bypassed per-task write approval")
	}
	after, err := os.ReadFile(filepath.Join(canonicalWorkspace, "fixture.txt"))
	if err != nil || string(after) != string(unchanged) {
		t.Fatal("write task changed the fixture before its independent approval")
	}
	approvalID, _ := structured(t, write)["approvalId"].(string)
	if approvalID != "" {
		runBridgeAdmin(t, dataDir, "approval", "reject", approvalID)
	}
	t.Log("BRIDGE_TERMINAL_SESSION_ATTACH_OK")
}

func runBridgeAdmin(t *testing.T, dataDir string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	commandArgs := append([]string{"run", "../../cmd/bridge-admin"}, args...)
	command := exec.CommandContext(ctx, "go", commandArgs...)
	command.Env = replaceEnvironment(os.Environ(), "BRIDGE_DATA_DIR", dataDir)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("bridge-admin %s failed (output_bytes=%d)", strings.Join(args, " "), len(output))
	}
	return output
}

func mcpThreadListContains(t *testing.T, result *sdk.CallToolResult, threadID string) bool {
	t.Helper()
	if result.StructuredContent == nil {
		return false
	}
	encoded, err := json.Marshal(result.StructuredContent)
	return err == nil && strings.Contains(string(encoded), threadID)
}
