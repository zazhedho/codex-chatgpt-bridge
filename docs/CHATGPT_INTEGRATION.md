# ChatGPT Integration Readiness

Status as of 2026-10-19: **not connected**. No tunnel, connector, account, workspace, or launchd configuration was inspected or changed. The `tunnel-client` binary is not installed on this Mac. Follow this runbook only after explicitly authorizing external setup.

## Platform requirements

- Custom MCP and full MCP support are available in ChatGPT Business and Enterprise/Edu on the web. Pro supports read/fetch only, which is insufficient for this bridge's task creation and cancellation tools. Mobile clients are not supported for this custom MCP flow.
- In Business, only workspace admins and owners can create and publish. In Enterprise/Edu, admins and owners can create and test; admins can grant selected members create permission through RBAC. Only admins and owners can publish. Keep this connector private and unpublished during testing.
- Creating or editing a tunnel needs Platform `Tunnels Read + Manage`. Running `tunnel-client` and selecting the tunnel needs `Tunnels Read + Use`. The tunnel must be associated with the Platform organization and target ChatGPT workspace. Platform access and ChatGPT workspace access are separate.
- The Mac needs outbound HTTPS to `api.openai.com:443` and local access to `127.0.0.1:8787`. No inbound public listener is needed.

These requirements are from the current [Secure MCP Tunnel guide](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels) and [ChatGPT custom MCP help](https://help.openai.com/en/articles/12584461-connecting-custom-mcp-servers-in-chatgpt). This machine's account and workspace eligibility have **not** been checked.

## Authentication design

The bridge requires a bearer token for every request to `/mcp` and `/healthz`. Configure `tunnel-client` to inject that token locally for both runtime MCP traffic and MCP discovery/probes. Configure the ChatGPT connector as **No authentication**; do not configure OAuth for this bridge.

This avoids forwarding a ChatGPT OAuth bearer to the bridge. The tunnel client documentation says connector-forwarded headers override static `mcp.extra_headers` case-insensitively. It also supports separate `mcp.discovery_extra_headers`, whose values can reference a local secret file. If OAuth is selected and ChatGPT forwards `Authorization`, that value replaces the bridge token and bridge authentication will reject it. The documented precedence is verified; the ChatGPT no-auth forwarding behavior has not been tested end to end.

The tunnel runtime API key authenticates `tunnel-client` to OpenAI. It is separate from the bridge bearer token. Keep both out of chat, command arguments, checked-in profiles, and logs. Store runtime keys in the local process environment or a supported secret reference; store the bridge token in a mode-0600 local file referenced by the profile.

Example profile shape only; replace placeholders locally, never in ChatGPT:

```yaml
config_version: 1
control_plane:
  base_url: https://api.openai.com
  tunnel_id: <tunnel-id>
  api_key: env:CONTROL_PLANE_API_KEY
mcp:
  server_urls:
    - channel: main
      url: http://127.0.0.1:8787/mcp
  extra_headers:
    Authorization: file:/Users/<local-user>/.config/codex-chatgpt-bridge/bridge-token
  discovery_extra_headers:
    Authorization: file:/Users/<local-user>/.config/codex-chatgpt-bridge/bridge-token
```

Use the currently supported official `tunnel-client` release and verify its config schema with `tunnel-client help quickstart` before applying this profile. Header precedence and secret references are documented in [tunnel-client configuration](https://github.com/openai/tunnel-client/blob/master/docs/configuration.md) and [connector behavior](https://github.com/openai/tunnel-client/blob/master/docs/connectors.md).

## Limits and bridge behavior

- Secure MCP Tunnel allows a maximum 10 MiB serialized request/response and a 120-second absolute request deadline. Queue time consumes the deadline; progress messages do not reset it.
- The bridge caps each MCP tool output at 1 MiB, request bodies at 64 KiB, prompts at 20 KiB, and task results/diffs at 64 KiB. An oversized tool response returns a safe error. Workspace and approval lists do not paginate; very large lists fail at the output cap.
- Tasks are asynchronous: `codex_start_task` accepts and queues work, then returns a task ID. Poll status/progress and read results with separate calls. The worker permits one active task and up to 20 queued tasks. Task execution has a 20-minute limit, but no individual MCP request waits for the full task duration.
- The local MCP request deadline fit is based on code limits and local tests, not on an actual ChatGPT tunnel call. Do not treat the tunnel's 120-second deadline as a task runtime limit.

## Manual setup and verification

Do not perform these steps until external setup is explicitly authorized. Keep the bridge foreground-only; no launchd service is part of this runbook.

1. **Confirm access.** In the intended ChatGPT workspace, confirm a supported Business or Enterprise/Edu plan and your create/test role. In the intended Platform organization, confirm `Tunnels Read + Manage` if creating/editing a tunnel and `Tunnels Read + Use` for running/selecting it. Confirm target workspace association is allowed.
2. **Prepare a disposable workspace.** Locally create and approve a fixture workspace with only a harmless marker file. Do not register a production project for initial verification. Do not call remote `workspace_register` to approve itself; approval stays local through `bridge-admin`.
3. **Prepare local auth.** In a local zsh terminal, generate a bridge token into a private file, then export the same value for the foreground bridge. These commands do not print the token; do not enable shell tracing:

   ```sh
   umask 077
   mkdir -p "$HOME/.config/codex-chatgpt-bridge"
   chmod 700 "$HOME/.config/codex-chatgpt-bridge"
   openssl rand -hex 32 | tr -d '\n' > "$HOME/.config/codex-chatgpt-bridge/bridge-token"
   chmod 600 "$HOME/.config/codex-chatgpt-bridge/bridge-token"
   export BRIDGE_AUTH_TOKEN="$(cat "$HOME/.config/codex-chatgpt-bridge/bridge-token")"
   ```

   Keep the MCP profile's file reference pointed at this file. Do not paste either token into ChatGPT or logs.
4. **Start and test the bridge locally.** Run the documented foreground `make run` command. Use MCP Inspector or an equivalent local MCP client against `http://127.0.0.1:8787/mcp` with `Authorization: Bearer <token>`. Verify discovery, `bridge_get_status`, `bridge_get_capabilities`, and `workspace_list`. Verify requests without a token receive 401. Never port-forward the listener.
5. **Create or reuse a tunnel.** In Platform tunnel settings, create or select an authorized tunnel; associate it with the intended Platform organization and ChatGPT workspace. Create a restricted runtime key for `tunnel-client`. Enter it only into a local secure environment input. Do not include it in this repository or chat.
6. **Install and validate the official client.** Download `tunnel-client` from Platform tunnel settings or the official OpenAI release. Check `tunnel-client help quickstart`, save the profile under the user's config directory with mode `0600`, then set the runtime API key through a hidden local prompt and environment reference:

   ```zsh
   read -rs "CONTROL_PLANE_API_KEY?Tunnel runtime key: "
   export CONTROL_PLANE_API_KEY
   tunnel-client doctor --profile <profile> --explain
   ```

   Correct errors before proceeding. Enter the runtime key only in this local terminal; never place it in the profile as a literal.
7. **Start the tunnel manually.** In a second foreground terminal, re-enter the runtime key through the hidden prompt, then run `tunnel-client run --profile <profile>`. Confirm its local health/readiness UI or endpoint reports ready and connected. Keep raw HTTP logging disabled. Do not expose its local admin UI.
8. **Create a private ChatGPT connection.** In ChatGPT web: Plugins → `+` → Add custom MCP server → Connection: Tunnel. Select the same tunnel. Set Authentication to **No authentication**, review the warnings, and create a private/unpublished app for testing. Do not publish it to a workspace.
9. **Verify end to end.** Confirm all 15 expected tools are discovered. In ChatGPT, call `bridge_get_status`, `bridge_get_capabilities`, and `workspace_list`. Then start one harmless read-only task against the approved disposable fixture, poll it using separate status/progress calls, and read the result. Confirm the expected marker, no fixture changes, and a corresponding local audit record. The task should be a real Codex App Server run, not a mock. Keep each MCP call short; never wait for full task completion inside `codex_start_task`.
10. **Verify rejection paths.** Confirm a tool call with no bridge bearer is rejected at the local bridge, and confirm workspace approval still requires the local admin command. Do not test privileged actions; none are implemented.

Tunnel limits and setup steps are documented in the [Secure MCP Tunnel guide](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels). ChatGPT's current UI and role rules are documented in [Connecting custom MCP servers](https://help.openai.com/en/articles/12584461-connecting-custom-mcp-servers-in-chatgpt).

## Rollback

1. Cancel any in-flight disposable task from ChatGPT or the local admin workflow; confirm it reaches a terminal state.
2. Stop `tunnel-client` with Ctrl-C. This ends outbound polling and request forwarding.
3. Stop the Go bridge with Ctrl-C. Do not delete its SQLite directory or the original TypeScript data.
4. In ChatGPT, remove the private test app/connector. Do not publish it.
5. In Platform settings, revoke the runtime key and delete the tunnel only if those resources were created for this test and deletion is explicitly authorized. Otherwise leave existing shared resources unchanged.
6. Remove the local bridge-token file and temporary tunnel profile only after the processes are stopped and no longer need them. Preserve the Go and TypeScript databases unless separately authorized to delete them.
7. Revoke/rotate any token that may have been exposed. Do not rely on connector removal alone to revoke a leaked credential.

## Readiness evidence and limits

- Local environment: `codex-cli 0.163.0`; `tunnel-client` is absent. Go App Server schema generation confirmed the installed CLI accepts a `config` field on both thread start and resume. The code supplies the fixed read-only permission profile to both operations.
- The baseline reports a real new-thread E2E run against a harmless fixture; it was not rerun in this gate. The resumed-thread sandbox effect has not been independently exercised.
- No Secure MCP Tunnel, ChatGPT connector, account role, workspace association, forwarded header, launchd service, or ChatGPT Web call has been tested or activated.
