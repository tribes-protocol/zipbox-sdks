> Zipbox docs, markdown edition of https://zipbox.ai/docs/logs
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# Logs

Read a box's system journal with logs(), and treat command output as the command log.

A box keeps two kinds of logs: the **system journal** (services, the agent, cron, everything that writes to journald) and **command output**, which the box spools ([Run commands](https://zipbox.ai/docs/commands.md)) for every command you run.

## Read the journal

`logs({ since, lines })` runs `journalctl -o short-iso --no-pager` on the box (with `--since <since>` and `-n <lines>` when given) and returns its text. `since` takes anything journalctl does: `"10 min ago"`, `"today"`, an ISO timestamp. The TypeScript SDK returns the last 200 lines when `lines` is omitted; the other SDKs and the CLI return everything in range. The Go SDK (`LogOptions.Unit`) and the CLI (`-u`) also filter by systemd unit.

#### TypeScript

```ts
const logs = await box.logs({ since: '10 min ago' })
console.log(logs)
```

#### Python

```python
print(box.logs(since="10 min ago"))
```

#### Go

```go
logs, err := box.Logs(ctx, zipbox.LogOptions{Since: "10 min ago"})
fmt.Println(logs)
```

#### Rust

```rust
let options = zipbox::LogOptions {
    since: Some("10 min ago".into()),
    ..Default::default()
};
let logs = sbx.logs(options).await?;
println!("{logs}");
```

#### CLI

```sh
zipbox logs $ID --since '10 min ago'
```

#### curl

```sh
curl -N -X POST "$BASE/exec" -H "Authorization: Bearer $BOX_TOKEN" \
  -d '{"argv":["journalctl","-o","short-iso","--no-pager",
               "--since","10 min ago"]}'
```

Need one service? Run `journalctl -u <unit>` with [exec](https://zipbox.ai/docs/commands.md) directly.

## Command output is the command log

Every command's stdout and stderr are kept on the box after it exits (up to 64 MiB per stream, 512 MiB per box, oldest evicted first). `GET /exec` lists recent commands; `GET /exec/:id/events?since=0-0` replays one from the start. A background dev server's output is always there to read later - you don't need to have been attached.

## What is not exposed

The host's own logs (the machine your box runs on) stay with Zipbox. Nothing about another customer's box is reachable.
