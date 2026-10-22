# Approval model

## Workspace registration

Remote MCP callers can request a workspace registration and inspect the pending request, but cannot approve it. Check the canonical path and approve only through the local CLI:

```sh
go run ./cmd/bridge-admin workspace pending
go run ./cmd/bridge-admin workspace approve <request-id>
```

The local CLI records the macOS username as the approver.

## Global Codex session catalog and workspace trust

Both grants default to disabled. Enabling the catalog allows authenticated MCP clients to see bounded metadata pages for local Codex CLI sessions. It does not grant workspace access or allow resume. Only approved Bridge workspaces can be resumed; unapproved sessions show a project basename and require normal workspace registration approval first.

Set `BRIDGE_DATA_DIR` to the live Bridge database before running these local commands:

```sh
go run ./cmd/bridge-admin codex-session catalog status
go run ./cmd/bridge-admin codex-session catalog enable
go run ./cmd/bridge-admin codex-session trust <approved-workspace-id>
go run ./cmd/bridge-admin codex-session untrust <approved-workspace-id>
go run ./cmd/bridge-admin codex-session catalog disable
```

`catalog enable` is the one-time local owner grant. `trust` is separate for each already approved workspace and is bound to its canonical filesystem identity. Neither grant enables workspace writes. Disabling the global catalog stops discovery; untrusting a workspace blocks future resumes for sessions selected through that trust. Audit rows record each grant and revocation.

ChatGPT calls `codex_list_local_sessions` with an optional `workspaceId`, `search`, `limit` (maximum 50), and the returned opaque `nextCursor`. After the local owner grants global catalog access, the list includes session ID, display name, project, activity time, App Server status, and link status; it omits raw paths and conversation turns. `displayName` prefers the App Server's `thread.name`, then uses a secret-redacted, bounded `thread.preview`, then `Untitled session`. Search matches display names on each bounded page; follow `nextCursor` to continue through later pages. To continue, call `codex_resume_thread` with the chosen `workspaceId`, `threadId`, and prompt. Each request names its thread; no shared active-session pointer exists.

An active status is never taken over. `canResume` is based on App Server metadata and may be advisory if a separate Terminal process still owns a thread; in that case `thread/resume` returns the App Server conflict and Bridge leaves the process alone. Existing per-session attach approval remains available for an approved workspace without trust.

## Attach a Terminal Codex session

Discovery is local and workspace-scoped. First inspect the CLI-only metadata for an already approved workspace, then request attach from ChatGPT and approve that exact thread locally:

```sh
go run ./cmd/bridge-admin codex-session list <workspace-id>
# ChatGPT calls codex_request_session_attach using a listed threadId.
go run ./cmd/bridge-admin codex-session pending
go run ./cmd/bridge-admin codex-session show <request-id>
go run ./cmd/bridge-admin codex-session approve <request-id>
```

Attach approval is separate from workspace registration and write-task approval. It expires after 15 minutes, is single-use, and binds the thread ID, approved workspace file identity, CLI source/originator, canonical path, and session update time. Approval is rejected if the session changed, is active, belongs to another source/workspace, or the workspace root was replaced. The bridge stores the thread-to-workspace binding, not its conversation history. Close the Terminal Codex session before approval; App Server status is not a cross-process lock, and resume may reject a thread that still has an active writer.

After attach, MCP may resume the thread under the current Bridge sandbox. Prior conversation is context only and cannot grant permissions. Any source modification still requires the existing, separate local approval for that exact write task.

## Workspace-write task

Write mode has two independent gates:

1. A local administrator enables write for an already approved workspace. Existing workspaces remain read-only by default.
2. The user locally approves each exact write task once. ChatGPT text and Cloudflare authentication do not approve it.

Example:

Run these commands in a local terminal with `BRIDGE_DATA_DIR` set to the same database directory as the running bridge. If unset, `bridge-admin` uses its separate default directory and will not affect the active bridge.

```sh
go run ./cmd/bridge-admin workspace enable-write <workspace-id>
go run ./cmd/bridge-admin approval list
go run ./cmd/bridge-admin approval show <approval-id>
go run ./cmd/bridge-admin approval approve <approval-id>
```

ChatGPT requests the task with `codex_start_write_task` (`workspaceId`, `prompt`, optional bridge-owned `threadId`). The bridge returns a task ID and approval ID with status `waiting_approval`. No worker job is queued before approval. Review the workspace, thread, exact prompt, and parameter hash in the local approval details before approving. The approval expires after five minutes.

The task and approval are created in one SQLite transaction. The approval hash binds the exact prompt, workspace ID, task ID, execution mode, and optional thread ID. The worker verifies that binding and current workspace write permission, consumes the approval once, clears the stored prompt, and queues the task atomically. Rejection, expiry, cancellation before approval, or disabling workspace write also clears the stored prompt and prevents execution. A restart never replays a consumed or queued task.

After approval, ChatGPT can poll `codex_get_task` and `codex_get_progress`, then review `codex_get_result` and `codex_get_changes`. A completed task does not mean the implementation or tests are correct. Review the diff and test output; request any revision as a new write task and approve it separately.

Disable workspace write when no longer needed:

```sh
go run ./cmd/bridge-admin workspace disable-write <workspace-id>
```

The command refuses to disable while a write task is running. It rejects pending approvals and cancels waiting or queued write tasks.

## Privileged execution

Workspace-write is bounded to one approved project root, keeps sandbox networking disabled, denies temp-directory writes, and does not grant privileged execution. It does not authorize Git commit/push, cloud changes, deployment, database administration, or access outside the project. No privileged runner is connected.

Future privileged execution must use a separately authenticated local approval channel and isolated runner. Approval must bind workspace, actor, operation, target, environment, exact parameters, expiry, and SHA-256 of any executable artifact. The runner must verify the binding at use time, consume approval once, and receive no Codex credentials or inherited shell secrets.

See [security](SECURITY.md). No provider-specific adapter or privileged executor is enabled.
