# codex-chatgpt-bridge

Local Go MCP bridge between ChatGPT-compatible clients and Codex App Server on macOS. It listens on loopback only. Tasks are read-only by default; workspace writes require a local workspace opt-in and one approval per task.

## Status

- **Implemented:** 20 MCP tools, bearer or Cloudflare Access authentication, explicit/local-approved workspaces, SQLite task and audit storage, async tasks, progress, cancellation, thread resume, attach approval, global-session catalog, root-scoped read-only sessions, and optional local project binding.
- **Tested locally:** unit, race, vet, build, authenticated MCP contracts, and read-only plus approved workspace-write Codex sandbox E2E on Codex CLI 0.163.0.
- **Sandbox gate:** new, same-process resume, and post-restart turns passed through the real Codex App Server using a deterministic local provider stub. The stub is not a real-model behavior test.
- **Workspace-write E2E:** local approval enabled a real fixture edit and new file; App Server command output proved fixture assertions; the Codex macOS sandbox probe verified in-workspace writes and denied sibling, symlink, temporary-directory, and network access.
- **Previously verified live:** ChatGPT called `bridge_get_status`, `bridge_get_capabilities`, and `workspace_list` through the configured Cloudflare OAuth/Tunnel route. Refresh ChatGPT tools to discover `codex_start_write_task`.
- **Not verified this turn:** the new write tool through ChatGPT Web; no tunnel, OAuth, connector, or running bridge configuration was changed.
- **Unsupported:** arbitrary shell/RPC passthrough and external/production privileged actions.

See [migration results](docs/MIGRATION.md), [architecture](docs/ARCHITECTURE.md), and [security](docs/SECURITY.md).

## Prerequisites

- macOS and Go `1.27` or later.
- Codex CLI with App Server support and a working local sign-in. This Mac was tested with `codex-cli 0.163.0`.
- `openssl` for a local bearer token.

Node.js is not required to build or run Go. The original TypeScript source, tests, and SQLite data remain in place for rollback; Go uses a separate data directory by default.

## Build and test

```sh
make build
make test
make test-race
make vet
make test-write-e2e
```

The Terminal-session attach E2E is opt-in. It requires a disposable CLI session, an already locally approved fixture workspace, and an isolated `BRIDGE_DATA_DIR`; its setup and commands are in [migration notes](docs/MIGRATION.md#attach-a-terminal-created-codex-session).

Optional real Codex integration test. It uses only a temporary fixture and the locally signed-in Codex App Server:

```sh
BRIDGE_E2E=1 go test ./tests/e2e -run TestRealCodexThroughMCPAndSandbox -count=1 -v
```

## Run the bridge

Set a local token and, optionally, a data directory. The token is never printed by the bridge.

```sh
export BRIDGE_AUTH_TOKEN="$(openssl rand -hex 32)"
export BRIDGE_DATA_DIR="$HOME/Library/Application Support/codex-chatgpt-bridge-go"
make run
```

The bridge defaults to `127.0.0.1:8787`. It rejects non-loopback bind addresses. `BRIDGE_DATA_DIR` must be an absolute path. `CODEX_EXECUTABLE` can select a Codex binary; `CODEX_HOME` can select its local Codex home. Keep the token in the local shell or a secure local store. Do not paste it into ChatGPT.

Stop with **Ctrl-C** or SIGTERM. The process handles graceful shutdown and does not install a launchd service.

## Register a workspace

Registration creates a pending request. Approve it locally after checking the path. Remote MCP callers cannot approve workspaces.

```sh
go run ./cmd/bridge-admin workspace request backend-go "$HOME/Code/backend"
go run ./cmd/bridge-admin workspace pending
go run ./cmd/bridge-admin workspace approve <request-id>
go run ./cmd/bridge-admin workspace list
```

Register only project roots you trust. The bridge canonicalizes the path, checks symlinks and file identity, and revalidates it on use. Do not register your home directory or a secrets directory.

## Request a source change

Workspace writes stay off until enabled locally. Enable only a workspace you reviewed:

Run the commands below in a local terminal with `BRIDGE_DATA_DIR` set to the same database directory as the running bridge. If unset, `bridge-admin` uses its separate default directory and will not affect the active bridge.

```sh
go run ./cmd/bridge-admin workspace enable-write <workspace-id>
```

Ask ChatGPT to call `codex_start_write_task` with the workspace ID and implementation prompt. The bridge returns `waiting_approval`, a task ID, and an approval ID without running Codex. Inspect the exact request locally, then approve once:

```sh
go run ./cmd/bridge-admin approval show <approval-id>
go run ./cmd/bridge-admin approval approve <approval-id>
```

ChatGPT can poll `codex_get_task` and `codex_get_progress`, then read `codex_get_result` and `codex_get_changes`. Review the diff and test output; completion does not certify correctness. Disable workspace writes when done:

```sh
go run ./cmd/bridge-admin workspace disable-write <workspace-id>
```

Pending prompts are stored locally in the expiring approval record so the worker can run them after approval. The bridge clears that prompt on approval consumption, rejection, cancellation, expiry, or workspace write disable.

## Test MCP locally

With the bridge running, use MCP Inspector against `http://127.0.0.1:8787/mcp` using Streamable HTTP and `Authorization: Bearer <local token>`. `/healthz` is also loopback-only and authenticated.

The MCP surface has 20 tools:

- Workspace: `workspace_list`, `workspace_get`, `workspace_register`.
- Codex: `codex_list_threads`, `codex_list_local_sessions`, `codex_list_session_projects`, `codex_request_session_project_binding`, `codex_start_task`, `codex_start_write_task`, `codex_request_session_attach`, `codex_resume_thread`, `codex_get_task`, `codex_get_progress`, `codex_get_result`, `codex_get_changes`, `codex_cancel_task`.
- Approvals and bridge: `approval_list_pending`, `approval_get_details`, `bridge_get_status`, `bridge_get_capabilities`.

`codex_start_task` remains read-only. `codex_start_write_task` only creates a pending local approval; the worker cannot run it until `bridge-admin` approves the exact task. Both return a `taskId`; poll task/progress and read results or changes after completion. Task execution fails closed if App Server does not advertise a supported permission profile.

### Attach a Terminal-created session

Global metadata discovery is off by default. After explicitly enabling it locally, `codex_list_local_sessions` returns one sorted, searchable page from Codex App Server `thread/list`; it does not return conversation turns or filesystem paths. New workspaces still need local registration approval. Trust an approved workspace locally once to select its CLI sessions without per-session attach approval. Untrusted workspaces retain the individual attach flow. Resume always supplies `workspaceId` and `threadId`; there is no active-session pointer. Trust never grants write permission, and every write task still needs its own local approval.

Run these commands with `BRIDGE_DATA_DIR` set to the same data directory as the running bridge:

```sh
go run ./cmd/bridge-admin codex-session catalog status
go run ./cmd/bridge-admin codex-session catalog enable
go run ./cmd/bridge-admin codex-session trust <approved-workspace-id>
```

Revoke with `go run ./cmd/bridge-admin codex-session untrust <workspace-id>` and `go run ./cmd/bridge-admin codex-session catalog disable`. Catalog enablement and workspace trust are independent, persistent local grants. Both default to off. Codex may report `notLoaded` while another Terminal process still owns a session; App Server rejects an active-writer resume, and Bridge does not terminate or take over that process.

## Trusted Workspace Roots — Quick Start

Use one local approval for a parent folder when its Codex sessions should be discoverable and resumable. A session started directly at that root gets a root-scoped, read-only view across repositories beneath it. A session started inside a Git repository remains repository-scoped. Trusting one root does not trust another and never approves writes. Run commands from the repository root.

1. Set the same data directory used by the running bridge. macOS default:

   ```sh
   export BRIDGE_DATA_DIR="${BRIDGE_DATA_DIR:-$HOME/Library/Application Support/codex-chatgpt-bridge-go}"
   ```

   In another supported environment, set `BRIDGE_DATA_DIR` to the absolute data directory configured for that bridge.

2. Enable the global session catalog once if it is disabled:

   ```sh
   go run ./cmd/bridge-admin codex-session catalog status
   go run ./cmd/bridge-admin codex-session catalog enable
   ```

3. Register a root you choose. The command returns a pending `requestId`:

   ```sh
   go run ./cmd/bridge-admin trusted-root add "$HOME/Code/Project"
   go run ./cmd/bridge-admin trusted-root list
   ```

4. Approve that ID locally and verify it:

   ```sh
   go run ./cmd/bridge-admin trusted-root approve <REQUEST_ID>
   go run ./cmd/bridge-admin trusted-root verify <ROOT_ID>
   go run ./cmd/bridge-admin trusted-root list
   ```

5. Add and approve each additional root the same way. A root-scoped session can read across repositories inside its one root. A session started inside a `.git` repository or worktree is scoped to that repository.

6. In ChatGPT, call `codex_list_local_sessions`. For `canResume: true`, call `codex_resume_thread` with its `workspaceId` and `threadId`. `connectionStatus: trusted_root` means a session started directly at the trusted root; it resumes without project binding and can read repositories under that root. A session inside a Git repository remains repository-scoped. Only sessions with an ambiguous non-root working directory need the optional project binding. To create one, call `codex_list_session_projects`, choose the exact repository by root-relative path, then call `codex_request_session_project_binding` with that `projectId`. Review and approve the request locally:

   ```sh
   go run ./cmd/bridge-admin codex-session project-binding pending
   go run ./cmd/bridge-admin codex-session project-binding show <REQUEST_ID>
   go run ./cmd/bridge-admin codex-session project-binding approve <REQUEST_ID>
   ```

   Refresh `codex_list_local_sessions`, then resume the now `project_bound` entry with its exact IDs. A project binding grants read-only resume for that thread and repository; it does not enable workspace writes.

7. Root-scoped sessions remain read-only. To change one repository, use a repository-scoped workspace and its separate local workspace-write and per-task approval flow. The current write API does not support multi-repository target lists, so it rejects root-wide write scope.

8. Disable trust temporarily or revoke it permanently:

   ```sh
   go run ./cmd/bridge-admin trusted-root disable <ROOT_ID>
   go run ./cmd/bridge-admin trusted-root enable <ROOT_ID>
   go run ./cmd/bridge-admin trusted-root revoke <ROOT_ID>
   ```

For full setup, recovery, command reference, and security details, see [Trusted Workspace Roots](docs/trusted-workspace-roots.md).

## ChatGPT integration

The configured Cloudflare Access/Tunnel route and ChatGPT connector were previously used for read-only MCP calls. This change does not alter them. Refresh tools in ChatGPT before using the new `codex_start_write_task` contract; complete its local approval from Terminal as described above. See [Cloudflare integration](docs/CLOUDFLARE_MCP_INTEGRATION.md).

## Data and rollback

Go stores task metadata, progress, results, diffs, approvals, and audit records in SQLite under `BRIDGE_DATA_DIR`; outputs are local plaintext and expire after 30 days by default. `BRIDGE_OUTPUT_RETENTION_DAYS=0` disables expiry. TypeScript data is not imported or modified.

Rollback: stop the Go bridge and use the retained TypeScript implementation with its original `npm` scripts and database. No data conversion or deletion is required.

## Docs

- [Architecture](docs/ARCHITECTURE.md)
- [Security](docs/SECURITY.md)
- [macOS setup](docs/SETUP_MACOS.md)
- [ChatGPT integration](docs/CHATGPT_INTEGRATION.md)
- [Approval model](docs/APPROVAL_MODEL.md)
- [Trusted Workspace Roots](docs/trusted-workspace-roots.md)
- [Troubleshooting](docs/TROUBLESHOOTING.md)
- [Extending adapters](docs/EXTENDING_ADAPTERS.md)
- [Migration and MCP contracts](docs/MIGRATION.md)
