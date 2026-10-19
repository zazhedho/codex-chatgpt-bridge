package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"

	"example.com/codex-chatgpt-bridge/internal/audit"
	"example.com/codex-chatgpt-bridge/internal/repository/sqlite"
	"example.com/codex-chatgpt-bridge/internal/security"
	"example.com/codex-chatgpt-bridge/internal/service"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 1 && args[0] == "pending" {
		args = []string{"workspace", "pending"}
	}
	if len(args) == 2 && (args[0] == "approve" || args[0] == "reject") {
		args = []string{"workspace", args[0], args[1]}
	}
	if len(args) < 2 {
		return fmt.Errorf("%s", usage)
	}
	dir := os.Getenv("BRIDGE_DATA_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		dir = filepath.Join(home, "Library", "Application Support", "codex-chatgpt-bridge-go")
	}
	store, err := sqlite.Open(dir)
	if err != nil {
		return err
	}
	defer store.Close()
	current, err := user.Current()
	if err != nil {
		return err
	}
	auditLogger := audit.New(store)
	home, _ := os.UserHomeDir()
	workspaces := service.NewWorkspaceService(store, auditLogger, home, os.Getenv("CODEX_HOME"))
	ctx := context.Background()
	switch args[0] {
	case "workspace":
		return runWorkspace(ctx, args[1:], workspaces, auditLogger, current.Username)
	case "approval":
		approvals := service.NewApprovalService(store, workspaces)
		return runApproval(ctx, args[1:], approvals, auditLogger, current.Username)
	default:
		return fmt.Errorf("%s", usage)
	}
}

func runWorkspace(ctx context.Context, args []string, workspaces *service.WorkspaceService, auditLogger *audit.Logger, actor string) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", usage)
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return fmt.Errorf("%s", usage)
		}
		values, err := workspaces.ListLocal(ctx)
		if err != nil {
			return err
		}
		return print(values)
	case "pending":
		if len(args) != 1 {
			return fmt.Errorf("%s", usage)
		}
		requests, err := workspaces.PendingRequestsLocal(ctx)
		if err != nil {
			return err
		}
		return print(requests)
	case "request":
		if len(args) != 3 {
			return fmt.Errorf("%s", usage)
		}
		request, err := workspaces.RequestRegistrationLocal(ctx, args[1], args[2], actor)
		if err != nil {
			return err
		}
		return print(map[string]any{"requestId": request.ID, "workspaceId": request.WorkspaceID, "expiresAt": request.ExpiresAt.UnixMilli()})
	case "approve":
		if len(args) != 2 {
			return fmt.Errorf("%s", usage)
		}
		workspace, err := workspaces.ApproveRegistration(ctx, args[1], actor)
		if err != nil {
			return err
		}
		return print(map[string]any{"approved": true, "workspaceId": workspace.ID, "rootPath": workspace.RootPath})
	case "reject":
		if len(args) != 2 {
			return fmt.Errorf("%s", usage)
		}
		return workspaces.RejectRegistration(ctx, args[1], actor)
	default:
		return fmt.Errorf("%s", usage)
	}
}

func runApproval(ctx context.Context, args []string, approvals *service.ApprovalService, auditLogger *audit.Logger, actor string) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", usage)
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return fmt.Errorf("%s", usage)
		}
		values, err := approvals.ListPendingLocal(ctx)
		if err != nil {
			return err
		}
		return print(values)
	case "show":
		if len(args) != 2 {
			return fmt.Errorf("%s", usage)
		}
		value, err := approvals.GetLocal(ctx, args[1])
		if err != nil {
			return err
		}
		return print(value)
	case "approve":
		if len(args) != 2 {
			return fmt.Errorf("%s", usage)
		}
		value, err := approvals.ApproveLocal(ctx, args[1], service.ApprovalActor{Channel: "local", Subject: actor})
		if err != nil {
			return err
		}
		if err := auditLogger.Record(ctx, audit.Event{Operation: "approval.approved", Principal: "local:" + actor, WorkspaceID: value.WorkspaceID, Outcome: "success"}); err != nil {
			return err
		}
		return print(map[string]any{"approved": true, "approvalId": value.ID, "parametersHash": value.ParametersHash, "artifactHash": nullable(value.ArtifactHash)})
	case "reject":
		if len(args) != 2 {
			return fmt.Errorf("%s", usage)
		}
		if err := approvals.RejectLocal(ctx, args[1], service.ApprovalActor{Channel: "local", Subject: actor}); err != nil {
			return err
		}
		return auditLogger.Record(ctx, audit.Event{Operation: "approval.rejected", Principal: "local:" + actor, Outcome: "success"})
	default:
		return fmt.Errorf("%s", usage)
	}
}

func print(value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout, security.RedactSecrets(string(encoded)))
	return err
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

const usage = "Usage: bridge-admin workspace list|pending|request <workspace-id> <absolute-path>|approve <request-id>|reject <request-id>\n       bridge-admin approval list|show <id>|approve <id>|reject <id>"
