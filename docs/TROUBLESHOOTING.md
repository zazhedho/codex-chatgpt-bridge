# Troubleshooting

## Startup rejects `BRIDGE_AUTH_TOKEN`

Set a local token of at least 32 bytes in the same shell:

```sh
export BRIDGE_AUTH_TOKEN="$(openssl rand -hex 32)"
make run
```

Do not print or paste it into ChatGPT. The bridge does not load `.env` files.

## MCP returns 401

Send `Authorization: Bearer <token>` to `/mcp` and `/healthz`. Verify the client and bridge use the same token. Tokens are never logged or returned.

## Codex App Server unavailable or task start is rejected

Check `codex --version`, `codex app-server --help`, and `CODEX_EXECUTABLE`. Sign in to Codex locally if needed. Task execution fails closed unless App Server reports a supported version (0.138.0 or newer) and accepts the fixed read-only permission profile. App Server is experimental; after upgrading Codex, run `make test-e2e` before enabling task execution.

## Workspace missing or rejected

Use `go run ./cmd/bridge-admin workspace request`, inspect `workspace pending`, then approve locally. Ensure bridge and admin CLI use the same `BRIDGE_DATA_DIR`. Paths must exist, be canonical, and pass symlink and identity checks. Register a project root, not the home directory.

## Task does not progress

Poll `codex_get_task` and `codex_get_progress`. The default is one active task and 20 queued tasks; task timeout is 20 minutes. Check structured bridge logs for the task ID and safe error classification. Prompts and full results are not logged. Mac sleep, network loss, App Server failure, or Codex exit can interrupt a task. Restart does not replay it.

## Tunnel or ChatGPT connector

Not configured. No tunnel client or connector has been run. Once separately approved, check tunnel readiness, workspace association/roles, local URL `http://127.0.0.1:8787/mcp`, and forwarded bearer auth. Follow official [Secure MCP Tunnel guidance](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels). ChatGPT availability depends on account and workspace controls.

## Reset or uninstall

Stop the process first. Review retained task and audit records before removing `BRIDGE_DATA_DIR`. Uninstall by removing only this project and its data directory after deciding whether to archive the data. No launchd job is installed.
