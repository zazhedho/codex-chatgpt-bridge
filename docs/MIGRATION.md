# Go migration

## Baseline audit — 2026-10-19

- Local tools: Go `1.27.2` (`darwin/arm64`), Codex CLI `0.163.0`, Node `24.21.0`, npm `12.2.0`.
- TypeScript baseline: `npm test` passed 27 tests across 7 files; `npm run build` passed.
- Repository has no `.git` directory. No branch can be created safely without first initializing a repository and deciding what to include. Keep TypeScript files in place, add Go at project root, and do not push.
- TypeScript SQLite schema: `workspace_requests`, `workspaces`, `tasks`, `approvals`, `audit_events`. Task rows persist prompt SHA-256, progress JSON, result, diff, error, thread/turn IDs. Prompts themselves are not persisted. Result/progress/diff and approval parameters are plaintext. Existing TypeScript database is not imported or opened by Go.
- The installed CLI exposes `codex app-server --stdio` and `app-server generate-json-schema`. It is experimental. Generated local protocol schemas confirm JSONL initialization, `thread/start`, `thread/resume`, `turn/start`, `turn/interrupt`, notifications, and server approval requests. The installed initialize response reports App Server version `0.163.0`.
- The installed schema lacks `readOnly.access.readableRoots`; permission profiles are the supported restriction used by this build. Codex's profile is fixed at thread creation/resume, denies filesystem roots, permits the registered workspace read-only, denies known sensitive paths, disables sandboxed command networking, MCP servers, and web search. Task start fails closed if App Server does not report profile support (0.138.0+). Real local App Server E2E proved a random fixture read, sibling-file denial (`Operation not permitted`), and workspace-write denial on this host. This is evidence for the tested Codex/macOS versions, not a guarantee about future versions.

## TypeScript MCP contract inventory

All 15 tools require `Authorization: Bearer <BRIDGE_AUTH_TOKEN>`. Missing/invalid auth is HTTP 401 with `{"error":"Unauthorized"}`. MCP tool failures are `isError: true` with sanitized text (maximum 500 characters); success returns JSON as text and `structuredContent`. Global limits: 64 KiB HTTP body, 20,000-character task prompt, 64,000-character result/diff, 2,000-character progress entry, 100 retained progress entries. Queue capacity is 20 and global/workspace concurrency is 1. Default execution timeout is 20 minutes.

| Tool | Input | Output | Authorization, side effect, annotation |
|---|---|---|---|
| `workspace_list` | `{}` | `{workspaces: Workspace[]}` | Registered workspaces only; read-only/idempotent. |
| `workspace_get` | `{workspaceId}`; `[a-z][a-z0-9-]{1,62}` | `{workspace: Workspace}` | Existing workspace, revalidate canonical root; read-only/idempotent. |
| `workspace_register` | `{workspaceId, path}`; ID above, path 1–4096 chars | `{requestId, workspaceId, status:"pending", expiresAt}` | Creates pending request only; local CLI approval required; non-idempotent. |
| `codex_list_threads` | `{workspaceId}` | `{threads:[{threadId,taskId,status,updatedAt}]}` | Bridge-managed, individually attached, and selected trusted sessions for an approved workspace; read-only/idempotent; max 500. |
| `codex_list_local_sessions` | optional `{workspaceId,search,limit,cursor}` | metadata page and `nextCursor` | Global CLI session catalog; local owner grant required; max 50; no conversation history. |
| `codex_start_task` | `{workspaceId,prompt}`; trimmed nonempty prompt ≤20,000 chars | `{taskId,status,threadId,createdAt}` | Creates async read-only task; queue bounded; non-idempotent. |
| `codex_resume_thread` | `{workspaceId,threadId,prompt}`; thread ID 1–256 chars, prompt as above | Same as start | Thread must be bridge-owned and bound to same workspace; non-idempotent. |
| `codex_get_task` | `{taskId}`; UUID | `{task: Task}` | Task must exist and remain attached to an authorized workspace; read-only/idempotent. |
| `codex_get_progress` | `{taskId}`; UUID | `{taskId,status,events:[{at,text}]}` | Same task authorization; read-only/idempotent. |
| `codex_get_result` | `{taskId}`; UUID | `{taskId,status,result,error}` | Same task authorization; read-only/idempotent. |
| `codex_get_changes` | `{taskId}`; UUID | `{taskId,status,diff}` | Same task authorization; read-only/idempotent. |
| `codex_cancel_task` | `{taskId}`; UUID | `{taskId,status}` | Queued/running task cancellation; destructive/non-idempotent in MCP annotations. |
| `approval_list_pending` | `{}` | `{workspaceRegistrations:[{requestId,workspaceId,rootPath,expiresAt}],operationApprovals:[ApprovalSummary]}` | Read-only/idempotent; secret-like parameters redacted. |
| `approval_get_details` | `{approvalId}`; UUID | `{details: object}` | Pending workspace request or approval; read-only/idempotent; secret-like values redacted. |
| `bridge_get_status` | `{}` | `{status:"ok",uptimeSeconds,codexAppServer:"connected"|"disconnected",activeTasks,queuedTasks}` | Read-only/idempotent. |
| `bridge_get_capabilities` | `{}` | `{mcpTransport,codexTransport,workspaces,codexFilesystem,codexNetwork,codexMcpServers,privilegedExecution,secureTunnel,chatgptConnection}` | Read-only/idempotent; capability literals disclose local-only state. |

`Workspace` fields: `id`, `rootPath`, `allowedDirectories`, `defaultModel:null`, `sandboxPermissions:{filesystem:"read-only",network:false}`, `allowedOperations:["analysis"]`, `approvalRequirements:["workspace-registration"]`, `maxConcurrentTasks:1`, `timeoutMs`, `gitRepository:string|null`, `createdAt`.

`Task` fields: `id`, `workspaceId`, `threadId:string|null`, `turnId:string|null`, `status` (`queued`, `running`, `waiting_approval`, `completed`, `failed`, `cancelled`, `interrupted`), `progress`, `result`, `diff`, `error`, `createdAt`, `updatedAt`.

`ApprovalSummary` fields: `id`, `workspaceId:string|null`, `operationType`, `targetResource`, `parameters`, `parametersHash`, `artifactHash:string|null`, `status`, `expiresAt`. Details additionally include `approvedBy`, `createdAt`, `consumedAt`.

The original single bearer token represents one local principal with access to every explicitly approved workspace. It is not per-user or per-workspace identity. Go keeps that single-principal model and reauthorizes each resource through its registered workspace/thread/task binding. Remote approval remains unavailable.

## Go workspace-write extension — 2026-10-20

The Go MCP surface adds `codex_start_write_task` while preserving the 15 original tool contracts. Its input is `{workspaceId, prompt, threadId?}` and its output is `{taskId, status:"waiting_approval", threadId, executionMode:"workspace-write", approvalId, createdAt}`. The workspace must already be locally approved and locally write-enabled. The tool creates an expiring task approval; it never approves or queues a write task itself.

Existing workspaces migrate with `write_enabled=0`; task rows migrate with `execution_mode="read-only"`. Write approvals expire after five minutes, bind task/workspace/thread/mode/exact prompt with SHA-256, and can be consumed once only by the worker after local CLI approval. Prompt text is stored only in the pending approval parameters, then cleared on consume, rejection, expiry, cancellation, or workspace write disable.

The workspace-write profile uses Codex's supported `:workspace` parent, denies `:root`, `/tmp`, and `$TMPDIR`, permits writes only to the current `:workspace_roots` root, keeps sensitive-file denials, disables network and MCP servers, and grants a read-only exception only for the resolved Codex executable needed by its macOS sandbox helper. The App Server response must report the correct profile, workspace root, `workspaceWrite`, network disabled, no additional writable roots, and both temporary-directory exclusions.

## Additional Go MCP tool

| Tool | Input | Output | Authorization, side effect, annotation |
|---|---|---|---|
| `codex_start_write_task` | `{workspaceId,prompt,threadId?}`; prompt ≤20,000 chars; optional bridge-owned thread | `{taskId,status:"waiting_approval",threadId,executionMode:"workspace-write",approvalId,createdAt}` | Requires authenticated caller, approved workspace, local workspace write opt-in, bridge-owned same-workspace thread if supplied, and available App Server profile support. Creates task plus pending approval atomically; no Codex turn before local `bridge-admin approval approve`. Mutating, destructive, non-idempotent. |

## Go design decisions

- Root `go.mod`; retain `src/`, `tests/`, `package.json`, and TypeScript docs/history as a rollback reference. Do not migrate or overwrite the TypeScript database.
- Official MCP Go SDK, Streamable HTTP, typed tool schemas and handlers. Services receive narrow interfaces; handlers never query SQLite or issue raw Codex RPC.
- `database/sql` + pure-Go SQLite driver; embedded numbered SQL migrations; a separate `BRIDGE_DATA_DIR` default ending in `codex-chatgpt-bridge-go`.
- Codex client owns JSONL framing, IDs, pending calls, notifications and process lifecycle. Public gateway exposes only fixed thread/turn operations. Server-initiated approval requests are denied. No `command/exec`, `process/spawn`, or generic RPC tool is exposed.
- Read-only-by-default plus locally approved workspace-write execution is gated on App Server profile support and covered by real `0.163.0` sandbox E2E. Re-run `make test-e2e` and `make test-write-e2e` after Codex or macOS updates. Codex/macOS sandbox enforcement is a residual platform trust boundary.
- Task output retention: expire result, diff and progress after 30 days by default; preserve task metadata and audit history. Add an `outputExpired` flag to result/task responses so retention is visible. `BRIDGE_OUTPUT_RETENTION_DAYS` can adjust the period; `0` disables automatic output expiry. Database remains plaintext and user-controlled.
- This workspace-write change did not alter Cloudflare, ChatGPT, launchd, privileged runners, cloud adapters, TypeScript packages, or remote Git state.

## Rollback

The original TypeScript source, package manifests, tests, and dependencies remain unchanged. Stop the Go binary and use the existing Node commands from the repository. Go uses a separate database directory, so rollback does not need a database conversion or deletion.

## Go migration verification

On 2026-10-19, `go test ./...`, `go test -race ./...`, `go vet ./...`, and `make build` passed. The local admin CLI smoke test requested, listed, approved, and listed a disposable workspace.

The opt-in read-only E2E passed on Codex App Server `0.163.0` with marker `BRIDGE_MCP_CODEX_TURN_OK`. The workspace-write E2E passed on `0.163.0`: a real MCP request created a pending task; local `bridge-admin` approved it once; Codex changed a fixture source file, created a file, and reported a fixture assertion; the returned MCP diff contained both changes; a separate `codex sandbox` probe using the equivalent fixed profile observed in-workspace writes and OS denials for sibling reads/writes, symlink escape, temp writes, and network. The host also ran `go test ./...` against the fixture. These checks use only disposable workspaces and no production resources.

## Attach a Terminal-created Codex session

Codex CLI `0.163.0` exposes the official App Server `thread/list`, `thread/read`, and `thread/resume` methods. The bridge calls `thread/list` with exact workspace, source-kind and state-database-only filters; local `bridge-admin` shows only approved CLI metadata. It reads with `includeTurns:false`, so it does not copy old turns into MCP. `thread/resume` retains the Codex rollout context, but the adapter reapplies the current workspace sandbox and continuation policy. The upstream App Server is experimental; see the [Codex App Server documentation](https://developers.openai.com/codex/app-server).

The installed TUI reported `source=vscode`, `originator=codex-tui`, while `codex exec` reported `source=exec`, `originator=codex_exec`. The bridge accepts only these exact CLI classifications plus `source=cli`; generic editor sessions remain excluded. A separate Codex process may still hold the rollout writer after App Server metadata says `notLoaded`; App Server then rejects resume with “already has an active writer.” Close the original CLI session and retry. The bridge does not terminate or take over that process.

After approving the workspace locally, discover and request attach as follows:

```sh
go run ./cmd/bridge-admin codex-session list <workspace-id>
# ChatGPT calls codex_request_session_attach with one listed threadId.
go run ./cmd/bridge-admin codex-session pending
go run ./cmd/bridge-admin codex-session show <request-id>
go run ./cmd/bridge-admin codex-session approve <request-id>
```

The request lasts 15 minutes, binds the approved workspace file identity and session update time, and is consumed once. After attach, ChatGPT uses `codex_list_threads` and `codex_resume_thread`. Write tasks still need their independent local write approval.

## Global Codex CLI catalog and persistent workspace trust

The global catalog is disabled after migration and must be explicitly enabled by the local owner. Workspace session trust also defaults off, including for workspaces that were previously approved. Use the same `BRIDGE_DATA_DIR` as the running Bridge:

```sh
go run ./cmd/bridge-admin codex-session catalog status
go run ./cmd/bridge-admin codex-session catalog enable
go run ./cmd/bridge-admin codex-session trust <approved-workspace-id>
```

After that, refresh ChatGPT tools and call `codex_list_local_sessions`. It returns one `thread/list` metadata page sorted by latest activity. Use `workspaceId` to scope to one approved project, `search` to search each page's displayed title, `limit` up to 50, and pass `nextCursor` back unchanged with the same workspace/search filters. Each result includes `displayName`, preferring Codex's `thread.name`, falling back to a sanitized preview capped at 160 characters, then `Untitled session`. Search can return fewer matches than the requested limit; continue with `nextCursor` to search later pages. The tool returns no conversation turns or full paths. Sessions from unapproved workspaces cannot be resumed.

Call `codex_resume_thread` with both the selected `workspaceId` and `threadId`; each task is routed to that exact thread. Workspace trust suppresses repeat attach approval only. Write mode remains read-only unless separately enabled and approved per task. Revoke with:

```sh
go run ./cmd/bridge-admin codex-session untrust <approved-workspace-id>
go run ./cmd/bridge-admin codex-session catalog disable
```

Catalog disable stops discovery. Workspace untrust blocks future resumes from trust-bound thread mappings. Individual attach approvals remain separate. App Server metadata cannot guarantee that another Terminal process is idle; close its session before resuming. A conflict is returned rather than taking over the writer.

### Legacy sessions started from a trusted parent

If a CLI session's recorded working directory is exactly the trusted parent directory, Bridge cannot infer its intended repository from its title or conversation. The catalog reports `project_selection_required` and sets `canResume` to false. ChatGPT then lists bounded choices with `codex_list_session_projects({threadId})`, using only root-relative Git repository metadata. After the user selects a repository, `codex_request_session_project_binding({threadId, projectId})` creates a pending request; it does not authorize resume.

Review and approve the request locally against the same `BRIDGE_DATA_DIR` as the running Bridge:

```sh
go run ./cmd/bridge-admin codex-session project-binding pending
go run ./cmd/bridge-admin codex-session project-binding show <REQUEST_ID>
go run ./cmd/bridge-admin codex-session project-binding approve <REQUEST_ID>
go run ./cmd/bridge-admin codex-session project-binding list
```

The pending request expires after 15 minutes. Approval rechecks thread identity and activity metadata, trusted-root status and filesystem identity, and the selected repository's canonical path, filesystem identity, and `.git` boundary. The persistent binding applies only to that thread and repository, grants read-only resume, and can be revoked with `codex-session project-binding revoke <THREAD_ID>`. It does not grant write access; the workspace must separately be locally write-enabled and every write task needs its own approval. Sessions already inside a repository continue to use automatic nearest-boundary resolution. Symlinks outside the root, active threads, changed paths, and ambiguous Git boundaries fail closed.

### Local real-session E2E

`TestAttachAndResumeTerminalCodexSession` is opt-in. It requires a disposable, locally created CLI session, a disposable workspace under `$HOME/Library/Caches/codex-bridge-session-e2e.*`, an approved workspace record, and a separate test data directory. The test discovers via the actual `bridge-admin`, requests attach over authenticated local MCP, approves through the CLI, resumes through the real App Server, checks prior marker and coding-plan context, and confirms a write request remains pending without changing files. It also verifies an unattached session cannot be listed or resumed.

```sh
BRIDGE_ATTACH_E2E=1 \
BRIDGE_ATTACH_E2E_WORKSPACE=<fixture-workspace> \
BRIDGE_ATTACH_E2E_WORKSPACE_ID=<approved-id> \
BRIDGE_ATTACH_E2E_THREAD_ID=<thread-id-from-bridge-admin> \
BRIDGE_ATTACH_E2E_MARKER=<harmless-fixture-marker> \
BRIDGE_ATTACH_E2E_DATA_DIR=<isolated-bridge-data-dir> \
go test ./tests/e2e -run TestAttachAndResumeTerminalCodexSession -count=1 -v
```

The E2E passed on the installed `0.163.0` App Server with a completed `codex exec` Terminal session. A separately created TUI fixture reached local approval but App Server correctly rejected its resume because another writer still owned the rollout; that session was not taken over. No private session was used.
