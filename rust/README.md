# zipbox

Drive [Zipbox](https://zipbox.ai) boxes from Rust: create a box, run commands, move
files, open a terminal, publish a port, pause it when you are done. Nothing on a box
stops on a clock.

```sh
cargo add zipbox tokio --features tokio/macros,tokio/rt
export ZIPBOX_API_KEY=zb_…   # zipbox.ai → Settings → API keys
```

## Quickstart

```rust
#[tokio::main(flavor = "current_thread")]
async fn main() -> zipbox::Result<()> {
    let zb = zipbox::Zipbox::from_env()?;
    // the options apply only when the box is created; an existing box comes back as it is
    let opts = zipbox::CreateOptions { tier: Some("s".into()), ..Default::default() };
    let sandbox = zb.sandboxes().get_or_create_with("demo", opts).await?;
    let out = sandbox.exec("echo hello").await?;
    print!("{}", out.stdout);
    Ok(())
}
```

## What you can do

| Area      | Calls                                                                                                                                                                                          |
| --------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Lifecycle | `zb.sandboxes().create / get_or_create / get_or_create_with / list / get / wait_ready / pause / resume / restart / cancel_transition / delete / delete_with / set_tier`                        |
| Fork      | `zb.sandboxes().fork(source_id, ForkOptions::new(name, policy))` → the new box; `get_fork(child_id)`; `sandbox.fork(options)`, `sandbox.fork_status()`                                         |
| Commands  | `sandbox.exec`, `exec_with`, `exec_stream` (`Stream` of events), `exec_background` → `Command` (`get`, `events(since)`, `wait`, `stdin`, `close_stdin`, `signal`), `run_code`, `run_code_with` |
| Files     | `sandbox.files().read / read_text / write / list / stat / mkdir / move_to / copy / delete / upload / download`                                                                                 |
| Terminal  | `sandbox.pty().create(PtyOptions)` / `attach(session_id, PtyOptions)` → `Pty` (`Stream` of `PtyEvent`, `write`, `resize`, `takeover`, `detach`, `close`)                                       |
| Previews  | `sandbox.preview().create(port, label)` → `https://<label>.<box host>`; `remove(label)`                                                                                                        |
| Other     | `sandbox.logs(LogOptions { since, lines, unit })`, `ssh_info`, `screenshot`; `zb.account().balance / usage / whoami`                                                                           |
| Tailscale | `sandbox.tailscale().status / connect(auth_key) / disconnect`; `sandbox.set_private(enabled)`; `CreateOptions { private: Some(true), .. }`                                                     |

- A command that exits non-zero is a result (`ExecResult::exit_code`), never an error.
- Every failure is a `zipbox::Error` with a stable `code` (`not_found`, `rate_limited`, …)
  and a `docs_url` (`https://zipbox.ai/docs/errors#<code>`).
- An account holds 250 machine names, and a deleted box keeps its name: reuse a fixed pool
  of names with `get_or_create_with` instead of a fresh name per job. A new name past the
  limit returns `ErrorCode::MachineLimitReached`
  ([zipbox.ai/docs/machine-limit](https://zipbox.ai/docs/machine-limit)).
- Relative paths resolve against `/root/workspace`.
- No request or command has a deadline unless you set one (`ExecOptions::timeout_ms`).
- A `Pty` reconnects by itself, forever, with backoff, and re-attaches its session.
- Box tokens are fetched from the control plane and cached; you never handle them.
- A created box's `base_url` changes once, to `box--<name>`, a few minutes after create,
  and the SDK follows it: a box call (or `Pty` dial) that never reached the box (refused
  connect, DNS, TLS handshake) re-reads the box with the wait below and retries there,
  with backoff and no attempt ceiling. A paused, failed or deleted box ends the call with
  the wait's error.

## Tailscale and private boxes

```rust
let zb = zipbox::Zipbox::from_env()?;
let sandbox = zb
    .sandboxes()
    .create(zipbox::CreateOptions {
        name: Some("build".into()),
        private: Some(true), // never public
        ..Default::default()
    })
    .await?;
let auth_key = std::env::var("TS_AUTHKEY").expect("a one-off key from your Tailscale admin");
sandbox.tailscale().connect(&auth_key).await?;
println!("{:?}", sandbox.tailscale().status().await?);
```

`connect` seals the auth key on this machine to the box's own key (P-256 ECDH →
HKDF-SHA256 → AES-256-GCM) and sends only the ciphertext; Zipbox relays it and never
sees the key. Read the key from a file or the environment, never from argv. A private
box is reachable only from devices on your tailnet: off it, box calls fail with
`box_private_unreachable` within 5 s instead of hanging (an open `Pty` keeps
reconnecting). Lifecycle calls, `set_private` and `tailscale()` go through the control
plane and always work. A private box that has not joined your tailnet cannot be ready,
so `create` and `get_or_create` return it once it is running with its key.

Every wait (`create`, `get_or_create`, `wait_ready`, `fork`, `resume`) reads the box's `readiness`
on `GET /sandbox/:id/wait`, which the server answers when the box gets there: it asks again
while the box is `settling`, with no deadline (a retryable failed read waits on a backoff
ladder; an API older than `/wait` is polled instead), returns on `ready` or, for a private
box, `awaits_tailnet`, and raises on the rest, never retryable: paused `box_not_running`, a
failed start `start_failed`, deleting or gone `conflict`. Rows carry `phase` and
`allowed_actions`; a detail also carries `readiness`.

`pause(id)` returns the box paused (nothing is billed while it is paused) and `resume(id)`
returns it ready: at once when the server answers with the final state, else after waiting
for it. A resume on a box whose Pause is still landing waits it out first. `create`
returns at once when the server bound a booted machine inside the request. `stop` and
`start` are aliases of `pause` and `resume`: the same request and the same result.
A box call whose request never reached the box runs this same wait before it retries.
One retry rule: an error is retried when its `retryable` is true.

## Fork a box

```rust
let zb = zipbox::Zipbox::from_env()?;
let child = zb
    .sandboxes()
    .fork(
        "sbx_source",
        zipbox::ForkOptions::new("demo-2", zipbox::ForkSourcePolicy::PauseAndResume),
    )
    .await?; // waits until the new box is ready
println!("{}", child.exec("git status").await?.stdout);
```

A fork is a new box with the source's files, installed software and settings, the same
size, and a new identity. Running processes and RAM are not copied. It starts public,
whatever the source's privacy, unless `private: Some(true)`
(`ForkOptions { private: Some(true), ..ForkOptions::new(name, policy) }`).
`RequirePaused` refuses a running source (`ErrorCode::PauseRequired`); `PauseAndResume`
pauses it for the capture (its sessions disconnect) and resumes it right after. The
request carries one `Idempotency-Key`, reused on every retry, so a retry never makes two
boxes. A fork that ends without a box fails with the fork's own code (`copy_failed`,
`source_gone`, …); a cancelled one with `conflict`. `wait: false` returns the accepted
box at once; follow it with `get_fork(child.id())`. Dropping the future only stops the
wait: cancel the fork with `cancel_transition(child.id())`. A source holding
machine-scoped credentials cannot be forked with an API key
(`ErrorCode::RequiresInteractive`): fork it from the dashboard.

`tailscale_auth_key: Some(key)` joins the new box to your tailnet once it is running: the
key is sealed on this machine to the new box's own key, exactly like
`tailscale().connect`, and never sent with the fork. `Debug` on `ForkOptions` redacts it.

Configuration: `Zipbox::builder()` (`api_key`, `base_url`, `max_retries`,
`retry_base_delay`, `retry_max_delay`, `poll_interval`, `poll_max_interval`,
`pty_base_delay`, `pty_max_delay`, `http_client`), or the `ZIPBOX_API_KEY` /
`ZIPBOX_API_URL` environment variables.

Docs: [zipbox.ai/docs/sdk-rust](https://zipbox.ai/docs/sdk-rust) ·
API reference: [zipbox.ai/openapi.json](https://zipbox.ai/openapi.json) ·
License: Apache-2.0
