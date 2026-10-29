> Zipbox docs, markdown edition of https://zipbox.ai/docs/sdk-python
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# Python SDK

The zipbox package - sync Zipbox and async AsyncZipbox clients, every method, streaming, PTY and errors.

`zipbox` is a typed client (it ships `py.typed`) over the [REST API](https://zipbox.ai/docs/http-api.md) and the [Box API](https://zipbox.ai/docs/box-api.md), built on `httpx` and `websockets`. Python 3.10+. Every method exists twice: on `Zipbox` (blocking) and on `AsyncZipbox` (`await`).

## Install

```sh
pip install zipbox
```

## Quickstart

Create a box, run a command, write and read a file, open a preview, stop the box.

```python
from zipbox import Zipbox

zb = Zipbox()  # or Zipbox(api_key="zb_...")
box = zb.sandboxes.get_or_create("py-quickstart", tier="s")

result = box.exec("python3 --version")
print(result.exit_code, result.stdout)

box.files.write("hello.txt", "hi from Python\n")
print(box.files.read_text("hello.txt"))

box.exec_background("python3 -m http.server 8000 --bind 127.0.0.1")
print(box.preview.create(8000).url)

zb.sandboxes.stop(box.id)
```

## Async

```python
import asyncio
from zipbox import AsyncZipbox

async def main() -> None:
    async with AsyncZipbox() as zb:
        box = await zb.sandboxes.get_or_create("py-quickstart", tier="s")
        result = await box.exec("echo hello")
        print(result.stdout)

asyncio.run(main())
```

## Client

```python
zb = Zipbox(
    # default: ZIPBOX_API_KEY
    api_key="zb_...",
    # default: ZIPBOX_API_URL, then this
    base_url="https://api.tribes.xyz",
    # retries rate_limited / unavailable on safe requests
    max_retries=3,
    # retry backoff: first delay and cap, with jitter
    retry_base_delay_s=0.5,
    retry_max_delay_s=30.0,
    # readiness checks in create: first delay and cap
    poll_interval_s=1.0,
    poll_max_interval_s=10.0,
    # PTY reconnects: first delay and cap (no attempt limit)
    pty_base_delay_s=0.5,
    pty_max_delay_s=30.0,
)
```

No request has a timeout. To bound one, pass your own `http_client=httpx.Client(timeout=…)` (`httpx.AsyncClient` for `AsyncZipbox`), or `timeout_ms=` on a command. Both clients are context managers (`with Zipbox() as zb:`) and have `close()`.

## Boxes

| Method                                                                                         | Returns                                            |
| ---------------------------------------------------------------------------------------------- | -------------------------------------------------- |
| `zb.sandboxes.create(name=None, tier=None, ssh_public_key=None, mailbox=None, private=None)`   | `Sandbox`, ready (with `private`, running)         |
| `zb.sandboxes.get_or_create(name, tier=None, ssh_public_key=None, mailbox=None, private=None)` | `Sandbox`                                          |
| `zb.sandboxes.wait_ready(id)`                                                                  | `Sandbox`, ready (private and not joined: running) |
| `zb.sandboxes.list()`                                                                          | `list[SandboxInfo]`                                |
| `zb.sandboxes.get(id)`                                                                         | `Sandbox`                                          |
| `zb.sandboxes.start(id)` / `stop(id)` / `restart(id)`                                          | `None`                                             |
| `zb.sandboxes.set_tier(id, tier)`                                                              | `None`                                             |
| `zb.sandboxes.delete(id, archive=None)`                                                        | `None`                                             |
| `zb.sandbox(id)`                                                                               | `Sandbox` handle, no network call                  |

`get_or_create` takes every `create` option except `name` and applies them only when it creates the box. A box that already has the name is returned as it is, even when it differs (another tier, not private): that is not an error and nothing is resized.

```python
box = zb.sandboxes.get_or_create("ci-runner", tier="l", mailbox=True)
```

A `Sandbox` has `id`, `info` (the last `SandboxDetail` read), `refresh()`, `wait_ready()`, `start()` / `stop()` / `restart()` / `set_tier(tier)` / `delete(archive=None)`, and the methods below.

## Tailscale and private mode

| Method                            | Returns                   |
| --------------------------------- | ------------------------- |
| `box.tailscale.status()`          | `TailscaleStatus \| None` |
| `box.tailscale.connect(auth_key)` | `None`                    |
| `box.tailscale.disconnect()`      | `None`                    |
| `box.set_private(enabled)`        | `None`                    |

`connect` reads the box's `agent_public_key` fresh, seals the auth key on your machine with `cryptography`, and sends only the envelope; on `box_key_changed` it re-reads the key and re-seals once. `TailscaleStatus` has `state`, `tailnet`, `ipv4`, `ipv6`, `dns_name`, `key_expiry`, `pending` and `peers` (a tuple of `TailscalePeer`: `name`, `dns_name`, `os`, `online`, `ipv4`, `connection`). `info.tailscale_supported` is `False` when the box's ZipOS predates Tailscale; `connect` then raises `tailscale_requires_update`. With `private=True`, `create` returns once the box is running with its key, because a private box is not ready until it joins your tailnet. On a private box, box calls from off your tailnet raise `box_private_unreachable`. `AsyncZipbox` has the same methods as coroutines. See [Tailscale and private boxes](https://zipbox.ai/docs/tailscale.md).

```python
box = zb.sandboxes.create(name="build", private=True)
box.tailscale.connect(os.environ["TS_AUTHKEY"])
```

## Commands

| Method                                                                         | Returns                                                       |
| ------------------------------------------------------------------------------ | ------------------------------------------------------------- |
| `box.exec(cmd, cwd=None, env=None, user=None, timeout_ms=None)`                | `ExecResult(id, exit_code, stdout, stderr)`                   |
| `box.exec_stream(cmd, ...)`                                                    | iterator of `ExecEvent` (async iterator on `AsyncZipbox`)     |
| `box.exec_background(cmd, stdin=False, ...)`                                   | `Command` (`id`, `info`, `get()`, `events()`, `wait()`, …)    |
| `box.run_code(language, code, cwd=None, env=None, user=None, timeout_ms=None)` | `ExecResult`; `"python"`, `"node"` (ES module) or `"bash"`    |
| `box.commands.handle(id)`                                                      | `Command` handle, no network call                             |
| `box.commands.get(id)` / `list()`                                              | `CommandInfo` / `list[CommandInfo]`                           |
| `box.commands.events(id, since=None)`                                          | iterator of `ExecEvent` to the exit; re-attaches after a drop |
| `box.commands.stdin(id, data)` / `close_stdin(id)`                             | `None`                                                        |
| `box.commands.signal(id, signal="TERM")`                                       | `None`                                                        |

`cmd` is a string (`bash -lc`) or a list (argv, no shell). A stream that drops after the command started re-attaches from its last cursor by itself, with capped backoff and no attempt limit.

```python
for event in box.exec_stream("pytest -q"):
    if event.type in ("stdout", "stderr"):
        print(event.data, end="")
    elif event.type == "exit":
        print("exit", event.exit_code)
```

## Files

| Method                                        | Returns           |
| --------------------------------------------- | ----------------- |
| `box.files.read(path)`                        | `bytes`           |
| `box.files.read_text(path)`                   | `str`             |
| `box.files.write(path, data: str \| bytes)`   | `None`            |
| `box.files.list(path)`                        | `list[FileEntry]` |
| `box.files.stat(path)`                        | `FileEntry`       |
| `box.files.mkdir(path)`                       | `None`            |
| `box.files.move(src, dst)`                    | `None`            |
| `box.files.copy(src, dst)`                    | `None`            |
| `box.files.delete(path)`                      | `None`            |
| `box.files.upload(local_path, remote_path)`   | `None`            |
| `box.files.download(remote_path, local_path)` | `None`            |

## PTY

```python
pty = box.pty.create(cols=120, rows=40)
pty.write("ls -la\r")
pty.resize(160, 48)
for chunk in pty:          # output as it arrives; reconnects by itself
    print(chunk, end="")
again = box.pty.attach(pty.session_id)
again.close()
```

`box.pty.create(cols=None, rows=None, client_label=None)` opens a session (no size = the box's default) and `box.pty.attach(session_id, cols=None, rows=None, client_label=None)` re-attaches one. A `Pty` has `session_id`, `write(data)`, `resize(cols, rows)`, `takeover()` (take the session over from another client), `on_data(fn)`, `wait()` (the exit code), `detach()` (leave the session running) and `close()` (kill it).

## Previews, logs, SSH, account

| Method                                        | Returns                          |
| --------------------------------------------- | -------------------------------- |
| `box.preview.create(port, label=None)`        | `Preview(url, label)`            |
| `box.preview.remove(label)`                   | `None`; a label or the full host |
| `box.logs(since=None, lines=None, unit=None)` | `str`                            |
| `box.ssh_info()`                              | `SshInfo(host, user, command)`   |
| `box.screenshot()`                            | `bytes` PNG of the desktop       |
| `zb.account.balance()` / `usage()`            | `dict`                           |
| `zb.account.whoami()`                         | `dict` (`userId`, `apiKey`)      |

## Errors

Every failure raises `zipbox.ZipboxError` with `code`, `status`, `message`, `request_id`, `retryable`, `retry_after_ms` and `docs_url`. A non-zero exit code is a result.

```python
from zipbox import ZipboxError

try:
    zb.sandboxes.start(box_id)
except ZipboxError as error:
    if error.code == "insufficient_credits":
        print("Top up first:", error.docs_url)
    else:
        raise
```
