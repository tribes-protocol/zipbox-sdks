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

Create a box, run a command, write and read a file, open a preview, pause the box.

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

zb.sandboxes.pause(box.id)
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
    # readiness checks: first delay and cap; grows 1.25x per check, jittered
    poll_interval_s=1.0,
    poll_max_interval_s=10.0,
    # PTY reconnects: first delay and cap (no attempt limit)
    pty_base_delay_s=0.5,
    pty_max_delay_s=30.0,
)
```

No request has a timeout. To bound one, pass your own `http_client=httpx.Client(timeout=…)` (`httpx.AsyncClient` for `AsyncZipbox`), or `timeout_ms=` on a command. Both clients are context managers (`with Zipbox() as zb:`) and have `close()`.

## Boxes

| Method                                                                                           | Returns                                                                          |
| ------------------------------------------------------------------------------------------------ | -------------------------------------------------------------------------------- |
| `zb.sandboxes.create(name=None, tier=None, ssh_public_key=None, mailbox=None, private=None)`     | `Sandbox`, ready (with `private`, running)                                       |
| `zb.sandboxes.get_or_create(name, tier=None, ssh_public_key=None, mailbox=None, private=None)`   | `Sandbox`                                                                        |
| `zb.sandboxes.wait_ready(id)`                                                                    | `Sandbox`, ready (private and not joined: running)                               |
| `zb.sandboxes.list()`                                                                            | `list[SandboxInfo]`                                                              |
| `zb.sandboxes.get(id)`                                                                           | `Sandbox`                                                                        |
| `zb.sandboxes.pause(id)`                                                                         | `Sandbox`, paused                                                                |
| `zb.sandboxes.resume(id)`                                                                        | `Sandbox`, ready                                                                 |
| `zb.sandboxes.stop(id)` / `start(id)`                                                            | aliases of `pause` / `resume`: the same request and result                       |
| `zb.sandboxes.restart(id)`                                                                       | `None`                                                                           |
| `zb.sandboxes.cancel_transition(id)`                                                             | `"cleared"` / `"cancelling"` / `"none"` (`conflict` past the point of no return) |
| `zb.sandboxes.fork(id, name=, source_policy=, private=None, tailscale_auth_key=None, wait=True)` | `Sandbox`: the new box, ready (private: running; `wait=False`: as accepted)      |
| `zb.sandboxes.get_fork(id)`                                                                      | `SandboxFork` of the new box `id`                                                |
| `zb.sandboxes.set_tier(id, tier)`                                                                | `None`                                                                           |
| `zb.sandboxes.delete(id, archive=None)`                                                          | `None`                                                                           |
| `zb.sandbox(id)`                                                                                 | `Sandbox` handle, no network call                                                |

Every wait (`create`, `get_or_create`, `wait_ready`, `resume`, `fork`) waits on `GET /sandbox/:id/wait?until=ready`, which the server answers as soon as the box's [`readiness`](https://zipbox.ai/docs/http-api.md#status-phase-and-readiness) is no longer `settling`, so it returns the moment the box is up; a held answer that is still `settling` is asked again at once, with no deadline, and a read that fails with a retryable error waits on the poll ladder. An API older than `/wait` is polled on `GET /sandbox/:id` instead. `pause` waits the same way with `until=paused`, and only when the server did not answer with the paused box already; a box that reads running again (its pause was cancelled) raises `conflict`. The wait returns on `ready`, or on `awaits_tailnet` for a private box, and raises on the rest, never retryable: paused `box_not_running`, a failed start `start_failed`, deleting or gone `conflict`. A box call on a private box that has not joined a tailnet raises `tailnet_not_connected`. Every row carries `phase` and `allowed_actions`; a detail also carries `readiness`. Against a control plane that does not send `readiness` yet, the SDK reads `ready` and `status` as before.

A created box's `base_url` changes once, a few minutes after create: from its boot address to `box--<name>`. The SDK follows it. A box call whose request never left (a refused connect, DNS, a TLS handshake) waits a rung of its backoff, re-reads the box with the same wait and retries there, with no attempt ceiling; a paused, failed or deleted box ends the call with that wait's error. A PTY whose dial fails does the same before it redials.

`get_or_create` takes every `create` option except `name` and applies them only when it creates the box. A box that already has the name is returned as it is, even when it differs (another tier, not private): that is not an error and nothing is resized.

```python
box = zb.sandboxes.get_or_create("ci-runner", tier="l", mailbox=True)
```

A `Sandbox` has `id`, `info` (the last `SandboxDetail` read), `refresh()`, `wait_ready()`, `pause()` / `resume()` (each returns the new `SandboxDetail`; `stop()` / `start()` are aliases) / `restart()` / `cancel_transition()` / `set_tier(tier)` / `delete(archive=None)`, `fork(name=, source_policy=, …)` / `fork_status()`, and the methods below.

### Fork

`fork` makes a new box from a consistent capture of the source's disk: its files, software and settings, with a new name and identity ([Fork a box](https://zipbox.ai/docs/fork.md)). `source_policy` is required: `"require_paused"` refuses a running source with `pause_required`, `"pause_and_resume"` pauses it for the capture and resumes it right after. One `Idempotency-Key` per call is reused on every retry. `tailscale_auth_key` stays on your machine: it is sealed to the new box once that box runs, never part of the fork request, and needs `wait` (default `True`). A new box its fork removed (a cancel or a failure) raises its fork's error code, or `conflict` when the fork was cancelled. Interrupting the wait does not cancel the fork; `cancel_transition(child.id)` does. `AsyncZipbox` has the same methods.

```python
child = box.fork(name="ci-runner-2", source_policy="pause_and_resume")
```

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
    zb.sandboxes.resume(box_id)
except ZipboxError as error:
    if error.code == "insufficient_credits":
        print("Top up first:", error.docs_url)
    else:
        raise
```
