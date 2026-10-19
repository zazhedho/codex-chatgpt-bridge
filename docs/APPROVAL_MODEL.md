# Approval model

## Current MVP

Workspace registration has an independent local approval step. Remote MCP callers can request registration and inspect pending items, but cannot approve them. The local admin CLI records the macOS username as approver:

```sh
go run ./cmd/bridge-admin workspace pending
go run ./cmd/bridge-admin workspace approve <request-id>
go run ./cmd/bridge-admin approval list
go run ./cmd/bridge-admin approval show <approval-id>
go run ./cmd/bridge-admin approval approve <approval-id>
go run ./cmd/bridge-admin approval reject <approval-id>
```

Operation approval metadata can bind a one-time decision to normalized parameters, an optional SHA-256 artifact hash, and an expiry. The MVP has no privileged runner, and approval metadata does not execute anything. MCP exposes only list/details for approvals.

## Before privileged execution

Future work must use a separately authenticated local approval channel and an isolated runner. Approval must bind workspace, actor, operation, target, environment, exact parameters, expiry, and SHA-256 of any executable artifact. The runner must verify the binding at use time, consume approval once, and receive no Codex credentials or inherited shell secrets.

ChatGPT natural-language confirmation and prompt filtering are not authorization boundaries. This MVP does not implement Terraform, cloud, Git push/merge, database, deployment, workspace-write, or other privileged actions.

See [security](SECURITY.md). No provider-specific adapter or privileged executor is enabled.
