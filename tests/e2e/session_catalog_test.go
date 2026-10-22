package e2e

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/codex"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/domain"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/security"
)

func TestGlobalCatalogSwitchesDisposableCLISessionsAcrossWorkspaces(t *testing.T) {
	if os.Getenv("BRIDGE_SESSION_CATALOG_E2E") != "1" {
		t.Skip("set BRIDGE_SESSION_CATALOG_E2E=1 to run disposable Codex CLI session switching E2E")
	}
	base := t.TempDir()
	codexPath, cliEnv := setupDisposableCodexCLI(t, base)
	var err error

	workspaceRoot := filepath.Join(base, "workspaces")
	workspaceA := filepath.Join(workspaceRoot, "project-a")
	workspaceB := filepath.Join(workspaceRoot, "project-b")
	for _, workspace := range []string{workspaceA, workspaceB} {
		if err := os.MkdirAll(workspace, 0o700); err != nil {
			t.Fatal("create disposable workspace")
		}
		gitInit := exec.Command("git", "-C", workspace, "init", "-q")
		if output, err := gitInit.CombinedOutput(); err != nil {
			t.Fatalf("initialize disposable fixture repository (output_bytes=%d)", len(output))
		}
	}
	markerA := fixtureMarker(t, "SESSION_CATALOG_CONTEXT_A_")
	markerB := fixtureMarker(t, "SESSION_CATALOG_CONTEXT_B_")
	threadA := createDisposableCLISession(t, codexPath, cliEnv, workspaceA, markerA)
	time.Sleep(1100 * time.Millisecond)
	threadB := createDisposableCLISession(t, codexPath, cliEnv, workspaceB, markerB)
	if threadA == threadB {
		t.Fatal("Codex CLI reused one thread ID for separate workspace fixtures")
	}

	dataDir := filepath.Join(base, "bridge-data")
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
	metadataClient := codex.NewClient(codexPath, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer metadataClient.Close()
	metadata, err := metadataClient.ListCLISessionsPage(ctx, "", "", "", 50)
	if err != nil {
		t.Fatal("read disposable session titles through App Server thread/list")
	}
	metadataByID := make(map[string]domain.CodexSession, len(metadata.Sessions))
	for _, item := range metadata.Sessions {
		metadataByID[item.ID] = item
	}
	session = harness.connect(t, ctx)
	registerApprovedWorkspace(t, ctx, session, harness, "catalog-project-a", workspaceA)
	registerApprovedWorkspace(t, ctx, session, harness, "catalog-project-b", workspaceB)
	if denied := callTool(t, ctx, session, "codex_list_local_sessions", map[string]any{}); !denied.IsError {
		t.Fatal("global session catalog was available before local owner approval")
	}
	runBridgeAdmin(t, dataDir, "codex-session", "catalog", "enable")
	runBridgeAdmin(t, dataDir, "codex-session", "trust", "catalog-project-a")
	runBridgeAdmin(t, dataDir, "codex-session", "trust", "catalog-project-b")

	first := callTool(t, ctx, session, "codex_list_local_sessions", map[string]any{"limit": 1})
	if first.IsError {
		t.Fatal("owner-approved global session catalog request failed")
	}
	firstOutput := structured(t, first)
	firstRows, ok := firstOutput["sessions"].([]any)
	firstCursor, _ := firstOutput["nextCursor"].(string)
	if !ok || len(firstRows) != 1 || firstCursor == "" {
		t.Fatal("global catalog did not return a bounded first page")
	}
	firstJSON, err := json.Marshal(firstOutput)
	if err != nil || strings.Contains(string(firstJSON), `"turns"`) {
		t.Fatal("global catalog exposed conversation turns")
	}
	firstRow := catalogRow(t, firstRows[0])
	displayName, _ := firstRow["displayName"].(string)
	if strings.TrimSpace(displayName) == "" {
		t.Fatal("session catalog did not provide a display name")
	}
	second := callTool(t, ctx, session, "codex_list_local_sessions", map[string]any{"limit": 1, "cursor": firstCursor})
	if second.IsError {
		t.Fatal("global session catalog second page request failed")
	}
	secondRows, _ := structured(t, second)["sessions"].([]any)
	if len(secondRows) != 1 {
		t.Fatal("global catalog did not return the second workspace session")
	}
	if got := catalogRow(t, firstRows[0]); got["threadId"] != threadB || got["workspaceId"] != "catalog-project-b" || got["connectionStatus"] != "trusted" || got["canResume"] != true {
		t.Fatal("most recently active trusted session was not first in the global catalog")
	}
	assertCatalogNameMatchesMetadata(t, catalogRow(t, firstRows[0]), metadataByID[threadB])
	if got := catalogRow(t, secondRows[0]); got["threadId"] != threadA || got["workspaceId"] != "catalog-project-a" || got["connectionStatus"] != "trusted" || got["canResume"] != true {
		t.Fatal("older trusted session was not returned on the next catalog page")
	}
	assertCatalogNameMatchesMetadata(t, catalogRow(t, secondRows[0]), metadataByID[threadA])

	filtered := callTool(t, ctx, session, "codex_list_local_sessions", map[string]any{"workspaceId": "catalog-project-a", "limit": 10})
	filteredRows, _ := structured(t, filtered)["sessions"].([]any)
	if filtered.IsError || len(filteredRows) != 1 || catalogRow(t, filteredRows[0])["threadId"] != threadA {
		t.Fatal("workspace filter returned a session from another workspace")
	}
	if displayName != "Untitled session" {
		searched := callTool(t, ctx, session, "codex_list_local_sessions", map[string]any{"search": displayName, "limit": 10})
		searchedRows, _ := structured(t, searched)["sessions"].([]any)
		found := false
		for _, row := range searchedRows {
			if catalogRow(t, row)["threadId"] == threadB {
				found = true
			}
		}
		if searched.IsError || !found {
			t.Fatal("Codex session display name could not be searched")
		}
	}
	if crossWorkspace := callTool(t, ctx, session, "codex_resume_thread", map[string]any{"workspaceId": "catalog-project-a", "threadId": threadB, "prompt": "This cross-workspace request must be denied."}); !crossWorkspace.IsError {
		t.Fatal("trusted session was resumable through a different workspace")
	}

	for _, test := range []struct {
		workspaceID string
		threadID    string
		marker      string
		otherMarker string
	}{{"catalog-project-a", threadA, markerA, markerB}, {"catalog-project-b", threadB, markerB, markerA}} {
		evidence := runTask(t, ctx, session, "codex_resume_thread", map[string]any{
			"workspaceId": test.workspaceID,
			"threadId":    test.threadID,
			"prompt":      "Recall the exact session-only marker from the earlier terminal Codex conversation. Do not read files or use tools. Return only that marker.",
		})
		if evidence.TaskStatus != string(domain.TaskSucceeded) || evidence.ThreadID != test.threadID || !strings.Contains(evidence.Result, test.marker) || strings.Contains(evidence.Result, test.otherMarker) {
			t.Fatalf("explicit thread switching lost or crossed conversation context (status=%s thread_match=%t expected_context=%t wrong_context=%t)", evidence.TaskStatus, evidence.ThreadID == test.threadID, strings.Contains(evidence.Result, test.marker), strings.Contains(evidence.Result, test.otherMarker))
		}
		thread, err := harness.store.GetThread(ctx, test.threadID)
		if err != nil || thread.Source != domain.ThreadSourceCodexCLITrusted || thread.WorkspaceID != test.workspaceID {
			t.Fatal("trusted workspace session was not bound without per-session attach approval")
		}
	}
	runBridgeAdmin(t, dataDir, "codex-session", "untrust", "catalog-project-a")
	if revoked := callTool(t, ctx, session, "codex_resume_thread", map[string]any{"workspaceId": "catalog-project-a", "threadId": threadA, "prompt": "This revoked workspace trust must deny resume."}); !revoked.IsError {
		t.Fatal("workspace trust revocation did not block future resumes")
	}
	runBridgeAdmin(t, dataDir, "codex-session", "catalog", "disable")
	if revoked := callTool(t, ctx, session, "codex_list_local_sessions", map[string]any{}); !revoked.IsError {
		t.Fatal("global catalog revocation did not block future discovery")
	}
	t.Log("BRIDGE_GLOBAL_SESSION_SWITCH_E2E_OK: two disposable codex exec sessions, two trusted workspaces, metadata-only paginated catalog, explicit A-to-B resume, context markers remained isolated")
}

func TestTrustedRootsResumeDisposableCLISessionsAcrossRoots(t *testing.T) {
	if os.Getenv("BRIDGE_TRUSTED_ROOT_E2E") != "1" {
		t.Skip("set BRIDGE_TRUSTED_ROOT_E2E=1 to run disposable Codex sessions through trusted roots")
	}
	base := t.TempDir()
	codexPath, cliEnv := setupDisposableCodexCLI(t, base)
	workspaceRoot := filepath.Join(base, "workspaces")
	rootA := filepath.Join(workspaceRoot, "root-a")
	rootB := filepath.Join(workspaceRoot, "root-b")
	repoA := filepath.Join(rootA, "project-a")
	repoA2 := filepath.Join(rootA, "project-a2")
	repoB := filepath.Join(rootB, "project-b")
	outsideRepo := filepath.Join(base, "outside", "project-outside")
	for _, workspace := range []string{repoA, repoA2, repoB, outsideRepo} {
		if err := os.MkdirAll(workspace, 0o700); err != nil {
			t.Fatal("create disposable project")
		}
		gitInit := exec.Command("git", "-C", workspace, "init", "-q")
		if output, err := gitInit.CombinedOutput(); err != nil {
			t.Fatalf("initialize disposable fixture repository (output_bytes=%d)", len(output))
		}
	}
	markers := []string{
		fixtureMarker(t, "TRUSTED_ROOT_CONTEXT_A1_"),
		fixtureMarker(t, "TRUSTED_ROOT_CONTEXT_A2_"),
		fixtureMarker(t, "TRUSTED_ROOT_CONTEXT_A3_"),
		fixtureMarker(t, "TRUSTED_ROOT_CONTEXT_B1_"),
		fixtureMarker(t, "TRUSTED_ROOT_CONTEXT_OUTSIDE_"),
	}
	threadIDs := make([]string, len(markers))
	for index, workspace := range []string{repoA, repoA, repoA2, repoB, outsideRepo} {
		threadIDs[index] = createDisposableCLISession(t, codexPath, cliEnv, workspace, markers[index])
		if index+1 < len(markers) {
			time.Sleep(1100 * time.Millisecond)
		}
	}
	for i := range threadIDs {
		for j := i + 1; j < len(threadIDs); j++ {
			if threadIDs[i] == threadIDs[j] {
				t.Fatal("Codex CLI reused a thread ID for separate disposable conversations")
			}
		}
	}

	dataDir := filepath.Join(base, "bridge-data")
	approveRoot := func(path string) string {
		t.Helper()
		output := runBridgeAdmin(t, dataDir, "trusted-root", "add", path)
		var request struct {
			RequestID string `json:"requestId"`
		}
		if err := json.Unmarshal(output, &request); err != nil || request.RequestID == "" {
			t.Fatal("local root request returned no request ID")
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
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()
	session = harness.connect(t, ctx)
	catalog := callTool(t, ctx, session, "codex_list_local_sessions", map[string]any{"limit": 50})
	if catalog.IsError {
		t.Fatal("owner-approved root catalog request failed")
	}
	rows, _ := structured(t, catalog)["sessions"].([]any)
	byThread := make(map[string]map[string]any, len(rows))
	for _, row := range rows {
		entry := catalogRow(t, row)
		threadID, _ := entry["threadId"].(string)
		byThread[threadID] = entry
	}
	if len(byThread) != 4 {
		t.Fatalf("catalog returned %d in-root sessions; wanted four and no outside session", len(byThread))
	}
	for _, index := range []int{0, 1, 2, 3} {
		entry, ok := byThread[threadIDs[index]]
		if !ok || entry["canResume"] != true {
			t.Fatalf("trusted CLI session %d was not resumable", index+1)
		}
	}
	if _, found := byThread[threadIDs[4]]; found {
		t.Fatal("session outside approved roots leaked into the catalog")
	}
	if byThread[threadIDs[0]]["workspaceId"] != byThread[threadIDs[1]]["workspaceId"] ||
		byThread[threadIDs[0]]["workspaceId"] == byThread[threadIDs[2]]["workspaceId"] ||
		byThread[threadIDs[3]]["workspaceId"] == byThread[threadIDs[2]]["workspaceId"] {
		t.Fatal("session-to-project mapping did not preserve repository boundaries")
	}
	if result := callTool(t, ctx, session, "codex_resume_thread", map[string]any{
		"workspaceId": byThread[threadIDs[0]]["workspaceId"], "threadId": threadIDs[3], "prompt": "This cross-root session must be denied.",
	}); !result.IsError {
		t.Fatal("thread was resumable through another project workspace")
	}

	for _, index := range []int{0, 1, 3, 0} {
		entry := byThread[threadIDs[index]]
		resumed := runTask(t, ctx, session, "codex_resume_thread", map[string]any{
			"workspaceId": entry["workspaceId"],
			"threadId":    threadIDs[index],
			"prompt":      "Return only the exact harmless marker from this same Terminal session. Do not read files or invoke tools.",
		})
		if resumed.TaskStatus != string(domain.TaskSucceeded) || resumed.ThreadID != threadIDs[index] || !strings.Contains(resumed.Result, markers[index]) {
			t.Fatalf("resume switch %d lost its session context (status=%s thread_match=%t marker_match=%t)", index+1, resumed.TaskStatus, resumed.ThreadID == threadIDs[index], strings.Contains(resumed.Result, markers[index]))
		}
		for otherIndex, marker := range markers {
			if otherIndex != index && strings.Contains(resumed.Result, marker) {
				t.Fatalf("resume switch %d returned another session's context", index+1)
			}
		}
		thread, err := harness.store.GetThread(ctx, threadIDs[index])
		wantRoot := rootAID
		if index == 3 {
			wantRoot = rootBID
		}
		if err != nil || thread.Source != domain.ThreadSourceCodexCLIRoot || thread.TrustedRootID != wantRoot || thread.WorkspaceID != entry["workspaceId"] {
			t.Fatalf("session %d was not bound to its locally approved root", index+1)
		}
	}
	if result := callTool(t, ctx, session, "codex_start_write_task", map[string]any{
		"workspaceId": byThread[threadIDs[0]]["workspaceId"], "threadId": threadIDs[0], "prompt": "This root-trust write must be denied by default.",
	}); !result.IsError {
		t.Fatal("trusted-root resume implicitly enabled workspace writes")
	}
	runBridgeAdmin(t, dataDir, "trusted-root", "revoke", rootBID)
	if revoked := callTool(t, ctx, session, "codex_resume_thread", map[string]any{
		"workspaceId": byThread[threadIDs[3]]["workspaceId"], "threadId": threadIDs[3], "prompt": "This revoked-root session must be denied.",
	}); !revoked.IsError {
		t.Fatal("revoked root session remained resumable")
	}
	t.Log("BRIDGE_TRUSTED_ROOT_SESSION_E2E_OK: four disposable CLI sessions under two locally approved roots, repository boundaries isolated, outside session hidden, explicit A-to-B switching retained per-thread context")
}

func setupDisposableCodexCLI(t *testing.T, base string) (string, []string) {
	t.Helper()
	codexPath := os.Getenv("CODEX_EXECUTABLE")
	if codexPath == "" {
		codexPath = "codex"
	}
	var err error
	codexPath, err = exec.LookPath(codexPath)
	if err != nil {
		t.Fatal("Codex CLI is unavailable")
	}
	if resolved, resolveErr := filepath.EvalSymlinks(codexPath); resolveErr == nil {
		codexPath = resolved
	}
	cliHome := filepath.Join(base, "codex-home")
	if err := os.Mkdir(cliHome, 0o700); err != nil {
		t.Fatal("create isolated Codex CLI home")
	}
	sourceHome := os.Getenv("CODEX_HOME")
	if sourceHome == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			t.Fatal("resolve Codex home")
		}
		sourceHome = filepath.Join(userHome, ".codex")
	}
	if err := os.Symlink(filepath.Join(sourceHome, "auth.json"), filepath.Join(cliHome, "auth.json")); err != nil {
		t.Fatal("link existing Codex authentication into isolated fixture home")
	}
	t.Setenv("CODEX_HOME", cliHome)
	cliEnv := codexE2EEnvironment(os.Environ(), cliHome)
	status := exec.Command(codexPath, "login", "status")
	status.Env = cliEnv
	statusOutput, err := status.CombinedOutput()
	if err != nil || !strings.Contains(string(statusOutput), "Logged in using ChatGPT") {
		t.Skip("disposable E2E requires existing ChatGPT Codex authentication; API-key providers are not used")
	}
	return codexPath, cliEnv
}

func createDisposableCLISession(t *testing.T, codexPath string, env []string, workspace, marker string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, codexPath, "exec", "--json", "--sandbox", "read-only", "--cd", workspace, "--skip-git-repo-check",
		"Remember this session-only test marker for a later turn: "+marker+". Do not invoke tools or access files. Reply exactly SESSION_READY.")
	command.Env = env
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("create disposable Codex CLI session failed (output_bytes=%d)", len(output))
	}
	for _, line := range strings.Split(string(output), "\n") {
		var event struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
		}
		if json.Unmarshal([]byte(line), &event) == nil && event.Type == "thread.started" && event.ThreadID != "" {
			return event.ThreadID
		}
	}
	t.Fatal("Codex CLI completed without a thread.started event")
	return ""
}

func codexE2EEnvironment(env []string, codexHome string) []string {
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if key != "CODEX_HOME" && key != "OPENAI_API_KEY" && key != "CODEX_API_KEY" {
			result = append(result, entry)
		}
	}
	return append(result, "CODEX_HOME="+codexHome)
}

func catalogRow(t *testing.T, value any) map[string]any {
	t.Helper()
	row, ok := value.(map[string]any)
	if !ok {
		t.Fatal("session catalog returned an invalid row")
	}
	return row
}

func assertCatalogNameMatchesMetadata(t *testing.T, row map[string]any, session domain.CodexSession) {
	t.Helper()
	if row["displayName"] != expectedCatalogName(session) {
		t.Fatal("Bridge displayName does not match Codex App Server thread title metadata")
	}
	t.Logf("disposable session title metadata matched: thread_name_present=%t preview_fallback=%t", strings.TrimSpace(session.Title) != "", strings.TrimSpace(session.Title) == "" && strings.TrimSpace(session.Preview) != "")
}

func expectedCatalogName(session domain.CodexSession) string {
	value := session.Title
	limit := 0
	if strings.TrimSpace(value) == "" {
		value = session.Preview
		limit = 160
	}
	value = security.RedactSecrets(value)
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return "Untitled session"
	}
	if limit > 0 {
		runes := []rune(value)
		if len(runes) > limit {
			value = string(runes[:limit-1]) + "…"
		}
	}
	return value
}
