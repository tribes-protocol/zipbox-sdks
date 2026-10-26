> Zipbox docs, markdown edition of https://zipbox.ai/docs/api-keys
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# API keys

Account keys (zb_…) - create, delete, what a key can and cannot do, rate limits and OAuth rows.

An API key is an account credential. It does everything the dashboard can do on your boxes - create, start, stop, run commands, move files - and nothing that touches money or other credentials.

## Create a key

Open **Settings → API keys** on [zipbox.ai](https://zipbox.ai/settings) and press **Add API key** at the top right of the section (the **Docs** button next to it opens these pages in a new tab). Name the key and create it. The secret (`zb_` followed by 43 characters) is shown **once**; Zipbox keeps only its SHA-256, so a lost key cannot be recovered, only replaced.

```sh
export ZIPBOX_API_KEY=zb_...
```

Every SDK, the CLI and the stdio MCP server read `ZIPBOX_API_KEY` when no key is passed. You can hold up to **20 keys** per account; give each machine, CI system or agent its own so you can delete one without touching the others.

Keys live at the account level only. There is nothing key-related inside a box: the box's own **Settings → Provider keys** tab is for third-party provider keys (OpenAI, Anthropic and so on) and is unrelated.

## Delete a key

Press **Delete** next to the key in Settings. The key is gone for good: its row disappears from the list, it fails on the control plane at once, and within seconds every running box stops accepting that key's box tokens and closes the sockets they opened (stopped boxes learn it when they start). Apps connected through OAuth with that key are signed out. Other keys are untouched, and deleting one frees a slot under the 20-key limit. A command started with the key keeps running: the box is still yours, and deleting the box is the kill switch.

## What a key can and cannot do

A key can do everything on your own boxes: every route on the [REST API](https://zipbox.ai/docs/http-api.md) and the [Box API](https://zipbox.ai/docs/box-api.md), plus reading your balance and usage.

A key **cannot**:

- create, list or delete API keys,
- approve an OAuth client (connect an app),
- buy credits, open the billing portal, or change auto top-up,
- claim a signup grant,
- connect GitHub or write provider keys.

Those answer `403 forbidden`. Keys are not for browsers: a request carrying an `Origin` header is refused with `403`, so a key pasted into a web page cannot be used from it.

## How a key reaches a box

The key never travels to a box. `GET /sandbox/:id` called with your key returns `baseUrl` (the box's HTTPS origin) and `boxToken`, a token derived from your key and that one box. The box accepts it as a bearer. It never expires, it works only on that box, and it stops working when the key is deleted. The SDKs fetch it on first use, cache it, and re-fetch it once if a box answers `401`. See [Box API](https://zipbox.ai/docs/box-api.md#authentication).

## Rate limits

Each key may make **600 requests per minute** to the control plane. Past that the control plane answers `429` with `Retry-After` and `X-RateLimit-Remaining`; the SDKs wait and retry. Traffic to a box is not rate-limited, only concurrency-capped (32 live commands and 8 file transfers per box). Creating, starting and resizing a box keep the credit check: with no balance they answer `402 insufficient_credits`.

## OAuth rows

When you connect an app through OAuth (Claude, Codex, or any MCP client that registers itself), the consent mints a key of kind `oauth`, listed as `<client name> (OAuth)` - for example "Claude (OAuth)". Delete it to disconnect that app; its next call fails with `401` and it asks you to sign in again. Keys made by `zipbox login` are listed with kind `cli`. See [OAuth](https://zipbox.ai/docs/oauth.md).

## Check which key you are using

#### TypeScript

```ts
const me = await zb.account.whoami()
console.log(me.userId, me.apiKey) // a plain JSON object
```

#### Python

```python
me = zb.account.whoami()
print(me["userId"], me["apiKey"]["name"])  # a plain dict
```

#### Go

```go
me, err := c.Account.WhoAmI(ctx)
fmt.Println(me.UserID, me.APIKey.Name)
```

#### Rust

```rust
let me = zb.account().whoami().await?;
println!("{} {}", me["userId"], me["apiKey"]["name"]); // serde_json::Value
```

#### CLI

```sh
zipbox whoami
```

#### curl

```sh
curl https://api.tribes.xyz/account/whoami \
  -H "Authorization: Bearer $ZIPBOX_API_KEY"
```

## Secret scanning

Keys start with `zb_` so secret scanners can find them. If one leaks into a repository or a log, delete it in Settings and create a new one; nothing else needs to change.
