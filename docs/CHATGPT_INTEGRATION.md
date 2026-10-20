# ChatGPT Integration Readiness

This document records the earlier OpenAI Secure MCP Tunnel attempt and its static-bearer design. The current Cloudflare Access + Managed OAuth path is documented in [CLOUDFLARE_MCP_INTEGRATION.md](CLOUDFLARE_MCP_INTEGRATION.md); the two auth modes are exclusive.

Status as of 2026-10-19: **blocked before Platform setup**. OpenAI Platform settings returned HTTP 403 in this session, and no Platform credentials were available locally. Therefore the intended organization/workspace, existing tunnels, roles, and billing context remain unknown; no tunnel or runtime key was created. No ChatGPT connector or launchd service was created.

The official `tunnel-client` 0.0.16 is installed on macOS arm64 through the OpenAI Homebrew tap. Homebrew verified the official formula SHA-256. The CLI help and `sample_mcp_remote_no_auth` were reviewed. An isolated profile is prepared outside the repository, but it has no tunnel ID or runtime key. Local bridge auth and MCP initialize/tool discovery were tested separately.

## Platform requirements

- ChatGPT custom MCP access depends on the target workspace plan, developer-mode policy, and user role. This account/workspace was not inspected, so eligibility is **unknown**. Ask the workspace admin to confirm private custom MCP testing is allowed.
- Creating/editing a tunnel needs Platform `Tunnels Read + Manage`. Running `tunnel-client` and selecting a tunnel needs `Tunnels Read + Use`. These are separate from ChatGPT workspace permissions. Associate only the intended Platform organization and target ChatGPT workspace.
- The Mac needs outbound HTTPS to `api.openai.com:443` and local access to `127.0.0.1:8787`. No inbound public listener is needed.

These requirements are from the current [Secure MCP Tunnel guide](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels) and [ChatGPT custom MCP help](https://help.openai.com/en/articles/12584461-connecting-custom-mcp-servers-in-chatgpt). This machine's account and workspace eligibility have **not** been checked.

## Authentication design

The bridge requires a bearer token for every MCP request. The prepared tunnel profile points both `mcp.extra_headers.Authorization` and `mcp.discovery_extra_headers.Authorization` to a mode-0600 file whose complete value is `Bearer <bridge-token>`. The control-plane runtime key is a separate mode-0600 file; it is never the bridge bearer.

This avoids forwarding a ChatGPT OAuth bearer to the bridge. The current tunnel-client documentation says connector-forwarded headers override static MCP headers case-insensitively; discovery headers override runtime headers for discovery, and forwarded connector headers still win. Therefore the planned future connector must use **No authentication**. That no-auth forwarding behavior has not been tested because no connector was created.

The tunnel runtime API key authenticates `tunnel-client` to OpenAI. It is separate from the bridge bearer token. Keep both out of chat, command arguments, checked-in profiles, and logs. The prepared profile uses `file:` references for both secrets.

Example profile shape only; replace placeholders locally, never in ChatGPT:

```yaml
config_version: 1
control_plane:
  base_url: https://api.openai.com
  tunnel_id: <tunnel-id>
  api_key: file:/path/outside/repository/runtime-api-key
mcp:
  server_urls:
    - channel: main
      url: http://127.0.0.1:8787/mcp
  extra_headers:
    Authorization: file:/path/outside/repository/bridge-authorization-header
  discovery_extra_headers:
    Authorization: file:/path/outside/repository/bridge-authorization-header
```

The bridge-authorization-header file contains the full header value `Bearer <bridge-token>` with no newline. Current `tunnel-client` accepts `env:` and `file:` references for MCP headers and control-plane keys. Connector-forwarded `Authorization` takes precedence over local static headers. See [tunnel-client configuration](https://github.com/openai/tunnel-client/blob/master/docs/configuration.md) and [connector behavior](https://github.com/openai/tunnel-client/blob/master/docs/connectors.md).

## Limits and bridge behavior

- Secure MCP Tunnel allows a maximum 10 MiB serialized request/response and a 120-second absolute request deadline. Queue time consumes the deadline; progress messages do not reset it.
- The bridge caps each MCP tool output at 1 MiB, request bodies at 64 KiB, prompts at 20 KiB, and task results/diffs at 64 KiB. An oversized tool response returns a safe error. Workspace and approval lists do not paginate; very large lists fail at the output cap.
- Tasks are asynchronous: `codex_start_task` accepts and queues work, then returns a task ID. Poll status/progress and read results with separate calls. The worker permits one active task and up to 20 queued tasks. Task execution has a 20-minute limit, but no individual MCP request waits for the full task duration.
- The local MCP request deadline fit is based on code limits and local tests, not on an actual ChatGPT tunnel call. Do not treat the tunnel's 120-second deadline as a task runtime limit.

## Manual setup and verification

Gate B setup is authorized, but account UI access is required for Platform resource checks. Keep both processes foreground-only. **Stop before the ChatGPT connector steps; those belong to a later Gate C.**

1. **Inspect Platform manually.** Open `https://platform.openai.com/settings/organization/tunnels` in a signed-in browser. Select the intended organization, inspect existing tunnels, and confirm the target ChatGPT workspace. This session received HTTP 403 and cannot report whether a suitable tunnel exists. Do not guess or use another organization.
2. **Confirm roles and billing.** Confirm the tunnel manager has `Tunnels Read + Manage` and the runtime-key principal has `Tunnels Read + Use`. If the UI shows unexpected paid resources or charges, stop. Current docs establish these permissions, but this session could not verify account roles or pricing.
3. **Create or reuse a private tunnel.** Prefer a suitable existing tunnel. Otherwise, create `codex-chatgpt-bridge` in the intended organization and associate only the intended ChatGPT workspace for later private testing. Record the non-secret tunnel ID locally. Do not create a ChatGPT connector.
4. **Create a restricted runtime key.** Use the Platform Runtime API Keys page, not an admin key. Grant only Tunnels Read + Use. Save the key locally to the protected `runtime-api-key` file referenced by the prepared profile. Do not paste it into chat or a command argument.
5. **Prepare a disposable workspace and local auth.** The current Gate B fixture is already created and locally approved under an isolated data directory. To create a fresh one, use a harmless marker only. Generate a new bridge token and a separate header-value file; do not enable shell tracing:

   ```sh
   umask 077
   GATE_B_DIR="$HOME/Library/Application Support/codex-chatgpt-bridge-gate-b"
   umask 077
   mkdir -p "$GATE_B_DIR/secrets"
   openssl rand -hex 32 | tr -d '\n' > "$GATE_B_DIR/secrets/bridge-token"
   { printf 'Bearer '; cat "$GATE_B_DIR/secrets/bridge-token"; } > "$GATE_B_DIR/secrets/bridge-authorization-header"
   chmod 600 "$GATE_B_DIR/secrets/bridge-token" "$GATE_B_DIR/secrets/bridge-authorization-header"
   export BRIDGE_AUTH_TOKEN="$(cat "$GATE_B_DIR/secrets/bridge-token")"
   ```

   Keep the MCP profile's file reference pointed at this file. Do not paste either token into ChatGPT or logs.
6. **Start and verify the local bridge.** Use the isolated `BRIDGE_DATA_DIR` and bind `BRIDGE_HOST=127.0.0.1`, `BRIDGE_PORT=8787`. Verify missing/invalid credentials return 401 and an authenticated MCP client completes initialize and `tools/list`. Never port-forward this listener.
7. **Install/profile the official client.** The supported macOS install path is `brew install openai/tools/tunnel-client`; this Mac has v0.0.16. Review `tunnel-client help quickstart`, `tunnel-client help samples`, `tunnel-client --help`, and `profiles samples show sample_mcp_remote_no_auth` before changing configuration. The prepared profile is named `codex-bridge-local` under the isolated profile directory. Its control-plane key uses `file:`, and both MCP Authorization headers use the protected full-header file.

   After setting the tunnel ID and saving the restricted runtime key locally, run:

   ```zsh
   TUNNEL_CLIENT_PROFILE_DIR="$GATE_B_DIR/profile-store" tunnel-client doctor --profile codex-bridge-local --explain
   ```

   Fix doctor errors; do not weaken bridge authentication. If it passes, start `tunnel-client run --profile codex-bridge-local` in the foreground. Read the URL from the profile's health URL file, then check `/healthz`, `/readyz`, and `/ui` on loopback. Use `tunnel-client health --url-file <path> --require-control-plane-poll --json` to require poll evidence. Stop with Ctrl-C. Do not enable the remote UI.

8. **Future Gate C only — connector.** Do not create or activate the ChatGPT connector during Gate B. Later testing requires separate authorization and verification that ChatGPT “No authentication” does not override the local static Authorization header.

OpenAI's current limits include a 120-second absolute MCP request deadline and 10 MiB serialized request/response cap. Tunnel setup and role requirements are in the [Secure MCP Tunnel guide](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels) and the official [tunnel-client onboarding](https://github.com/openai/tunnel-client/blob/master/docs/onboarding.md).

## Rollback

1. Cancel any in-flight disposable task from ChatGPT or the local admin workflow; confirm it reaches a terminal state.
2. Stop `tunnel-client` with Ctrl-C. This ends outbound polling and request forwarding; this Gate B session left it stopped because no tunnel/key were available.
3. Stop the Go bridge with Ctrl-C. Do not delete its SQLite directory or the original TypeScript data.
4. In ChatGPT, remove the private test app/connector. Do not publish it.
5. In Platform settings, revoke the runtime key and delete the tunnel only if those resources were created for this test and deletion is explicitly authorized. Otherwise leave existing/shared resources unchanged.
6. Remove the local bridge-token file and temporary tunnel profile only after the processes are stopped and no longer need them. Preserve the Go and TypeScript databases unless separately authorized to delete them.
7. Revoke/rotate any token that may have been exposed. Do not rely on connector removal alone to revoke a leaked credential.

## Readiness evidence and limits

- Host: macOS 27.0.1 arm64, Go 1.27.2, Codex CLI 0.163.0, tunnel-client 0.0.16. Bridge build passed.
- Local isolated bridge: loopback `127.0.0.1:8787`; missing/invalid GET and POST auth returned 401; authenticated MCP initialize and `tools/list` returned 15 tools. One disposable workspace is approved locally.
- The isolated profile was accepted by `tunnel-client profiles add`; it references a protected runtime-key file and complete `Bearer ...` header file for both runtime and discovery.
- Platform settings were not inspectable: unauthenticated API request returned 401; Platform settings returned 403 in curl and the browser session. Existing tunnel inventory, organization/workspace, roles, runtime key, and price context remain **unknown**.
- No `doctor`, tunnel `run`, health/readiness, outbound polling, connector, or ChatGPT Web call was performed. No tunnel resource was created.
