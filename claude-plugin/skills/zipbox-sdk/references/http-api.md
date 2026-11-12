> Zipbox docs, markdown edition of https://zipbox.ai/docs/http-api
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# REST API

The control plane over plain HTTP - authentication, every route, GET /sandbox/:id and the box token, waiting on a box, the error envelope and idempotency.

Everything the SDKs do is plain HTTPS and JSON. The control plane lives at `https://api.tribes.xyz`; each box serves its own [Box API](https://zipbox.ai/docs/box-api.md) at `https://box--<name>.zbox.sh`. The machine-readable contract for both is [/openapi.json](https://zipbox.ai/openapi.json).

## Authentication

Send your [API key](https://zipbox.ai/docs/api-keys.md) as a bearer token:

```sh
curl https://api.tribes.xyz/sandbox -H "Authorization: Bearer $ZIPBOX_API_KEY"
```

Requests that carry an `Origin` header are refused with `403`: keys are for servers, scripts and agents, never browsers. Each key may make 600 requests a minute; past that the answer is `429` with `Retry-After` and `X-RateLimit-Remaining`.

## Routes

| Method and path                       | Does                                                                                           |
| ------------------------------------- | ---------------------------------------------------------------------------------------------- |
| `GET /account/whoami`                 | `{userId, apiKey}` - who the key belongs to                                                    |
| `GET /billing/summary`                | Credit balance and auto top-up state                                                           |
| `GET /billing/usage`                  | Recent metered usage                                                                           |
| `GET /sandbox`                        | Your boxes                                                                                     |
| `GET /sandbox/:id`                    | One box, plus `ready`, `baseUrl` and `boxToken`                                                |
| `GET /sandbox/:id/wait`               | `?until=ready\|paused`: answers the box once it gets there ([Waiting](#waiting-for-a-box))     |
| `POST /sandbox/identity/claim`        | Create a box: `{name?, tier?, sshPublicKey?, timeZone?, mailbox?, private?}`                   |
| `POST /sandbox/:id/pause`             | Pause: the box bills nothing while paused → `200` paused, or `202` on its way                  |
| `POST /sandbox/:id/resume`            | Resume a paused box → `200` ready, or `202` on its way (`/restore` is the same route)          |
| `POST /sandbox/:id/reset`             | Restart: hard reboot, files survive                                                            |
| `POST /sandbox/:id/transition/cancel` | Cancel the pause, resume, restart, resize or fork in flight; `409` past the point of no return |
| `POST /sandbox/:id/fork`              | Fork a box: `{name, sourcePolicy}` with an `Idempotency-Key` → `202` ([Fork](https://zipbox.ai/docs/fork.md))      |
| `GET /sandbox/:id/fork`               | The fork making this (new) box: phase, source recovery, waits, error                           |
| `POST /sandbox/:id/tier`              | Resize: `{tier}`                                                                               |
| `DELETE /sandbox/:id`                 | Delete: `{archiveBeforeDestroy?}` (default keeps a 30-day recoverable copy)                    |
| `GET /sandbox/:id/tailscale`          | The box's tailnet status: `{tailscale, privateMode}`                                           |
| `PUT /sandbox/:id/tailscale`          | Join your tailnet: `{envelope, agentPublicKey}` (a sealed auth key) → `202`                    |
| `DELETE /sandbox/:id/tailscale`       | Leave the tailnet → `200`                                                                      |
| `PUT /sandbox/:id/private`            | Private mode on or off: `{enabled}`                                                            |

Three more routes exist for the dashboard and answer `403` to a key: `GET /account/apikeys`, `POST /account/apikeys` and `DELETE /account/apikeys/:id` (key management), and `POST /oauth/consent` (the [OAuth](https://zipbox.ai/docs/oauth.md) consent page).

## A box

`GET /sandbox` and `GET /sandbox/:id` return box rows. The documented fields:

| Field                                                                   | Meaning                                                                                                                                                                                                                                                                                                                   |
| ----------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `id`                                                                    | The box id used in every route                                                                                                                                                                                                                                                                                            |
| `name`, `domain`                                                        | The box's name: `<name>.<domain>`, e.g. `demo.zbox.sh`, and its previews                                                                                                                                                                                                                                                  |
| `status`                                                                | `provisioning`, `running`, `stopping` (a pause, delete or resize has been accepted and the box has not finished going down; never `ready`), `paused` (not billed; never `ready`) or `deleting` (signing out of its tailnet before deletion; never billed). See [Status, phase and readiness](#status-phase-and-readiness) |
| `phase`                                                                 | What the box is doing now, read from every lifecycle field at once (below)                                                                                                                                                                                                                                                |
| `allowedActions`                                                        | The lifecycle verbs the box accepts in its current state (below)                                                                                                                                                                                                                                                          |
| `tier`, `pendingTier`, `machineSize`                                    | Current size, a resize in flight, and the vCPU / memory / disk it runs at                                                                                                                                                                                                                                                 |
| `isArchived`                                                            | A backup of the disk is in cold storage                                                                                                                                                                                                                                                                                   |
| `ipv6`                                                                  | The box's public IPv6 address (SSH goes straight here, unless private)                                                                                                                                                                                                                                                    |
| `privateMode`                                                           | Only devices on your tailnet can reach the box ([Tailscale](https://zipbox.ai/docs/tailscale.md))                                                                                                                                                                                                                                             |
| `tailscale`                                                             | Its tailnet status, or null when it never joined ([Tailscale](https://zipbox.ai/docs/tailscale.md#read-the-status))                                                                                                                                                                                                                           |
| `agentPublicKey`                                                        | The box's own P-256 public key (SPKI DER, base64), to seal credentials to. Null until the box has started; re-read it right before sealing                                                                                                                                                                                |
| `cpuPct`, `memPct`, `diskPct`, `netRxBps`, `netTxBps`, `statsUpdatedAt` | The latest resource sample, the same one the dashboard shows                                                                                                                                                                                                                                                              |
| `startFailedAt`                                                         | Set when the last start (a create, a resume, a restart) failed                                                                                                                                                                                                                                                            |
| `fork`                                                                  | The fork still making this box ([Fork](https://zipbox.ai/docs/fork.md#rest-reference)); null on every box that is not being forked into                                                                                                                                                                                                       |
| `claimedAt`, `createdAt`, `updatedAt`                                   | Timestamps                                                                                                                                                                                                                                                                                                                |

Other fields are internal and may change without notice.

## Status, phase and readiness

`status` is the stored lifecycle state. A deleted box is not served at all (`404 not_found`); `destroyed` is read only on a retained record of a box that will not come back (a failed or cancelled first boot), which `getOrCreate` creates over.

| `status`       | Means                                                                                                                         | What follows                                                                 |
| -------------- | ----------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------- |
| `provisioning` | Booting, resuming from cold storage, or a fork's new box being made                                                           | `running`, or back to `paused` when a resume fails or is cancelled           |
| `running`      | Up. It may still be finishing a boot, a restart, or the start of a pause or delete (`phase` says which)                       | `stopping`, `paused`, or `deleting` for a box on a tailnet                   |
| `stopping`     | A pause, a delete or the first leg of a resize has been accepted and has not landed yet                                       | `paused`, or gone (`404`) for a delete                                       |
| `paused`       | Paused: not billed. Also between the legs of a resize, after a failed start (`startFailedAt` set), or evicted to cold storage | `running` on a resume (`provisioning` first when it boots from cold storage) |
| `deleting`     | Signing out of its tailnet before the delete. Never billed                                                                    | Gone (`404`)                                                                 |
| `destroyed`    | A retained record of a box that will not come back: its first boot failed or was cancelled before it ran                      | Nothing; `getOrCreate` creates the name again                                |

`phase` reads the status together with the change in flight, the fork making the box, a pending resize and the last start, so you never re-derive them:

| `phase`         | The box is                                                                |
| --------------- | ------------------------------------------------------------------------- |
| `starting`      | Booting, resuming, restarting, or coming back after a cancelled resize    |
| `running`       | Up, with nothing in flight                                                |
| `stopping`      | Being paused                                                              |
| `paused`        | Paused, with nothing in flight that will resume it                        |
| `resizing`      | Moving to `pendingTier`, including the wait for room between its two legs |
| `forking`       | Being made by a fork, or having its disk captured for one                 |
| `deleting`      | Being deleted, including the backup a delete takes first                  |
| `delete_failed` | A delete was refused. The box still runs; delete it again                 |
| `start_failed`  | Its last start (a create or a resume) failed                              |
| `gone`          | A retained record of a box that will not come back                        |

`allowedActions` lists the verbs the box's state accepts now: `resume`, `pause`, `restart`, `delete`, `cancel_transition` and `fork`. A running box reads `["pause","restart","delete","fork"]`; a paused one `["resume","delete"]`, plus `fork` when it can be forked. A verb it does not list would be refused. A listed verb can still be refused for a reason outside the box: credit (`402`), capacity (`503`), or an argument (a fork's `sourcePolicy`, a name). A listed `fork` can also answer `409 source_busy` while an operator holds the box for maintenance, or while a running box's serving epoch lags a claim: neither is on the row, so retry the fork later.

`readiness` (on `GET /sandbox/:id`, `GET /sandbox/:id/wait` and the lifecycle answers) says whether the box will answer. It is what every SDK wait reads:

| `readiness`      | Means                                                                                                                                                  | A wait                                  |
| ---------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------ | --------------------------------------- |
| `ready`          | Running, and its connection gate passed                                                                                                                | Returns                                 |
| `settling`       | Coming up, going through a change, or not connectable yet. Every transient is here: no room, a host offline, a busy host, an upgrade waiting to resume | Keeps waiting, with no deadline         |
| `awaits_tailnet` | A private box running with its key that has not joined a tailnet. Only your auth key moves it on                                                       | Returns, so you can connect it          |
| `paused`         | Paused, with nothing in flight that will resume it                                                                                                     | Raises `box_not_running`, not retryable |
| `start_failed`   | The server answered its last start (a create or a resume) as failed                                                                                    | Raises `start_failed`, not retryable    |
| `deleting`       | A delete is in flight                                                                                                                                  | Raises `conflict`, not retryable        |
| `gone`           | It will not come back                                                                                                                                  | Raises `conflict`, not retryable        |

`readiness` is computed only where the connection gate runs: `GET /sandbox/:id`, `GET /sandbox/:id/wait` and the answers of create, pause and resume. `GET /sandbox` does not run that gate for every row, so it carries `phase` and `allowedActions` but no `readiness`: the same box never reads `ready` on one route and `settling` on the other. `readiness` is `ready` only when `ready` is true; `ready` can be true while `readiness` is still `settling` (a restart that has not taken the box down yet). Wait on `readiness`.

## GET /sandbox/:id and the box token

`GET /sandbox/:id` adds five fields to the row:

- `ready` - the box is running and serving you, and accepts every live key's token. Only then can you call it.
- `readiness` - whether the box will answer, from the same gate as `ready` ([above](#status-phase-and-readiness)).
- `httpsReady` - `baseUrl` completes a TLS handshake with a trusted certificate. A box never waits for its certificate to boot, so a freshly named or restarted box can be `ready` a few seconds to minutes before this turns true; until then a request fails in the TLS handshake, before anything is sent, and is safe to retry. The SDKs retry it for you.
- `baseUrl` - the box's HTTPS origin, e.g. `https://box--demo.zbox.sh`, or `https://box.demo.zbox.sh` while the box is [private](https://zipbox.ai/docs/tailscale.md#make-a-box-private). Null until `ready`. Previews live under the name, not this origin: `https://<label>.demo.zbox.sh`. It is not permanent: a freshly created box answers first on the machine's boot address (`box--<another-name>.zbox.sh`), and its `baseUrl` changes once, to `box--<name>`, a few minutes after create, when its own name resolves everywhere. Both origins answer while it changes. A client that gets a connect or TLS failure on a box call (nothing was sent) reads `GET /sandbox/:id` again and retries on the `baseUrl` it returns. The SDKs do this for you.
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

The token is the same every time you ask (it is derived from your key and the box, never stored), has no expiry, and is accepted by no other box, so it stays valid when `baseUrl` changes. Deleting the key kills every token derived from it. If the box answers `401`, fetch this route again and retry once. If this route answers but the box still says `401`, the key is live and the box has not received its token yet (a key created moments ago reaches running boxes within seconds): retry shortly. A deleted key fails this route itself.

## Create a box

```sh
curl -X POST https://api.tribes.xyz/sandbox/identity/claim \
  -H "Authorization: Bearer $ZIPBOX_API_KEY" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"name":"demo","tier":"s"}'
```

The answer is `{sandboxId, name, status, created, …}` plus the detail fields of `GET /sandbox/:id`: `ready`, `readiness`, `httpsReady`, `baseUrl`, `boxToken`, `phase` and `allowedActions`. When a booted machine was bound to the box inside the request, it says `ready: true` with `baseUrl` and `boxToken`, and the box can be called at once. Otherwise the box boots in the background: wait with `GET /sandbox/:id/wait?until=ready` ([below](#waiting-for-a-box)). It ends `ready` (or `awaits_tailnet` for a private box); `paused`, `start_failed`, `deleting` and `gone` mean it will not come up by itself. `name` is the box's permanent subdomain: lower case letters, digits and hyphens. With no credit balance the answer is `402 insufficient_credits`. A name the account has never used, once it already holds 250, answers `403 machine_limit_reached`; claim one of its existing names instead (the name of a box you deleted boots again under the same subdomain, mailbox and wallets), see [Machine limit](https://zipbox.ai/docs/machine-limit.md).

A box is created without a mailbox. Its address `<name>@zbox.sh` is reserved with the name, and the mailbox itself is created the first time the box or its owner uses mail: the first `tribes-email` call from inside the box, the first time the box is opened in the dashboard, or the first `/sandbox/:id/email` request. Pass `"mailbox": true` to create it at claim time instead, for a box that must be able to receive mail before anything on it has asked; it is created in the background and the answer does not wait for it.

A box is also created without wallets, and nothing about a create waits on them. Its EVM and Solana wallet pair is created once, the first time it is used: `tribes-wallet` inside the box, or the owner opening the box's Wallet app in the dashboard. From then on the box can receive funds. Spending needs the box's signer, which the owner's browser attaches while the dashboard is open and the box is running; until then `tribes-wallet` refuses to sign and says so. A box driven only through the API can therefore hold and receive funds but cannot spend them until its owner opens the dashboard once. Either way the mailbox belongs to the name, not the box: reusing a name (`getOrCreate`) reuses its mailbox, so prefer one long-lived name per recurring job over a fresh box per run.

## Pause and resume

```sh
curl -X POST https://api.tribes.xyz/sandbox/$ID/pause \
  -H "Authorization: Bearer $ZIPBOX_API_KEY"
curl -X POST https://api.tribes.xyz/sandbox/$ID/resume \
  -H "Authorization: Bearer $ZIPBOX_API_KEY"
```

Both answer with the box in the same shape as `GET /sandbox/:id` (the row plus `phase`, `allowedActions`, `readiness`, `ready`, `httpsReady`, `baseUrl` and `boxToken`):

- `200` - the final state. The box's host answered within the request: a pause reads `status: paused`, `readiness: paused`; a resume reads `ready: true`, `readiness: ready`, with its `baseUrl` and `boxToken`, so you can call it at once.
- `202` - the box in transition (`phase` `stopping` or `starting`, `readiness: settling`). The host has not answered yet, or the resume is a boot from cold storage; the change carries on by itself. Wait for it with `GET /sandbox/:id/wait` ([below](#waiting-for-a-box)).

`POST /sandbox/:id/restore` is the same route as `/resume`. A resume on a box whose pause is still landing (`stopping`) answers `409 conflict`: wait with `?until=paused`, then resume. `POST /sandbox/:id/reset` (restart) usually answers `202`: a restart is a boot.

## Waiting for a box

`GET /sandbox/:id/wait?until=ready` and `?until=paused` hold the request until the box's `readiness` reaches `until`, then answer `200` with the box in the shape of `GET /sandbox/:id`. The server is told by the box's host when it gets there; nothing polls. It also answers, with the box as it is, when the box reaches a state `until` cannot come from by itself (for `ready`: `paused`, `start_failed`, `deleting`, `gone` or `awaits_tailnet`; for `paused`: `deleting` or `gone`), and when its hold limit (about 50 seconds) passes. Read `readiness` in the answer: if it is not what you waited for and still `settling`, ask again at once. The SDKs do this with no deadline of their own, until the box gets there, raises its verdict, or the caller cancels. The route answers `404` when the box is not yours.

```sh
curl "https://api.tribes.xyz/sandbox/$ID/wait?until=ready" \
  -H "Authorization: Bearer $ZIPBOX_API_KEY"
```

An API older than this route answers it `404` for a box that exists. A client then reads `GET /sandbox/:id`: a `200` means the route is missing, so poll `GET /sandbox/:id` with backoff instead; a `404` means the box is gone or not yours. The SDKs do this for you.

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

## Fork a box

```sh
curl -X POST https://api.tribes.xyz/sandbox/$ID/fork \
  -H "Authorization: Bearer $ZIPBOX_API_KEY" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"name":"demo-2","sourcePolicy":"require_paused"}'
```

`:id` is the source. The answer is `202 {operationId, sandbox, fork}`: the new box's row (`status: provisioning`) and the fork, which runs on without your request. Follow it with `GET /sandbox/:id/fork` on the **new** box's id, or that box's own row, which carries `fork` while it runs; cancel it with `POST /sandbox/:id/transition/cancel` on the new box. `sourcePolicy` is `require_paused` (a running source answers `409 pause_required`) or `pause_and_resume` (a running source pauses for its capture and resumes right after). `private: true` starts the new box private; absent or false, it starts public whatever the source is. The body takes nothing else: no Tailscale key, which you deliver to the new box afterwards with `PUT /sandbox/:id/tailscale`. What is copied, the errors and the private-box flow: [Fork a box](https://zipbox.ai/docs/fork.md).

## Idempotency

`POST /sandbox/identity/claim` accepts an `Idempotency-Key` header. Replaying the same key within 24 hours returns the original answer instead of creating a second box, so a create that timed out on your side is always safe to retry. The SDKs send one automatically.

With `Accept: application/x-ndjson`, a create that places a box answers as a stream. The first line, sent the moment the box is placed, is `{"type":"placed","sandboxId":"…","baseUrl":"…"}`. It is informational: do not resolve or connect to that `baseUrl` before the last line. Its DNS record may not exist yet, and an early lookup can land on the zone's black-hole wildcard (`*.zbox.sh` answers `192.0.2.1` / `100::` for a name with no record) or cache the NXDOMAIN for the zone's 1800 s minimum, so the box's first call would fail. Use the `baseUrl` in the final answer. The last line is `{"type":"answer","status":200,"headers":{…},"body":{…}}`: the status, headers and body the create would have answered without the stream. Everything answered before the box is placed (a refusal, a replayed `Idempotency-Key`, a private box) comes back as a plain answer with its own status. A stream that ends before its `answer` line is safe to retry with the same `Idempotency-Key`. The SDKs do all of this for you.

`POST /sandbox/:id/fork` **requires** one (`400 invalid_request` without it). The same key with the same body returns the same `202`, operation and new box, also after the fork has been cancelled; the same key with a different body answers `409 idempotency_mismatch`. Generate one per fork you mean to make and reuse it on every retry of that fork; the SDKs do.

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
