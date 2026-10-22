# Architecture

```text
ChatGPT Web (OAuth and read-only tools previously verified; refresh needed for the write tool)
  → authenticated Streamable HTTP on 127.0.0.1
  → MCP handlers
  → workspace/task/approval services
  → repositories and bounded worker
  → Codex App Server JSONL over child-process stdio
  → Codex permission profile for the registered workspace
```

## Package boundaries

- `internal/domain`: task states, workspace/thread/approval entities, and invariants. No MCP, SQLite, or Codex imports.
- `internal/handler/mcp`: 20 MCP contracts, request/response mapping, authentication-wrapped HTTP handler, bounded input, and sanitized errors.
- `internal/service`: workspace ownership, authorization, task and approval rules. It depends on narrow repository, queue, gate, and audit interfaces.
- `internal/repository/sqlite`: `database/sql`, embedded migrations, SQLite transactions/constraints, retention, and audit persistence.
- `internal/codex`: child-process lifecycle, JSONL framing, request IDs, version checks, fixed App Server methods, and event mapping.
- `internal/worker`: bounded asynchronous queue, task contexts, cancellation, shutdown, restart behavior, and result persistence.
- `internal/security`: bearer auth, path identity checks, Codex permission profile, and best-effort secret redaction.
- `internal/app`: configuration, dependency wiring, loopback server, retention maintenance, signals, and graceful shutdown.

Audit records use an application-owned `audit.Event`; SQLite field types do not leak into services or handlers.

## Task flow

`codex_start_task` and `codex_resume_thread` create read-only tasks. `codex_start_write_task` requires local workspace write opt-in and creates a `waiting_approval` task plus an expiring approval in one SQLite transaction. MCP cannot approve it. `bridge-admin` approves the exact prompt/workspace/thread binding once; the worker atomically consumes the approval, clears the stored prompt, revalidates workspace identity and write policy, then queues the task. The adapter starts or resumes a bridge-owned thread with the fixed workspace-write profile and starts a turn asynchronously.

`codex_list_local_sessions` requires a local grant for global catalog access, default off. A session can be selected through an explicitly trusted workspace, a session attach approval, or an approved Trusted Workspace Root. The tool calls the installed App Server's `thread/list` with state-database-only metadata, CLI source filters, recent-activity ordering, and a bounded page size. Search matches the displayed name within each page, then preserves the App Server cursor for the next page; this supports both `thread.name` and preview-backed names. For each result, MCP returns `thread.name` as `displayName`; if absent, it returns a secret-redacted, single-line `thread.preview` capped at 160 characters, or `Untitled session` when both are empty. This preview comes only from authorized catalog metadata; Bridge does not read turns or raw conversation history. Full paths remain hidden. Sessions outside an approved workspace or active trusted root are omitted. Cursors bind to the original workspace/search filter. The current catalog requests `archived:false`, so archived threads are not listed; Bridge has no unarchive operation.

Workspace trust is stored on the approved workspace row and remains bound to its canonical path, device, and inode. Trusted roots are separately stored, approved only by the local owner, and revalidated against filesystem identity. When a session working directory is inside a repository, Bridge finds the nearest `.git` boundary, creates a logical project workspace only then, and binds the exact thread ID. A legacy session started directly from a trusted parent is ambiguous: `codex_list_session_projects` offers bounded root-relative repository choices, and `codex_request_session_project_binding` creates a pending owner-approval request. `bridge-admin codex-session project-binding approve` revalidates and persists the exact thread/repository/filesystem binding; the resulting workspace is read-only and scoped to that repository. Symlink escapes and changed/ambiguous boundaries fail closed. `codex_resume_thread` always takes explicit workspace and thread IDs. Workspace, root, or project-binding trust never enables writes: each write task still needs its own approval. `TaskService` and the worker revalidate the selected binding before execution.

The global catalog grant controls discovery only. Disabling it stops future `codex_list_local_sessions` calls. Workspace trust, per-session attach, and root trust remain separate permissions; root management is local-only and has no MCP tool. Disabling or revoking a root blocks discovery/resume granted through that root. Separately approved workspace trust or attach records remain independent.

The App Server's thread status is advisory for other Codex CLI processes. If it reports `active`, the catalog marks the session non-resumable and Bridge rejects resume. If another process still owns the thread while App Server says `notLoaded`, App Server itself can reject `thread/resume`; Bridge never kills or takes over that process.

Progress, terminal state, result, and diff are persisted. Cancellation interrupts a running turn or clears a pending write approval. Restart marks queued/running tasks interrupted and never replays them; an unconsumed approval can still be locally approved before expiry after restart.

The worker uses a task context independent of the initial MCP request. App Server `thread/start`, `thread/resume`, and `turn/start` calls have bounded request contexts independent of task cancellation; if cancellation races with `turn/start`, the adapter waits for the returned turn ID and interrupts it.

## Codex filesystem policy

The installed `codex-cli 0.163.0` accepts App Server `thread/start.config` permission profiles. Read-only tasks use `:root = deny`, `:minimal = read`, and one canonical workspace root with `.` read-only. Approved write tasks use the same root denial and sensitive-path rules, with `.` write only for that runtime workspace; `.git`, `.codex`, `/tmp`, and `$TMPDIR` remain read-only or denied. The exact resolved Codex executable receives a read grant so its local sandbox helper can start. Sandboxed command network access, Codex MCP servers, and web search are disabled. App Server response validation requires the expected profile, exactly the authorized runtime root, no extra writable roots, both temporary-directory exclusions, and network off.

The App Server initialize response identifies its version. Execution is rejected unless the version is at least `0.138.0`, the documented permission-profile minimum. The real local E2E test used App Server `0.163.0`: a random fixture marker was read, a sibling file returned `Operation not permitted`, and a workspace write probe returned `Operation not permitted`.

This verifies the installed Mac and tested path cases. It is not a proof against future Codex/macOS sandbox vulnerabilities. Tests must be rerun before trusting changed App Server sandbox behavior.

## Persistence and limits

Go defaults to `~/Library/Application Support/codex-chatgpt-bridge-go/bridge.sqlite`; it refuses to open a TypeScript MVP database. SQLite data is mode-restricted and remains plaintext. Prompt text is not persisted; the task stores its SHA-256. Results, diffs, progress, approvals, and audit metadata can contain project-derived information. Output expires 30 days after a task's last update by default, with startup and daily sweeps. Metadata and audit history remain.

MCP request bodies are bounded at 64 KiB, prompts at 20,000 UTF-8 characters, task output at 64 KiB, progress entries at 2 KiB, and retained progress at 100 entries. Concurrency is one active task and a bounded queue of 20.

## Verification state

- **Implemented:** all 20 MCP tools; SQLite migrations; local workspace, attach, catalog, root, and project-binding controls; write opt-in and one-time write-task approvals; bearer/Cloudflare Access auth; async task lifecycle; cancellation; read-only default and workspace-write Codex profiles; version gate; audit; admin CLI.
- **Session attach:** local discovery uses `thread/list` scoped to the approved canonical workspace and recognizes `source=cli`, `source=exec` with `originator=codex_exec`, and `source=vscode` with `originator=codex-tui`. Attach approval is local, one-time, and binds workspace identity and thread metadata. App Server reads use `includeTurns:false`.
- **Tested locally:** unit, repository, MCP contract/auth, worker/concurrency, and Codex JSONL protocol tests; real Codex workspace-write fixture E2E with local CLI approval.
- **Tested end-to-end locally:** MCP SDK client → Go HTTP/MCP handler → service → worker → actual Codex App Server `0.163.0` → result. Marker: `BRIDGE_MCP_CODEX_TURN_OK`.
- **Write E2E:** MCP write request → `waiting_approval` → local `bridge-admin` approval → real Codex edit/new file → App Server command evidence → MCP diff/result. A separate invocation of the same fixed permission profile denied sibling reads/writes, symlink escape, temp writes, and network.
- **Not tested:** Secure MCP Tunnel, Custom MCP Connector, ChatGPT Web, account eligibility, launchd.
- **Unsupported:** unrestricted shell/App Server passthrough, external privileged actions, and attaching non-CLI or unapproved threads.

The App Server is experimental. The bridge supports only the fixed methods in `internal/codex`; it does not expose generic JSON-RPC.
