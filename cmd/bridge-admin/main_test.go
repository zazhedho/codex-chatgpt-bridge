package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zazhedho/codex-chatgpt-bridge/internal/repository/sqlite"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/service"
)

func TestWorkspaceWritePermissionChangesOnlyThroughLocalAdmin(t *testing.T) {
	dataDir := t.TempDir()
	home := t.TempDir()
	store, err := sqlite.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	workspaces := service.NewWorkspaceService(store, nil, home, "")
	root := filepath.Join(home, "fixture")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	request, err := workspaces.RequestRegistrationLocal(context.Background(), "fixture-project", root, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspaces.ApproveRegistration(context.Background(), request.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIDGE_DATA_DIR", dataDir)
	if err := run([]string{"workspace", "enable-write", "fixture-project"}); err != nil {
		t.Fatalf("local workspace enable-write failed: %v", err)
	}
	store, err = sqlite.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	workspace, err := store.GetWorkspace(context.Background(), "fixture-project")
	if err != nil {
		t.Fatal(err)
	}
	if !workspace.WriteEnabled {
		t.Fatal("bridge-admin did not enable workspace writes")
	}
}

func TestTrustedRootCLIRoundTripAndRevocation(t *testing.T) {
	dataDir := t.TempDir()
	rootPath := filepath.Join(t.TempDir(), "trusted")
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIDGE_DATA_DIR", dataDir)

	output := runCLI(t, "trusted-root", "add", rootPath)
	var added struct {
		RequestID string `json:"requestId"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(output, &added); err != nil || added.RequestID == "" || added.Status != "pending" {
		t.Fatalf("add output=%s err=%v", output, err)
	}
	if output = runCLI(t, "trusted-root", "approve", added.RequestID); len(output) == 0 {
		t.Fatal("approve returned empty result")
	}
	if output = runCLI(t, "trusted-root", "list"); len(output) == 0 {
		t.Fatal("list returned empty result")
	}
	if output = runCLI(t, "trusted-root", "show", added.RequestID); len(output) == 0 {
		t.Fatal("show returned empty result")
	}
	if output = runCLI(t, "trusted-root", "verify", added.RequestID); !containsJSON(output, `"verified": true`) {
		t.Fatalf("verify output=%s", output)
	}
	if output = runCLI(t, "trusted-root", "disable", added.RequestID); !containsJSON(output, `"status": "disabled"`) {
		t.Fatalf("disable output=%s", output)
	}
	if err := run([]string{"trusted-root", "verify", added.RequestID}); err == nil {
		t.Fatal("disabled trust verified as active")
	}
	if output = runCLI(t, "trusted-root", "enable", added.RequestID); !containsJSON(output, `"status": "approved"`) {
		t.Fatalf("enable output=%s", output)
	}
	if output = runCLI(t, "trusted-root", "revoke", added.RequestID); !containsJSON(output, `"status": "revoked"`) {
		t.Fatalf("revoke output=%s", output)
	}
	if err := run([]string{"trusted-root", "enable", added.RequestID}); err == nil {
		t.Fatal("revoked trust was re-enabled")
	}
}

func runCLI(t *testing.T, args ...string) []byte {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "bridge-admin-output-*")
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	os.Stdout = file
	runErr := run(args)
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	output, readErr := io.ReadAll(file)
	os.Stdout = previous
	if closeErr := file.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if runErr != nil {
		t.Fatalf("bridge-admin %v: %v", args, runErr)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	return output
}

func containsJSON(value []byte, fragment string) bool {
	return strings.Contains(string(value), fragment)
}

func TestSessionCatalogAndWorkspaceTrustAreLocalRevocableGrants(t *testing.T) {
	dataDir := t.TempDir()
	home := t.TempDir()
	store, err := sqlite.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	workspaces := service.NewWorkspaceService(store, nil, home, "")
	root := filepath.Join(home, "fixture")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	request, err := workspaces.RequestRegistrationLocal(context.Background(), "fixture-project", root, "tester")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := workspaces.ApproveRegistration(context.Background(), request.ID, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if enabled, err := store.SessionCatalogEnabled(context.Background()); err != nil || enabled {
		t.Fatalf("new global catalog default=(%v,%v), want disabled", enabled, err)
	}
	if workspace.SessionTrusted {
		t.Fatal("new workspace was trusted by default")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIDGE_DATA_DIR", dataDir)
	if err := run([]string{"codex-session", "catalog", "enable"}); err != nil {
		t.Fatalf("local global catalog grant failed: %v", err)
	}
	if err := run([]string{"codex-session", "trust", "fixture-project"}); err != nil {
		t.Fatalf("local workspace trust failed: %v", err)
	}
	store, err = sqlite.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if enabled, err := store.SessionCatalogEnabled(context.Background()); err != nil || !enabled {
		t.Fatalf("global catalog grant=(%v,%v), want enabled", enabled, err)
	}
	workspace, err = store.GetWorkspace(context.Background(), "fixture-project")
	if err != nil || !workspace.SessionTrusted {
		t.Fatalf("workspace trust=(%+v,%v), want trusted", workspace, err)
	}
	if err := run([]string{"codex-session", "untrust", "fixture-project"}); err != nil {
		t.Fatalf("local workspace trust revoke failed: %v", err)
	}
	if err := run([]string{"codex-session", "catalog", "disable"}); err != nil {
		t.Fatalf("local global catalog revoke failed: %v", err)
	}
	workspace, err = store.GetWorkspace(context.Background(), "fixture-project")
	if err != nil || workspace.SessionTrusted {
		t.Fatalf("workspace trust remained after revoke=(%+v,%v)", workspace, err)
	}
	if enabled, err := store.SessionCatalogEnabled(context.Background()); err != nil || enabled {
		t.Fatalf("global catalog remained enabled after revoke=(%v,%v)", enabled, err)
	}
}
