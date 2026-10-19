# ChatGPT integration

## Status

Not configured, not connected, and not tested from ChatGPT Web. Account and workspace eligibility cannot be inspected from this Mac. No tunnel-client was installed or run.

## Verified platform behavior

- OpenAI Secure MCP Tunnel uses an outbound-only `tunnel-client` connection to OpenAI and forwards MCP requests to a private local endpoint. It requires outbound HTTPS to `api.openai.com:443` and local reachability to the MCP server. It does not require an inbound public port. See [Secure MCP Tunnel](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels).
- Current `tunnel-client` configuration supports a local HTTP MCP URL and secret-backed `mcp.extra_headers`; `Authorization` can be provided to the local MCP origin using an environment or file reference. See the official [tunnel-client configuration](https://github.com/openai/tunnel-client/blob/master/docs/configuration.md). This bridge expects `Authorization: Bearer <BRIDGE_AUTH_TOKEN>`. The exact connector/tunnel behavior still needs validation before use.
- Custom MCP setup in ChatGPT is documented for web. Full tool access is subject to plan/workspace and admin controls; Pro availability is limited compared with Business, Enterprise, and Edu. Business connector publication is admin/owner controlled; Enterprise/Edu admins can control developer permissions. See [ChatGPT custom MCP help](https://help.openai.com/en/articles/12584461-connecting-custom-mcp-servers-in-chatgpt).
- Tunnel creation/edit and use require appropriate Platform organization and ChatGPT workspace association/roles. An eligible role generally needs tunnel Read/Manage to create and Read/Use to run or select. Exact account permissions have not been checked.

## Manual steps after approval

1. Confirm the intended ChatGPT plan, workspace, admin permission, Platform organization, and Tunnels Read/Manage/Use roles.
2. Download the official latest tunnel-client from Platform Tunnel settings or the [OpenAI tunnel-client releases](https://github.com/openai/tunnel-client/releases). Review its current `help quickstart` and configuration before running.
3. Create/select a tunnel and associate it with the target ChatGPT workspace. This creates an external OpenAI resource; do not do it until approved.
4. Run the bridge in the foreground with a local bearer token. Configure tunnel-client to reach `http://127.0.0.1:8787/mcp` and inject `Authorization: Bearer ...` from a local secret environment/file reference. Do not put the token in command arguments, YAML literals, ChatGPT, or logs. Confirm with `tunnel-client doctor` and its loopback health/readiness UI.
5. In ChatGPT Web, create a developer-mode custom MCP app, select Tunnel, select the tunnel, configure its supported authentication, and review the discovered tools. Use a dummy registered workspace first.
6. Test status, then start one harmless task and poll its result. Verify the returned task ID, progress, and result. Confirm cancellation before allowing broader use.

Do not assume step 4's static header will be the final ChatGPT auth contract: tunnel forwarded headers may override static headers, and ChatGPT/tunnel configuration must be tested together. Keep bridge bearer auth enabled. Do not paste secrets into this conversation.

## Not tested / unsupported here

- User account plan, workspace settings, role assignments, tunnel association, and connector availability.
- Tunnel creation, tunnel-client installation/run, MCP discovery through tunnel, or ChatGPT Web end-to-end task execution.
- ChatGPT mobile custom MCP access. Use web as the documented target for a future test.

References: [Secure MCP Tunnel](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels), [ChatGPT custom MCP servers](https://help.openai.com/en/articles/12584461-connecting-custom-mcp-servers-in-chatgpt), [tunnel-client configuration](https://github.com/openai/tunnel-client/blob/master/docs/configuration.md).
