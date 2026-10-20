> Zipbox docs, markdown edition of https://zipbox.ai/docs/errors
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# Errors and limits

Every error code with its cause and fix, HTTP statuses, box-side and client-side errors, retry rules, limits, and what is not in this version.

Every error a public caller sees - control plane, box, SDK, CLI or MCP - resolves to one of nine codes. Each error's `docsUrl` points at the section below named after its code.

## Nothing times out on a clock

No box, command, PTY, preview or token ends because time passed. There is no idle timeout, no auto-stop, no maximum session length, and no default command deadline. A box runs until you stop or delete it; a command runs until it exits or you signal it (or until the `timeoutMs` you chose). If something ends, a person or the box ended it.

## HTTP statuses

| Status | Code                   | Retry?                                                      |
| ------ | ---------------------- | ----------------------------------------------------------- |
| 400    | `invalid_request`      | No - fix the request                                        |
| 401    | `unauthorized`         | No - fix the credential (once on a box: re-fetch the token) |
| 402    | `insufficient_credits` | No - add credit                                             |
| 403    | `forbidden`            | No                                                          |
| 404    | `not_found`            | No                                                          |
| 409    | `conflict`             | No - the box is in the wrong state; read it and act on that |
| 413    | `invalid_request`      | No - the file is over 250 MiB                               |
| 422    | `invalid_request`      | No - fix the request                                        |
| 429    | `rate_limited`         | Yes, after `Retry-After`                                    |
| 502    | `box_not_running`      | Yes, once the box is running                                |
| 503    | `unavailable`          | Yes, with backoff (any other 5xx too)                       |

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

The SDKs raise it as one error type carrying the same fields, and the CLI prints it with `--json`. Quote the `requestId` when you contact support.

## invalid_request

The request does not match the contract: a missing or malformed field, a relative path sent to a box route, a body over 250 MiB, or both `cmd` and `argv` in one exec. The `message` names the field. Fix the request; retrying it unchanged fails the same way.

## unauthorized

The credential is missing, malformed or deleted. On the control plane: check `ZIPBOX_API_KEY` and that the key still shows in Settings → API keys. On a box: fetch `GET /sandbox/:id` again for a fresh `boxToken` and retry once - the SDKs do this. A second `401` from the box means the key was deleted.

## insufficient_credits

Creating, starting or resizing a box needs a credit balance above $0. Add credit at zipbox.ai/billing (a key cannot buy credit), then retry. Running boxes are not stopped by this error.

## forbidden

The key is not allowed to do this: key management, OAuth consent, buying credit, the billing portal, auto top-up, signup grants, GitHub connect and provider-key writes are dashboard-only. A request with an `Origin` header (a browser) is also refused. Do it in the dashboard instead.

## not_found

No such box (or it is not yours), no such command id, or no such file. Box ids come from `GET /sandbox`; command ids from `exec` or `GET /exec`.

## conflict

The box or resource is in a state that does not allow this: starting a box that is already running, restarting one that is stopped, resizing during a resize, a file operation whose destination exists. Read the box (`GET /sandbox/:id`) and act on its `status`.

## rate_limited

More than 600 requests a minute with one key, or a box past 32 running commands or 8 file transfers. Wait `Retry-After` seconds (or `retryAfterMs`) and retry. The SDKs retry safe requests on their own with backoff and jitter.

## box_not_running

The box is stopped, starting or restarting, so it has nothing to answer with. Start it (`start(id)`), or wait until `GET /sandbox/:id` reports `ready: true`, then retry.

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
- **Transport errors** - DNS, TLS or a dropped connection. Retried like `unavailable` on safe requests. A box's address needs DNS for `<name>.zbox.sh`; SSH additionally needs IPv6.

## Retries

Retry only `retryable` errors: `rate_limited`, `unavailable`, `box_not_running`. Honour `Retry-After`; otherwise back off exponentially with jitter. The SDKs retry GET requests and `create` (which carries an `Idempotency-Key`, so a retry never makes two boxes) up to `maxRetries` (default 3). They never retry anything else on their own, because a repeated `exec` runs the command twice.

## Limits

| Limit                                              | Value                                                            |
| -------------------------------------------------- | ---------------------------------------------------------------- |
| Control-plane requests per key                     | 600 per minute (`429` + `Retry-After` + `X-RateLimit-Remaining`) |
| API keys per account                               | 20                                                               |
| Running commands per box                           | 32                                                               |
| Concurrent file transfers per box                  | 8                                                                |
| File read / write per request                      | 250 MiB                                                          |
| Command output kept per stream                     | 64 MiB (then a `gap` event)                                      |
| Command output kept per box                        | 512 MiB (oldest finished evicted first)                          |
| MCP tool result shown to a model                   | 32 KiB (the rest via `get_command_output`)                       |
| Idempotency window for create                      | 24 hours                                                         |
| Box lifetime, command duration, PTY session, token | Unlimited                                                        |

## Out of v1

Documented gaps, not hidden stubs:

- **Named snapshots and templates.** A box is its own template; reuse it with `getOrCreate(name)`.
- **Volumes** shared between boxes.
- **LSP** language-server endpoints.
- **Outbound network policy.** Metered-provider egress exists; it is not user-configurable.
- **Per-key scopes.** Every key has full access to the account's boxes.
- **Memory pause and resume.** Stop archives the disk; restart reboots. Files survive both; running processes do not.
- **File watch** events.
- **Private or signed previews.** Previews are public.

## FAQ

### "box_not_running" right after create

`create` returns once the box is ready, but a box you got from `get` or `list` may still be starting or may be stopped. Read `get(id)`: start it if `status` is `archived`, or wait until `ready` is true.

### "unauthorized" from the box but not from the control plane

The box has not yet received the token list after a start, or the key was deleted. Fetch `GET /sandbox/:id` again and retry once.

### "Your balance is at or below $0 — top up to a positive balance to boot a machine."

`insufficient_credits` (402) from `create`; `start` and `setTier` say the same with "unarchive this sandbox" or "upgrade this sandbox". Top up in the dashboard; running boxes keep running.

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
