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
const box = await zb.sandboxes.getOrCreate('ts-quickstart')

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
  retryBaseDelayMs: 500, // doubles per attempt, capped at 30 s, with jitter
  pollIntervalMs: 1000 // first readiness check in create; grows to 10 s
  // also: fetch, WebSocket
})
```

No request has a timeout unless you pass an `AbortSignal`.

## Boxes

| Method                                                 | Returns                           | Route                                      |
| ------------------------------------------------------ | --------------------------------- | ------------------------------------------ |
| `zb.sandboxes.create({ name?, tier?, sshPublicKey? })` | `Sandbox` (ready)                 | claim, then `GET /sandbox/:id` until ready |
| `zb.sandboxes.getOrCreate(name, opts?)`                | `Sandbox`                         | `GET /sandbox`, then create                |
| `zb.sandboxes.list()`                                  | `SandboxInfo[]`                   | `GET /sandbox`                             |
| `zb.sandboxes.get(id)`                                 | `Sandbox`                         | `GET /sandbox/:id`                         |
| `zb.sandboxes.start(id)` / `stop(id)` / `restart(id)`  | `void`                            | restore / pause / reset                    |
| `zb.sandboxes.setTier(id, tier)`                       | `void`                            | `POST /sandbox/:id/tier`                   |
| `zb.sandboxes.delete(id, { archive? })`                | `void`                            | `DELETE /sandbox/:id`                      |
| `zb.sandbox(id)`                                       | `Sandbox` handle, no network call |                                            |

A `Sandbox` has `id`, the getters `name`, `status`, `tier`, `ready`, `baseUrl` and `ipv6`, `info` (the last `SandboxDetail` read, with every documented row field such as `cpuPct`), `refresh()`, `start()` / `stop()` / `restart()` / `setTier(tier)` / `delete()`, and the methods below. Its box token is fetched on first use, cached for the life of the client, and re-fetched once on a `401`.

## Commands

| Method                                                                  | Returns                                                                                |
| ----------------------------------------------------------------------- | -------------------------------------------------------------------------------------- |
| `box.exec(cmd, { cwd?, env?, user?, timeoutMs?, signal? })`             | `{ id, exitCode, stdout, stderr }`                                                     |
| `box.execStream(cmd, opts?)`                                            | `AsyncIterable<ExecEvent>`                                                             |
| `box.execBackground(cmd, { stdin?, ... })`                              | `Command` (`id`, `get()`, `events()`, `wait()`, `stdin()`, `closeStdin()`, `signal()`) |
| `box.runCode(language, code)`                                           | same as `exec`; `language` is `python`, `node` or `bash`                               |
| `box.commands.get(id)`                                                  | `CommandRecord`                                                                        |
| `box.commands.list()`                                                   | `CommandRecord[]`                                                                      |
| `box.commands.events(id, { since? })`                                   | `AsyncIterable<ExecEvent>`, resumable                                                  |
| `box.commands.stdin(id, data)` / `box.commands.closeStdin(id)`          | `void`                                                                                 |
| `box.commands.signal(id, 'TERM' \| 'KILL' \| 'INT' \| 'HUP' \| 'QUIT')` | `void`                                                                                 |

`cmd` is a string (run with `bash -lc`) or an argv array (no shell). An `ExecEvent` is `{ type: 'start' | 'stdout' | 'stderr' | 'gap' | 'exit', cursor, ... }`.

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

| Method                              | Does                                    |
| ----------------------------------- | --------------------------------------- |
| `box.pty.create({ cols, rows })`    | Open a new shell session                |
| `box.pty.attach(sessionId)`         | Re-attach a session; the screen replays |
| `pty.write(data)`                   | Send input                              |
| `pty.resize(cols, rows)`            | Resize                                  |
| `pty.onData(fn)` / `pty.onExit(fn)` | Output and exit                         |
| `pty.onEnd(fn)`                     | The PTY stopped for good, with a reason |
| `pty.detach()`                      | Disconnect; the session keeps running   |
| `pty.close()`                       | Kill the session                        |

A dropped socket reconnects by itself with exponential backoff (cap 30 s, jitter) and no attempt limit; the backoff resets when a frame arrives.

## Previews, logs, SSH, account

| Method                                 | Returns                         |
| -------------------------------------- | ------------------------------- |
| `box.preview.create(port, { label? })` | `{ url, label }`                |
| `box.preview.remove(label)`            | `void`                          |
| `box.logs({ since?, lines? })`         | `string` (journal entries)      |
| `box.sshInfo()`                        | `{ host, user, command }`       |
| `box.screenshot()`                     | `Uint8Array` PNG of the desktop |
| `zb.account.balance()`                 | `/billing/summary` as an object |
| `zb.account.usage()`                   | `/billing/usage` as an object   |
| `zb.account.whoami()`                  | `{ userId, apiKey }`            |

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
