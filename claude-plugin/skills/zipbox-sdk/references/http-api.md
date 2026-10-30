> Zipbox docs, markdown edition of https://zipbox.ai/docs/http-api
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# REST API

The control plane over plain HTTP - authentication, every route, GET /sandbox/:id and the box token, the error envelope and idempotency.

Everything the SDKs do is plain HTTPS and JSON. The control plane lives at `https://api.tribes.xyz`; each box serves its own [Box API](https://zipbox.ai/docs/box-api.md) at `https://box--<name>.zbox.sh`. The machine-readable contract for both is [/openapi.json](https://zipbox.ai/openapi.json).

## Authentication

Send your [API key](https://zipbox.ai/docs/api-keys.md) as a bearer token:

```sh
curl https://api.tribes.xyz/sandbox -H "Authorization: Bearer $ZIPBOX_API_KEY"
```

Requests that carry an `Origin` header are refused with `403`: keys are for servers, scripts and agents, never browsers. Each key may make 600 requests a minute; past that the answer is `429` with `Retry-After` and `X-RateLimit-Remaining`.

## Routes

| Method and path                 | Does                                                                         |
| ------------------------------- | ---------------------------------------------------------------------------- |
| `GET /account/whoami`           | `{userId, apiKey}` - who the key belongs to                                  |
| `GET /billing/summary`          | Credit balance and auto top-up state                                         |
| `GET /billing/usage`            | Recent metered usage                                                         |
| `GET /sandbox`                  | Your boxes                                                                   |
| `GET /sandbox/:id`              | One box, plus `ready`, `baseUrl` and `boxToken`                              |
| `POST /sandbox/identity/claim`  | Create a box: `{name?, tier?, sshPublicKey?, timeZone?, mailbox?, private?}` |
| `POST /sandbox/:id/pause`       | Stop: archive the disk; bills nothing while stopped                          |
| `POST /sandbox/:id/restore`     | Start a stopped box                                                          |
| `POST /sandbox/:id/reset`       | Restart: hard reboot, files survive                                          |
| `POST /sandbox/:id/tier`        | Resize: `{tier}`                                                             |
| `DELETE /sandbox/:id`           | Delete: `{archiveBeforeDestroy?}` (default keeps a 30-day recoverable copy)  |
| `GET /sandbox/:id/tailscale`    | The box's tailnet status: `{tailscale, privateMode}`                         |
| `PUT /sandbox/:id/tailscale`    | Join your tailnet: `{envelope, agentPublicKey}` (a sealed auth key) → `202`  |
| `DELETE /sandbox/:id/tailscale` | Leave the tailnet → `200`                                                    |
| `PUT /sandbox/:id/private`      | Private mode on or off: `{enabled}`                                          |

Three more routes exist for the dashboard and answer `403` to a key: `GET /account/apikeys`, `POST /account/apikeys` and `DELETE /account/apikeys/:id` (key management), and `POST /oauth/consent` (the [OAuth](https://zipbox.ai/docs/oauth.md) consent page).

## A box

`GET /sandbox` and `GET /sandbox/:id` return box rows. The documented fields:

| Field                                                                   | Meaning                                                                                                                                                                                                                                                |
| ----------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `id`                                                                    | The box id used in every route                                                                                                                                                                                                                         |
| `name`, `domain`                                                        | The box's name: `<name>.<domain>`, e.g. `demo.zbox.sh`, and its previews                                                                                                                                                                               |
| `status`                                                                | `provisioning`, `running`, `stopping` (a stop, delete or resize has been accepted and the box has not finished going down; never `ready`), `stopped`, `archived`, `deleting` (signing out of its tailnet before deletion; never billed) or `destroyed` |
| `tier`, `pendingTier`, `machineSize`                                    | Current size, a resize in flight, and the vCPU / memory / disk it runs at                                                                                                                                                                              |
| `isArchived`                                                            | The disk is in cold storage (stopped, or being stopped)                                                                                                                                                                                                |
| `ipv6`                                                                  | The box's public IPv6 address (SSH goes straight here, unless private)                                                                                                                                                                                 |
| `privateMode`                                                           | Only devices on your tailnet can reach the box ([Tailscale](https://zipbox.ai/docs/tailscale.md))                                                                                                                                                                          |
| `tailscale`                                                             | Its tailnet status, or null when it never joined ([Tailscale](https://zipbox.ai/docs/tailscale.md#read-the-status))                                                                                                                                                        |
| `agentPublicKey`                                                        | The box's own P-256 public key (SPKI DER, base64), to seal credentials to. Null until the box has started; re-read it right before sealing                                                                                                             |
| `cpuPct`, `memPct`, `diskPct`, `netRxBps`, `netTxBps`, `statsUpdatedAt` | The latest resource sample, the same one the dashboard shows                                                                                                                                                                                           |
| `startFailedAt`                                                         | Set when the last start failed                                                                                                                                                                                                                         |
| `claimedAt`, `createdAt`, `updatedAt`                                   | Timestamps                                                                                                                                                                                                                                             |

Other fields are internal and may change without notice.

## GET /sandbox/:id and the box token

`GET /sandbox/:id` adds four fields to the row:

- `ready` - the box is running and serving you, and accepts every live key's token. Only then can you call it.
- `httpsReady` - `baseUrl` completes a TLS handshake with a trusted certificate. A box never waits for its certificate to boot, so a freshly named or restarted box can be `ready` a few seconds to minutes before this turns true; until then a request fails in the TLS handshake, before anything is sent, and is safe to retry. The SDKs retry it for you.
- `baseUrl` - the box's HTTPS origin, e.g. `https://box--demo.zbox.sh`, or `https://box.demo.zbox.sh` while the box is [private](https://zipbox.ai/docs/tailscale.md#make-a-box-private). Null until `ready`. Previews live under the name, not this origin: `https://<label>.demo.zbox.sh`.
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
  "baseUrl": "https://box--demo.zbox.sh",
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

The answer is `{sandboxId, name, status, created, …}`. The box boots in the background; read `GET /sandbox/:id` (with backoff) until `ready` is true. `name` is the box's permanent subdomain: lower case letters, digits and hyphens. With no credit balance the answer is `402 insufficient_credits`. A name the account has never used, once it already holds 250, answers `403 machine_limit_reached`; claim one of its existing names instead (the name of a box you deleted boots again under the same subdomain, mailbox and wallets), see [Machine limit](https://zipbox.ai/docs/machine-limit.md).

A box is created without a mailbox. Its address `<name>@zbox.sh` is reserved with the name, and the mailbox itself is created the first time the box or its owner uses mail: the first `tribes-email` call from inside the box, the first time the box is opened in the dashboard, or the first `/sandbox/:id/email` request. Pass `"mailbox": true` to create it at claim time instead, for a box that must be able to receive mail before anything on it has asked. Either way the mailbox belongs to the name, not the box: reusing a name (`getOrCreate`) reuses its mailbox, so prefer one long-lived name per recurring job over a fresh box per run.

## Tailscale and private mode

The how-to, with samples in every language, is [Tailscale and private boxes](https://zipbox.ai/docs/tailscale.md). These routes go through the control plane, so they work for a private box from anywhere.

`GET /sandbox/:id/tailscale` answers `{tailscale, privateMode, supported}`. `tailscale` is `{state, tailnet, ipv4, ipv6, dnsName, keyExpiry, pending, peers}`, or null when the box never joined and no key is pending. `peers` is your other devices on the tailnet as the box sees them, online first and at most 50: `{name, dnsName, os, online, ipv4, connection}`, with `connection` `direct`, `relay` or null (no recent traffic). `supported` (also `tailscaleSupported` on every box row) is false when the box's ZipOS predates Tailscale, and null until the box has reported since it booted.

`PUT /sandbox/:id/tailscale` takes an auth key **sealed to the box** - never the key itself:

```json
{
  "envelope": { "ephemeralPublicKey": "…", "iv": "…", "ciphertext": "…" },
  "agentPublicKey": "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE…"
}
```

`agentPublicKey` is the value you sealed to, read from `GET /sandbox/:id` right before sealing ([how to seal](https://zipbox.ai/docs/tailscale.md#how-the-key-is-sealed)). The answer is `202 {pending: true}`; the box reports its state on its own heartbeat. A newer key replaces one that has not been delivered yet.

`DELETE /sandbox/:id/tailscale` logs the box out, removes it from your tailnet admin and wipes its Tailscale state (`200 {success}`). A private box is made public first, in the same call; `409 box_not_running` when the box is not running.

`PUT /sandbox/:id/private` with `{"enabled": true}` or `{"enabled": false}` answers `200 {privateMode}`. Pass `"private": true` to `POST /sandbox/identity/claim` to create a box that is never public.

| Status | Code                                    | When                                                                                  |
| ------ | --------------------------------------- | ------------------------------------------------------------------------------------- |
| 409    | `box_not_running`                       | `PUT …/tailscale` on a box that is not provisioning or running                        |
| 409    | `box_key_changed`                       | `agentPublicKey` is not the box's current key: read it again, re-seal, send once more |
| 409    | `tailscale_unavailable_egress_lockdown` | The box has company egress lockdown, which Tailscale cannot pass                      |
| 409    | `tailnet_not_connected`                 | `PUT …/private` `{enabled: true}` while the box is not connected with a tailnet IPv4  |
| 409    | `tailscale_requires_update`             | The box's ZipOS predates Tailscale or private mode: update it first                   |

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

[/openapi.json](https://zipbox.ai/openapi.json) is the OpenAPI 3.1 contract for the control plane **and** the box. Box routes are listed under a `/box` prefix with their own server (`https://{box}`); the prefix is a documentation device - on the wire, `/box/exec` is `https://box--<name>.zbox.sh/exec`. It is generated from the same schemas the control plane validates with, so a route cannot exist without appearing in it.
