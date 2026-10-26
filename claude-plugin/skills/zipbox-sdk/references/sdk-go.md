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
	box, err := c.Sandboxes.GetOrCreate(ctx, "go-quickstart")
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

`zipbox.New(apiKey, opts...)` returns a `*Client`. It reads `ZIPBOX_API_KEY` when `apiKey` is empty and `ZIPBOX_API_URL` for the control plane (default `https://api.tribes.xyz`). Options: `WithBaseURL`, `WithHTTPClient`, `WithMaxRetries` (default 3), `WithBackoff(base, max)` (default 500 ms, 30 s), `WithPollInterval(min, max)`. Nothing has a deadline unless your `ctx` does.

## Boxes

| Method                                                                    | Returns                        |
| ------------------------------------------------------------------------- | ------------------------------ |
| `c.Sandboxes.Create(ctx, zipbox.CreateOptions{Name, Tier, SSHPublicKey})` | `*Box`, ready                  |
| `c.Sandboxes.GetOrCreate(ctx, name)`                                      | `*Box`                         |
| `c.Sandboxes.WaitReady(ctx, id)`                                          | `*Box`, ready                  |
| `c.Sandboxes.List(ctx)`                                                   | `[]Sandbox`                    |
| `c.Sandboxes.Get(ctx, id)`                                                | `*SandboxDetail`               |
| `c.Sandboxes.Start(ctx, id)` / `Stop` / `Restart`                         | `error`                        |
| `c.Sandboxes.SetTier(ctx, id, tier)`                                      | `error`                        |
| `c.Sandboxes.Delete(ctx, id)`                                             | `error`                        |
| `c.Sandbox(id)`                                                           | `*Box` handle, no network call |
| `box.Refresh(ctx)`                                                        | `*SandboxDetail`               |

## Commands

| Method                                     | Returns                                                             |
| ------------------------------------------ | ------------------------------------------------------------------- |
| `box.Exec(ctx, cmd, opts...)`              | `*ExecResult{ID, ExitCode, Stdout, Stderr, Signal, TimedOut}`       |
| `box.ExecArgv(ctx, argv, opts...)`         | `*ExecResult`; no shell                                             |
| `box.ExecStream(ctx, cmd, opts...)`        | `*ExecStream` of `ExecEvent`s                                       |
| `box.ExecBackground(ctx, cmd, opts...)`    | `*Command` (embeds `CommandRecord`; adds `Wait`, `Events`, …)       |
| `box.RunCode(ctx, zipbox.Python, code)`    | `*ExecResult`; also `zipbox.Node`, `zipbox.Bash`                    |
| `box.Command(id)`                          | `*Command` handle, no network call                                  |
| `box.Commands.Get(ctx, id)`                | `*CommandRecord`                                                    |
| `box.Commands.List(ctx)`                   | `[]CommandRecord`                                                   |
| `box.Commands.Events(ctx, id, since)`      | `*ExecStream`, resumable from a cursor                              |
| `box.Commands.Stdin(ctx, id, r io.Reader)` | `error`                                                             |
| `box.Commands.CloseStdin(ctx, id)`         | `error`                                                             |
| `box.Commands.Signal(ctx, id, sig)`        | `error`; `zipbox.SIGTERM`, `SIGKILL`, `SIGINT`, `SIGHUP`, `SIGQUIT` |

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

`stream.Cursor()` is the position to pass as `since` to `Commands.Events` after a drop; `cmd.Wait(ctx)` on a `*Command` does that for you. Cancelling `ctx` disconnects a stream; that stops a foreground command. Use `ExecBackground` for work that must outlive the call.

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

`box.Pty.Create(ctx, zipbox.PtyOptions{Cols, Rows})` returns a `*Pty` that is an `io.ReadWriteCloser` with `Resize(cols, rows)`, `Detach()` (leave the session running), `ExitCode()` and `SessionID`; `Close()` kills the session. `box.Pty.Attach(ctx, sessionID, opts)` re-attaches a session and the box replays its screen. It reconnects by itself with backoff and no attempt limit.

## Previews, logs, SSH, account

| Method                                                 | Returns                                              |
| ------------------------------------------------------ | ---------------------------------------------------- |
| `box.Preview.Create(ctx, port, label)`                 | `*Preview{URL, Label, Host}`; `""` label = `p<port>` |
| `box.Preview.Remove(ctx, label)`                       | `error`                                              |
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
