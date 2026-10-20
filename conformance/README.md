# SDK conformance suite

One set of fixtures, four SDKs. Every SDK (`sdks/typescript`, `sdks/python`, `sdks/go`,
`sdks/rust`) runs every file in `fixtures/` against a local mock server in its own test
suite. A fixture that one SDK passes and another fails is a bug in the failing SDK.

The wire contract itself is `packages/public-api-contract/openapi.json` (served at
`https://zipbox.ai/openapi.json`). These fixtures pin the client behaviour that an
OpenAPI document cannot: token caching, 401 recovery, retries, path resolution, SSE
collection and error mapping.

## Fixture shape

```json
{
  "name": "exec_streams_and_collects",
  "description": "…",
  "call": { "op": "sandbox.exec", "args": { "id": "sbx_1", "cmd": "…" } },
  "exchanges": [{ "request": { … }, "response": { … } }],
  "expect": { "result": { … } } // or { "error": { "code": "…", "status": 404 } }
}
```

- `call.op` is an SDK-agnostic operation name (table below). The runner maps it to the
  SDK's own method and passes `args` in that language's idiom.
- `exchanges` are served **in order**. The mock server fails the test when a request
  arrives that does not match the next exchange, and when the call finishes with
  exchanges left unserved.
- `expect.result` is a **subset** match against the SDK's return value converted to
  JSON (camelCase keys; a Python/Go/Rust runner converts its own field names). `null`
  means the call returns nothing. `{"length": n, "items": [...]}` matches a list: its
  length, and each item as a subset at the same index. `{"text": "…"}` matches a
  string or bytes result decoded as UTF-8.
- `expect.error` is a subset match against the raised error: `code` (the public
  error code), `status` (HTTP status, when there was one), `requestId`, `docsUrl`.

### Matching a request

| Field      | Rule                                                                                                                                  |
| ---------- | ------------------------------------------------------------------------------------------------------------------------------------- |
| `method`   | Exact.                                                                                                                                |
| `path`     | Path plus query string, exact. Query parameters are percent-encoded (`/` → `%2F`).                                                    |
| `headers`  | Each listed header must be present (names lower-case) with this value. `{{ANY}}` = any non-empty value. Unlisted headers are ignored. |
| `body`     | JSON body, deep-equal after parsing. Absent = not checked.                                                                            |
| `bodyText` | Raw body, exact string. Absent = not checked.                                                                                         |

### Serving a response

`status`, optional `headers`, and either `body` (serialized as JSON with
`content-type: application/json`) or `bodyText` (sent verbatim, with the listed
`content-type`).

### Placeholders

| Placeholder | Replaced with                                                                                                                                                                                                      |
| ----------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `{{BASE}}`  | The mock server's origin, e.g. `http://127.0.0.1:43123`. Control plane and box are the same server: box routes (`/exec`, `/files`, …) and control-plane routes (`/sandbox`, `/account`, `/billing`) never collide. |
| `{{HOST}}`  | The hostname of `{{BASE}}` (no port).                                                                                                                                                                              |
| `{{ANY}}`   | Header matching only: any non-empty value.                                                                                                                                                                         |

Runners construct the client with `apiKey = "zb_" + "T" * 43` and
`baseUrl = {{BASE}}`, with retry backoff and create polling shortened so the suite runs
in milliseconds (the delays are configuration, never skipped logic).

## Operations

| `op`                     | Args                              | Behaviour pinned                                                                                             |
| ------------------------ | --------------------------------- | ------------------------------------------------------------------------------------------------------------ |
| `sandboxes.list`         | —                                 | `GET /sandbox`                                                                                               |
| `sandboxes.get`          | `id`                              | `GET /sandbox/:id`                                                                                           |
| `sandboxes.create`       | `name?`, `tier?`, `sshPublicKey?` | `POST /sandbox/identity/claim` with an `Idempotency-Key`, then `GET /sandbox/:id` with backoff until `ready` |
| `sandboxes.start`        | `id`                              | `POST /sandbox/:id/restore`                                                                                  |
| `sandboxes.stop`         | `id`                              | `POST /sandbox/:id/pause`                                                                                    |
| `sandboxes.restart`      | `id`                              | `POST /sandbox/:id/reset`                                                                                    |
| `sandboxes.setTier`      | `id`, `tier`                      | `POST /sandbox/:id/tier`                                                                                     |
| `sandboxes.delete`       | `id`                              | `DELETE /sandbox/:id`                                                                                        |
| `sandbox.exec`           | `id`, `cmd`                       | Box token fetched lazily and cached; `POST /exec` SSE collected to `{id, exitCode, stdout, stderr}`          |
| `sandbox.execBackground` | `id`, `cmd`                       | `POST /exec` with `background: true` → the command record                                                    |
| `commands.events`        | `id`, `commandId`, `since?`       | `GET /exec/:id/events?since=` → `{events: [{type, cursor, …payload}]}`                                       |
| `commands.signal`        | `id`, `commandId`, `signal`       | `POST /exec/:id/signal`                                                                                      |
| `files.read`             | `id`, `path`                      | `GET /files/download?path=`                                                                                  |
| `files.write`            | `id`, `path`, `content`           | `POST /files/write?dir=&name=`, raw body                                                                     |
| `files.list`             | `id`, `path`                      | `GET /files?path=` → the entries                                                                             |
| `files.stat`             | `id`, `path`                      | Lists the parent, returns the entry (`not_found` if absent)                                                  |
| `files.mkdir`            | `id`, `path`                      | `POST /files/mutate` `{action: mkdir}`                                                                       |
| `files.move`             | `id`, `path`, `destination`       | `POST /files/mutate` `{action: move}`                                                                        |
| `files.delete`           | `id`, `path`                      | `POST /files/mutate` `{action: delete}`                                                                      |
| `preview.create`         | `id`, `port`, `label?`            | Exec `tribes-caddy add <label>.<box host> 127.0.0.1:<port>`; returns `{url}`                                 |
| `account.balance`        | —                                 | `GET /billing/summary`                                                                                       |
| `account.whoami`         | —                                 | `GET /account/whoami`                                                                                        |

## Client rules every SDK implements

1. **Auth.** `Authorization: Bearer <apiKey>` on the control plane; the key comes from
   the constructor or `ZIPBOX_API_KEY`. The control plane URL comes from the
   constructor, `ZIPBOX_API_URL`, or the default `https://api.tribes.xyz`.
2. **Box token.** Fetched from `GET /sandbox/:id` on first use of a box, cached per box
   id for the client's lifetime. A box answering 401 re-fetches the detail **once** and
   retries the request once; a second 401 raises `unauthorized`. A box whose detail is
   not `ready` raises `box_not_running` without calling the box.
3. **Errors.** A body shaped `{error: {code, …}}` is the envelope: raise its `code`,
   `message`, `requestId`, `retryable`, `retryAfterMs`, `docsUrl`. Any other non-2xx
   (box routes answer `{error: "<message>"}`) maps by status: 400/413/422
   `invalid_request`, 401 `unauthorized`, 402 `insufficient_credits`, 403 `forbidden`,
   404 `not_found`, 409 `conflict`, 429 `rate_limited`, 502 `box_not_running`, other
   5xx `unavailable`. `docsUrl` is always `https://zipbox.ai/docs/errors#<code>`.
4. **Retries.** `rate_limited` and `unavailable` are retried on GET requests and on
   `create` (it carries an `Idempotency-Key`), honouring `Retry-After`, with exponential
   backoff and jitter otherwise, up to `maxRetries` (default 3). Nothing else is retried.
5. **Exit codes are results.** `exec` returns a non-zero `exitCode`; it never raises for it.
6. **Paths.** A path that does not start with `/` is joined to `/root/workspace`.
7. **No timeouts by default.** No SDK sets a request or exec deadline unless the caller
   passes one. A box runs until someone stops it.
8. **PTY reconnect.** A PTY socket that drops reconnects with exponential backoff
   (cap 30 s, jitter) and no attempt ceiling; the ladder resets only when a frame is
   received. It re-attaches the same session id with the same `clientId` and `resume=1`.
