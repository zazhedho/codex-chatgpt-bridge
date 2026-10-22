package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"time"

	"github.com/zazhedho/codex-chatgpt-bridge/internal/audit"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/codex"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/domain"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/repository/sqlite"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/security"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/service"
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
	workspaces := service.NewWorkspaceService(store, auditLogger, home, os.Getenv("CODEX_HOME"), store)
	trustedRoots := service.NewTrustedWorkspaceRootService(store, workspaces, auditLogger, home, os.Getenv("CODEX_HOME"))
	ctx := context.Background()
	switch args[0] {
	case "trusted-root":
		return runTrustedRoot(ctx, args[1:], trustedRoots, current.Username)
	case "workspace":
		return runWorkspace(ctx, args[1:], workspaces, auditLogger, current.Username)
	case "approval":
		approvals := service.NewApprovalService(store, workspaces)
		return runApproval(ctx, args[1:], approvals, auditLogger, current.Username)
	case "codex-session":
		executable := os.Getenv("CODEX_EXECUTABLE")
		if executable == "" {
			executable = "codex"
		}
		client := codex.NewClient(executable, slog.New(slog.NewTextHandler(io.Discard, nil)))
		defer client.Close()
		sessions := service.NewSessionService(store, workspaces, client, auditLogger, trustedRoots)
		operationCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return runCodexSession(operationCtx, args[1:], sessions, current.Username)
	default:
		return fmt.Errorf("%s", usage)
	}
}

func runTrustedRoot(ctx context.Context, args []string, roots *service.TrustedWorkspaceRootService, actor string) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", usage)
	}
	switch args[0] {
	case "add":
		if len(args) != 2 {
			return fmt.Errorf("%s", usage)
		}
		root, err := roots.AddLocal(ctx, args[1], actor)
		if err != nil {
			return err
		}
		return print(map[string]any{"requestId": root.ID, "status": root.Status, "rootPath": root.RootPath, "expiresAt": root.ExpiresAt.UnixMilli()})
	case "approve":
		if len(args) != 2 {
			return fmt.Errorf("%s", usage)
		}
		root, err := roots.ApproveLocal(ctx, args[1], actor)
		if err != nil {
			return err
		}
		return print(root)
	case "list":
		if len(args) != 1 {
			return fmt.Errorf("%s", usage)
		}
		values, err := roots.ListLocal(ctx, actor)
		if err != nil {
			return err
		}
		return print(values)
	case "show":
		if len(args) != 2 {
			return fmt.Errorf("%s", usage)
		}
		root, err := roots.GetLocal(ctx, args[1], actor)
		if err != nil {
			return err
		}
		return print(root)
	case "verify":
		if len(args) != 2 {
			return fmt.Errorf("%s", usage)
		}
		root, err := roots.VerifyLocal(ctx, args[1], actor)
		if err != nil {
			return err
		}
		return print(map[string]any{"verified": true, "rootId": root.ID, "status": root.Status, "rootPath": root.RootPath})
	case "disable", "enable", "revoke":
		if len(args) != 2 {
			return fmt.Errorf("%s", usage)
		}
		status := map[string]string{"disable": domain.TrustedRootDisabled, "enable": domain.TrustedRootApproved, "revoke": domain.TrustedRootRevoked}[args[0]]
		if err := roots.SetStatusLocal(ctx, args[1], status, actor); err != nil {
			return err
		}
		return print(map[string]any{"rootId": args[1], "status": status})
	default:
		return fmt.Errorf("%s", usage)
	}
}

func runCodexSession(ctx context.Context, args []string, sessions *service.SessionService, actor string) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", usage)
	}
	switch args[0] {
	case "catalog":
		if len(args) != 2 {
			return fmt.Errorf("%s", usage)
		}
		switch args[1] {
		case "enable", "disable":
			enabled := args[1] == "enable"
			if err := sessions.SetCatalogAccessLocal(ctx, enabled, actor); err != nil {
				return err
			}
			return print(map[string]any{"globalSessionCatalogEnabled": enabled})
		case "status":
			enabled, err := sessions.CatalogAccessLocal(ctx, actor)
			if err != nil {
				return err
			}
			return print(map[string]any{"globalSessionCatalogEnabled": enabled})
		default:
			return fmt.Errorf("%s", usage)
		}
	case "trust", "untrust":
		if len(args) != 2 {
			return fmt.Errorf("%s", usage)
		}
		trusted := args[0] == "trust"
		if err := sessions.SetWorkspaceTrustLocal(ctx, args[1], trusted, actor); err != nil {
			return err
		}
		return print(map[string]any{"workspaceId": args[1], "sessionTrusted": trusted})
	case "list":
		if len(args) != 2 {
			return fmt.Errorf("%s", usage)
		}
		values, err := sessions.ListLocal(ctx, args[1], actor)
		if err != nil {
			return err
		}
		return print(values)
	case "pending":
		if len(args) != 1 {
			return fmt.Errorf("%s", usage)
		}
		values, err := sessions.PendingLocal(ctx, actor)
		if err != nil {
			return err
		}
		return print(values)
	case "show":
		if len(args) != 2 {
			return fmt.Errorf("%s", usage)
		}
		value, err := sessions.ShowPendingLocal(ctx, args[1], actor)
		if err != nil {
			return err
		}
		return print(value)
	case "approve":
		if len(args) != 2 {
			return fmt.Errorf("%s", usage)
		}
		thread, err := sessions.ApproveLocal(ctx, args[1], actor)
		if err != nil {
			return err
		}
		return print(map[string]any{"attached": true, "threadId": thread.ID, "workspaceId": thread.WorkspaceID})
	case "reject":
		if len(args) != 2 {
			return fmt.Errorf("%s", usage)
		}
		return sessions.RejectLocal(ctx, args[1], actor)
	case "project-binding":
		if len(args) < 2 {
			return fmt.Errorf("%s", usage)
		}
		switch args[1] {
		case "pending":
			if len(args) != 2 {
				return fmt.Errorf("%s", usage)
			}
			values, err := sessions.PendingProjectBindingsLocal(ctx, actor)
			if err != nil {
				return err
			}
			return print(values)
		case "show":
			if len(args) != 3 {
				return fmt.Errorf("%s", usage)
			}
			value, err := sessions.ProjectBindingLocal(ctx, args[2], actor)
			if err != nil {
				return err
			}
			return print(value)
		case "approve":
			if len(args) != 3 {
				return fmt.Errorf("%s", usage)
			}
			value, err := sessions.ApproveProjectBindingLocal(ctx, args[2], actor)
			if err != nil {
				return err
			}
			return print(value)
		case "reject":
			if len(args) != 3 {
				return fmt.Errorf("%s", usage)
			}
			return sessions.RejectProjectBindingLocal(ctx, args[2], actor)
		case "list":
			if len(args) != 2 {
				return fmt.Errorf("%s", usage)
			}
			values, err := sessions.ListProjectBindingsLocal(ctx, actor)
			if err != nil {
				return err
			}
			return print(values)
		case "revoke":
			if len(args) != 3 {
				return fmt.Errorf("%s", usage)
			}
			if err := sessions.RevokeProjectBindingLocal(ctx, args[2], actor); err != nil {
				return err
			}
			return print(map[string]any{"threadId": args[2], "status": domain.SessionProjectBindingRevoked})
		default:
			return fmt.Errorf("%s", usage)
		}
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
	case "enable-write", "disable-write":
		if len(args) != 2 {
			return fmt.Errorf("%s", usage)
		}
		enabled := args[0] == "enable-write"
		if err := workspaces.SetWriteEnabledLocal(ctx, args[1], enabled, actor); err != nil {
			return err
		}
		return print(map[string]any{"workspaceId": args[1], "writeEnabled": enabled})
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

const usage = "Usage: bridge-admin trusted-root add <ROOT_PATH>|approve <REQUEST_ID>|list|show <ROOT_ID>|verify <ROOT_ID>|disable <ROOT_ID>|enable <ROOT_ID>|revoke <ROOT_ID>\n       bridge-admin workspace list|pending|request <workspace-id> <absolute-path>|approve <request-id>|reject <request-id>|enable-write <workspace-id>|disable-write <workspace-id>\n       bridge-admin approval list|show <id>|approve <id>|reject <id>\n       bridge-admin codex-session catalog enable|disable|status|trust <workspace-id>|untrust <workspace-id>|list <workspace-id>|pending|show <request-id>|approve <request-id>|reject <request-id>\n       bridge-admin codex-session project-binding pending|show <request-id>|approve <request-id>|reject <request-id>|list|revoke <thread-id>"
