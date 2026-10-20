# Zipbox SDKs

Drive [Zipbox](https://zipbox.ai) boxes from code: create a box, run commands, move
files, open a terminal, publish a port, stop it when you are done. Nothing on a box
stops on a clock.

| Language   | Package                                | Path            | Docs                                                           |
| ---------- | -------------------------------------- | --------------- | -------------------------------------------------------------- |
| TypeScript | `@zipbox/sdk` (npm)                    | `typescript/`   | [zipbox.ai/docs/sdk-ts](https://zipbox.ai/docs/sdk-ts)         |
| Python     | `zipbox` (PyPI)                        | `python/`       | [zipbox.ai/docs/sdk-python](https://zipbox.ai/docs/sdk-python) |
| Go         | `github.com/tribes-protocol/zipbox-go` | `go/`           | [zipbox.ai/docs/sdk-go](https://zipbox.ai/docs/sdk-go)         |
| Rust       | `zipbox` (crates.io)                   | `rust/`         | [zipbox.ai/docs/sdk-rust](https://zipbox.ai/docs/sdk-rust)     |
| CLI        | `zipbox` (Homebrew, curl, PowerShell)  | `go/cmd/zipbox` | [zipbox.ai/docs/cli](https://zipbox.ai/docs/cli)               |

Every SDK is a thin, hand-written client over one contract
(`packages/public-api-contract/openapi.json`, served at
[zipbox.ai/openapi.json](https://zipbox.ai/openapi.json)) and passes the same
[conformance suite](conformance/README.md).

## Authentication

Create an API key at zipbox.ai → Settings → API keys and export it:

```sh
export ZIPBOX_API_KEY=zb_…
```

The SDKs read `ZIPBOX_API_KEY` (and optionally `ZIPBOX_API_URL`) when no key is passed.
Box tokens — the per-box credential a box accepts — are fetched and cached by the
client; you never handle them.

## Surface

The same operations in every language, in that language's idiom.

| Operation                       | TypeScript                                                         | Python                                 | Go                                     | Rust                                     |
| ------------------------------- | ------------------------------------------------------------------ | -------------------------------------- | -------------------------------------- | ---------------------------------------- |
| Client                          | `new Zipbox({ apiKey })`                                           | `Zipbox(api_key=)` / `AsyncZipbox`     | `zipbox.New(apiKey)`                   | `Zipbox::new(api_key)`                   |
| Create (waits until ready)      | `zb.sandboxes.create({ name })`                                    | `zb.sandboxes.create(name=)`           | `c.Sandboxes.Create(ctx, opts)`        | `zb.sandboxes().create(opts)`            |
| Get or create by name           | `zb.sandboxes.getOrCreate(name)`                                   | `zb.sandboxes.get_or_create(name)`     | `c.Sandboxes.GetOrCreate(ctx, name)`   | `zb.sandboxes().get_or_create(name)`     |
| List / get                      | `list()` / `get(id)`                                               | `list()` / `get(id)`                   | `List(ctx)` / `Get(ctx, id)`           | `list()` / `get(id)`                     |
| Start / stop / restart / delete | `start(id)` …                                                      | `start(id)` …                          | `Start(ctx, id)` …                     | `start(id)` …                            |
| Change tier                     | `setTier(id, tier)`                                                | `set_tier(id, tier)`                   | `SetTier(ctx, id, tier)`               | `set_tier(id, tier)`                     |
| Run a command                   | `box.exec(cmd)`                                                    | `box.exec(cmd)`                        | `box.Exec(ctx, cmd)`                   | `box.exec(cmd)`                          |
| Stream a command                | `box.execStream(cmd)` (AsyncIterable)                              | `box.exec_stream(cmd)` (iterator)      | `box.ExecStream(ctx, cmd)`             | `box.exec_stream(cmd)` (`Stream`)        |
| Background command              | `box.execBackground(cmd)`                                          | `box.exec_background(cmd)`             | `box.ExecBackground(ctx, cmd)`         | `box.exec_background(cmd)`               |
| Run code                        | `box.runCode(lang, code)`                                          | `box.run_code(lang, code)`             | `box.RunCode(ctx, lang, code)`         | `box.run_code(lang, code)`               |
| Command handle                  | `box.commands.get/events/stdin/signal`                             | `box.commands.get/events/stdin/signal` | `box.Commands.Get/Events/Stdin/Signal` | `box.commands().get/events/stdin/signal` |
| Files                           | `box.files.read/write/list/stat/mkdir/move/delete/upload/download` | same, snake_case                       | `box.Files.Read/Write/…`               | `box.files().read/write/…`               |
| PTY                             | `box.pty.create()` / `attach(id)` → `write`, `resize`, `close`     | same                                   | `box.Pty.Create(ctx)` …                | `box.pty().create()` …                   |
| Preview a port                  | `box.preview.create(port)` / `remove(label)`                       | same                                   | `box.Preview.Create(ctx, port)`        | `box.preview().create(port)`             |
| Logs                            | `box.logs({ since })`                                              | `box.logs(since=)`                     | `box.Logs(ctx, since)`                 | `box.logs(since)`                        |
| SSH line                        | `box.sshInfo()`                                                    | `box.ssh_info()`                       | `box.SSHInfo(ctx)`                     | `box.ssh_info()`                         |
| Account                         | `zb.account.balance()` / `usage()`                                 | same                                   | `c.Account.Balance(ctx)` …             | `zb.account().balance()` …               |

`box` is the handle returned by `create`, `get`, `getOrCreate` or `zb.sandbox(id)`.

## Errors

Every failure is one error type carrying `code` (`invalid_request`, `unauthorized`,
`forbidden`, `not_found`, `insufficient_credits`, `conflict`, `rate_limited`,
`box_not_running`, `unavailable`), `status`, `message`, `requestId`, `retryable`,
`retryAfterMs` and `docsUrl` (`https://zipbox.ai/docs/errors#<code>`). A command that
exits non-zero is a result, never an error.

## Layout

- `conformance/` — fixtures every SDK runs against a mock server.
- `examples/` — runnable projects (framework integrations, CI, agents driving boxes).
- `.claude-plugin/marketplace.json` — the Claude Code plugin marketplace for the public
  mirror. It declares one plugin, `zipbox`, rooted at the mirror's top level, whose skill
  is discovered at `skills/zipbox-sdk/SKILL.md`. The mirror export copies
  `harnesses/setup/skills/zipbox-sdk/` (with its generated `references/`) to that path;
  `npx skills add tribes-protocol/zipbox-sdks --skill zipbox-sdk` reads the same copy.

This tree is the only place the SDKs are edited. Releases export it to the public
mirror `tribes-protocol/zipbox-sdks` (Apache-2.0), which the package registries and Go
module resolution point at.
