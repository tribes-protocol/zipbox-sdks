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

The same operations with the same parameters, defaults and semantics in every language,
in that language's idiom (`.cursor/rules/sdk-feature-and-param-parity.mdc`). A change to
one SDK's surface lands in all four, the docs and the conformance fixtures together.

| Operation                      | TypeScript                                                                               | Python                                                          | Go                                                                        | Rust                                                              |
| ------------------------------ | ---------------------------------------------------------------------------------------- | --------------------------------------------------------------- | ------------------------------------------------------------------------- | ----------------------------------------------------------------- |
| Client                         | `new Zipbox({ apiKey, baseUrl, maxRetries, … })`                                         | `Zipbox(api_key=, base_url=, max_retries=, …)` / `AsyncZipbox`  | `zipbox.New(apiKey, opts...)`                                             | `Zipbox::builder()…build()`                                       |
| Create (waits until ready)     | `zb.sandboxes.create({ name })`                                                          | `zb.sandboxes.create(name=)`                                    | `c.Sandboxes.Create(ctx, opts)`                                           | `zb.sandboxes().create(opts)`                                     |
| Get or create by name          | `zb.sandboxes.getOrCreate(name, { tier })`                                               | `zb.sandboxes.get_or_create(name, tier=)`                       | `c.Sandboxes.GetOrCreate(ctx, name, zipbox.CreateOptions{Tier})`          | `zb.sandboxes().get_or_create_with(name, opts)`                   |
| Wait until ready               | `zb.sandboxes.waitReady(id)`                                                             | `zb.sandboxes.wait_ready(id)`                                   | `c.Sandboxes.WaitReady(ctx, id)`                                          | `zb.sandboxes().wait_ready(id)`                                   |
| List / get                     | `list()` / `get(id)`                                                                     | `list()` / `get(id)`                                            | `List(ctx)` / `Get(ctx, id)`                                              | `list()` / `get(id)`                                              |
| Start / stop / restart         | `start(id)` …                                                                            | `start(id)` …                                                   | `Start(ctx, id)` …                                                        | `start(id)` …                                                     |
| Delete (optionally no archive) | `delete(id, { archive })`                                                                | `delete(id, archive=)`                                          | `Delete(ctx, id, zipbox.DeleteOptions{Archive})`                          | `delete_with(id, DeleteOptions { archive })`                      |
| Change tier                    | `setTier(id, tier)`                                                                      | `set_tier(id, tier)`                                            | `SetTier(ctx, id, tier)`                                                  | `set_tier(id, tier)`                                              |
| Run a command                  | `box.exec(cmd)`                                                                          | `box.exec(cmd)`                                                 | `box.Exec(ctx, cmd)`                                                      | `box.exec(cmd)`                                                   |
| Stream a command               | `box.execStream(cmd)` (AsyncIterable)                                                    | `box.exec_stream(cmd)` (iterator)                               | `box.ExecStream(ctx, cmd)`                                                | `box.exec_stream(cmd)` (`Stream`)                                 |
| Background command             | `box.execBackground(cmd)`                                                                | `box.exec_background(cmd)`                                      | `box.ExecBackground(ctx, cmd)`                                            | `box.exec_background(cmd)`                                        |
| Run code                       | `box.runCode(lang, code, { cwd })`                                                       | `box.run_code(lang, code, cwd=)`                                | `box.RunCode(ctx, lang, code, opts...)`                                   | `box.run_code_with(lang, code, opts)`                             |
| Command handle                 | `box.commands.handle/get/events/stdin/closeStdin/signal`                                 | `box.commands.handle/get/events/stdin/close_stdin/signal`       | `box.Command(id)`, `box.Commands.Get/Events/Stdin/CloseStdin/Signal`      | `box.commands().command/get/events/stdin/close_stdin/signal`      |
| Files                          | `box.files.read/write/list/stat/mkdir/move/copy/delete/upload/download`                  | same, snake_case                                                | `box.Files.Read/Write/…/Copy/…`                                           | `box.files().read/write/…/copy/…`                                 |
| PTY                            | `box.pty.create({ cols, rows })` / `attach(id)` → `write`, `resize`, `takeover`, `close` | same                                                            | `box.Pty.Create(ctx, opts)` …                                             | `box.pty().create(opts)` …                                        |
| Preview a port                 | `box.preview.create(port)` / `remove(label)`                                             | same                                                            | `box.Preview.Create(ctx, port, label)`                                    | `box.preview().create(port, label)`                               |
| Logs                           | `box.logs({ since, lines, unit })`                                                       | `box.logs(since=, lines=, unit=)`                               | `box.Logs(ctx, zipbox.LogOptions{Since, Lines, Unit})`                    | `box.logs(LogOptions { since, lines, unit })`                     |
| SSH line                       | `box.sshInfo()`                                                                          | `box.ssh_info()`                                                | `box.SSHInfo(ctx)`                                                        | `box.ssh_info()`                                                  |
| Account                        | `zb.account.balance()` / `usage()`                                                       | same                                                            | `c.Account.Balance(ctx)` …                                                | `zb.account().balance()` …                                        |
| Tailscale                      | `box.tailscale.status()` / `connect(authKey)` / `disconnect()`                           | `box.tailscale.status()` / `connect(auth_key)` / `disconnect()` | `box.Tailscale.Status(ctx)` / `Connect(ctx, authKey)` / `Disconnect(ctx)` | `box.tailscale().status()` / `connect(auth_key)` / `disconnect()` |
| Private mode                   | `box.setPrivate(enabled)`                                                                | `box.set_private(enabled)`                                      | `box.SetPrivate(ctx, enabled)`                                            | `box.set_private(enabled)`                                        |

`box` is the handle returned by `create`, `get`, `getOrCreate` or `zb.sandbox(id)`. `create`
also takes `private` (a box that is never public); it then returns once the box is running,
because a private box is not ready until it joins a tailnet. `getOrCreate` takes every
`create` option except the name and applies them only when it creates the box: a box that
already has the name is returned as it is (a different tier is not an error and is not
resized). An account holds 250 machine names and a deleted box keeps its name, so reuse a
fixed pool of names with get-or-create rather than a fresh name per job; a new name past the
limit raises `machine_limit_reached` ([zipbox.ai/docs/machine-limit](https://zipbox.ai/docs/machine-limit)).

`tailscale.connect` seals the auth key on the caller's machine to the box's own
`agentPublicKey` (P-256 ECDH → HKDF-SHA256 → AES-256-GCM, the same envelope the dashboard
seals) and sends only the ciphertext, so Zipbox never holds the key. Every SDK reproduces the
fixed-key vector in `conformance/vectors/box_envelope.json` byte for byte.

## Errors

Every failure is one error type carrying `code` (`invalid_request`, `unauthorized`,
`forbidden`, `not_found`, `insufficient_credits`, `conflict`, `rate_limited`,
`box_not_running`, `unavailable`, `machine_limit_reached`, `box_key_changed`, `tailnet_not_connected`,
`tailscale_unavailable_egress_lockdown`, `tailscale_requires_update`, `box_private_unreachable`), `status`, `message`, `requestId`, `retryable`,
`retryAfterMs` and `docsUrl` (`https://zipbox.ai/docs/errors#<code>`). A command that
exits non-zero is a result, never an error.

## Layout

- `conformance/` — fixtures every SDK runs against a mock server.
- `examples/` — runnable projects (framework integrations, CI, agents driving boxes).
- `claude-plugin/` — the Zipbox plugin for Claude Code: the hosted MCP server
  (`.mcp.json`), a mod with the `/zipbox` pane and commands (`hooks/`, tested by
  `claude plugin test`), and the `zipbox-sdk` skill. Install it with
  `claude plugin marketplace add tribes-protocol/zipbox-sdks`, then
  `claude plugin install zipbox@zipbox-sdks`; see
  [zipbox.ai/docs/integrations](https://zipbox.ai/docs/integrations).
- `.claude-plugin/marketplace.json` — the marketplace that lists that plugin. The mirror
  export copies `harnesses/setup/skills/zipbox-sdk/` (with its generated `references/`)
  to `skills/zipbox-sdk/` for `npx skills add`, and into `claude-plugin/skills/zipbox-sdk/`
  for the plugin.

This tree is the only place the SDKs are edited. Releases export it to the public
mirror `tribes-protocol/zipbox-sdks` (Apache-2.0), which the package registries and Go
module resolution point at.
