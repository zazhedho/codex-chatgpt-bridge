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
| `codex_list_threads` | `{workspaceId}` | `{threads:[{threadId,taskId,status,updatedAt}]}` | Bridge-managed threads for registered workspace; read-only/idempotent; max 500. |
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

## Go design decisions

- Root `go.mod`; retain `src/`, `tests/`, `package.json`, and TypeScript docs/history as a rollback reference. Do not migrate or overwrite the TypeScript database.
- Official MCP Go SDK, Streamable HTTP, typed tool schemas and handlers. Services receive narrow interfaces; handlers never query SQLite or issue raw Codex RPC.
- `database/sql` + pure-Go SQLite driver; embedded numbered SQL migrations; a separate `BRIDGE_DATA_DIR` default ending in `codex-chatgpt-bridge-go`.
- Codex client owns JSONL framing, IDs, pending calls, notifications and process lifecycle. Public gateway exposes only fixed thread/turn operations. Server-initiated approval requests are denied. No `command/exec`, `process/spawn`, or generic RPC tool is exposed.
- Read-only workspace-confined execution is gated on App Server profile support and covered by a real `0.163.0` sandbox E2E. Re-run `make test-e2e` after Codex or macOS updates. Codex/macOS sandbox enforcement is a residual platform trust boundary.
- Task output retention: expire result, diff and progress after 30 days by default; preserve task metadata and audit history. Add an `outputExpired` flag to result/task responses so retention is visible. `BRIDGE_OUTPUT_RETENTION_DAYS` can adjust the period; `0` disables automatic output expiry. Database remains plaintext and user-controlled.
- No tunnel, ChatGPT connector, launchd, workspace writes, privileged runner, cloud adapter, package migration, or remote push during this work.

## Rollback

The original TypeScript source, package manifests, tests, and dependencies remain unchanged. Stop the Go binary and use the existing Node commands from the repository. Go uses a separate database directory, so rollback does not need a database conversion or deletion.

## Go migration verification

On 2026-10-19, `go test ./...`, `go test -race ./...`, `go vet ./...`, and `make build` passed. The local admin CLI smoke test requested, listed, approved, and listed a disposable workspace.

The opt-in real local E2E test passed on Codex App Server `0.163.0` using a temporary fixture and actual Codex output. It checked workspace read, sibling denial (`Operation not permitted`), write denial, and marker `BRIDGE_MCP_CODEX_TURN_OK`. This is not a mocked Codex result. MCP → App Server was tested locally only. Secure MCP Tunnel, Custom MCP Connector, ChatGPT Web, launchd, and any connector-based external connectivity remain unconfigured and untested.
