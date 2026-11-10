> Zipbox docs, markdown edition of https://zipbox.ai/docs/sdk-ts
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# TypeScript SDK

The @zipbox/sdk package - install, quickstart, every method, streaming with AsyncIterable, PTY reconnect and AbortSignal.

`@zipbox/sdk` is a thin client over the [REST API](https://zipbox.ai/docs/http-api.md) and the [Box API](https://zipbox.ai/docs/box-api.md). It uses only `fetch` and `WebSocket`, runs on Node 20+, Bun and Deno, and ships its own `.d.ts` types and JSDoc on every method. API keys are for servers: never ship one to a browser page (the control plane refuses requests that carry an `Origin` header). On Node 20, pass a `WebSocket` constructor (e.g. from the `ws` package) to use PTYs; Node 22+ has one built in.

## Install

```sh
npm install @zipbox/sdk
```

## Quickstart

Create a box, run a command, write and read a file, open a preview, stop the box.

```ts
import { Zipbox } from '@zipbox/sdk'

const zb = new Zipbox() // or new Zipbox({ apiKey: 'zb_...' })
const box = await zb.sandboxes.getOrCreate('ts-quickstart', { tier: 's' })

const { exitCode, stdout } = await box.exec('node --version')
console.log(exitCode, stdout)

await box.files.write('hello.txt', 'hi from TypeScript\n')
console.log(await box.files.readText('hello.txt'))

await box.execBackground('python3 -m http.server 8000 --bind 127.0.0.1')
const { url } = await box.preview.create(8000)
console.log(url)

await zb.sandboxes.stop(box.id)
```

## Client

```ts
const zb = new Zipbox({
  apiKey: process.env.ZIPBOX_API_KEY, // default: ZIPBOX_API_KEY
  baseUrl: 'https://api.tribes.xyz', // default: ZIPBOX_API_URL, then this
  maxRetries: 3, // retries of rate_limited / unavailable on safe requests
  retryBaseDelayMs: 500, // doubles per attempt, with jitter
  retryMaxDelayMs: 30_000, // cap on that retry delay
  pollIntervalMs: 1000, // first readiness check; grows 1.25x, with jitter
  pollMaxIntervalMs: 10_000, // cap on the readiness checks
  ptyBaseDelayMs: 500, // first PTY reconnect delay
  ptyMaxDelayMs: 30_000 // cap on the PTY reconnect delay
  // also: fetch, WebSocket
})
```

No request has a timeout unless you pass an `AbortSignal`.

## Boxes

| Method                                                                              | Returns                                                                        | Route                                                  |
| ----------------------------------------------------------------------------------- | ------------------------------------------------------------------------------ | ------------------------------------------------------ |
| `zb.sandboxes.create({ name?, tier?, sshPublicKey?, mailbox?, private? })`          | `Sandbox` (ready; with `private`, running)                                     | claim, then `GET /sandbox/:id` until ready             |
| `zb.sandboxes.getOrCreate(name, { tier?, sshPublicKey?, mailbox?, private? })`      | `Sandbox`                                                                      | `GET /sandbox`, then create                            |
| `zb.sandboxes.waitReady(id)`                                                        | `Sandbox` (ready; private and not joined: running)                             | `GET /sandbox/:id` until ready                         |
| `zb.sandboxes.list()`                                                               | `SandboxInfo[]`                                                                | `GET /sandbox`                                         |
| `zb.sandboxes.get(id)`                                                              | `Sandbox`                                                                      | `GET /sandbox/:id`                                     |
| `zb.sandboxes.start(id)` / `stop(id)` / `restart(id)`                               | `void`                                                                         | restore / pause / reset                                |
| `zb.sandboxes.cancelTransition(id)`                                                 | `'cleared' \| 'cancelling' \| 'none'` (`conflict` past the point of no return) | `POST /sandbox/:id/transition/cancel`                  |
| `zb.sandboxes.fork(id, { name, sourcePolicy, private?, tailscaleAuthKey?, wait? })` | `Sandbox`: the new box (ready; private: running; `wait: false`: as accepted)   | `POST /sandbox/:id/fork`, then the new box until ready |
| `zb.sandboxes.getFork(id)`                                                          | `SandboxFork` of the new box `id`                                              | `GET /sandbox/:id/fork`                                |
| `zb.sandboxes.setTier(id, tier)`                                                    | `void`                                                                         | `POST /sandbox/:id/tier`                               |
| `zb.sandboxes.delete(id, { archive? })`                                             | `void`                                                                         | `DELETE /sandbox/:id`                                  |
| `zb.sandbox(id)`                                                                    | `Sandbox` handle, no network call                                              |                                                        |

Every wait (`create`, `getOrCreate`, `waitReady`, `fork`) reads the box's [`readiness`](https://zipbox.ai/docs/http-api.md#status-phase-and-readiness): it keeps reading while the box is `settling`, with no deadline, and a read that fails with a retryable error stays on the same ladder. It returns on `ready`, or on `awaits_tailnet` for a private box, and raises on the rest, never retryable: paused `box_not_running`, a failed start `start_failed`, deleting or gone `conflict`. A box call on a private box that has not joined a tailnet raises `tailnet_not_connected`. Every row carries `phase` and `allowedActions`; a detail also carries `readiness`. Against a control plane that does not send `readiness` yet, the SDK reads `ready` and `status` as before.

A created box's `baseUrl` changes once, to `box--<name>`, a few minutes after create, and the SDK follows it. A box call that fails before anything was sent (connect, DNS, TLS handshake) waits one step of its backoff ladder (500 ms doubling to 30 s, jitter), re-reads the box with the same wait `create` uses, and retries on the address that read returned, with no attempt ceiling; the ladder starts over only when the box answers. A box that reads paused, failed or deleted ends the call with that wait's error (`box_not_running`, `start_failed`, `conflict`). A PTY whose dial fails re-reads the box the same way before it redials. A request that was sent is never repeated this way.

`getOrCreate` takes every `create` option except `name` and applies them only when it creates the box. A box that already has the name is returned as it is, even when it differs (another tier, not private): that is not an error and nothing is resized.

```ts
const box = await zb.sandboxes.getOrCreate('ci-runner', {
  tier: 'l',
  mailbox: true
})
```

A `Sandbox` has `id`, the getters `name`, `status`, `tier`, `ready`, `baseUrl` and `ipv6`, `info` (the last `SandboxDetail` read, with every documented row field such as `cpuPct`), `refresh()`, `waitReady()`, `start()` / `stop()` / `restart()` / `cancelTransition()` / `setTier(tier)` / `delete({ archive? })`, `fork({ name, sourcePolicy, … })` / `forkStatus()`, and the methods below. Its box token and address are fetched on first use, cached for the life of the client, re-fetched once on a `401`, and re-read on a box call nothing was sent for (above).

### Fork

`fork` makes a new box from a consistent capture of the source's disk: its files, software and settings, with a new name and identity ([Fork a box](https://zipbox.ai/docs/fork.md)). `sourcePolicy` is required: `'require_paused'` refuses a running source with `pause_required`, `'pause_and_resume'` pauses it for the capture and resumes it right after. One `Idempotency-Key` per call is reused on every retry. `tailscaleAuthKey` stays on your machine: it is sealed to the new box once that box runs, never part of the fork request, and needs `wait` (default `true`). A new box its fork removed (a cancel or a failure) raises its fork's error code, or `conflict` when the fork was cancelled. A `signal` stops only the wait; cancel the fork with `cancelTransition(child.id)`.

```ts
const child = await box.fork({
  name: 'ci-runner-2',
  sourcePolicy: 'pause_and_resume'
})
```

## Tailscale and private mode

| Method                           | Returns                   | Route                                                  |
| -------------------------------- | ------------------------- | ------------------------------------------------------ |
| `box.tailscale.status()`         | `TailscaleStatus \| null` | `GET /sandbox/:id/tailscale`                           |
| `box.tailscale.connect(authKey)` | `void`                    | `GET /sandbox/:id`, seal, `PUT /sandbox/:id/tailscale` |
| `box.tailscale.disconnect()`     | `void`                    | `DELETE /sandbox/:id/tailscale`                        |
| `box.setPrivate(enabled)`        | `void`                    | `PUT /sandbox/:id/private`                             |

`connect` reads the box's `agentPublicKey` fresh, seals the auth key with WebCrypto on your machine, and sends only the envelope; on `box_key_changed` it re-reads the key and re-seals once. `TailscaleStatus` is `{ state, tailnet, ipv4, ipv6, dnsName, keyExpiry, pending, peers }`, where `peers` is your other devices (`{ name, dnsName, os, online, ipv4, connection }`). `info.tailscaleSupported` is `false` when the box's ZipOS predates Tailscale; `connect` then throws `tailscale_requires_update`. With `private: true`, `create` returns once the box is running with its key, because a private box is not `ready` until it joins your tailnet. On a private box, box calls from off your tailnet throw `box_private_unreachable`. See [Tailscale and private boxes](https://zipbox.ai/docs/tailscale.md).

```ts
const box = await zb.sandboxes.create({ name: 'build', private: true })
await box.tailscale.connect(process.env.TS_AUTHKEY!)
// 'joining', then 'connected'
console.log((await box.tailscale.status())?.state)
```

## Commands

| Method                                                                  | Returns                                                                                |
| ----------------------------------------------------------------------- | -------------------------------------------------------------------------------------- |
| `box.exec(cmd, { cwd?, env?, user?, timeoutMs?, signal? })`             | `{ id, exitCode, stdout, stderr }`                                                     |
| `box.execStream(cmd, opts?)`                                            | `AsyncIterable<ExecEvent>`                                                             |
| `box.execBackground(cmd, { stdin?, ... })`                              | `Command` (`id`, `get()`, `events()`, `wait()`, `stdin()`, `closeStdin()`, `signal()`) |
| `box.runCode(language, code, { cwd?, env?, user?, timeoutMs? })`        | same as `exec`; `language` is `python`, `node` (ES module) or `bash`                   |
| `box.commands.handle(id)`                                               | `Command` handle, no network call                                                      |
| `box.commands.get(id)`                                                  | `CommandRecord`                                                                        |
| `box.commands.list()`                                                   | `CommandRecord[]`                                                                      |
| `box.commands.events(id, { since? })`                                   | `AsyncIterable<ExecEvent>` to the exit; re-attaches after a drop                       |
| `box.commands.stdin(id, data)` / `box.commands.closeStdin(id)`          | `void`                                                                                 |
| `box.commands.signal(id, 'TERM' \| 'KILL' \| 'INT' \| 'HUP' \| 'QUIT')` | `void`                                                                                 |

`cmd` is a string (run with `bash -lc`) or an argv array (no shell). A stream that drops after the command started re-attaches from its last cursor by itself, with capped backoff and no attempt limit. An `ExecEvent` is `{ type: 'start' | 'stdout' | 'stderr' | 'gap' | 'exit', cursor, ... }`.

```ts
for await (const event of box.execStream('npm run build')) {
  if (event.type === 'stdout') process.stdout.write(event.data)
  if (event.type === 'exit') console.log('exit', event.exitCode)
}
```

Re-attach later from the last cursor you saw:

```ts
for await (const event of box.commands.events(cmdId, { since: lastCursor })) {
  lastCursor = event.cursor
}
```

## Files

| Method                                              | Returns       |
| --------------------------------------------------- | ------------- |
| `box.files.read(path)`                              | `Uint8Array`  |
| `box.files.readText(path)`                          | `string`      |
| `box.files.write(path, data: string \| Uint8Array)` | `void`        |
| `box.files.list(path)`                              | `FileEntry[]` |
| `box.files.stat(path)`                              | `FileEntry`   |
| `box.files.mkdir(path)`                             | `void`        |
| `box.files.move(from, to)`                          | `void`        |
| `box.files.copy(from, to)`                          | `void`        |
| `box.files.delete(path)`                            | `void`        |
| `box.files.upload(localPath, remotePath)`           | `void` (Node) |
| `box.files.download(remotePath, localPath)`         | `void` (Node) |

Relative paths resolve under `/root/workspace`.

## PTY

```ts
const pty = await box.pty.create({ cols: 120, rows: 40 })
pty.onData((data) => process.stdout.write(data))
pty.write('ls -la\r')
pty.resize(160, 48)
// later, from anywhere:
const again = await box.pty.attach(pty.sessionId)
await again.close()
```

| Method                                                      | Does                                                  |
| ----------------------------------------------------------- | ----------------------------------------------------- |
| `box.pty.create({ cols?, rows?, clientLabel? })`            | Open a new shell session; no size = the box's default |
| `box.pty.attach(sessionId, { cols?, rows?, clientLabel? })` | Re-attach a session; the screen replays               |
| `pty.takeover()`                                            | Take the session over from another client             |
| `pty.write(data)`                                           | Send input                                            |
| `pty.resize(cols, rows)`                                    | Resize                                                |
| `pty.onData(fn)` / `pty.onExit(fn)`                         | Output and exit                                       |
| `pty.onEnd(fn)`                                             | The PTY stopped for good, with a reason               |
| `pty.detach()`                                              | Disconnect; the session keeps running                 |
| `pty.close()`                                               | Kill the session                                      |

A dropped socket reconnects by itself with exponential backoff (`ptyBaseDelayMs`, capped at `ptyMaxDelayMs`, jitter) and no attempt limit; the backoff resets when a frame arrives.

## Previews, logs, SSH, account

| Method                                 | Returns                          |
| -------------------------------------- | -------------------------------- |
| `box.preview.create(port, { label? })` | `{ url, label }`                 |
| `box.preview.remove(label)`            | `void`; a label or the full host |
| `box.logs({ since?, lines?, unit? })`  | `string` (journal entries)       |
| `box.sshInfo()`                        | `{ host, user, command }`        |
| `box.screenshot()`                     | `Uint8Array` PNG of the desktop  |
| `zb.account.balance()`                 | `/billing/summary` as an object  |
| `zb.account.usage()`                   | `/billing/usage` as an object    |
| `zb.account.whoami()`                  | `{ userId, apiKey }`             |

## Errors

Every failure throws a `ZipboxError` with `code`, `status`, `message`, `requestId`, `retryable`, `retryAfterMs` and `docsUrl`. A non-zero exit code is a result, not an error.

```ts
import { ZipboxError } from '@zipbox/sdk'

try {
  await zb.sandboxes.start(id)
} catch (error) {
  if (error instanceof ZipboxError && error.code === 'insufficient_credits') {
    console.log('Top up first:', error.docsUrl)
  } else throw error
}
```

Codes are listed on [Errors and limits](https://zipbox.ai/docs/errors.md).

## Cancellation

Every method takes an optional `{ signal }` (`AbortSignal`). Aborting `exec` disconnects the stream, which stops a foreground command; use `execBackground` for work that must outlive your process.

```ts
const controller = new AbortController()
setTimeout(() => controller.abort(), 60_000)
await box.exec('npm test', { signal: controller.signal })
```
