# zipbox

Drive [Zipbox](https://zipbox.ai) boxes from Python: create a box, run commands, move
files, open a terminal, publish a port. Boxes are long-lived machines — nothing on a box
stops on a clock, and no SDK call has a default timeout.

```sh
pip install zipbox
export ZIPBOX_API_KEY=zb_…   # zipbox.ai → Settings → API keys
```

## Quickstart

```python
from zipbox import Zipbox

zb = Zipbox()
box = zb.sandboxes.get_or_create("demo")
result = box.exec("echo hello from $(hostname)")
print(result.exit_code, result.stdout)
box.files.write("notes/hello.txt", "hi")
print(box.preview.create(3000).url)
```

Async is the same surface with `await`:

```python
from zipbox import AsyncZipbox

async with AsyncZipbox() as zb:
    box = await zb.sandboxes.get_or_create("demo")
    print((await box.exec("uname -a")).stdout)
```

## What is in it

| Area      | Calls                                                                                                                                             |
| --------- | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| Lifecycle | `zb.sandboxes.create(name=, tier=)`, `get_or_create`, `list`, `get`, `start`, `stop`, `restart`, `delete`, `set_tier`; `zb.sandbox(id)`           |
| Exec      | `box.exec(cmd)` → `ExecResult(id, exit_code, stdout, stderr)`; `exec_stream(cmd)`; `exec_background(cmd)` → `Command`; `run_code("python", code)` |
| Commands  | `Command.get()`, `events(since=)`, `wait()`, `stdin(data)`, `close_stdin()`, `signal("TERM")`; `box.commands.list/get/events/stdin/signal`        |
| Files     | `box.files.read`, `read_text`, `write`, `list`, `stat`, `mkdir`, `move`, `delete`, `upload`, `download`                                           |
| PTY       | `box.pty.create(cols=, rows=)` / `attach(session_id)` → `write`, `resize`, `on_data`, iterate, `detach`, `close`, `wait`                          |
| Previews  | `box.preview.create(port, label=)` → `Preview(url, label)`; `remove(label)`                                                                       |
| Other     | `box.logs(since=)`, `box.ssh_info()`, `box.screenshot()`, `zb.account.balance()`, `usage()`                                                       |

A string command runs under `bash -lc` (your PATH, nvm, pyenv); a list is spawned as argv.
A command that exits non-zero is a result: check `exit_code`. Relative paths resolve
against `/root/workspace`.

`exec` and `exec_stream` kill the command if you disconnect before it exits. A command
started with `exec_background` keeps running with nobody attached — re-attach any time
with `box.commands.handle(id).events(since=cursor)`.

## Errors

Every failure raises `ZipboxError` with `code` (`invalid_request`, `unauthorized`,
`forbidden`, `not_found`, `insufficient_credits`, `conflict`, `rate_limited`,
`box_not_running`, `unavailable`), `status`, `message`, `request_id`, `retryable`,
`retry_after_ms` and `docs_url`. `rate_limited` and `unavailable` are retried
automatically on reads and on `create` (`max_retries`, default 3).

## Configuration

`Zipbox(api_key=None, base_url=None, max_retries=3, http_client=None)`. The key and URL
default to `ZIPBOX_API_KEY` and `ZIPBOX_API_URL` (else `https://api.tribes.xyz`).

Docs: [zipbox.ai/docs/sdk-python](https://zipbox.ai/docs/sdk-python) · Agent index:
[zipbox.ai/llms.txt](https://zipbox.ai/llms.txt) · Apache-2.0.
