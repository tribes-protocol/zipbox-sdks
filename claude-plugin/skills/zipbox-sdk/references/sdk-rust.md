> Zipbox docs, markdown edition of https://zipbox.ai/docs/sdk-rust
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# Rust SDK

The zipbox crate - async Tokio client, every method, Stream output, PTY and thiserror errors.

The `zipbox` crate is an async client over the [REST API](https://zipbox.ai/docs/http-api.md) and the [Box API](https://zipbox.ai/docs/box-api.md), built on Tokio, `reqwest` and `tokio-tungstenite`. Streams implement `futures::Stream`; every failure is one `thiserror` struct, `zipbox::Error`, carrying an `ErrorCode`.

## Install

```sh
cargo add zipbox
cargo add tokio --features full
```

## Quickstart

Create a box, run a command, write and read a file, open a preview, stop the box. (`box` is a Rust keyword, so the examples call the handle `sbx`.)

```rust
use zipbox::Zipbox;

#[tokio::main]
async fn main() -> Result<(), zipbox::Error> {
    let zb = Zipbox::from_env()?; // or Zipbox::new("zb_...")
    let opts = zipbox::CreateOptions {
        tier: Some("s".into()),
        ..Default::default()
    };
    let sbx = zb.sandboxes().get_or_create_with("rust-quickstart", opts).await?;

    let res = sbx.exec("uname -a").await?;
    println!("{:?} {}", res.exit_code, res.stdout); // exit_code: Option<i32>

    sbx.files().write("hello.txt", "hi from Rust\n").await?;
    println!("{}", sbx.files().read_text("hello.txt").await?);

    sbx.exec_background("python3 -m http.server 8000 --bind 127.0.0.1").await?;
    println!("{}", sbx.preview().create(8000, None).await?.url);

    zb.sandboxes().stop(sbx.id()).await?;
    Ok(())
}
```

## Client

`Zipbox::new(api_key)` or `Zipbox::from_env()` (reads `ZIPBOX_API_KEY` and, optionally, `ZIPBOX_API_URL`; default control plane `https://api.tribes.xyz`). `Zipbox::builder()` sets `api_key`, `base_url`, `max_retries` (default 3), `retry_base_delay` / `retry_max_delay` (500 ms / 30 s), `poll_interval` / `poll_max_interval` (the readiness checks in `create`; 1 s / 10 s), `pty_base_delay` / `pty_max_delay` (PTY reconnects; 500 ms / 30 s) and `http_client`, then `.build()?`. No request has a timeout unless you wrap it in one.

## Boxes

| Method                                                                                  | Returns                                         |
| --------------------------------------------------------------------------------------- | ----------------------------------------------- |
| `zb.sandboxes().create(CreateOptions { name, tier, ssh_public_key, mailbox, private })` | `Sandbox`, ready (with `private`, running)      |
| `zb.sandboxes().get_or_create(name)` / `get_or_create_with(name, CreateOptions)`        | `Sandbox`                                       |
| `zb.sandboxes().list()`                                                                 | `Vec<SandboxInfo>`                              |
| `zb.sandboxes().get(id)`                                                                | `Sandbox`                                       |
| `zb.sandboxes().start(id)` / `stop` / `restart`                                         | `()`                                            |
| `zb.sandboxes().set_tier(id, tier)`                                                     | `()`                                            |
| `zb.sandboxes().delete(id)` / `delete_with(id, DeleteOptions { archive })`              | `()`                                            |
| `zb.sandboxes().wait_ready(id)`                                                         | `Sandbox`, ready (private, not joined: running) |
| `zb.sandbox(id)`                                                                        | `Sandbox` handle, no network call               |

`get_or_create_with` takes every `CreateOptions` field except `name` (leave it `None`) and applies them only when it creates the box. A box that already has the name is returned as it is, even when it differs (another tier, not private): that is not an error and nothing is resized.

```rust
let opts = zipbox::CreateOptions {
    tier: Some("l".into()),
    mailbox: Some(true),
    ..Default::default()
};
let sbx = zb.sandboxes().get_or_create_with("ci-runner", opts).await?;
```

A `Sandbox` has `id()`, `info()` (the last `SandboxDetail` read), `refresh()` and `wait_ready()`, plus `start`, `stop`, `restart`, `set_tier`, `delete` and `delete_with` on itself.

## Tailscale and private mode

| Method                              | Returns                   |
| ----------------------------------- | ------------------------- |
| `sbx.tailscale().status()`          | `Option<TailscaleStatus>` |
| `sbx.tailscale().connect(auth_key)` | `()`                      |
| `sbx.tailscale().disconnect()`      | `()`                      |
| `sbx.set_private(enabled)`          | `()`                      |

`connect` reads the box's `agent_public_key` fresh, seals the auth key on your machine (`p256`, `hkdf`, `aes-gcm`), and sends only the envelope; on `box_key_changed` it re-reads the key and re-seals once. `TailscaleStatus` has `state`, `tailnet`, `ipv4`, `ipv6`, `dns_name`, `key_expiry`, `pending` and `peers` (your other devices: `name`, `dns_name`, `os`, `online`, `ipv4`, `connection`). `SandboxInfo::tailscale_supported` is `Some(false)` when the box's ZipOS predates Tailscale; `connect` then returns `ErrorCode::TailscaleRequiresUpdate`. With `private: Some(true)`, `create` returns once the box is running with its key, because a private box is not ready until it joins your tailnet. On a private box, box calls from off your tailnet return `box_private_unreachable`. See [Tailscale and private boxes](https://zipbox.ai/docs/tailscale.md).

```rust
let sbx = zb.sandboxes().get(id).await?;
sbx.tailscale().connect(&std::env::var("TS_AUTHKEY")?).await?;
sbx.set_private(true).await?;
```

## Commands

| Method                                                                   | Returns                                                                       |
| ------------------------------------------------------------------------ | ----------------------------------------------------------------------------- |
| `sbx.exec(cmd)` / `exec_with(cmd, ExecOptions)`                          | `ExecResult { id, exit_code, stdout, stderr }`                                |
| `sbx.exec_stream(cmd)` / `exec_stream_with(…)`                           | `impl Stream<Item = Result<ExecEvent>>`                                       |
| `sbx.exec_background(cmd)` / `exec_background_with(…)`                   | `Command` (`id()`, `get`, `events`, `wait`, `stdin`, `close_stdin`, `signal`) |
| `sbx.run_code(Language::Python, code)` / `run_code_with(…, ExecOptions)` | `ExecResult`; also `Language::Node` (ES module), `Language::Bash`             |
| `sbx.commands().get(id)` / `list()`                                      | `ExecRecord` / `Vec<ExecRecord>`                                              |
| `sbx.commands().command(id)`                                             | `Command` handle, no network call                                             |
| `sbx.commands().events(id, since: Option<&str>)`                         | `Stream` of `ExecEvent` to the exit; re-attaches after a drop                 |
| `sbx.commands().stdin(id, data)` / `close_stdin(id)`                     | `()`                                                                          |
| `sbx.commands().signal(id, Signal::Term)`                                | `()`                                                                          |

`ExecOptions` has `argv`, `cwd`, `env`, `timeout_ms`, `user` and `stdin` and implements `Default`. A stream that drops after the command started re-attaches from its last cursor by itself, with capped backoff and no attempt limit; `command.wait()` collects everything to the exit into an `ExecResult`. `ExecEvent` is an enum: `Start`, `Output { stream, data, .. }`, `Gap`, `Exit { exit_code, .. }`; `event.cursor()` is its resume point. The streams are not `Unpin`, so pin them before polling:

```rust
use futures::StreamExt;

let mut stream = Box::pin(sbx.exec_stream("cargo test").await?);
while let Some(event) = stream.next().await {
    if let zipbox::ExecEvent::Output { data, .. } = event? {
        print!("{data}");
    }
}
```

Dropping a stream disconnects it, which stops a foreground command; `exec_background` survives.

## Files

| Method                                          | Returns          |
| ----------------------------------------------- | ---------------- |
| `sbx.files().read(path)`                        | `Vec<u8>`        |
| `sbx.files().read_text(path)`                   | `String`         |
| `sbx.files().write(path, data)`                 | `()`             |
| `sbx.files().list(path)`                        | `Vec<FileEntry>` |
| `sbx.files().stat(path)`                        | `FileEntry`      |
| `sbx.files().mkdir(path)`                       | `()`             |
| `sbx.files().move_to(from, to)`                 | `()`             |
| `sbx.files().copy(from, to)`                    | `()`             |
| `sbx.files().delete(path)`                      | `()`             |
| `sbx.files().upload(local_path, remote_path)`   | `()`             |
| `sbx.files().download(remote_path, local_path)` | `()`             |

## PTY

`sbx.pty().create(PtyOptions { cols, rows, client_label })` opens a session (no size = the box's default); `sbx.pty().attach(session_id, PtyOptions)` re-attaches it. A `Pty` has `session_id()`, `write(data)`, `resize(cols, rows)` and `takeover()` (all synchronous), `detach()` (leave the session running) and `close().await` (kill it), and it is a `Stream` of `PtyEvent` (`Output`, `ReplayComplete`, `Reconnecting`, `Taken`, `Exit`, `Closed`). It reconnects by itself with backoff and no attempt limit.

## Previews, logs, SSH, account

| Method                                            | Returns                           |
| ------------------------------------------------- | --------------------------------- |
| `sbx.preview().create(port, label: Option<&str>)` | `Preview { url, label }`          |
| `sbx.preview().remove(label)`                     | `()`; a label or the full host    |
| `sbx.logs(LogOptions { since, lines, unit })`     | `String`                          |
| `sbx.screenshot()`                                | `Vec<u8>` PNG of the desktop      |
| `sbx.ssh_info()`                                  | `SshInfo { host, user, command }` |
| `zb.account().balance()` / `usage()`              | `serde_json::Value`               |
| `zb.account().whoami()`                           | `serde_json::Value`               |

## Errors

Every failure is a `zipbox::Error` with `code` (`zipbox::ErrorCode`), `status`, `message`, `request_id`, `retryable`, `retry_after` (a `Duration`) and `docs_url`; a transport failure has code `Unavailable` and its cause in `source`. A non-zero exit code is a result.

```rust
if let Err(e) = zb.sandboxes().start(&id).await {
    if e.code == zipbox::ErrorCode::InsufficientCredits {
        println!("top up: {}", e.docs_url);
    } else {
        return Err(e);
    }
}
```
