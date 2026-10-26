> Zipbox docs, markdown edition of https://zipbox.ai/docs/terminals
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# Terminals

Open an interactive terminal (PTY) on a box, resize it, and reconnect to the same shell after a drop.

A terminal is a shell session on the box that outlives every connection to it. Open one, type into it, resize it; if the connection drops, reconnect and the screen replays where it was. The websocket protocol behind it is in [Box API](https://zipbox.ai/docs/box-api.md#pty).

## Open a terminal

#### TypeScript

```ts
const pty = await box.pty.create({ cols: 120, rows: 40 })
pty.onData((data) => process.stdout.write(data))
pty.write('htop\r')
pty.resize(160, 48)
```

#### Python

```python
pty = box.pty.create(cols=120, rows=40)
pty.write("htop\r")
for data in pty:
    print(data, end="")
```

#### Go

```go
pty, err := box.Pty.Create(ctx, zipbox.PtyOptions{Cols: 120, Rows: 40})
_, _ = pty.Write([]byte("htop\r"))
_ = pty.Resize(160, 48)
```

#### Rust

```rust
let pty = sbx.pty().create(120, 40).await?;
pty.write("htop\r")?;
pty.resize(160, 48)?;
```

#### CLI

```sh
zipbox shell $ID
```

#### curl

```sh
# Websockets need a websocket client, e.g. websocat
SESSION="bash-$(uuidgen | tr A-Z a-z)"
websocat -H "Sec-WebSocket-Protocol: tribes.session, $BOX_TOKEN" \
  "wss://${BASE#https://}/?session=$SESSION&clientId=cli1"
```

Input is raw terminal input: send `\r` for Enter, and control characters as bytes (`\u0003` is Ctrl-C).

## Reconnect

A dropped terminal reconnects by itself, with exponential backoff (capped at 30 seconds, with jitter) and no attempt limit; the backoff resets only once output arrives again. Nothing closes a terminal on a timer.

To pick up a session from another process, `attach(sessionId)` it: the shell, its running programs and the screen are all still there.

## Sessions and the dashboard

A terminal your code opens is its own session. It never takes the box's terminal away from you in the dashboard, and the dashboard never takes it away from your code. Close a session with `close()` when you are done; it otherwise runs until the box stops.
