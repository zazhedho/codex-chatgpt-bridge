# codex-chatgpt-bridge

Local Go MCP bridge between ChatGPT-compatible clients and Codex App Server on macOS. It listens on loopback only. Codex tasks run asynchronously in a verified read-only workspace permission profile.

## Status

- **Implemented:** 15 MCP tools, bearer authentication, explicit/local-approved workspaces, SQLite task and audit storage, async tasks, progress, cancellation, thread resume, and a fixed Codex App Server JSONL client.
- **Tested locally:** 52 Go tests, race detector, vet, build, authenticated MCP initialization and `tools/list`, and 401 rejection for unauthenticated/invalid-token GET and POST requests.
- **Sandbox gate:** new, same-process resume, and post-restart turns passed through the real Codex App Server using a deterministic local provider stub. The stub is not a real-model behavior test.
- **Gate B local prep:** official `tunnel-client` 0.0.16 installed for macOS arm64; a protected profile and token files live outside the repository. A disposable fixture workspace is locally approved.
- **Not verified:** Platform organization/workspace, existing tunnel resources, tunnel roles, runtime API key, `doctor`, tunnel readiness/polling, or ChatGPT Web. Platform settings returned 403 in this session; no tunnel or key was created.
- **Not activated:** tunnel-client runtime, ChatGPT connector, launchd, public listener, workspace writes, or privileged operations.
- **Unsupported:** workspace writes, arbitrary shell/RPC, external/production privileged actions, arbitrary existing CLI session adoption.

See [migration results](docs/MIGRATION.md), [architecture](docs/ARCHITECTURE.md), and [security](docs/SECURITY.md).

## Prerequisites

- macOS and Go `1.27` or later.
- Codex CLI with App Server support and a working local sign-in. This Mac was tested with `codex-cli 0.163.0`.
- `openssl` for a local bearer token.

Node.js is not required to build or run Go. The original TypeScript source, tests, and SQLite data remain in place for rollback; Go uses a separate data directory by default.

## Build and test

```sh
cd /Users/zaqiakhana/Code/Project/codex-chatgpt-bridge
make build
make test
make test-race
make vet
```

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

## Test MCP locally

With the bridge running, use MCP Inspector against `http://127.0.0.1:8787/mcp` using Streamable HTTP and `Authorization: Bearer <local token>`. `/healthz` is also loopback-only and authenticated.

The MCP surface has 15 tools:

- Workspace: `workspace_list`, `workspace_get`, `workspace_register`.
- Codex: `codex_list_threads`, `codex_start_task`, `codex_resume_thread`, `codex_get_task`, `codex_get_progress`, `codex_get_result`, `codex_get_changes`, `codex_cancel_task`.
- Approvals and bridge: `approval_list_pending`, `approval_get_details`, `bridge_get_status`, `bridge_get_capabilities`.

Call `codex_start_task` to get a `taskId`; poll task/progress; read result or changes after completion. Workspaces must be locally approved. Task execution fails closed if App Server does not advertise a version that supports Codex permission profiles.

## ChatGPT integration

Not connected. The bridge has no public listener. `tunnel-client` is installed, but no Platform tunnel or runtime key is configured. The prepared profile injects the bridge bearer from a protected local file for both MCP requests and discovery. Connector-forwarded `Authorization` overrides static headers; no-auth connector forwarding remains unverified. See the [ChatGPT integration runbook](docs/CHATGPT_INTEGRATION.md). No connector was created.

## Data and rollback

Go stores task metadata, progress, results, diffs, approvals, and audit records in SQLite under `BRIDGE_DATA_DIR`; outputs are local plaintext and expire after 30 days by default. `BRIDGE_OUTPUT_RETENTION_DAYS=0` disables expiry. TypeScript data is not imported or modified.

Rollback: stop the Go bridge and use the retained TypeScript implementation with its original `npm` scripts and database. No data conversion or deletion is required.

## Docs

- [Architecture](docs/ARCHITECTURE.md)
- [Security](docs/SECURITY.md)
- [macOS setup](docs/SETUP_MACOS.md)
- [ChatGPT integration](docs/CHATGPT_INTEGRATION.md)
- [Approval model](docs/APPROVAL_MODEL.md)
- [Troubleshooting](docs/TROUBLESHOOTING.md)
- [Extending adapters](docs/EXTENDING_ADAPTERS.md)
- [Migration and MCP contracts](docs/MIGRATION.md)
