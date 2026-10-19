# Architecture

```text
MCP client (ChatGPT not connected)
  → authenticated Streamable HTTP on 127.0.0.1
  → MCP handlers
  → workspace/task/approval services
  → repositories and bounded worker
  → Codex App Server JSONL over child-process stdio
  → Codex permission profile for the registered workspace
```

## Package boundaries

- `internal/domain`: task states, workspace/thread/approval entities, and invariants. No MCP, SQLite, or Codex imports.
- `internal/handler/mcp`: 15 MCP contracts, request/response mapping, authentication-wrapped HTTP handler, bounded input, and sanitized errors.
- `internal/service`: workspace ownership, authorization, task and approval rules. It depends on narrow repository, queue, gate, and audit interfaces.
- `internal/repository/sqlite`: `database/sql`, embedded migrations, SQLite transactions/constraints, retention, and audit persistence.
- `internal/codex`: child-process lifecycle, JSONL framing, request IDs, version checks, fixed App Server methods, and event mapping.
- `internal/worker`: bounded asynchronous queue, task contexts, cancellation, shutdown, restart behavior, and result persistence.
- `internal/security`: bearer auth, path identity checks, Codex permission profile, and best-effort secret redaction.
- `internal/app`: configuration, dependency wiring, loopback server, retention maintenance, signals, and graceful shutdown.

Audit records use an application-owned `audit.Event`; SQLite field types do not leak into services or handlers.

## Task flow

`codex_start_task` and `codex_resume_thread` authorize the registered workspace, verify that the local App Server supports the required permission-profile version, save a queued task, and return its ID. The bounded worker creates or resumes a bridge-owned thread and starts a turn asynchronously. Progress, terminal state, result, and diff are persisted. Cancellation interrupts the active turn. Restart marks queued/running tasks interrupted; it never replays them.

The worker uses a task context independent of the initial MCP request. App Server `thread/start`, `thread/resume`, and `turn/start` calls have bounded request contexts independent of task cancellation; if cancellation races with `turn/start`, the adapter waits for the returned turn ID and interrupts it.

## Codex filesystem policy

The installed `codex-cli 0.163.0` accepts App Server `thread/start.config` permission profiles. The bridge selects a fixed profile with `:root = deny`, `:minimal = read`, one canonical workspace root with `.` read-only, sensitive-path deny rules, network disabled for sandboxed commands, no Codex MCP servers, and web search disabled. It does not combine this profile with `turn/start.sandboxPolicy` because Codex documents those controls as mutually exclusive.

The App Server initialize response identifies its version. Execution is rejected unless the version is at least `0.138.0`, the documented permission-profile minimum. The real local E2E test used App Server `0.163.0`: a random fixture marker was read, a sibling file returned `Operation not permitted`, and a workspace write probe returned `Operation not permitted`.

This verifies the installed Mac and tested path cases. It is not a proof against future Codex/macOS sandbox vulnerabilities. Tests must be rerun before trusting changed App Server sandbox behavior.

## Persistence and limits

Go defaults to `~/Library/Application Support/codex-chatgpt-bridge-go/bridge.sqlite`; it refuses to open a TypeScript MVP database. SQLite data is mode-restricted and remains plaintext. Prompt text is not persisted; the task stores its SHA-256. Results, diffs, progress, approvals, and audit metadata can contain project-derived information. Output expires 30 days after a task's last update by default, with startup and daily sweeps. Metadata and audit history remain.

MCP request bodies are bounded at 64 KiB, prompts at 20,000 UTF-8 characters, task output at 64 KiB, progress entries at 2 KiB, and retained progress at 100 entries. Concurrency is one active task and a bounded queue of 20.

## Verification state

- **Implemented:** all 15 MCP tools; SQLite migrations; local workspace approval; bearer auth; async task lifecycle; cancellation; read-only Codex permission profile; version gate; audit; admin CLI.
- **Tested locally:** unit, repository, MCP contract/auth, worker/concurrency, and Codex JSONL protocol tests.
- **Tested end-to-end locally:** MCP SDK client → Go HTTP/MCP handler → service → worker → actual Codex App Server `0.163.0` → result. Marker: `BRIDGE_MCP_CODEX_TURN_OK`.
- **Not tested:** Secure MCP Tunnel, Custom MCP Connector, ChatGPT Web, account eligibility, launchd.
- **Unsupported:** workspace writes, unrestricted shell/App Server passthrough, external privileged actions, arbitrary existing CLI thread adoption.

The App Server is experimental. The bridge supports only the fixed methods in `internal/codex`; it does not expose generic JSON-RPC.
