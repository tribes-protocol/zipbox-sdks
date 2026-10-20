> Zipbox docs, markdown edition of https://zipbox.ai/docs/commands
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# Run commands

Run a command and read its exit code, stream its output, start a background job and re-attach, write to stdin, send signals and run code.

Commands run as `root` through `bash -lc`, in `/root/workspace`, with the same environment your terminal has: `PATH`, nvm, pyenv and the agent env are all there. Nothing has a default deadline. The wire format (request fields, event stream, routes) is in [Box API](https://zipbox.ai/docs/box-api.md#exec).

## Run a command

`exec` waits for the command to exit and returns its exit code, stdout and stderr. `cwd` is absolute or relative to `/root/workspace`; `env` adds variables.

#### TypeScript

```ts
const result = await box.exec('npm test', { cwd: 'app', env: { CI: '1' } })
console.log(result.exitCode, result.stdout, result.stderr)
```

#### Python

```python
result = box.exec("npm test", cwd="app", env={"CI": "1"})
print(result.exit_code, result.stdout, result.stderr)
```

#### Go

```go
res, err := box.Exec(ctx, "npm test",
	zipbox.WithCwd("app"),
	zipbox.WithEnv(map[string]string{"CI": "1"}),
)
fmt.Println(res.ExitCode, res.Stdout, res.Stderr)
```

#### Rust

```rust
let mut options = zipbox::ExecOptions::default();
options.cwd = Some("app".into());
options.env.insert("CI".into(), "1".into());
let res = sbx.exec_with("npm test", options).await?;
println!("{:?} {} {}", res.exit_code, res.stdout, res.stderr);
```

#### CLI

```sh
# exits with the remote exit code
zipbox exec $ID --cwd app -e CI=1 -- npm test
```

#### curl

```sh
curl -N -X POST "$BASE/exec" -H "Authorization: Bearer $BOX_TOKEN" \
  -H 'Accept: text/event-stream' -H 'Content-Type: application/json' \
  -d '{"cmd":"npm test","cwd":"app","env":{"CI":"1"}}'
```

The exit code is a result: a failing test suite returns `exitCode: 1` and raises nothing. Pass `timeoutMs` only when you want a deadline; the command is killed when it passes.

## Stream output

Read output as it is written instead of waiting for the exit. A streamed command is killed if you disconnect before it exits; start it in the background if it must outlive the connection.

#### TypeScript

```ts
for await (const event of box.execStream('npm run build')) {
  if (event.type === 'stdout') process.stdout.write(event.data)
}
```

#### Python

```python
for event in box.exec_stream("npm run build"):
    if event.type == "stdout":
        print(event.data, end="")
```

#### Go

```go
stream, err := box.ExecStream(ctx, "npm run build")
for stream.Next() {
	fmt.Print(stream.Event().Data)
}
```

#### Rust

```rust
use futures::StreamExt;

let mut stream = Box::pin(sbx.exec_stream("npm run build").await?);
while let Some(event) = stream.next().await {
    if let zipbox::ExecEvent::Output { data, .. } = event? {
        print!("{data}");
    }
}
```

#### CLI

```sh
# streams as it runs
zipbox exec $ID -- npm run build
```

#### curl

```sh
curl -N -X POST "$BASE/exec" -H "Authorization: Bearer $BOX_TOKEN" \
  -H 'Accept: text/event-stream' -d '{"cmd":"npm run build"}'
```

## Background commands

A background command keeps running whether or not anyone is attached: across your client disconnecting, and across a restart of the agent on the box. Its output is spooled on the box, so you can re-attach from any cursor (`0-0` replays everything).

#### TypeScript

```ts
const cmd = await box.execBackground('npm run dev')
for await (const event of box.commands.events(cmd.id, { since: '0-0' })) {
  console.log(event)
}
```

#### Python

```python
cmd = box.exec_background("npm run dev")
for event in box.commands.events(cmd.id, since="0-0"):
    print(event)
```

#### Go

```go
cmd, err := box.ExecBackground(ctx, "npm run dev")
events, err := box.Commands.Events(ctx, cmd.ID, "0-0")
```

#### Rust

```rust
let cmd = sbx.exec_background("npm run dev").await?;
let since = Some("0-0");
let mut events = Box::pin(sbx.commands().events(cmd.id(), since).await?);
```

#### CLI

```sh
zipbox exec $ID --background -- npm run dev
```

#### curl

```sh
# start it
curl -X POST "$BASE/exec" -H "Authorization: Bearer $BOX_TOKEN" \
  -d '{"cmd":"npm run dev","background":true}'
# re-attach from the start
curl -N "$BASE/exec/$CMD/events?since=0-0" \
  -H "Authorization: Bearer $BOX_TOKEN"
```

List running and recent commands with `commands.list()`, and read one command's state with `commands.get(id)`.

## Write to stdin and send signals

Start the command with `stdin` on, then write to it. Stdin closes if the box's agent restarts; output and the process carry on.

#### TypeScript

```ts
const cmd = await box.execBackground('python3 -i', { stdin: true })
await box.commands.stdin(cmd.id, 'print(6 * 7)\n')
await box.commands.signal(cmd.id, 'TERM')
```

#### Python

```python
cmd = box.exec_background("python3 -i", stdin=True)
box.commands.stdin(cmd.id, "print(6 * 7)\n")
box.commands.signal(cmd.id, "TERM")
```

#### Go

```go
cmd, err := box.ExecBackground(ctx, "python3 -i", zipbox.WithStdin())
_ = box.Commands.Stdin(ctx, cmd.ID, strings.NewReader("print(6 * 7)\n"))
_ = box.Commands.Signal(ctx, cmd.ID, zipbox.SIGTERM)
```

#### Rust

```rust
let opts = zipbox::ExecOptions { stdin: true, ..Default::default() };
let cmd = sbx.exec_background_with("python3 -i", opts).await?;
sbx.commands().stdin(cmd.id(), "print(6 * 7)\n").await?;
sbx.commands().signal(cmd.id(), zipbox::Signal::Term).await?;
```

#### CLI

```sh
# exec does not forward stdin; pipe code to run instead
echo 'print(6 * 7)' | zipbox run $ID --lang python -
```

#### curl

```sh
# write to the command's stdin
curl -X POST "$BASE/exec/$CMD/stdin" -H "Authorization: Bearer $BOX_TOKEN" \
  --data-binary $'print(6 * 7)\n'
# send it a signal
curl -X POST "$BASE/exec/$CMD/signal" -H "Authorization: Bearer $BOX_TOKEN" \
  -d '{"signal":"TERM"}'
```

## Run code

`runCode(language, code)` writes the code to a temporary file and runs it with `python3`, `node` or `bash`. It returns the same result as `exec`.

#### TypeScript

```ts
const out = await box.runCode('python', 'print(2 ** 10)')
```

#### Python

```python
out = box.run_code("python", "print(2 ** 10)")
```

#### Go

```go
out, err := box.RunCode(ctx, zipbox.Python, "print(2 ** 10)")
```

#### Rust

```rust
let out = sbx
    .run_code(zipbox::Language::Python, "print(2 ** 10)")
    .await?;
```

#### CLI

```sh
echo 'print(2 ** 10)' | zipbox run $ID --lang python -
```

#### curl

```sh
curl -N -X POST "$BASE/exec" -H "Authorization: Bearer $BOX_TOKEN" \
  -d '{"argv":["python3","-c","print(2 ** 10)"]}'
```

## Limits

At most 32 commands run at once per box; the 33rd is answered `429`. Output is spooled up to 64 MiB per stream per command; past that the stream emits one `gap` event and the command keeps running. Details: [Spool and limits](https://zipbox.ai/docs/box-api.md#spool-and-limits).
