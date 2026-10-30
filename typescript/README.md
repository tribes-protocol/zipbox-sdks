# @zipbox/sdk

TypeScript client for [Zipbox](https://zipbox.ai): create a box, run commands, move
files, open a terminal, publish a port. Nothing on a box stops on a clock.

```sh
npm install @zipbox/sdk
export ZIPBOX_API_KEY=zb_…   # zipbox.ai → Settings → API keys
```

## Quickstart

```ts
import { Zipbox } from '@zipbox/sdk'

const zb = new Zipbox()
const box = await zb.sandboxes.getOrCreate('demo', { tier: 's' })
const { stdout, exitCode } = await box.exec('uname -a && node --version')
console.log(exitCode, stdout)
```

Node 20+ (`fetch`, `WebSocket` on Node 22+; on Node 20 pass `WebSocket` from the `ws`
package for PTYs). API keys are for servers and CLIs: the control plane refuses a key
sent from a browser page.

## Client

```ts
new Zipbox({
  apiKey, // default: ZIPBOX_API_KEY
  baseUrl, // default: ZIPBOX_API_URL, else https://api.tribes.xyz
  maxRetries, // retries of one read or create (default 3); followed streams and PTYs never give up
  retryBaseDelayMs, // first retry delay (default 500); doubles up to retryMaxDelayMs (30000)
  pollIntervalMs, // first readiness check in create (default 1000); grows to pollMaxIntervalMs (10000)
  ptyBaseDelayMs, // first PTY reconnect delay (default 500); doubles up to ptyMaxDelayMs (30000)
  fetch, // custom fetch
  WebSocket // WebSocket constructor for PTYs
})
```

Every method takes an optional `{ signal }` (`AbortSignal`). No method sets a deadline
of its own.

## Boxes

```ts
const box = await zb.sandboxes.create({ name: 'ci-runner', tier: 's' }) // waits until ready
// the options apply only when the box is created; an existing box comes back as it is
const same = await zb.sandboxes.getOrCreate('ci-runner', { tier: 's' })
const ready = await zb.sandboxes.waitReady(id)
const rows = await zb.sandboxes.list()
const one = await zb.sandboxes.get(id)
await zb.sandboxes.stop(id) // disk archived, nothing billed while stopped
await zb.sandboxes.start(id)
await zb.sandboxes.restart(id) // files survive
await zb.sandboxes.setTier(id, 'm')
await zb.sandboxes.delete(id) // keeps a recoverable copy for 30 days by default
await zb.sandboxes.delete(id, { archive: false }) // no recoverable copy
const handle = zb.sandbox(id) // no network call
```

`create`, `get` and `getOrCreate` return a `Sandbox`: the row (`box.info`, `box.status`,
`box.name`, `box.baseUrl`, …) plus everything below.

An account holds 250 machine names, and a deleted box keeps its name: reuse a fixed pool of
names with `getOrCreate` instead of a fresh name per job. A new name past the limit raises
`machine_limit_reached` ([zipbox.ai/docs/machine-limit](https://zipbox.ai/docs/machine-limit)).

## Commands

```ts
const r = await box.exec('npm test', { cwd: 'app', env: { CI: '1' } })
r.exitCode // a non-zero exit is a result, never an exception

for await (const event of box.execStream(['make', 'build'])) {
  if (event.type === 'stdout') process.stdout.write(event.data)
}

const server = await box.execBackground('npm run dev', { cwd: 'app' })
for await (const event of server.events()) {
  /* follows output, resumes after a drop */
}
await server.signal('TERM')

await box.runCode('python', 'print(2 + 2)', { cwd: 'app' })
```

`execStream` ends the command if you stop iterating; `execBackground` keeps running
with nobody attached and survives the box agent restarting. `box.commands.list()`,
`get(id)`, `events(id, { since })`, `stdin(id, data)`, `closeStdin(id)`, `signal(id, sig)`
work on any command by id, and `box.commands.handle(id)` returns its `Command`. `events`
follows the command to its exit and re-attaches after a drop.

## Files

Relative paths resolve against `/root/workspace`.

```ts
await box.files.write('notes/todo.md', '# todo\n')
const text = await box.files.readText('notes/todo.md')
const entries = await box.files.list('.')
await box.files.mkdir('out')
await box.files.move('notes/todo.md', 'out/todo.md')
await box.files.copy('out/todo.md', 'out/todo.bak')
await box.files.delete('out')
await box.files.upload('./local.tar.gz', '/tmp/local.tar.gz')
await box.files.download('/tmp/report.pdf', './report.pdf')
```

## Terminal

```ts
const pty = await box.pty.create({ cols: 120, rows: 32 })
pty.onData((data) => process.stdout.write(data))
pty.write('htop\n')
pty.resize(160, 40)
pty.takeover() // take the session from another client holding it
pty.detach() // leaves the shell running; come back with box.pty.attach(pty.sessionId)
await pty.close() // kills the shell
```

A dropped socket reconnects by itself (exponential backoff, capped at 30 s, no attempt
limit) and replays the screen.

## Previews, logs, SSH, screenshots

```ts
const { url } = await box.preview.create(3000) // https://p3000.<box host>; tailnet-only on a private box
await box.preview.remove('p3000')
const logs = await box.logs({ since: '10 min ago', lines: 100, unit: 'caddy' })
const { command } = await box.sshInfo() // ssh root@<ipv6>
const png = await box.screenshot()
```

## Tailscale and private boxes

```ts
await box.tailscale.connect(authKey) // a one-off, pre-approved Tailscale auth key
await box.tailscale.status() // { state, tailnet, ipv4, ipv6, dnsName, keyExpiry, pending, peers } | null
await box.setPrivate(true) // only devices on your tailnet can reach it
await box.setPrivate(false)
await box.tailscale.disconnect()

const secret = await zb.sandboxes.create({ name: 'build', private: true }) // never public
await secret.tailscale.connect(authKey)
```

`connect` seals the key on this machine to the box's own key (P-256 ECDH, HKDF-SHA256,
AES-256-GCM). Zipbox relays it once and can never read it. It resolves once the sealed
key is accepted; `status().state` becomes `connected` when the box joins (`pending`
dropping without that means the key never arrived: connect again with a new key).

A private box answers only on your tailnet. `create({ private: true })` returns once the
box is running with its key, because it is not `ready` until it joins. From a device off
the tailnet, every box call (exec, files, PTY, previews) fails fast with
`box_private_unreachable`; lifecycle calls (start, stop, `setPrivate`, `tailscale.*`)
always work.

## Account

```ts
await zb.account.balance()
await zb.account.usage()
```

## Errors

Every failure is a `ZipboxError` with `code` (`invalid_request`, `unauthorized`,
`forbidden`, `not_found`, `insufficient_credits`, `conflict`, `rate_limited`,
`box_not_running`, `unavailable`, `machine_limit_reached`, `box_key_changed`, `tailnet_not_connected`,
`tailscale_unavailable_egress_lockdown`, `tailscale_requires_update`, `box_private_unreachable`), `status`,
`requestId`, `retryable`, `retryAfterMs` and `docsUrl`.

```ts
try {
  await zb.sandboxes.start(id)
} catch (error) {
  if (error instanceof ZipboxError && error.code === 'insufficient_credits') {
    console.error(error.message, error.docsUrl)
  }
}
```

Docs: [zipbox.ai/docs/sdk-ts](https://zipbox.ai/docs/sdk-ts) · For agents:
[zipbox.ai/llms.txt](https://zipbox.ai/llms.txt) · License: Apache-2.0
