# macOS setup

Foreground development setup. This does not install launchd.

## Requirements

- macOS on a supported Go architecture; this host uses Go `1.27.2` on `darwin/arm64`.
- Codex CLI with App Server support and a working local sign-in; tested here with `0.163.0`.
- `openssl` for a local bearer token.

Check the installed tools:

```sh
go version
codex --version
codex app-server --help
```

## Build and run

```sh
cd /Users/zaqiakhana/Code/Project/codex-chatgpt-bridge
export BRIDGE_AUTH_TOKEN="$(openssl rand -hex 32)"
export BRIDGE_DATA_DIR="$HOME/Library/Application Support/codex-chatgpt-bridge-go"
mkdir -p "$BRIDGE_DATA_DIR"
make build
make run
```

The server listens on `127.0.0.1:8787`. The token is never printed. Configuration uses environment variables; no `.env` file is read. Optional settings: `BRIDGE_HOST` (must remain a loopback IP), `BRIDGE_PORT`, `CODEX_EXECUTABLE`, `CODEX_HOME`, and `BRIDGE_OUTPUT_RETENTION_DAYS` (default 30; `0` disables output expiry).

For a direct binary run after `make build`, keep the same environment and run `./bin/bridge`. Stop with Ctrl-C or SIGTERM. Shutdown cancels workers and does not replay work after restart.

## Register a workspace

Use a second terminal with the same `BRIDGE_DATA_DIR`:

```sh
export BRIDGE_DATA_DIR="$HOME/Library/Application Support/codex-chatgpt-bridge-go"
go run ./cmd/bridge-admin workspace request backend-go "$HOME/Code/backend"
go run ./cmd/bridge-admin workspace pending
go run ./cmd/bridge-admin workspace approve <request-id>
go run ./cmd/bridge-admin workspace list
```

Inspect the canonical path before approval. The request expires after 30 minutes. Registration grants read-only task access to that root subject to Codex's permission profile. Do not register `$HOME`, credential directories, or production secrets.

## Enable and approve workspace writes

Write access remains disabled until a local admin opts in. Enable only a workspace reviewed for source contents and secrets:

```sh
go run ./cmd/bridge-admin workspace enable-write <workspace-id>
```

In ChatGPT, request an implementation with `codex_start_write_task`. The tool returns `waiting_approval`, `taskId`, and `approvalId`; no Codex turn starts yet. Review and approve that exact request locally:

```sh
go run ./cmd/bridge-admin approval show <approval-id>
go run ./cmd/bridge-admin approval approve <approval-id>
```

Then ChatGPT polls `codex_get_task` / `codex_get_progress` and reviews `codex_get_result` / `codex_get_changes`. A revision is a new task with a new local approval. Disable write after the work:

```sh
go run ./cmd/bridge-admin workspace disable-write <workspace-id>
```

Write approval expires after five minutes and is single-use. The bridge keeps the prompt in the local expiring approval row until consume/reject/expiry/cancel. Read-only workspaces are not upgraded automatically.

## Test and stop

```sh
make test
make test-race
make vet
make test-e2e
make test-write-e2e
make test-session-catalog-e2e
```

`make test-e2e` exercises the read-only fixture. `make test-write-e2e` invokes the signed-in local Codex App Server, creates a disposable writable fixture, and tests one local approval plus sandbox boundaries. `make test-session-catalog-e2e` creates two temporary `codex exec` sessions in separate fixture repositories, then tests metadata-only discovery and explicit resume switching. It uses a temporary Codex home linked to the existing ChatGPT authentication file and deletes the temporary thread data when the test ends. These opt-in tests may use the locally signed-in Codex account. `/healthz` and `/mcp` require the configured authentication mode and remain loopback-only.

The bridge requires the Mac to be awake and online. Sleep pauses local execution and breaks tunnel connectivity; an offline Mac cannot receive calls. Restart marks active work interrupted and does not rerun it. SQLite, workspace registrations, and Codex thread metadata remain on disk.

## launchd

Not installed or prepared. Persistent service setup is deferred until its service configuration and secret storage are reviewed and approved.
