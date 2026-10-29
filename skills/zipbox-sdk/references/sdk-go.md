> Zipbox docs, markdown edition of https://zipbox.ai/docs/sdk-go
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# Go SDK

The zipbox-go module - context-aware client, every method, streaming with io.Reader, PTY and typed errors.

`github.com/tribes-protocol/zipbox-go` is a client over the [REST API](https://zipbox.ai/docs/http-api.md) and the [Box API](https://zipbox.ai/docs/box-api.md): every method takes a `context.Context`, streams are `io.Reader`s or iterators, and errors are typed. It is the library the [CLI](https://zipbox.ai/docs/cli.md) is built on.

## Install

```sh
go get github.com/tribes-protocol/zipbox-go
```

## Quickstart

Create a box, run a command, write and read a file, open a preview, stop the box.

```go
package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/tribes-protocol/zipbox-go"
)

func main() {
	ctx := context.Background()
	c := zipbox.New("") // "" reads ZIPBOX_API_KEY
	opts := zipbox.CreateOptions{Tier: "s"}
	box, err := c.Sandboxes.GetOrCreate(ctx, "go-quickstart", opts)
	if err != nil {
		log.Fatal(err)
	}
	res, err := box.Exec(ctx, "go version")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.ExitCode, res.Stdout)

	_ = box.Files.Write(ctx, "hello.txt", strings.NewReader("hi from Go\n"))
	data, _ := box.Files.Read(ctx, "hello.txt")
	fmt.Print(string(data))

	_, _ = box.ExecBackground(ctx, "python3 -m http.server 8000 --bind 127.0.0.1")
	preview, _ := box.Preview.Create(ctx, 8000, "") // "" = label p8000
	fmt.Println(preview.URL)

	_ = c.Sandboxes.Stop(ctx, box.ID)
}
```

## Client

`zipbox.New(apiKey, opts...)` returns a `*Client`. It reads `ZIPBOX_API_KEY` when `apiKey` is empty and `ZIPBOX_API_URL` for the control plane (default `https://api.tribes.xyz`). Options: `WithBaseURL`, `WithHTTPClient`, `WithMaxRetries` (default 3), `WithBackoff(base, max)` for retries and command stream reconnects (default 500 ms, 30 s), `WithPollInterval(min, max)` for the readiness checks in `Create` (default 1 s, 10 s), `WithPtyBackoff(base, max)` for PTY reconnects (default 500 ms, 30 s). Nothing has a deadline unless your `ctx` does.

## Boxes

| Method                                                                                           | Returns                                         |
| ------------------------------------------------------------------------------------------------ | ----------------------------------------------- |
| `c.Sandboxes.Create(ctx, zipbox.CreateOptions{Name, Tier, SSHPublicKey, Mailbox, Private})`      | `*Box`, ready (with `Private`, running)         |
| `c.Sandboxes.GetOrCreate(ctx, name, zipbox.CreateOptions{Tier, SSHPublicKey, Mailbox, Private})` | `*Box`                                          |
| `c.Sandboxes.WaitReady(ctx, id)`                                                                 | `*Box`, ready (private and not joined: running) |
| `c.Sandboxes.List(ctx)`                                                                          | `[]Sandbox`                                     |
| `c.Sandboxes.Get(ctx, id)`                                                                       | `*SandboxDetail`                                |
| `c.Sandboxes.Start(ctx, id)` / `Stop` / `Restart`                                                | `error`                                         |
| `c.Sandboxes.SetTier(ctx, id, tier)`                                                             | `error`                                         |
| `c.Sandboxes.Delete(ctx, id, zipbox.DeleteOptions{Archive})`                                     | `error`                                         |
| `c.Sandbox(id)`                                                                                  | `*Box` handle, no network call                  |
| `box.Refresh(ctx)` / `box.WaitReady(ctx)`                                                        | `*SandboxDetail`                                |
| `box.Start(ctx)` / `Stop` / `Restart` / `SetTier(ctx, tier)` / `Delete(ctx, opts)`               | `error`                                         |

`GetOrCreate` takes every `CreateOptions` field except `Name` (leave it empty) and applies them only when it creates the box. A box that already has the name is returned as it is, even when it differs (another tier, not private): that is not an error and nothing is resized. `DeleteOptions.Archive` is a `*bool`: `nil` keeps the recoverable copy (the server default), `false` skips it.

```go
opts := zipbox.CreateOptions{Tier: "l", Mailbox: true}
box, err := c.Sandboxes.GetOrCreate(ctx, "ci-runner", opts)
```

## Tailscale and private mode

| Method                                | Returns                                     |
| ------------------------------------- | ------------------------------------------- |
| `box.Tailscale.Status(ctx)`           | `*TailscaleStatus`; `nil` when never joined |
| `box.Tailscale.Connect(ctx, authKey)` | `error`                                     |
| `box.Tailscale.Disconnect(ctx)`       | `error`                                     |
| `box.SetPrivate(ctx, enabled)`        | `error`                                     |

`Connect` reads the box's `AgentPublicKey` fresh, seals the auth key on your machine with the standard library (`crypto/ecdh`, `crypto/hkdf`, AES-GCM), and sends only the envelope; on `box_key_changed` it re-reads the key and re-seals once. `TailscaleStatus` has `State`, `Tailnet`, `IPv4`, `IPv6`, `DNSName`, `KeyExpiry`, `Pending` and `Peers` (your other devices: `Name`, `DNSName`, `OS`, `Online`, `IPv4`, `Connection`). `Sandbox.TailscaleSupported` is `false` when the box's ZipOS predates Tailscale; `Connect` then returns `tailscale_requires_update`. With `Private: true`, `Create` returns once the box is running with its key, because a private box is not ready until it joins your tailnet. On a private box, box calls from off your tailnet return `box_private_unreachable`. See [Tailscale and private boxes](https://zipbox.ai/docs/tailscale.md).

```go
box, err := c.Sandboxes.Create(ctx, zipbox.CreateOptions{
	Name:    "build",
	Private: true,
})
if err != nil {
	log.Fatal(err)
}
err = box.Tailscale.Connect(ctx, os.Getenv("TS_AUTHKEY"))
```

## Commands

| Method                                           | Returns                                                             |
| ------------------------------------------------ | ------------------------------------------------------------------- |
| `box.Exec(ctx, cmd, opts...)`                    | `*ExecResult{ID, ExitCode, Stdout, Stderr, Signal, TimedOut}`       |
| `box.ExecArgv(ctx, argv, opts...)`               | `*ExecResult`; no shell                                             |
| `box.ExecStream(ctx, cmd, opts...)`              | `*ExecStream` of `ExecEvent`s                                       |
| `box.ExecStreamArgv(ctx, argv, opts...)`         | `*ExecStream`; no shell                                             |
| `box.ExecBackground(ctx, cmd, opts...)`          | `*Command` (embeds `CommandRecord`; adds `Wait`, `Events`, …)       |
| `box.ExecBackgroundArgv(ctx, argv, opts...)`     | `*Command`; no shell                                                |
| `box.RunCode(ctx, zipbox.Python, code, opts...)` | `*ExecResult`; also `zipbox.Node` (ES module), `zipbox.Bash`        |
| `box.Command(id)`                                | `*Command` handle, no network call                                  |
| `box.Commands.Get(ctx, id)`                      | `*CommandRecord`                                                    |
| `box.Commands.List(ctx)`                         | `[]CommandRecord`                                                   |
| `box.Commands.Events(ctx, id, since)`            | `*ExecStream` to the exit; re-attaches after a drop                 |
| `box.Commands.Stdin(ctx, id, r io.Reader)`       | `error`                                                             |
| `box.Commands.CloseStdin(ctx, id)`               | `error`                                                             |
| `box.Commands.Signal(ctx, id, sig)`              | `error`; `zipbox.SIGTERM`, `SIGKILL`, `SIGINT`, `SIGHUP`, `SIGQUIT` |

Options: `zipbox.WithCwd(dir)`, `WithEnv(map)`, `WithUser(user)`, `WithTimeout(d)` (opt-in deadline), `WithStdin()` (background only). Read an `*ExecStream` like a `bufio.Scanner` and close it:

```go
stream, err := box.ExecStream(ctx, "npm run build")
if err != nil {
	log.Fatal(err)
}
defer stream.Close()
for stream.Next() {
	if ev := stream.Event(); ev.Type == zipbox.EventStdout {
		fmt.Print(ev.Data)
	}
}
if err := stream.Err(); err != nil {
	log.Fatal(err)
}
```

A stream that drops after the command started re-attaches from its last cursor by itself, with capped backoff and no attempt limit; only an answered error or `ctx` ends it. `stream.Cursor()` is the position to pass as `since` to `Commands.Events` to come back later; `cmd.Wait(ctx)` on a `*Command` collects everything to the exit. Cancelling `ctx` disconnects a stream; that stops a foreground command. Use `ExecBackground` for work that must outlive the call.

## Files

| Method                                           | Returns       |
| ------------------------------------------------ | ------------- |
| `box.Files.Read(ctx, path)`                      | `[]byte`      |
| `box.Files.ReadTo(ctx, path, w io.Writer)`       | `int64` bytes |
| `box.Files.Write(ctx, path, r io.Reader)`        | `error`       |
| `box.Files.List(ctx, path)`                      | `[]FileEntry` |
| `box.Files.Stat(ctx, path)`                      | `*FileEntry`  |
| `box.Files.Mkdir(ctx, path)`                     | `error`       |
| `box.Files.Move(ctx, from, to)`                  | `error`       |
| `box.Files.Copy(ctx, from, to)`                  | `error`       |
| `box.Files.Delete(ctx, path)`                    | `error`       |
| `box.Files.Upload(ctx, localPath, remotePath)`   | `error`       |
| `box.Files.Download(ctx, remotePath, localPath)` | `error`       |

## PTY

`box.Pty.Create(ctx, zipbox.PtyOptions{Cols, Rows, ClientLabel})` returns a `*Pty` that is an `io.ReadWriteCloser` with `Resize(cols, rows)`, `Takeover()` (take the session over from another client), `Detach()` (leave the session running), `ExitCode()` and `SessionID`; zero `Cols`/`Rows` leave the box's default size; `Close()` kills the session. `box.Pty.Attach(ctx, sessionID, opts)` re-attaches a session and the box replays its screen. It reconnects by itself with backoff and no attempt limit.

## Previews, logs, SSH, account

| Method                                                 | Returns                                              |
| ------------------------------------------------------ | ---------------------------------------------------- |
| `box.Preview.Create(ctx, port, label)`                 | `*Preview{URL, Label, Host}`; `""` label = `p<port>` |
| `box.Preview.Remove(ctx, label)`                       | `error`; a label or the full host                    |
| `box.Logs(ctx, zipbox.LogOptions{Since, Lines, Unit})` | `string`                                             |
| `box.SSHInfo(ctx)`                                     | `*SSHInfo{Host, User, Command}`                      |
| `box.Screenshot(ctx)`                                  | `[]byte` PNG of the desktop                          |
| `c.Account.Balance(ctx)` / `Usage(ctx)`                | `map[string]any`                                     |
| `c.Account.WhoAmI(ctx)`                                | `*WhoAmI{UserID, APIKey}`                            |

## Errors

Failures are `*zipbox.Error` with `Code`, `Status`, `Message`, `RequestID`, `Retryable`, `RetryAfter` (a `time.Duration`) and `DocsURL`. `zipbox.IsCode(err, code)` is the short form. A non-zero exit code is a result.

```go
var zerr *zipbox.Error
if errors.As(err, &zerr) && zerr.Code == zipbox.CodeInsufficientCredits {
	fmt.Println("top up first:", zerr.DocsURL)
}
```
