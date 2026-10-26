> Zipbox docs, markdown edition of https://zipbox.ai/docs/oauth
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# OAuth

For people building an MCP client - the authorization server's endpoints, dynamic registration, PKCE, what consent grants and how to remove it.

`mcp.zipbox.ai` is an OAuth 2.1 authorization server for the hosted [MCP server](https://zipbox.ai/docs/mcp.md). Any client that does dynamic client registration can connect; nothing is pre-registered or allowlisted.

## Endpoints

| Endpoint                                                       | Purpose                                             |
| -------------------------------------------------------------- | --------------------------------------------------- |
| `https://mcp.zipbox.ai/.well-known/oauth-authorization-server` | Authorization server metadata (RFC 8414)            |
| `https://mcp.zipbox.ai/.well-known/oauth-protected-resource`   | Protected resource metadata (MCP auth spec 2025-06) |
| `https://mcp.zipbox.ai/register`                               | Dynamic client registration (RFC 7591)              |
| `https://mcp.zipbox.ai/authorize`                              | Authorization endpoint                              |
| `https://mcp.zipbox.ai/token`                                  | Token endpoint (authorization code, refresh)        |

## The flow

1. **Discover.** An unauthenticated MCP request answers `401` with a `WWW-Authenticate` header pointing at the protected-resource metadata; that names the authorization server.
2. **Register.** `POST /register` with your `client_name` and exact `redirect_uris`. Keep the `client_id`.
3. **Authorize.** Send the user to `/authorize` with `response_type=code`, your `client_id`, `redirect_uri`, `state`, and a PKCE `code_challenge` with `code_challenge_method=S256`. **PKCE is required.**
4. **Consent.** The user signs in to Zipbox on `zipbox.ai/oauth/authorize`, sees your client's name and what it gets (full access to their boxes until they delete the key in Settings), and approves.
5. **Exchange.** The user returns to your `redirect_uri` with a `code`; `POST /token` with the `code` and your `code_verifier`. You get an access token and a refresh token.
6. **Call.** Send `Authorization: Bearer <access token>` on every MCP request. Refresh tokens rotate on use and are issued with a 30-day lifetime; when one lapses, run the flow again. The API key behind the grant never expires.

Redirect URIs must match exactly. The consent hand-off between zipbox.ai and the authorization server is single-use and expires after 60 seconds.

## What consent grants

Approving mints an [API key](https://zipbox.ai/docs/api-keys.md) of kind `oauth`, named `<client_name> (OAuth)` (or "OAuth client" when the client registered no name), and the MCP server acts with it. It can do everything an API key can on the user's boxes - and nothing a key cannot: no key management, no billing, no approving other apps. OAuth tokens themselves live with the authorization server and never in Zipbox's database.

## Remove access

The user deletes the app's row in **Settings → API keys**. The app's next MCP call fails with `401`, which makes a well-behaved client start the flow again. Other connected apps keep working.

## Without OAuth

Clients that cannot do OAuth send an API key instead: `Authorization: Bearer zb_…` on the same URL.
