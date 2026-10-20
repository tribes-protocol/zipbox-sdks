---
name: zipbox-sdk
description: >-
  Drive Zipbox boxes (long-lived Linux machines) from code or a terminal: create or
  reuse a box, run commands and stream their output, start background jobs and
  re-attach, read and write files, open a PTY, publish a port as a public HTTPS
  preview, read logs, stop or delete the box. Covers the TypeScript, Python, Go and
  Rust SDKs, the `zipbox` CLI, the MCP server and plain HTTP. Load it whenever a task
  says Zipbox, a box or sandbox to create or drive, ZIPBOX_API_KEY, @zipbox/sdk,
  zipbox-go, or the zipbox crate — including an agent on one box driving another.
allowed-tools: bash read
---

# Zipbox SDK

Zipbox gives you machines ("boxes") you drive from code. A box is a real, long-lived
Linux machine with root, a public IPv6 address, its own `<name>.zbox.sh` domain and a
desktop. **Nothing on a box stops on a clock** — no idle timeout, no auto-stop, no
default command deadline. It runs until someone stops it.

One credential does everything: an **account API key** (`zb_…`), read from
`ZIPBOX_API_KEY`. The SDKs, the CLI and the MCP server use it to reach the control
plane and fetch per-box tokens; you never handle the box tokens yourself.

The `references/` directory holds the full docs pages as markdown, generated from
zipbox.ai/docs. When this file and a reference disagree, the reference wins.

## Preflight

Run these before writing any code:

```sh
# the key: prints it if set, nothing if missing
printenv ZIPBOX_API_KEY
# the CLI, if you will use it
zipbox whoami
# the Python SDK
python3 -c "import zipbox"
# the TypeScript SDK
node -e "require.resolve('@zipbox/sdk')"
```

- **No key?** Ask the user to create one at zipbox.ai → **Settings → API keys** (it
  is shown once) and to make it available as `ZIPBOX_API_KEY`. On a Zipbox box, the
  user adds it as a **Key = value** variable in the box's **Settings → Provider
  keys**; it then appears in every new shell. Never ask for the key in chat, and never
  write it into a file in a repository.
- **Not on a Zipbox box?** Install the piece you need: `pip install zipbox`,
  `npm install @zipbox/sdk`, `go get github.com/tribes-protocol/zipbox-go`,
  `cargo add zipbox`, or the CLI with `curl -fsSL https://zipbox.ai/install.sh | sh`.
- **Inside a box, `zipbox` may already be a different command** (the box's own tool:
  `zipbox keys`, `zipbox-os`). Check `zipbox --help` — the account CLI has `sandbox`,
  `exec` and `files` subcommands.
- A box needs credit to boot. `402 insufficient_credits` means the user must top up
  in the dashboard; you cannot buy credit with a key.

## Pick the tool

| Situation                                     | Use                                      |
| --------------------------------------------- | ---------------------------------------- |
| Writing a program or a script                 | An SDK (Python shown first below)        |
| One-off commands in a terminal, CI steps      | The `zipbox` CLI                         |
| You are an MCP client (Claude, Codex, Cursor) | The MCP tools at `https://mcp.zipbox.ai` |
| A language with no SDK                        | Plain HTTP — `references/http-api.md`    |

## Python essentials

```python
from zipbox import Zipbox, ZipboxError

zb = Zipbox()                                   # reads ZIPBOX_API_KEY
# reuse by name; created + ready the first time
box = zb.sandboxes.get_or_create("worker")

# bash -lc; returns, never raises, on non-zero exit
r = box.exec("python3 --version")
print(r.exit_code, r.stdout, r.stderr)

# relative paths are under /root/workspace
box.files.write("notes/plan.md", "# plan\n")
print(box.files.read_text("notes/plan.md"))
print([e.name for e in box.files.list("notes")])

for event in box.exec_stream("pytest -q"):      # live output
    if event.type in ("stdout", "stderr"):
        print(event.data, end="")

cmd = box.exec_background("npm run dev -- --port 3000 --host 127.0.0.1")
# public https://app.<box host>
print(box.preview.create(3000, label="app").url)

# disk archived, bills nothing; start() brings it back
zb.sandboxes.stop(box.id)
```

Async code: `from zipbox import AsyncZipbox`, then the same calls with `await`
(`async with AsyncZipbox() as zb:`).

Other Python calls you will need: `zb.sandboxes.list()`, `get(id)`, `create(name=,
tier=)`, `start(id)`, `restart(id)`, `set_tier(id, tier)`, `delete(id)`;
`box.run_code("python", code)`; `box.commands.get/events/stdin/signal`;
`box.files.read/stat/mkdir/move/delete/upload/download`; `box.pty.create()`;
`box.logs(since="10 min ago")`; `box.ssh_info()`; `zb.account.balance()`.

### TypeScript delta

```ts
import { Zipbox } from '@zipbox/sdk'
const zb = new Zipbox()
const box = await zb.sandboxes.getOrCreate('worker')
const { exitCode, stdout } = await box.exec('node --version')
for await (const e of box.execStream('npm test'))
  if (e.type === 'stdout') process.stdout.write(e.data)
await box.files.write('a.txt', 'hi')
await box.files.readText('a.txt')
const { url } = await box.preview.create(3000)
```

camelCase names (`getOrCreate`, `execBackground`, `runCode`, `setTier`, `sshInfo`),
promises everywhere, `AbortSignal` via `{ signal }`. Node 20+ (PTYs on Node 20 need a
`WebSocket` constructor passed in). Never put a key in a browser page.

### Go delta

```go
c := zipbox.New("")                          // "" reads ZIPBOX_API_KEY
box, _ := c.Sandboxes.GetOrCreate(ctx, "worker")
res, _ := box.Exec(ctx, "go version")        // res.ExitCode, res.Stdout
_ = box.Files.Write(ctx, "a.txt", strings.NewReader("hi"))
p, _ := box.Preview.Create(ctx, 3000, "")    // p.URL; "" label = p3000
```

Every call takes a `context.Context`; cancelling it disconnects a stream. Errors are
`*zipbox.Error` (use `errors.As`).

### Rust delta

```rust
let zb = zipbox::Zipbox::from_env()?;
// `box` is a Rust keyword
let sbx = zb.sandboxes().get_or_create("worker").await?;
// res.exit_code, res.stdout
let res = sbx.exec("uname -a").await?;
sbx.files().write("a.txt", "hi").await?;
let url = sbx.preview().create(3000, None).await?.url;
```

Tokio; streams implement `futures::Stream` (pin them with `Box::pin` before
polling); every failure is one `zipbox::Error` struct with a `code: ErrorCode`.
`res.exit_code` is an `Option<i32>`.

### CLI

```sh
# create a box and wait until it is ready
zipbox sandbox create --name worker
# run a command; exits with the remote exit code
zipbox exec worker --cwd app -- make test
zipbox exec worker --background -- npm run dev
zipbox files put worker ./local.txt remote.txt
zipbox files cat worker remote.txt
zipbox preview worker 3000
zipbox sandbox stop worker
zipbox docs box-api                     # any docs page as markdown
```

Add `--json` to any command for machine-readable output.

## Common patterns

### Long-running job with re-attach

Start it in the background, keep its id, come back later from a cursor:

```python
cmd = box.exec_background("python3 train.py")
# ... later, maybe from another process:
for event in box.commands.events(cmd.id, since="0-0"):
    if event.type == "exit":
        print("finished with", event.exit_code)
```

A background command survives your client disconnecting and a restart of the box's
agent. A foreground `exec` is killed if you disconnect — use background for anything
that must outlive the call. Output is kept on the box (64 MiB per stream).

### Preview a dev server

1. Bind the server to `127.0.0.1:<port>` and start it with `exec_background`.
2. `box.preview.create(port, label="app")` → `https://app.<box host>`.
3. Previews are **public**: anyone with the URL can open them. Tell the user.
4. `box.preview.remove("app")` when done.

### Drive another box from this one

You are an agent on box A and need a clean machine for a job:

1. Preflight: `ZIPBOX_API_KEY` must be set on box A (see Preflight).
2. `zipbox sandbox create --name job-1` (or `get_or_create("job-1")`).
3. Run the work there with `exec`; copy results back with `files.read` / `zipbox files cat`.
4. **Stop it** (`zipbox sandbox stop job-1`) — nothing stops it for you, and a
   running box bills by the hour. Delete it if it will never be reused.

### Git repositories

Private GitHub repositories clone without a token in your code if the user connected
GitHub in zipbox.ai Settings: `box.exec("git clone https://github.com/org/repo")`.

### Run a snippet

`box.run_code("python", source)` writes the code to a temp file and runs it with
`python3` (also `"node"`, `"bash"`). It returns an exec result.

## Rules that save you a retry

- **Exit codes are results.** Check `exit_code`; a failing command does not raise.
- **Box not ready → `box_not_running`.** A box from `get`/`list` may be stopped:
  call `start(id)` and retry.
- **Stop what you start.** No idle timeout exists to clean up after you.
- **Relative paths** resolve under `/root/workspace`; raw HTTP box routes need
  absolute paths.
- **Do not retry `exec` blindly** — it runs the command again. The SDKs only retry
  safe requests.
- **Files over 250 MiB** fail with `invalid_request`; move them with `exec`
  (`curl`, `git`, `rsync`) instead.
- **Keys cannot** mint keys, approve OAuth apps, buy credit or connect GitHub — send
  the user to the dashboard for those.

## Limits

| Limit                               | Value                                           |
| ----------------------------------- | ----------------------------------------------- |
| Control-plane requests per key      | 600 / minute (`429` with `Retry-After`)         |
| Running commands per box            | 32 (`429` beyond)                               |
| Concurrent file transfers per box   | 8                                               |
| File read/write per request         | 250 MiB                                         |
| Output kept per command stream      | 64 MiB, then a `gap` event                      |
| Output kept per box                 | 512 MiB, oldest finished evicted first          |
| MCP tool result                     | 32 KiB shown; the rest via `get_command_output` |
| Box lifetime, command duration, PTY | Unlimited                                       |

Not in this version: named snapshots/templates (reuse a box by name instead), volumes,
LSP, outbound network policy, per-key scopes, memory pause/resume, file watch,
private previews.

## Errors

Every error carries a `code`; branch on it, not on the message:

| Code                          | Do                                                     |
| ----------------------------- | ------------------------------------------------------ |
| `unauthorized`                | Key missing or deleted: check `ZIPBOX_API_KEY`         |
| `insufficient_credits`        | Ask the user to add credit                             |
| `forbidden`                   | Dashboard-only action; send the user there             |
| `not_found`                   | Wrong box id, command id or path                       |
| `conflict`                    | Wrong state (e.g. already running): read the box first |
| `box_not_running`             | Start the box, then retry                              |
| `rate_limited`, `unavailable` | Wait (`retry_after_ms`) and retry                      |
| `invalid_request`             | Fix the request                                        |

Each error's `docs_url` / `docsUrl` points at the matching section of
`references/errors.md`.

## I need to… → read this

| I need to…                                             | Read                                                                                                 |
| ------------------------------------------------------ | ---------------------------------------------------------------------------------------------------- |
| Get started from nothing                               | `references/quickstart.md`                                                                           |
| Understand keys, revocation, rate limits               | `references/api-keys.md`                                                                             |
| Create, stop, start, resize, delete boxes              | `references/lifecycle.md`                                                                            |
| Stream, background, stdin, signals, files, PTY         | `references/box-api.md`                                                                              |
| Publish a port                                         | `references/previews.md`                                                                             |
| Read system logs                                       | `references/logs.md`                                                                                 |
| Call the API with curl or from another language        | `references/http-api.md`                                                                             |
| Exact method lists per language                        | `references/sdk-python.md`, `references/sdk-ts.md`, `references/sdk-go.md`, `references/sdk-rust.md` |
| Use the CLI, SSH, the desktop                          | `references/cli.md`                                                                                  |
| Connect an MCP client, the tool list                   | `references/mcp.md`, `references/oauth.md`                                                           |
| Framework integrations (AI SDK, LangChain, Agents SDK) | `references/examples.md`                                                                             |
| Decode an error or a limit                             | `references/errors.md`                                                                               |
| See everything Zipbox can and cannot do                | `references/api.md`                                                                                  |

Everything is also online: https://zipbox.ai/llms.txt (index), any page as markdown
at `https://zipbox.ai/docs/<page>.md`, and the API contract at
https://zipbox.ai/openapi.json.
