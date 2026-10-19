# Security

## Trust boundaries

MCP is an authenticated remote-facing control surface, even while the bridge currently binds only to loopback. The bearer token authorizes one local principal across all approved workspaces. It is not per-person identity. Local workspace approval is separate from MCP authentication.

Codex receives task text and can read source inside an approved workspace. Treat repository files, tool output, model responses, and task results as untrusted. Instructions in a repository cannot change bridge authorization or sandbox configuration.

## Enforced controls

- **Network listener:** numeric loopback only; MCP and health routes require bearer auth. Tokens need at least 32 bytes and are compared in constant time. Host/origin protection, bounded request bodies, and HTTP timeouts are enabled.
- **Workspace:** explicit registration and local approval; absolute canonical paths, symlink checks, stable device/inode checks, and revalidation on each workspace lookup. Tasks, threads, and results are bound to one workspace.
- **Codex profile:** fixed App Server permission profile with filesystem root denied, runtime-minimal reads, one approved workspace root read-only, sensitive-file denials, sandboxed command network disabled, Codex MCP servers disabled, and web search disabled. Profile support is version-gated; unsupported or missing App Server version fails closed. No caller-supplied sandbox settings are accepted.
- **App Server surface:** only `thread/start`, `thread/resume`, `turn/start`, and `turn/interrupt` are callable by the adapter. No generic RPC, `command/exec`, `process/spawn`, or shell tool is exposed through MCP. Server-initiated approval requests are declined.
- **Persistence:** Go uses a separate SQLite directory, mode-restricted files, explicit migrations, prepared SQL, foreign keys, transactions, and busy timeout. Prompts are stored only as SHA-256; task outputs remain plaintext locally and expire 30 days after task update by default.
- **Operations:** bounded prompt/body/output/progress sizes; one active worker and bounded queue; task cancellation and shutdown propagation; restart marks active work interrupted and never replays it.
- **Approvals:** remote callers may request workspace registration and inspect approval metadata. Only the local admin CLI can approve registrations or operation metadata. No privileged executor is connected.

The real local E2E test on Codex App Server `0.163.0` read a random marker from an approved fixture, attempted the sibling file and received `Operation not permitted`, and attempted a workspace write that also received `Operation not permitted`.

## Limits and residual risk

- The sandbox is enforced by Codex/macOS permission profiles. The E2E proves the tested runtime and probes, not that every future Codex/macOS version or kernel path is free of bypasses. App Server is experimental. Re-run the E2E after Codex updates before trusting task execution.
- `:minimal` exposes runtime paths needed by tools outside the project. The profile denies other filesystem roots, but source and tool output inside the registered project remain readable and may contain sensitive code.
- The Codex process needs its local sign-in to contact OpenAI. This is separate from sandboxed subprocess network policy; prompts and readable source are sent to the configured Codex service. The bridge does not pass unrelated environment secrets to the child process.
- Read-only filesystem permissions do not prevent confidentiality loss through model context or an App Server/runtime vulnerability. Never register production secrets or credential-heavy directories unless that exposure is acceptable.
- Secret redaction is heuristic and not a security boundary. Do not put credentials in prompts. MCP output, SQLite results, progress, diffs, and approval parameters may contain project information.
- SQLite is plaintext. Protect and back up `BRIDGE_DATA_DIR` accordingly. Expiry removes task output but preserves task metadata and audit rows.
- Anyone with the bearer token can access every approved workspace while the bridge is available. Keep the listener on loopback and the token local.
- Tunnel, ChatGPT connector, account eligibility, forwarded auth headers, and ChatGPT Web are not tested or configured. The tunnel must not be treated as a replacement for bridge auth.
- No workspace write, production operation, cloud action, privileged runner, launchd service, or public listener is enabled.

## Workspace rules

Register the smallest project root required. Do not register `$HOME`, `~/.ssh`, `~/.aws`, `~/.kube`, Codex home, or production credential stores. The Codex profile denies known sensitive paths inside a workspace, including environment files, keys, credentials, Terraform state, `.ssh`, `.aws`, `.kube`, and selected Git credential/config paths. These rules are defense in depth, not a reason to store secrets in a project root.

## Safe operating steps

1. Keep `BRIDGE_HOST` at `127.0.0.1`; never port-forward or reverse-proxy the listener publicly.
2. Keep `BRIDGE_AUTH_TOKEN` in a local shell or secure local store. Do not pass it in prompts, MCP responses, screenshots, command-line arguments, or source control.
3. Approve workspace requests locally after checking canonical path and contents.
4. Start with a disposable workspace and inspect task results before sharing them.
5. Review output retention and SQLite access before using confidential repositories.
6. Keep Secure MCP Tunnel and ChatGPT connector inactive until their account permissions and header forwarding are validated.

See [approval model](APPROVAL_MODEL.md) for why natural-language confirmation cannot authorize privileged execution.
