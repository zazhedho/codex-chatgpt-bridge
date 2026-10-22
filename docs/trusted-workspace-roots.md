# Trusted Workspace Roots

Trusted roots let an owner grant local, persistent read-only session discovery and resume within one chosen directory tree. ChatGPT cannot add, approve, disable, enable, or revoke roots.

## Terms

- **Project/repository**: the nearest directory containing a `.git` directory or a Git worktree `.git` pointer, found by walking upward from a Codex session's working directory.
- **Workspace**: the Bridge's permission boundary for a session. It is either one Git repository or one explicitly approved trusted root, bound to canonical path and filesystem identity.
- **Root-scoped session**: a Codex session whose original working directory is exactly a trusted root. It can read repositories under that one root, but cannot write within the root.
- **Project-scoped session**: a Codex session whose working directory is inside a Git repository or worktree. Its sandbox is limited to that repository.
- **Trusted root**: a directory explicitly added and approved by the owner through `bridge-admin`. It permits read-only metadata discovery and resume only within its own tree.
- **Codex session**: a thread returned by the Codex App Server `thread/list` API. The Bridge reads only metadata needed to identify and resume that thread. It does not bulk-read turns to build the catalog.

## Prerequisites

- Go and this repository's dependencies are available.
- The Go Bridge has a working local Codex App Server connection when tasks are run.
- You can run `bridge-admin` locally as the machine owner.
- The selected root exists and contains only projects you intend to make discoverable and resumable.
- You know the active Bridge data directory. `bridge-admin` must use the same `BRIDGE_DATA_DIR` as the running Bridge.

On macOS, both Bridge and `bridge-admin` default to:

```text
$HOME/Library/Application Support/codex-chatgpt-bridge-go
```

For any other supported environment, configure an absolute `BRIDGE_DATA_DIR` explicitly and use it for both processes. The database directory and files are permission-restricted by the application.

Run commands below from the repository root. Set the data directory once in the same Terminal session:

```sh
export BRIDGE_DATA_DIR="${BRIDGE_DATA_DIR:-$HOME/Library/Application Support/codex-chatgpt-bridge-go}"
```

If the running Bridge uses a custom directory, set `BRIDGE_DATA_DIR` to that exact directory instead.

## One-time setup

Global catalog access and root trust are separate local grants. Catalog access defaults to off. Check and enable it once:

```sh
go run ./cmd/bridge-admin codex-session catalog status
go run ./cmd/bridge-admin codex-session catalog enable
```

Add one root. The `add` command records a pending request; it does not activate trust:

```sh
go run ./cmd/bridge-admin trusted-root add "$HOME/Code/Project"
```

Copy the returned `requestId` into the local approval command:

```sh
go run ./cmd/bridge-admin trusted-root approve <REQUEST_ID>
```

The request expires after 30 minutes. Approval rechecks the canonical path and filesystem identity captured at registration. If the directory changed, add it again and inspect the new request.

Confirm the active root:

```sh
go run ./cmd/bridge-admin trusted-root list
go run ./cmd/bridge-admin trusted-root show <ROOT_ID>
go run ./cmd/bridge-admin trusted-root verify <ROOT_ID>
```

The ID returned by `trusted-root add` remains the root ID after approval. `verify` succeeds only while the root is approved and its path and filesystem identity still match.

## Multiple roots

Each root needs one local approval. Sessions started at that exact root receive root-scoped read-only access across repositories beneath it. Sessions started inside a repository remain project-scoped. Trust in one root does not include another root.

```sh
go run ./cmd/bridge-admin trusted-root add "<ROOT_A_PATH>"
go run ./cmd/bridge-admin trusted-root approve <ROOT_A_REQUEST_ID>

go run ./cmd/bridge-admin trusted-root add "<ROOT_B_PATH>"
go run ./cmd/bridge-admin trusted-root approve <ROOT_B_REQUEST_ID>

go run ./cmd/bridge-admin trusted-root list
```

These paths are examples only. Choose roots appropriate to your environment. Do not approve the home directory or another broad folder when a narrower parent is sufficient.

## Use sessions from ChatGPT

1. Call `codex_list_local_sessions` and choose an entry with `canResume: true`. `connectionStatus: trusted_root` means the original working directory is a trusted root and the session can read across repositories beneath it. `trusted` means it is repository-scoped.
2. Call `codex_resume_thread` with the entry's exact `workspaceId` and `threadId`. Root-scoped sessions do not need a project binding.
3. If an entry says `project_selection_required`, its working directory is neither the exact trusted root nor inside an identifiable Git repository. Use the optional project-binding workflow below only when you want to give that thread a narrower repository scope.
4. To switch sessions, resume again with the other entry's IDs. The Bridge has no global active-session pointer. Read task status, progress, result, or changes with the existing task tools.

An active thread is not resumable. The Bridge will not stop or take over another Codex CLI writer. A root-scoped session accepts read-only resumed work only; it cannot start a new unbound thread.

Root trust does not enable writes. The Bridge rejects write enablement and write tasks whose target is the entire trusted root. To modify one repository, use its repository-scoped workspace (or an optional project binding), enable that specific workspace locally, and approve each exact write task separately. The current write API does not accept a multi-repository target list. No commit or push occurs automatically.

## Root trust, session attach, and write approval

- **Trusted root** is a persistent local grant for metadata discovery and read-only resume within one root. A session at the root gets a root-scoped workspace; a session within a repository gets a repository-scoped workspace.
- **Session attach** is a separate, expiring local approval for one thread in an explicitly approved workspace. It remains the fallback for sessions outside trusted roots.
- **Project binding** is an optional, expiring local owner approval that narrows one legacy thread to one Git repository beneath its trusted root. It is not required for a session whose original working directory is exactly the trusted root. Binding grants no write permission.
- **Write approval** is a one-time approval for one exact task. The repository workspace must first be locally enabled for writes. Root trust and session attach never grant write permission.

## Command reference

All commands are local `bridge-admin` operations. No corresponding MCP tool is provided.

```text
bridge-admin trusted-root add <ROOT_PATH>
bridge-admin trusted-root approve <REQUEST_ID>
bridge-admin trusted-root list
bridge-admin trusted-root show <ROOT_ID>
bridge-admin trusted-root verify <ROOT_ID>
bridge-admin trusted-root disable <ROOT_ID>
bridge-admin trusted-root enable <ROOT_ID>
bridge-admin trusted-root revoke <ROOT_ID>
bridge-admin codex-session project-binding pending
bridge-admin codex-session project-binding show <REQUEST_ID>
bridge-admin codex-session project-binding approve <REQUEST_ID>
bridge-admin codex-session project-binding reject <REQUEST_ID>
bridge-admin codex-session project-binding list
bridge-admin codex-session project-binding revoke <THREAD_ID>
```

- `add`: register an absolute existing directory as pending. It does not approve it.
- `approve`: approve that pending request once. The ID is also the resulting root ID.
- `list`: show pending, approved, disabled, expired, and revoked entries.
- `show`: display the local root record.
- `verify`: recheck approval status, canonical path, and filesystem identity.
- `disable`: stop new catalog and resume access while preserving the record.
- `enable`: reactivate a disabled root after revalidation.
- `revoke`: permanently revoke that root record. To trust the same path later, add and approve it again.

Disable, enable, and revoke are local owner actions. A pending request can be revoked before approval. Revoked roots cannot be re-enabled.

## Project boundary rules

The Bridge canonicalizes the session working directory and verifies its filesystem identity under an approved root. If it is exactly the trusted root, the logical workspace is that root and the Codex read-only sandbox allows cross-repository references only within it. If the working directory is inside a Git repository, the Bridge walks upward to the nearest `.git` marker without running Git commands or following a `.git` pointer. This handles nested repositories and worktree `.git` files, and binds the logical workspace to that repository instead of its parent.

If a legacy session was started at the trusted root, no repository is inferred from its title: it resumes directly with root-scoped read-only permissions. If it was started at a different non-repository directory under the root, the catalog marks it `project_selection_required`. `codex_list_session_projects` lists bounded choices with root-relative paths only. A user may request an optional thread-to-repository binding; ChatGPT's selection alone does not grant it. The owner must approve the exact binding locally. Approval revalidates the thread metadata, trusted root, canonical paths, Git marker, and device/inode identities. Symlink escapes and changed repository identities fail closed.

The root and derived project workspace paths are revalidated against stored filesystem device/inode identity. A moved, replaced, remounted, or symlink-substituted path fails closed. For overlapping roots, the Bridge selects the most specific approved root that contains the full project boundary. A narrower root inside a repository cannot authorize that repository by itself; a separately approved enclosing root may authorize it. Existing workspaces retain their active root binding when possible.

## Environment variables

- `BRIDGE_DATA_DIR`: absolute path to the Bridge SQLite data directory. Set it for `bridge-admin` and the Bridge process to the same value.
- `CODEX_HOME`: optional Codex home path. The workspace path validator uses it to reject overlapping protected paths.
- `CODEX_EXECUTABLE`: optional Codex executable override; not needed for trusted-root management commands.

Do not copy credentials into this documentation or into the SQLite directory by hand.

## Disable, revoke, and recovery

Temporarily disable access:

```sh
go run ./cmd/bridge-admin trusted-root disable <ROOT_ID>
```

Re-enable after checking the path:

```sh
go run ./cmd/bridge-admin trusted-root enable <ROOT_ID>
go run ./cmd/bridge-admin trusted-root verify <ROOT_ID>
```

Permanently revoke:

```sh
go run ./cmd/bridge-admin trusted-root revoke <ROOT_ID>
```

The trust record persists across Bridge restarts because it is stored in SQLite. After reinstall, point `BRIDGE_DATA_DIR` to the same protected data directory. If that database is lost, add each root again and approve it locally. Do not delete or replace the Bridge database just to reset root trust; use the commands above.

Revocation prevents future discovery and resume through that root. It does not retroactively cancel a read-only turn already accepted by a worker; monitor or cancel active tasks through the existing task tools before revoking when immediate interruption is needed.

## Troubleshooting

- **`resource is not authorized`**: check `trusted-root list`, then `trusted-root verify <ROOT_ID>`. Ensure the session's working directory resolves under the approved root.
- **`project_selection_required`**: the session started at a non-root directory that is not inside a detectable Git repository. For narrower scope, list choices with `codex_list_session_projects`, request a binding, then approve it locally with `codex-session project-binding approve <REQUEST_ID>`. A session started exactly at a trusted root should instead show `connectionStatus: trusted_root` and `canResume: true`.
- **Binding request rejected or expired**: list pending requests and inspect with `codex-session project-binding show <REQUEST_ID>`. Requests expire after 15 minutes and are rejected if session metadata, root identity, project identity, or Git boundary changed. Request a new binding only after rechecking the selected repository.
- **`project_binding_revoked`**: the local owner revoked the thread binding. It cannot be resumed through that binding. A new explicit binding request and local approval are required.
- **`session_mapping_conflict`**: the thread ID is already bound to a different workspace/root. Do not move or overwrite its binding; select the originally associated workspace or use a separately approved attach flow.
- **`canResume: false` / active session**: another writer may own the session. Finish it in its current Codex process before resuming through the Bridge.
- **Trust disappears after restart**: verify the Bridge and CLI use the same `BRIDGE_DATA_DIR`.
- **Root verification fails after moving a directory**: the stored filesystem identity no longer matches. Revoke the stale root and add/approve the new canonical path.
- **Catalog access denied**: `codex-session catalog enable` is a separate local owner grant.
- **Writes remain denied**: root approval is not write permission. Root-scoped sessions cannot write across the root. Use a repository-scoped workspace, enable it locally, then approve each write task separately.

## Security considerations

- No root is trusted automatically. The initial request and approval happen through the local CLI.
- Root registration does not create an MCP administration capability. Remote callers cannot change root state.
- Trust is limited to read-only discovery/resume and never grants network access or root-wide writes.
- The effective sandbox root is the trusted root for root-scoped sessions and the Git repository for project-scoped sessions. The App Server adapter reapplies and validates the current permission profile on every resume.
- Root-scoped write is denied. Write approval remains separate and limited to an explicitly enabled repository workspace and exact task.
- A ChatGPT project selection is not local authorization. Only `bridge-admin codex-session project-binding approve` activates an optional binding; the request expires after 15 minutes and the binding is audited and revocable.
- Trusted roots expose session names and metadata from projects under those roots to the already authorized ChatGPT connector. Approve only directories whose project metadata you are willing to expose.
- A root can contain many projects; choose the narrowest useful parent. Protected home/Codex/credential paths remain rejected by workspace path policy.
- The `.git` marker is used only to identify a narrower project boundary. It is not executed, and its pointer is not followed for access.
- The catalog uses Codex thread metadata such as name and sanitized preview. It does not read raw conversation turns or credentials; preview text is redacted before it is returned.
