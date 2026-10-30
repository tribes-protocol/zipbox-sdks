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
# the options apply only when the box is created; an existing box comes back as it is
box = zb.sandboxes.get_or_create("demo", tier="s")
result = box.exec("echo hello from $(hostname)")
print(result.exit_code, result.stdout)
box.files.write("notes/hello.txt", "hi")
print(box.preview.create(3000).url)
```

Async is the same surface with `await`:

```python
from zipbox import AsyncZipbox

async with AsyncZipbox() as zb:
    box = await zb.sandboxes.get_or_create("demo", tier="s")
    print((await box.exec("uname -a")).stdout)
```

## What is in it

| Area      | Calls                                                                                                                                                                                                                    |
| --------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Lifecycle | `zb.sandboxes.create(name=, tier=, ssh_public_key=, mailbox=, private=)`, `get_or_create(name, tier=, …)`, `list`, `get`, `wait_ready`, `start`, `stop`, `restart`, `delete(id, archive=)`, `set_tier`; `zb.sandbox(id)` |
| Exec      | `box.exec(cmd)` → `ExecResult(id, exit_code, stdout, stderr)`; `exec_stream(cmd)`; `exec_background(cmd)` → `Command`; `run_code("python", code, cwd=)`                                                                  |
| Commands  | `Command.get()`, `events(since=)`, `wait()`, `stdin(data)`, `close_stdin()`, `signal("TERM")`; `box.commands.list/get/handle/events/stdin/close_stdin/signal`                                                            |
| Files     | `box.files.read`, `read_text`, `write`, `list`, `stat`, `mkdir`, `move`, `copy`, `delete`, `upload`, `download`                                                                                                          |
| PTY       | `box.pty.create(cols=, rows=, client_label=)` / `attach(session_id, …)` → `write`, `resize`, `takeover`, `on_data`, iterate, `detach`, `close`, `wait`                                                                   |
| Previews  | `box.preview.create(port, label=)` → `Preview(url, label)`; `remove(label)`                                                                                                                                              |
| Other     | `box.logs(since=, lines=, unit=)`, `box.ssh_info()`, `box.screenshot()`, `zb.account.balance()`, `usage()`, `whoami()`                                                                                                   |
| Tailscale | `box.tailscale.status()` → `TailscaleStatus` or `None`; `connect(auth_key)`; `disconnect()`; `box.set_private(enabled)`; `create(private=True)`                                                                          |

A string command runs under `bash -lc` (your PATH, nvm, pyenv); a list is spawned as argv.
A command that exits non-zero is a result: check `exit_code`. Relative paths resolve
against `/root/workspace`.

`exec` and `exec_stream` kill the command if you leave before it exits; a stream that
merely drops after the command started re-attaches from its last cursor by itself. A command
started with `exec_background` keeps running with nobody attached — re-attach any time
with `box.commands.handle(id).events(since=cursor)`.

## Tailscale and private boxes

```python
import os

box = zb.sandboxes.create(name="build", private=True)  # never public
box.tailscale.connect(os.environ["TS_AUTHKEY"])  # a one-off key from your Tailscale admin
print(box.tailscale.status())  # TailscaleStatus(state='joining', ...)
```

`connect` seals the auth key on this machine to the box's own key (P-256 ECDH →
HKDF-SHA256 → AES-256-GCM) and sends only the ciphertext; Zipbox relays it and never sees
the key. Read the key from a file or the environment, never from argv. A private box is
reachable only from devices on your tailnet: off it, box calls (`exec`, `files`, `pty`,
previews) raise `box_private_unreachable` within 5 s instead of hanging. Lifecycle calls,
`set_private` and `tailscale.*` go through the control plane and always work. A private
box that has not joined your tailnet yet cannot be ready, so `create(private=True)` and
`get_or_create` return it once it is running with its key; one that has joined waits for
ready as usual.

## Errors

Every failure raises `ZipboxError` with `code` (`invalid_request`, `unauthorized`,
`forbidden`, `not_found`, `insufficient_credits`, `conflict`, `rate_limited`,
`box_not_running`, `unavailable`, `machine_limit_reached`, `box_key_changed`, `tailnet_not_connected`,
`tailscale_unavailable_egress_lockdown`, `tailscale_requires_update`, `box_private_unreachable`), `status`, `message`, `request_id`, `retryable`,
`retry_after_ms` and `docs_url`. `rate_limited` and `unavailable` are retried
automatically on reads and on `create` (`max_retries`, default 3). An account holds 250
machine names, and a deleted box keeps its name: reuse a fixed pool of names with
`get_or_create` instead of a fresh name per job. A new name past the limit raises
`machine_limit_reached` ([zipbox.ai/docs/machine-limit](https://zipbox.ai/docs/machine-limit)).

## Configuration

`Zipbox(api_key=None, base_url=None, max_retries=3, http_client=None,
retry_base_delay_s=0.5, retry_max_delay_s=30.0, poll_interval_s=1.0,
poll_max_interval_s=10.0, pty_base_delay_s=0.5, pty_max_delay_s=30.0)`. The key and URL
default to `ZIPBOX_API_KEY` and `ZIPBOX_API_URL` (else `https://api.tribes.xyz`).

Docs: [zipbox.ai/docs/sdk-python](https://zipbox.ai/docs/sdk-python) · Agent index:
[zipbox.ai/llms.txt](https://zipbox.ai/llms.txt) · Apache-2.0.
