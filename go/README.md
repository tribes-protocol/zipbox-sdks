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
  `List`, `Get`, `Start`, `Stop`, `Restart`, `CancelTransition`, `Delete(ctx, id, DeleteOptions)`,
  `SetTier`, `WaitReady`, `Fork(ctx, sourceID, ForkOptions)`, `GetFork(ctx, childID)`.
  `c.Sandbox(id)` returns a handle without a call.
- Box handle: `Refresh`, `WaitReady`, `Start`, `Stop`, `Restart`, `CancelTransition`, `SetTier`, `Delete`,
  `Fork(ctx, ForkOptions)`, `ForkStatus`,
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
- `Fork(ctx, sourceID, ForkOptions{Name, SourcePolicy, Private, TailscaleAuthKey, NoWait})`: a
  new box with the source's files, settings and size, a new name and a fresh identity (RAM and
  running processes are not copied). It starts public, whatever the source's privacy, unless
  `Private` is set. `SourcePolicy` is `ForkRequirePaused` (a running
  source answers `CodePauseRequired`) or `ForkPauseAndResume` (it pauses for the capture and
  resumes right after). One `Idempotency-Key` per call, reused on every retry. It waits until
  the new box is ready (a private one: running with its key) unless `NoWait`; a new box its
  fork removed returns the fork's own code (`CodeCopyFailed`, …). `TailscaleAuthKey` is
  sealed on this machine to the new box once it runs, never sent to `/fork`, and never printed
  by `%v`. A ctx that ends stops only the wait; cancel the fork with `CancelTransition` on the
  new box. `GetFork(ctx, childID)` / `box.ForkStatus` read its phase, source recovery and error.
- `c.Account`: `WhoAmI`, `Balance`, `Usage`.

Every wait (`Create`, `GetOrCreate`, `WaitReady`, `Fork`) reads the box's `readiness`: it keeps reading
while the box is `settling`, with backoff and no deadline (a retryable failed read stays on
the same ladder), returns on `ready` or, for a private box, `awaits_tailnet`, and raises on
the rest, never retryable: paused `box_not_running`, a failed start `start_failed`, deleting
or gone `conflict`. Rows carry `Phase` and `AllowedActions`; a `SandboxDetail` also carries `Readiness`.
One retry rule: an error is retried when its `retryable` is true.

A created box's `BaseURL` moves once, from its boot address to `box--<name>`, a few minutes
after create, and the SDK follows it: a box call or PTY dial that fails before anything was
sent (connect, DNS, TLS handshake) waits one backoff rung, re-reads the box with the same wait
`Create` uses and retries on the `BaseURL` it returned, with no attempt ceiling (`ctx` bounds
it). A box that is paused, failed or deleted meanwhile ends the call with the wait's error.

Every failure is a `*zipbox.Error` (`errors.As`) carrying `Code`, `Status`,
`Message`, `RequestID`, `Retryable`, `RetryAfter` and `DocsURL`. A command that exits
non-zero is a result (`ExecResult.ExitCode`), never an error. No call has a default
deadline: bound one with a `context`. An account holds 250 machine names, and a deleted
box keeps its name: reuse a fixed pool of names with `GetOrCreate` instead of a fresh name
per job. A new name past the limit returns `CodeMachineLimitReached`
([zipbox.ai/docs/machine-limit](https://zipbox.ai/docs/machine-limit)).

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
zipbox fork demo --name demo-2 --pause              # pause demo for the capture; it resumes
zipbox private on demo
```

Commands: `login [--with-key]`, `logout`, `whoami`,
`sandbox {create,list,get,start,stop,restart,cancel,delete,tier}`, `fork`, `exec`, `run`,
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

`zipbox fork <source> --name <name> [--pause] [--private] [--tailscale-auth-key-file F|-] [--no-wait]`
forks a box. Without `--pause` a running source is refused (`pause_required`); `--pause`
pauses it for the capture and resumes it right after. The new box starts public unless
`--private`. The new box and its operation id are
printed as soon as the fork is accepted; Ctrl-C then stops only the wait, the fork keeps
running (`zipbox sandbox cancel <name>` cancels it). The Tailscale key comes from a file or
stdin, never from an argument, and is sealed to the new box once it runs.

### `--json` shapes

| Command                | Output                                                    |
| ---------------------- | --------------------------------------------------------- |
| `sandbox list`         | array of boxes (`id`, `name`, `status`, `tier`, …)        |
| `sandbox create/get`   | one box plus `ready`, `baseUrl`                           |
| `fork`                 | the new box plus `ready`, `fork` (`operationId`, `phase`) |
| `sandbox start/stop/…` | `{"id", "action"}`                                        |
| `exec`, `run`          | `{"id", "exitCode", "stdout", "stderr", "timedOut"}`      |
| `exec --background`    | the command record (`id`, `state`, `argv`, …)             |
| `files ls`             | array of `{"name", "kind", "sizeBytes", "modifiedAtMs"}`  |
| `ssh`                  | `{"host", "user", "command"}`                             |
| `preview`              | `{"url", "label", "host"}`                                |
| `tailscale status`     | `{"id", "tailscale": {"state", "tailnet", "ipv4", …}}`    |
| `tailscale connect`    | `{"id", "pending": true}`                                 |
| `private on/off`       | `{"id", "private"}`                                       |
| `whoami`, `login`      | `{"userId", "apiKey": {…}}`                               |
| errors (stderr)        | `{"error": {"code", "message", "docsUrl", …}}`            |

## Development

```sh
go vet ./... && go test -race ./...
```

The tests run the shared conformance suite in `../conformance/fixtures` and reproduce the
box envelope vector in `../conformance/vectors` byte for byte.
