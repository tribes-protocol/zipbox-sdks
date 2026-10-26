> Zipbox docs, markdown edition of https://zipbox.ai/docs/http-api
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# REST API

The control plane over plain HTTP - authentication, every route, GET /sandbox/:id and the box token, the error envelope and idempotency.

Everything the SDKs do is plain HTTPS and JSON. The control plane lives at `https://api.tribes.xyz`; each box serves its own [Box API](https://zipbox.ai/docs/box-api.md) at `https://<name>.zbox.sh`. The machine-readable contract for both is [/openapi.json](https://zipbox.ai/openapi.json).

## Authentication

Send your [API key](https://zipbox.ai/docs/api-keys.md) as a bearer token:

```sh
curl https://api.tribes.xyz/sandbox -H "Authorization: Bearer $ZIPBOX_API_KEY"
```

Requests that carry an `Origin` header are refused with `403`: keys are for servers, scripts and agents, never browsers. Each key may make 600 requests a minute; past that the answer is `429` with `Retry-After` and `X-RateLimit-Remaining`.

## Routes

| Method and path                | Does                                                                        |
| ------------------------------ | --------------------------------------------------------------------------- |
| `GET /account/whoami`          | `{userId, apiKey}` - who the key belongs to                                 |
| `GET /billing/summary`         | Credit balance and auto top-up state                                        |
| `GET /billing/usage`           | Recent metered usage                                                        |
| `GET /sandbox`                 | Your boxes                                                                  |
| `GET /sandbox/:id`             | One box, plus `ready`, `baseUrl` and `boxToken`                             |
| `POST /sandbox/identity/claim` | Create a box: `{name?, tier?, sshPublicKey?, timeZone?}`                    |
| `POST /sandbox/:id/pause`      | Stop: archive the disk; bills nothing while stopped                         |
| `POST /sandbox/:id/restore`    | Start a stopped box                                                         |
| `POST /sandbox/:id/reset`      | Restart: hard reboot, files survive                                         |
| `POST /sandbox/:id/tier`       | Resize: `{tier}`                                                            |
| `DELETE /sandbox/:id`          | Delete: `{archiveBeforeDestroy?}` (default keeps a 30-day recoverable copy) |

Three more routes exist for the dashboard and answer `403` to a key: `GET /account/apikeys`, `POST /account/apikeys` and `DELETE /account/apikeys/:id` (key management), and `POST /oauth/consent` (the [OAuth](https://zipbox.ai/docs/oauth.md) consent page).

## A box

`GET /sandbox` and `GET /sandbox/:id` return box rows. The documented fields:

| Field                                                                   | Meaning                                                                     |
| ----------------------------------------------------------------------- | --------------------------------------------------------------------------- |
| `id`                                                                    | The box id used in every route                                              |
| `name`, `domain`                                                        | The box answers at `<name>.<domain>`, e.g. `demo.zbox.sh`                   |
| `status`                                                                | `provisioning`, `running`, `stopping`, `stopped`, `archived` or `destroyed` |
| `tier`, `pendingTier`, `machineSize`                                    | Current size, a resize in flight, and the vCPU / memory / disk it runs at   |
| `isArchived`                                                            | The disk is in cold storage (stopped, or being stopped)                     |
| `ipv6`                                                                  | The box's public IPv6 address (SSH goes straight here)                      |
| `cpuPct`, `memPct`, `diskPct`, `netRxBps`, `netTxBps`, `statsUpdatedAt` | The latest resource sample, the same one the dashboard shows                |
| `startFailedAt`                                                         | Set when the last start failed                                              |
| `claimedAt`, `createdAt`, `updatedAt`                                   | Timestamps                                                                  |

Other fields are internal and may change without notice.

## GET /sandbox/:id and the box token

`GET /sandbox/:id` adds four fields to the row:

- `ready` - the box is running and serving you, and accepts every live key's token. Only then can you call it.
- `httpsReady` - `baseUrl` completes a TLS handshake with a trusted certificate. A box never waits for its certificate to boot, so a freshly named or restarted box can be `ready` a few seconds to minutes before this turns true; until then a request fails in the TLS handshake, before anything is sent, and is safe to retry. The SDKs retry it for you.
- `baseUrl` - the box's HTTPS origin, e.g. `https://demo.zbox.sh`. Null until `ready`.
- `boxToken` - the bearer the box accepts for **this key on this box**. Null until `ready`, and always null when the dashboard calls the route.

```sh
curl https://api.tribes.xyz/sandbox/$ID \
  -H "Authorization: Bearer $ZIPBOX_API_KEY"
```

```json
{
  "id": "4f9c…",
  "name": "demo",
  "status": "running",
  "tier": "s",
  "ready": true,
  "baseUrl": "https://demo.zbox.sh",
  "boxToken": "3b1d…"
}
```

The token is the same every time you ask (it is derived from your key and the box, never stored), has no expiry, and is accepted by no other box. Deleting the key kills every token derived from it. If the box answers `401`, fetch this route again and retry once. If this route answers but the box still says `401`, the key is live and the box has not received its token yet (a key created moments ago reaches running boxes within seconds): retry shortly. A deleted key fails this route itself.

## Create a box

```sh
curl -X POST https://api.tribes.xyz/sandbox/identity/claim \
  -H "Authorization: Bearer $ZIPBOX_API_KEY" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"name":"demo","tier":"s"}'
```

The answer is `{sandboxId, name, status, created, …}`. The box boots in the background; read `GET /sandbox/:id` (with backoff) until `ready` is true. `name` is the box's permanent subdomain: lower case letters, digits and hyphens. With no credit balance the answer is `402 insufficient_credits`.

## Idempotency

`POST /sandbox/identity/claim` accepts an `Idempotency-Key` header. Replaying the same key within 24 hours returns the original answer instead of creating a second box, so a create that timed out on your side is always safe to retry. The SDKs send one automatically.

## Error envelope

Every error from the control plane to an API key has one shape:

```json
{
  "error": {
    "code": "insufficient_credits",
    "message": "Your balance is at or below $0 — top up to boot a machine.",
    "requestId": "req_…",
    "retryable": false,
    "retryAfterMs": null,
    "docsUrl": "https://zipbox.ai/docs/errors#insufficient_credits"
  }
}
```

`code` is the contract; `message` is for people and may change. Retry only when `retryable` is true, after `retryAfterMs` (or `Retry-After`) when present. Every code, its cause and its fix is on [Errors and limits](https://zipbox.ai/docs/errors.md).

## Account

```sh
# who the key belongs to
curl https://api.tribes.xyz/account/whoami \
  -H "Authorization: Bearer $ZIPBOX_API_KEY"
# balance
curl https://api.tribes.xyz/billing/summary \
  -H "Authorization: Bearer $ZIPBOX_API_KEY"
# usage
curl https://api.tribes.xyz/billing/usage \
  -H "Authorization: Bearer $ZIPBOX_API_KEY"
```

Billing reads are allowed; buying credits, the billing portal and auto top-up changes are dashboard-only.

## OpenAPI

[/openapi.json](https://zipbox.ai/openapi.json) is the OpenAPI 3.1 contract for the control plane **and** the box. Box routes are listed under a `/box` prefix with their own server (`https://{box}`); the prefix is a documentation device - on the wire, `/box/exec` is `https://<name>.zbox.sh/exec`. It is generated from the same schemas the control plane validates with, so a route cannot exist without appearing in it.
