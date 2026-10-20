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
box = zb.sandboxes.get_or_create("py-quickstart")

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
        box = await zb.sandboxes.get_or_create("py-quickstart")
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
)
```

No request has a timeout. To bound one, pass your own `http_client=httpx.Client(timeout=…)` (`httpx.AsyncClient` for `AsyncZipbox`), or `timeout_ms=` on a command. Both clients are context managers (`with Zipbox() as zb:`) and have `close()`.

## Boxes

| Method                                                             | Returns                           |
| ------------------------------------------------------------------ | --------------------------------- |
| `zb.sandboxes.create(name=None, tier=None, ssh_public_key=None)`   | `Sandbox`, ready                  |
| `zb.sandboxes.get_or_create(name, tier=None, ssh_public_key=None)` | `Sandbox`                         |
| `zb.sandboxes.list()`                                              | `list[SandboxInfo]`               |
| `zb.sandboxes.get(id)`                                             | `Sandbox`                         |
| `zb.sandboxes.start(id)` / `stop(id)` / `restart(id)`              | `None`                            |
| `zb.sandboxes.set_tier(id, tier)`                                  | `None`                            |
| `zb.sandboxes.delete(id)`                                          | `None`                            |
| `zb.sandbox(id)`                                                   | `Sandbox` handle, no network call |

## Commands

| Method                                                          | Returns                                                    |
| --------------------------------------------------------------- | ---------------------------------------------------------- |
| `box.exec(cmd, cwd=None, env=None, user=None, timeout_ms=None)` | `ExecResult(id, exit_code, stdout, stderr)`                |
| `box.exec_stream(cmd, ...)`                                     | iterator of `ExecEvent` (async iterator on `AsyncZipbox`)  |
| `box.exec_background(cmd, stdin=False, ...)`                    | `Command` (`id`, `info`, `get()`, `events()`, `wait()`, …) |
| `box.run_code(language, code)`                                  | `ExecResult`; `"python"`, `"node"` or `"bash"`             |
| `box.commands.get(id)` / `list()`                               | `CommandInfo` / `list[CommandInfo]`                        |
| `box.commands.events(id, since=None)`                           | iterator of `ExecEvent`, resumable                         |
| `box.commands.stdin(id, data)` / `close_stdin(id)`              | `None`                                                     |
| `box.commands.signal(id, signal="TERM")`                        | `None`                                                     |

`cmd` is a string (`bash -lc`) or a list (argv, no shell).

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

## Previews, logs, SSH, account

| Method                                 | Returns                        |
| -------------------------------------- | ------------------------------ |
| `box.preview.create(port, label=None)` | `Preview(url, label)`          |
| `box.preview.remove(label)`            | `None`                         |
| `box.logs(since=None, lines=None)`     | `str`                          |
| `box.ssh_info()`                       | `SshInfo(host, user, command)` |
| `zb.account.balance()` / `usage()`     | `dict`                         |
| `zb.account.whoami()`                  | `dict` (`userId`, `apiKey`)    |

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
