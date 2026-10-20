# Cloudflare Access + ChatGPT MCP

Status: local Cloudflare JWT auth is implemented and tested. Keep account-specific tunnel IDs, hostnames, AUD tags, and identities out of this repository. Verify live Cloudflare and ChatGPT state separately before use; this runbook does not certify account billing or connector readiness.

## Cost and compatibility

- Cloudflare's Zero Trust Free plan is listed at $0 for up to 50 users. Cloudflare currently includes 1,000 tunnels per account; additional capacity is paid. Check the account usage page before creating anything. Do not select a paid capacity option.
- Cloudflare documents Managed OAuth for self-hosted and MCP Access applications. Its coding-agent documentation shows authorization code + PKCE S256, dynamic client registration, public-client token auth (`none`), refresh tokens, and RFC 8707 `resource`. The docs do not state plan-specific Managed OAuth billing or prove this account's eligibility. Confirm the feature is available without an upgrade in the account UI.
- ChatGPT custom MCP with full tool support requires a Business or Enterprise/Edu workspace. On Pro, custom servers are limited to read/fetch permissions. Workspace plan, policy, and the user's create permission are unknown. Confirm them before Gate C.
- A Cloudflare-managed domain/zone is required for the published hostname. Use only a dedicated subdomain such as `mcp.<YOUR_DOMAIN>`; leave root DNS records and nameservers alone.
- The protocol pieces look compatible on paper. Actual tenant metadata, callback URI acceptance, and ChatGPT OAuth behavior remain unverified until a later authorized ChatGPT connection test.

## Authentication behavior

`BRIDGE_AUTH_MODE` selects one exclusive mode:

- `bearer` (default): the current local `BRIDGE_AUTH_TOKEN` flow.
- `cloudflare_access`: only `Cf-Access-Jwt-Assertion` is accepted. There is no bearer fallback. The bridge verifies an RS256 JWT against the Cloudflare issuer's JWKS, checks issuer, audience, required expiration and not-before strictly (no clock-skew allowance), subject, and an exact allowed-email list. Keep the Mac clock synchronized. JWKS is fetched over HTTPS with a five-minute cache; unknown key IDs trigger refresh, and stale keys are not used after expiry if refresh fails.

Production issuer configuration must be the HTTPS Cloudflare Access team origin (`https://<team>.cloudflareaccess.com`), with no path or explicit port. The application audience must be copied from that Access application's AUD tag. Cloudflare documents that Managed OAuth access tokens are opaque and Access sends the signed JWT assertion to the origin.

Both modes keep the listener on loopback. Cloudflare identity does not approve a workspace: remote registration stays pending until the existing local admin workflow approves it. Workspace authorization and Codex's read-only sandbox remain unchanged.

The Go MCP SDK rejects a public `Host` sent to a localhost origin. Keep that DNS-rebinding control enabled: set the tunnel origin's HTTP Host header to `127.0.0.1:8787` (or the exact configured loopback host and port). The bridge checks that exact origin Host. Cloudflare must separately protect the public hostname with Access; the bridge never trusts `X-Forwarded-Host` or an unsigned identity header. Go's cross-origin protection also stays enabled.

## Local configuration

Use a dedicated, disposable `BRIDGE_DATA_DIR` and fixture workspace for first connection tests. Do not reuse the default application data or approve a private project for this gate.

```sh
export BRIDGE_AUTH_MODE=cloudflare_access
export BRIDGE_HOST=127.0.0.1
export BRIDGE_PORT=8787
export BRIDGE_CLOUDFLARE_ISSUER='https://<TEAM_NAME>.cloudflareaccess.com'
export BRIDGE_CLOUDFLARE_AUDIENCE='<ACCESS_APPLICATION_AUD_TAG>'
export BRIDGE_CLOUDFLARE_ALLOWED_EMAILS='<EXACT_ALLOWED_EMAIL>'
unset BRIDGE_AUTH_TOKEN
```

Set `BRIDGE_DATA_DIR` to an absolute path outside this repository. Use the Cloudflare team issuer and Access application's exact AUD tag from the dashboard. Never put credentials, tunnel tokens, or JWTs in this file or source control. Start the Go bridge in a foreground terminal. Its listener must remain `127.0.0.1:8787`.

## Named Tunnel and Access preparation

After confirming the free plan and domain in Cloudflare:

1. Inspect existing tunnels. Reuse only a dedicated tunnel with the intended organization and no unexpected users. Otherwise create one named `codex-chatgpt-bridge`; stop if the dashboard offers paid capacity or requires an upgrade.
2. Add only `mcp.<YOUR_DOMAIN>` as a published application route to `http://127.0.0.1:8787`. Do not edit the apex/root hostname.
3. Set the origin HTTP Host header to `127.0.0.1:8787`. Keep origin TLS verification enabled; this origin is local HTTP.
4. Create a self-hosted Access application scoped only to that hostname. Add an Allow policy for the one intended identity; do not add a broad email-domain, bypass, or public policy.
5. Enable Managed OAuth only if the dashboard confirms no plan upgrade or additional charge. Enable the origin Access JWT check with the same team and application AUD tag. Use the exact `Cf-Access-Jwt-Assertion` header; the Go bridge independently validates it.
6. For dynamic client registration, allow only the exact HTTPS redirect URI later shown in the ChatGPT connector UI. Do not guess the callback or enable a broad wildcard. This needs a separately authorized Gate C.
7. Protect the remotely managed tunnel token in a mode-0600 file outside the repository. On macOS, install the client from Cloudflare's official Homebrew method (`brew install cloudflared`) and verify `cloudflared --version`. Run it only in the foreground with `cloudflared tunnel run --token-file <PROTECTED_TOKEN_FILE>`; do not install a launch agent.

Cloudflare documents the `httpHostHeader` and origin Access `required`, `teamName`, and `audTag` settings in its [origin parameters reference](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/configure-tunnels/origin-parameters/). Cloudflare Access must inject and validate the signed assertion before proxying; the bridge rejects missing or invalid assertions regardless.

## Verification sequence

1. Run `go test ./...`, `go test -race ./...`, `go vet ./...`, and `make build` before any account change.
2. Start the bridge with isolated data, explicit Cloudflare mode, and only a harmless locally approved fixture workspace.
3. Check local unauthenticated `/mcp` and `/healthz` requests: they must fail. A bare `Authorization: Bearer ...` must also fail in Cloudflare mode.
4. Start `cloudflared` in the foreground. Check the named tunnel's healthy status in its dashboard. Keep the Cloudflare Access origin JWT requirement enabled.
5. From an unauthenticated client, confirm Access returns its managed OAuth challenge/metadata and does not reach an MCP tool. Inspect metadata only; never print tokens.
6. Check protected-resource metadata for the exact MCP resource URI and authorization server. Check authorization-server metadata for its endpoints, authorization-code and refresh grants, `S256`, DCR, and public-client token auth (`none`). ChatGPT sends the RFC 8707 `resource` parameter during authorization and token exchange; confirm it works in the actual OAuth flow.
7. If authorization-server metadata omits `authorization_response_iss_parameter_supported: true`, OpenAI documents a callback-ID-specific redirect: `https://chatgpt.com/connector/oauth/{callback_id}`. Get the exact callback URI from the ChatGPT connector UI. Add that exact URI to Cloudflare Managed OAuth's allowed redirect URIs; do not invent the ID or use a wildcard.
8. After authorization, add the custom MCP connection privately in ChatGPT. Use the exact MCP URL configured in Cloudflare, test OAuth and tool discovery from ChatGPT Web, and verify no write or privileged operation was enabled. Do not publish it to a workspace.

Local unit/integration tests use synthetic signing keys and a fake assertion verifier for MCP transport. They do not prove Cloudflare account eligibility, tunnel health, DNS, Managed OAuth, or ChatGPT end-to-end behavior.

## Stop and rollback

- Stop `cloudflared` and the bridge with Ctrl-C. No service or launchd item is installed.
- Do not delete shared tunnels, Access apps, DNS records, or data. If a dedicated test tunnel or Access app was created, disable it in Cloudflare before deleting it. Remove only the new `mcp` subdomain record if it was created for this test; never remove root records.
- Revoke the dedicated tunnel token if it was exposed. Rotate it from Cloudflare, update the protected local file, and do not paste either value into chat.
- To return to local bearer mode, set `BRIDGE_AUTH_MODE=bearer`, provide a fresh local `BRIDGE_AUTH_TOKEN` (at least 32 bytes), and leave the listener loopback-only. Do not configure both modes as a fallback.

## References

- [Cloudflare Tunnel setup](https://developers.cloudflare.com/tunnel/get-started/)
- [Cloudflare Managed OAuth](https://developers.cloudflare.com/cloudflare-one/access-controls/applications/http-apps/managed-oauth/)
- [Cloudflare Access JWT validation](https://developers.cloudflare.com/cloudflare-one/access-controls/applications/http-apps/authorization-cookie/validating-json/)
- [Cloudflare coding-agent OAuth metadata and resource flow](https://developers.cloudflare.com/cloudflare-one/access-controls/authenticate-agents/)
- [OpenAI MCP OAuth requirements](https://developers.openai.com/plugins/build/auth)
- [ChatGPT custom MCP availability and workspace roles](https://help.openai.com/en/articles/12584461-connecting-custom-mcp-servers-in-chatgpt)
