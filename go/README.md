# zipbox-go

The Go SDK and the `zipbox` CLI for [Zipbox](https://zipbox.ai): create boxes, run
commands, move files, open a terminal and publish ports from code. Nothing on a box
stops on a clock.

Docs: [zipbox.ai/docs/sdk-go](https://zipbox.ai/docs/sdk-go) ·
CLI: [zipbox.ai/docs/cli](https://zipbox.ai/docs/cli) ·
Index for agents: [zipbox.ai/llms.txt](https://zipbox.ai/llms.txt)

## Install

```sh
go get github.com/tribes-protocol/zipbox-go
export ZIPBOX_API_KEY=zb_…   # zipbox.ai → Settings → API keys
```

## Quickstart

```go
c := zipbox.New("") // reads ZIPBOX_API_KEY
// the options apply only when the box is created; an existing box comes back as it is
box, err := c.Sandboxes.GetOrCreate(ctx, "demo", zipbox.CreateOptions{Tier: "s"})
if err != nil {
	log.Fatal(err)
}
res, err := box.Exec(ctx, "uname -a")
if err != nil {
	log.Fatal(err)
}
fmt.Print(res.Stdout)
```

A runnable version is in [`../examples/go/quickstart`](../examples/go/quickstart).

## The surface

- `c.Sandboxes`: `Create(ctx, CreateOptions)`, `GetOrCreate(ctx, name, CreateOptions)`,
  `List`, `Get`, `Start`, `Stop`, `Restart`, `Delete(ctx, id, DeleteOptions)`, `SetTier`,
  `WaitReady`. `c.Sandbox(id)` returns a handle without a call.
- Box handle: `Refresh`, `WaitReady`, `Start`, `Stop`, `Restart`, `SetTier`, `Delete`,
  `Exec`, `ExecArgv`, `ExecStream`, `ExecStreamArgv`, `ExecBackground`,
  `ExecBackgroundArgv`, `RunCode`, `Logs(LogOptions{Since, Lines, Unit})`, `SSHInfo`,
  `Screenshot`, `Command(id)`.
- `box.Commands`: `Get`, `List`, `Events`, `Stdin`, `CloseStdin`, `Signal`; a
  `*Command` adds `Wait`. Every stream re-attaches from its cursor after a drop.
- `box.Files`: `Read`, `ReadTo`, `Write`, `List`, `Stat`, `Mkdir`, `Move`, `Copy`,
  `Delete`, `Upload`, `Download`. Relative paths resolve against `/root/workspace`.
- `box.Pty`: `Create`, `Attach` (`PtyOptions{Cols, Rows, ClientLabel}`) → an
  `io.ReadWriter` with `Resize`, `Takeover`, `Detach`, `Close` and `ExitCode`. A dropped socket reconnects by itself (exponential backoff, no
  attempt limit) and re-attaches the same session.
- `box.Preview`: `Create(port, label)` → `https://<label>.<box host>`, `Remove(label)`
  (a label or the full host).
  Previews are public unless the box is private.
- `box.Tailscale`: `Status` (nil when the box never joined a tailnet), `Connect(authKey)`,
  `Disconnect`. `Connect` seals the auth key on this machine to the box's own key
  (`AgentPublicKey`), so Zipbox relays it once and never sees it; it re-reads the key and
  re-seals once on `box_key_changed`. Use a one-off, pre-approved key.
- `box.SetPrivate(enabled)`: private mode ("tailnet only"). Needs the box connected
  (`tailnet_not_connected` otherwise). `CreateOptions{Private: true}` starts a box private;
  `Create` then returns once it is running with its key, since it is not ready until it joins.
  Box calls (exec, files, PTY, previews) on a private box work only from a device on your
  tailnet; elsewhere they fail fast with `box_private_unreachable`. Lifecycle calls always work.
- `c.Account`: `WhoAmI`, `Balance`, `Usage`.

Every failure is a `*zipbox.Error` (`errors.As`) carrying `Code`, `Status`,
`Message`, `RequestID`, `Retryable`, `RetryAfter` and `DocsURL`. A command that exits
non-zero is a result (`ExecResult.ExitCode`), never an error. No call has a default
deadline: bound one with a `context`.

## CLI

```sh
curl -fsSL https://zipbox.ai/install.sh | sh        # macOS, Linux
irm https://zipbox.ai/install.ps1 | iex             # Windows
brew install tribes-protocol/tap/zipbox             # Homebrew

zipbox login
zipbox sandbox create --name demo
zipbox exec demo -- make test                       # exits with the remote status
zipbox shell demo
zipbox tailscale connect demo < tskey.txt           # the auth key never goes on argv
zipbox private on demo
```

Commands: `login [--with-key]`, `logout`, `whoami`,
`sandbox {create,list,get,start,stop,restart,delete,tier}`, `exec`, `run`,
`files {ls,cat,put,get,rm,mkdir,mv}`, `shell`, `ssh`, `preview` (`preview rm`),
`tailscale {connect,status,disconnect}`, `private {on,off}`,
`logs`, `docs [slug]`, `mcp {start,init <claude|codex|cursor|windsurf>,config}`.
`--json` works on every command. `ZIPBOX_API_KEY` overrides the saved login, for CI.

`zipbox mcp start` serves the Zipbox MCP tools over stdio by bridging to the hosted
server (`https://mcp.zipbox.ai/mcp`, override with `ZIPBOX_MCP_URL`), so stdio
clients get exactly the hosted tool set. Like the hosted server, it cannot reach a
private box.

`zipbox tailscale connect <box>` reads a one-off Tailscale auth key from stdin (hidden on a
terminal) or `--auth-key-file F`, never from an argument. `zipbox sandbox create --private
--tailscale-auth-key-file F` creates a private box and joins it to your tailnet.

### `--json` shapes

| Command                | Output                                                   |
| ---------------------- | -------------------------------------------------------- |
| `sandbox list`         | array of boxes (`id`, `name`, `status`, `tier`, …)       |
| `sandbox create/get`   | one box plus `ready`, `baseUrl`                          |
| `sandbox start/stop/…` | `{"id", "action"}`                                       |
| `exec`, `run`          | `{"id", "exitCode", "stdout", "stderr", "timedOut"}`     |
| `exec --background`    | the command record (`id`, `state`, `argv`, …)            |
| `files ls`             | array of `{"name", "kind", "sizeBytes", "modifiedAtMs"}` |
| `ssh`                  | `{"host", "user", "command"}`                            |
| `preview`              | `{"url", "label", "host"}`                               |
| `tailscale status`     | `{"id", "tailscale": {"state", "tailnet", "ipv4", …}}`   |
| `tailscale connect`    | `{"id", "pending": true}`                                |
| `private on/off`       | `{"id", "private"}`                                      |
| `whoami`, `login`      | `{"userId", "apiKey": {…}}`                              |
| errors (stderr)        | `{"error": {"code", "message", "docsUrl", …}}`           |

## Development

```sh
go vet ./... && go test -race ./...
```

The tests run the shared conformance suite in `../conformance/fixtures` and reproduce the
box envelope vector in `../conformance/vectors` byte for byte.
