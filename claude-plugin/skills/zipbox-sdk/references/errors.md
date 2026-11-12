> Zipbox docs, markdown edition of https://zipbox.ai/docs/errors
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# Errors and limits

Every error code with its cause and fix, HTTP statuses, box-side and client-side errors, retry rules, limits, and what is not in this version.

Every error a public caller sees - control plane, box, SDK, CLI or MCP - resolves to one of twenty-five codes. Each error's `docsUrl` points at the section below named after its code.

## Nothing times out on a clock

No box, command, PTY, preview or token ends because time passed. There is no idle timeout, no auto-pause, no maximum session length, and no default command deadline. A box runs until you pause or delete it; a command runs until it exits or you signal it (or until the `timeoutMs` you chose). If something ends, a person or the box ended it.

## HTTP statuses

| Status | Code                                    | Retry?                                                      |
| ------ | --------------------------------------- | ----------------------------------------------------------- |
| 400    | `invalid_request`                       | No - fix the request                                        |
| 401    | `unauthorized`                          | No - fix the credential (once on a box: re-fetch the token) |
| 402    | `insufficient_credits`                  | No - add credit                                             |
| 403    | `forbidden`                             | No                                                          |
| 403    | `machine_limit_reached`                 | No - reuse one of your existing machine names               |
| 403    | `requires_interactive`                  | No - do it from the dashboard, signed in                    |
| 404    | `not_found`                             | No                                                          |
| 409    | `conflict`                              | No - the box is in the wrong state; read it and act on that |
| 409    | `name_taken`                            | No - pick another name                                      |
| 409    | `source_unsupported`                    | No - this box cannot be forked                              |
| 409    | `source_busy`                           | Later - once the source's change in flight has settled      |
| 409    | `pause_required`                        | No - send `pause_and_resume`, or pause the source first     |
| 409    | `idempotency_mismatch`                  | No - use a new `Idempotency-Key` for a different request    |
| 409    | `box_key_changed`                       | Once - re-read `agentPublicKey`, re-seal, send again        |
| 409    | `tailnet_not_connected`                 | No - connect the box to your tailnet first                  |
| 409    | `tailscale_unavailable_egress_lockdown` | No - not available on this box                              |
| 409    | `tailscale_requires_update`             | No - update the box's ZipOS first                           |
| 413    | `invalid_request`                       | No - the file is over 250 MiB                               |
| 422    | `invalid_request`                       | No - fix the request                                        |
| 429    | `rate_limited`                          | Yes, after `Retry-After`                                    |
| 502    | `box_not_running`                       | When `retryable`: the box is settling and comes back itself |
| 503    | `unavailable`                           | Yes, with backoff (any other 5xx too)                       |

## The error envelope

The control plane answers every error to an API key as:

```json
{
  "error": {
    "code": "rate_limited",
    "message": "Rate limit exceeded (600 requests per minute per key)",
    "requestId": "req_…",
    "retryable": true,
    "retryAfterMs": 1000,
    "docsUrl": "https://zipbox.ai/docs/errors#rate_limited"
  }
}
```

The SDKs raise it as one error type carrying the same fields, the CLI prints it with `--json`, and an MCP tool's error result carries it as its text. Quote the `requestId` when you contact support.

## invalid_request

The request does not match the contract: a missing or malformed field, a relative path sent to a box route, a body over 250 MiB, or both `cmd` and `argv` in one exec. The `message` names the field. Fix the request; retrying it unchanged fails the same way.

## unauthorized

The credential is missing, malformed or deleted. On the control plane: check `ZIPBOX_API_KEY` and that the key still shows in Settings → API keys. On a box: fetch `GET /sandbox/:id` again for a fresh `boxToken` and retry once - the SDKs do this. A deleted key fails that fetch itself, at the control plane. If the fetch succeeds and the box still answers `401`, the key is live and the box has not received it yet (a key created moments ago reaches its running boxes within seconds): retry with backoff. The SDKs raise that second `401` with `retryable: true`, and a terminal keeps reconnecting through it.

## insufficient_credits

Creating, resuming or resizing a box needs a credit balance above $0. Add credit at zipbox.ai/billing (a key cannot buy credit), then retry. Running boxes are not paused by this error.

## forbidden

The key is not allowed to do this: key management, OAuth consent, buying credit, the billing portal, auto top-up, signup grants, GitHub connect and provider-key writes are dashboard-only. A request with an `Origin` header (a browser) is also refused. Do it in the dashboard instead.

## machine_limit_reached

A create with a **new** name was refused because the account already holds 250 machine names. A name is permanent: its subdomain and mailbox address are reserved to your account the first time it is created, its mailbox and wallets once they are first used, and deleting the box keeps them. Nothing was created or charged.

Creating, starting or rebooting any name you already own keeps working, so the fix is to reuse names: keep a fixed pool (`worker-00` … `worker-49`) and call get-or-create with one of them instead of a fresh name per job. How, in every SDK, the CLI and MCP: [Machine limit and reusing names](https://zipbox.ai/docs/machine-limit.md). Need more than 250? Email support@tribes.xyz with your account email and what the fleet does.

## not_found

No such box (or it is not yours), no such command id, or no such file. Box ids come from `GET /sandbox`; command ids from `exec` or `GET /exec`.

## conflict

The box or resource is in a state that does not allow this: resuming a box that is already running, restarting one that is paused, resizing during a resize, a file operation whose destination exists. Read the box (`GET /sandbox/:id`) and act on its `status`. An SDK `pause` that is waiting for the box to read paused raises `conflict`, not retryable, when the box reads running again: its pause was cancelled (`cancelTransition`) or did not take effect.

## rate_limited

More than 600 requests a minute with one key, or a box past 32 running commands or 8 file transfers. Wait `Retry-After` seconds (or `retryAfterMs`) and retry. The SDKs retry safe requests on their own with backoff and jitter.

## box_not_running

The box is paused, starting or restarting, so it has nothing to answer with. `retryable` says which: true while its [`readiness`](https://zipbox.ai/docs/http-api.md#status-phase-and-readiness) is `settling` (it comes back by itself), false when it is paused or its start failed (nothing will resume it but you). Resume it (`resume(id)`), or wait with `GET /sandbox/:id/wait?until=ready`, then retry. An SDK wait on a paused box raises this, not retryable, instead of waiting.

## box_key_changed

A sealed credential (a Tailscale auth key) was sealed to an `agentPublicKey` that is no longer the box's key: claiming a box re-mints it. Read `GET /sandbox/:id` again, seal to the new `agentPublicKey`, and send once more. The SDKs do this exactly once; a second `box_key_changed` is raised to you.

## tailnet_not_connected

Private mode was turned on while the box is not connected to your tailnet with a tailnet IPv4, so nothing could reach it afterwards. Connect it first ([Tailscale](https://zipbox.ai/docs/tailscale.md#connect-a-box)), wait for `state: connected`, then turn private on. To start a box private from the beginning, pass `private: true` at create instead.

The SDKs raise it too, without a request, for a box call on a private box that has not joined a tailnet yet (`readiness: awaits_tailnet`): only your auth key moves it on, so it is not retryable. Connect it with `tailscale.connect`.

## tailscale_unavailable_egress_lockdown

The box runs under company egress lockdown, which only lets traffic out through the company proxy, so Tailscale cannot reach its servers or peers. Use a box without egress lockdown.

## tailscale_requires_update

The box's ZipOS is too old for what you asked: it reported that it has no managed Tailscale, so it could never use the key, or its ZipOS predates private boxes serving their own `box.`/`ide.`/`vnc.`/`tunnel.` names, so it cannot go private. Accept the ZipOS software update (**Settings → Software Update** in the dashboard; the box restarts into it), then try again. The dashboard says so ahead of time (**Update ZipOS to use Tailscale**, **Update ZipOS to go private**), and a box row carries `tailscaleSupported: false` for the first case. Applies to `PUT /sandbox/:id/tailscale` and to turning private mode on; turning private mode off is never refused.

## box_private_unreachable

The box is private: it can only be reached from devices on your tailnet, and this device is not on it (or Tailscale is off). Box calls (exec, files, PTY, previews, logs) fail fast with this code; lifecycle calls still work. Turn on Tailscale on this device ([tailscale.com/download](https://tailscale.com/download)) and retry. The hosted MCP server can never reach a private box ([Tailscale](https://zipbox.ai/docs/tailscale.md#reaching-a-private-box)).

## name_taken

A [fork](https://zipbox.ai/docs/fork.md) asked for a name that is in use: a box is using it, yours or another account's, or another account holds it. A name you hold on hold (its box was deleted) is not taken: the fork reuses it, with its mailbox and wallets. Nothing was reserved and the source was not touched. Pick another name and fork again.

## source_unsupported

The source is a kind of box a [fork](https://zipbox.ai/docs/fork.md) does not support. Answered as `409` before anything happens: an external machine, a warm-pool box, a company-runtime box, a dedicated enterprise box or a legacy tier. Raised as a fork's `error.code` when the copy found a filesystem layout it does not support (an unexpected mount layout, a platform path replaced by a link, a filesystem it cannot repair); the source was resumed if the fork had paused it. A host that cannot fork yet answers `503 unavailable` instead, which is retried.

## source_busy

The [fork](https://zipbox.ai/docs/fork.md) source is in another change: a pause, resume, restart, resize, software update or another fork. Nothing was reserved and the source was not touched. Wait until `GET /sandbox/:id` shows the change settled, then fork again; the SDKs do not retry it for you.

## pause_required

The [fork](https://zipbox.ai/docs/fork.md) source is running and the request said `sourcePolicy: require_paused`. A running box must stop writing to be captured consistently, which disconnects its sessions. Fork with `pause_and_resume` (the CLI's `--pause`, MCP's `pause: true`) to let it pause for the capture and resume right after, or pause it yourself first. Nothing was touched.

## requires_interactive

The request needs a signed-in owner, and an API key sent it. A [fork](https://zipbox.ai/docs/fork.md) of a source that holds machine-scoped provider keys or is a selected member of a custom API copies those credentials, and credential changes are never made with an API key. Refused before anything is reserved or the source is touched. Fork it from the dashboard, signed in as its owner; account-wide keys and every restriction still copy under an API key for any other source.

## idempotency_mismatch

The `Idempotency-Key` was already used for a different request: the same key with another name or source policy. A key names one request; replaying it with the same body returns the original answer. Send a new key for a different request.

## source_gone

A [fork](https://zipbox.ai/docs/fork.md) ended `failed` because its source was deleted before it was captured. The new box was discarded; its name stays reserved under your account.

## identity_reused

A [fork](https://zipbox.ai/docs/fork.md) ended `failed` because the new box came up holding one of its source's keys. It was stopped before it served anything and its disk was discarded, so no one could use the source's identity through it. The source was resumed if the fork had paused it. This is a platform defect, not something you did: contact support with the operation id.

## copy_failed

A [fork](https://zipbox.ai/docs/fork.md) ended `failed` because the copy could not produce a verified filesystem for the new box. The new box was discarded and the source was resumed if the fork had paused it. Forking the same source again fails the same way until its disk changes; contact support with the operation id.

## start_failed

A [fork](https://zipbox.ai/docs/fork.md) ended `failed` because the new box's first boot was answered as failed. Its disk was discarded and its name stays reserved; the source is unaffected. Contact support with the operation id.

## unavailable

A temporary failure on our side: capacity, maintenance, or a dependency that is down. Retry with exponential backoff and jitter; the SDKs do. The `message` says when it is maintenance.

## Box-side errors

The box's own routes predate the envelope and answer `{"error":"<message>"}` (sometimes with a `code`, e.g. `read_only`). The SDKs map them by status to the codes above. The ones you will meet:

| Message                     | Status | Code              | Fix                                                        |
| --------------------------- | ------ | ----------------- | ---------------------------------------------------------- |
| `path must be absolute`     | 400    | `invalid_request` | Send `/root/...` (the SDKs resolve relative paths for you) |
| `no such file`              | 404    | `not_found`       | Check the path                                             |
| `no such directory`         | 404    | `not_found`       | Writes need the directory to exist: `mkdir` first          |
| `upload too large`          | 413    | `invalid_request` | 250 MiB per write; move big files with `exec`              |
| `file too large`            | 413    | `invalid_request` | 250 MiB per download; move big files with `exec`           |
| `This folder is read-only.` | 403    | `forbidden`       | Write somewhere else                                       |
| Too many running commands   | 429    | `rate_limited`    | Wait for one of the 32 to exit                             |

## Client-side errors

These never reach a server:

- **No API key** - the SDK found no key argument and no `ZIPBOX_API_KEY`. Raised as `unauthorized` ("no API key: …"); the CLI says "not logged in: run `zipbox login`, or set ZIPBOX_API_KEY".
- **No WebSocket** - the TypeScript SDK on Node 20 has no built-in `WebSocket` for PTYs. Raised as `invalid_request` ("no WebSocket available: …"); pass `WebSocket` (e.g. from the `ws` package) to `new Zipbox()`.
- **Unsupported `runCode` language** - anything but `python`, `node` or `bash`, raised as `invalid_request`.
- **Private box off the tailnet** - a box call on a box with `privateMode: true` that does not answer a 5-second probe of its `/challenge` raises `box_private_unreachable` instead of hanging.
- **Transport errors** - DNS, TLS or a dropped connection. Retried like `unavailable` on safe requests. A box's address needs DNS for `box--<name>.zbox.sh`; SSH additionally needs IPv6. A box call that failed before anything was sent (a refused connection, DNS, a failed TLS handshake) is retried on every method, after the SDK re-reads the box: a new box's `baseUrl` changes once, a few minutes after create. That retry has no attempt limit; a box that was paused, failed to start or was deleted ends it with `box_not_running`, `start_failed` or `conflict`.

## Retries

Retry an error when its `retryable` is true, whatever its code: by default that is `rate_limited`, `unavailable` and `box_not_running`, and the envelope's own `retryable` overrides it (a `box_not_running` for a paused box is false). Honour `Retry-After` first, then `retryAfterMs`; otherwise back off exponentially with jitter. A code your SDK does not know yet maps by HTTP status. The SDKs retry GET requests, `create` and `fork` (each carries one `Idempotency-Key`, reused on every retry, so a retry never makes two boxes) up to `maxRetries` (default 3). A wait (`create`, `getOrCreate`, `waitReady`, `fork`) has no deadline: a retryable failure past `maxRetries` goes on its poll ladder, so a control-plane restart never ends it. They never retry anything else on their own, because a repeated `exec` runs the command twice; a request that was never sent cannot run twice, so it is the one kind retried on every method. The one exception is `box_key_changed`, which `tailscale.connect` re-seals and sends once more.

## Limits

| Limit                                              | Value                                                                |
| -------------------------------------------------- | -------------------------------------------------------------------- |
| Control-plane requests per key                     | 600 per minute (`429` + `Retry-After` + `X-RateLimit-Remaining`)     |
| API keys per account                               | 20                                                                   |
| Machines (names) per account                       | 250 - deleted boxes keep their name; [reuse it](https://zipbox.ai/docs/machine-limit.md) |
| Running commands per box                           | 32                                                                   |
| Concurrent file transfers per box                  | 8                                                                    |
| File read / write per request                      | 250 MiB                                                              |
| Command output kept per stream                     | 64 MiB (then a `gap` event)                                          |
| Command output kept per box                        | 512 MiB (oldest finished evicted first)                              |
| MCP tool result shown to a model                   | 32 KiB (the rest via `get_command_output`)                           |
| Idempotency window for create                      | 24 hours                                                             |
| Box lifetime, command duration, PTY session, token | Unlimited                                                            |

## Out of v1

Documented gaps, not hidden stubs:

- **Named snapshots and templates.** A box is its own template; reuse it with `getOrCreate(name, options)` (the create options apply only when the box is created).
- **Volumes** shared between boxes.
- **LSP** language-server endpoints.
- **Outbound network policy.** Metered-provider egress exists; it is not user-configurable.
- **Per-key scopes.** Every key has full access to the account's boxes.
- **File watch** events.
- **Signed previews.** A preview is public on a public box, and tailnet-only on a [private box](https://zipbox.ai/docs/tailscale.md).
- **Tailscale exit nodes, Funnel and `tailscale serve`.** See [what is not supported](https://zipbox.ai/docs/tailscale.md#what-is-not-supported).
- **MCP on a private box.** The hosted server cannot join your tailnet; box tools return `box_private_unreachable`.

## FAQ

### "box_not_running" right after create

`create` returns once the box is ready, but a box you got from `get` or `list` may still be starting or may be paused. Read `get(id)`: resume it if `status` is `paused`, or wait until it is ready (`waitReady`).

### "unauthorized" from the box but not from the control plane

The box has not yet received the key's token (a key created moments ago, or a box just resumed), or the key was deleted. Fetch `GET /sandbox/:id` again: a deleted key fails that fetch at the control plane. If it succeeds, retry the box with backoff until it accepts the key.

### "Your balance is at or below $0 — top up to a positive balance to boot a machine."

`insufficient_credits` (402) from `create`; `resume` and `setTier` say the same with "unarchive this sandbox" or "upgrade this sandbox". Top up in the dashboard; running boxes keep running.

### "You've reached this account's limit of 250 machines."

`machine_limit_reached` (403) from `create` with a name the account has never used. Boot one of your existing names instead (get-or-create with that name); see [Machine limit and reusing names](https://zipbox.ai/docs/machine-limit.md).

### "upload too large"

The request body is over 250 MiB. Split the file, or download it from inside the box with `exec` (`curl -O …`).

### "path must be absolute"

A raw box route got a relative path. Send `/root/workspace/…`, or use an SDK, which resolves relative paths under `/root/workspace`.

### "This action needs a signed-in zipbox.ai session; API keys cannot perform it."

`forbidden` (403): the route is dashboard-only - key management, OAuth consent, buying credit, GitHub connect. Keys cannot mint keys; create them in Settings → API keys.

### "API keys cannot be used from a browser. Call the API from a server or the CLI."

`forbidden` (403): the request carried an `Origin` header. Call the API from a server, a script or the CLI, never from a web page.

### "Rate limit exceeded (600 requests per minute per key)"

`rate_limited` (429): wait `Retry-After` seconds and retry. Give each machine or agent its own key; the limit is per key.
