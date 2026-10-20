> Zipbox docs, markdown edition of https://zipbox.ai/docs/box-api
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# Box API

The routes every box serves at its own HTTPS origin - exec and its event stream, background commands, files and the PTY websocket.

Every box serves its own API at its HTTPS origin, `https://<name>.zbox.sh`. The SDKs call it directly - there is no proxy in between - so a command's output streams from the box to you without a hop. This page is the wire reference; the how-to with samples in every language is in the guides: [Run commands](https://zipbox.ai/docs/commands.md), [Files](https://zipbox.ai/docs/files.md) and [Terminals](https://zipbox.ai/docs/terminals.md).

## Authentication

Ask the control plane for the box once:

```sh
curl https://api.tribes.xyz/sandbox/$ID \
  -H "Authorization: Bearer $ZIPBOX_API_KEY"
```

When `ready` is `true`, the answer carries `baseUrl` (the origin to call) and `boxToken`. Send the token as a bearer on every box request:

```sh
curl "$BASE/exec" -H "Authorization: Bearer $BOX_TOKEN" ...
```

The box token is derived from your key and this one box. It has **no expiry and no refresh**: a client holding a terminal open for a week needs nothing. It works on no other box, and it stops working within seconds of the key being deleted.

**401 recovery.** If a box answers `401` (its token list had not arrived yet after a start, or the key was deleted), fetch `GET /sandbox/:id` again and retry once. A second `401` means the key is deleted. The SDKs do this for you.

A box that is not `ready` (stopped, starting) has no `baseUrl`; the SDKs raise `box_not_running` without calling it.

## Exec

Samples: [Run commands](https://zipbox.ai/docs/commands.md).

`POST /exec` runs a command as the box user (`root`), with the same environment your terminal has. The body:

| Field        | Type     | Meaning                                                                     |
| ------------ | -------- | --------------------------------------------------------------------------- |
| `cmd`        | string   | Run through `bash -lc`, so `PATH`, nvm, pyenv and the agent env are present |
| `argv`       | string[] | Spawned directly, no shell. Send exactly one of `cmd` and `argv`            |
| `cwd`        | string   | Absolute, or relative to `/root/workspace` (the default)                    |
| `env`        | object   | Extra environment variables                                                 |
| `user`       | string   | Run as another local user                                                   |
| `timeoutMs`  | number   | Opt-in deadline; the command is killed when it passes. **No default**       |
| `background` | boolean  | Answer `{id}` at once and keep running with nobody attached                 |
| `stdin`      | boolean  | Open a stdin pipe fed by `POST /exec/:id/stdin`. Default: `/dev/null`       |

Without `background`, the response is a server-sent event stream (send `Accept: text/event-stream`). The command is killed if you disconnect before it exits - start it with `background: true` if it must outlive the connection.

### Events

The stream carries five event types. Every event's `id:` is a cursor, `<stdout bytes>-<stderr bytes>` recorded so far.

| Event    | Data                                                             | Meaning                                              |
| -------- | ---------------------------------------------------------------- | ---------------------------------------------------- |
| `start`  | `{"id":"cmd_…"}`                                                 | The command is running; keep the id to re-attach     |
| `stdout` | `{"data":"…"}`                                                   | Output, UTF-8 (invalid bytes become U+FFFD)          |
| `stderr` | `{"data":"…"}`                                                   | Same, for stderr                                     |
| `gap`    | `{"stream":"stdout","message":"…"}`                              | That stream passed 64 MiB; later output is discarded |
| `exit`   | `{"state":"exited","exitCode":0,"signal":null,"timedOut":false}` | The last event                                       |

```text
id: 0-0
event: start
data: {"id":"cmd_7f3a"}

id: 6-0
event: stdout
data: {"data":"hello\n"}

id: 6-0
event: exit
data: {"state":"exited","exitCode":0,"signal":null,"timedOut":false}
```

`exitCode` follows the shell: `0`-`255`, `128 + N` when killed by signal `N`. `state` is `unknown` in one case only: the box's agent restarted at the exact moment the command was being spawned, and the record says so rather than guessing.

## Background commands

Samples: [Background commands](https://zipbox.ai/docs/commands.md#background-commands). Send `"background": true` to `POST /exec` and the answer is `{id}` at once; the command keeps running with nobody attached, across disconnects and a restart of the box's agent. Its output is spooled on the box; re-attach from any cursor.

| Route                         | Does                                                                           |
| ----------------------------- | ------------------------------------------------------------------------------ |
| `GET /exec`                   | Running and recently finished commands                                         |
| `GET /exec/:id`               | One command: `state`, `exitCode`, `signal`, `startedAt`, `endedAt`, bytes      |
| `GET /exec/:id/events?since=` | Replay from a cursor, then follow live until `exit`. `Last-Event-ID` works too |
| `POST /exec/:id/stdin`        | Append the raw request body to stdin; `?close=1` closes it                     |
| `POST /exec/:id/signal`       | `{"signal":"TERM"}` - one of `TERM`, `KILL`, `INT`, `HUP`, `QUIT`              |

### stdin and signals

Start the command with `"stdin": true`, then append to its stdin with `POST /exec/:id/stdin` (the raw body; `?close=1` closes it). Stdin closes if the box's agent restarts; output and the process carry on. Samples: [Write to stdin and send signals](https://zipbox.ai/docs/commands.md#write-to-stdin-and-send-signals).

### Run code

`runCode` in the SDKs is `POST /exec` with `argv`, for example `["python3","-c","print(2 ** 10)"]`. There is no separate route.

### Spool and limits

Output is spooled on the box: up to **64 MiB per stream per command** and **512 MiB across all commands**; the oldest finished spools are evicted first. A stream past its cap emits one `gap` event and keeps running - the process is always drained, so a slow reader never blocks it. At most **32 commands** may be running at once; the 33rd is answered `429`.

## Files

Samples: [Files](https://zipbox.ai/docs/files.md).

Paths are absolute inside the box. The SDKs resolve a relative path against `/root/workspace`. Symlinks are followed.

| Route                          | Does                                                                                                           |
| ------------------------------ | -------------------------------------------------------------------------------------------------------------- |
| `GET /files?path=`             | List a directory: `{path, entries: [{name, kind, sizeBytes, modifiedAtMs}], truncated}`                        |
| `GET /files/download?path=`    | Read a file's bytes (up to 250 MiB)                                                                            |
| `POST /files/write?dir=&name=` | Write the raw request body to `dir/name`, atomically (temp file, then rename). Up to 250 MiB; `dir` must exist |
| `POST /files/mutate`           | `{"action":"mkdir"\|"rename"\|"copy"\|"move"\|"delete","path":…,"destination":…}`                              |

`kind` is `file`, `directory`, `symlink` or `other`. Files over 250 MiB answer `413`; split them, or move them with `exec` (`curl` from the box, `git`, `rsync`). At most **8 transfers** run at once per box; more answer `429`.

`stat(path)` lists the parent directory and returns the matching entry. `upload(localPath, remotePath)` and `download(remotePath, localPath)` stream between a local file and the box. File routes that predate the public API answer errors as `{"error":"no such file"}`; the SDKs classify them by HTTP status (404 → `not_found`).

## PTY

Samples: [Terminals](https://zipbox.ai/docs/terminals.md).

The terminal websocket is the PTY API. Connect to `wss://<box>/?session=<id>&clientId=<id>` with the box token in the subprotocol header - a websocket handshake cannot carry `Authorization`:

```text
Sec-WebSocket-Protocol: tribes.session, <boxToken>
```

A session is a shell (`bash-<uuid>`, minted by the client) that outlives every socket attached to it: disconnect, reconnect with the same `clientId` and `resume=1`, and the screen replays. Sessions an API client opens never take the machine away from your dashboard.

| Client frame               | Meaning                                             |
| -------------------------- | --------------------------------------------------- |
| `{"t":"i","d":"ls\r"}`     | Input                                               |
| `{"t":"r","c":120,"r":40}` | Resize (cols, rows)                                 |
| `{"t":"takeover"}`         | Take the session from another socket attached to it |
| `{"t":"ping","n":1}`       | Keepalive                                           |

The server sends `{"t":"o","d":…}` (output), `{"t":"x","code":…}` (the shell exited), `replay-complete`, `blocked` / `taken` / `owned` (who holds the session) and `pong`. `POST /session/close?session=&clientId=` kills a session.

**Reconnect.** A dropped PTY reconnects by itself, with exponential backoff (capped at 30 seconds, with jitter) and no attempt limit; the backoff resets only once a frame arrives. `attach(sessionId)` re-attaches a session from another process. Nothing closes a PTY on a timer.

## Concurrency and errors

Per box: 32 running commands and 8 file transfers; beyond either the box answers `429`. Nothing on the box is rate-limited otherwise and nothing times out on a clock. Box routes answer errors as `{"error":"<message>"}`; the SDKs classify them by HTTP status into the same codes as the control plane's [error envelope](https://zipbox.ai/docs/errors.md). See [Errors and limits](https://zipbox.ai/docs/errors.md#box-side-errors) for every code.
