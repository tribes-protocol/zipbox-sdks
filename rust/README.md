# zipbox

Drive [Zipbox](https://zipbox.ai) boxes from Rust: create a box, run commands, move
files, open a terminal, publish a port, stop it when you are done. Nothing on a box
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
| Lifecycle | `zb.sandboxes().create / get_or_create / get_or_create_with / list / get / wait_ready / start / stop / restart / delete / delete_with / set_tier`                                              |
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

Configuration: `Zipbox::builder()` (`api_key`, `base_url`, `max_retries`,
`retry_base_delay`, `retry_max_delay`, `poll_interval`, `poll_max_interval`,
`pty_base_delay`, `pty_max_delay`, `http_client`), or the `ZIPBOX_API_KEY` /
`ZIPBOX_API_URL` environment variables.

Docs: [zipbox.ai/docs/sdk-rust](https://zipbox.ai/docs/sdk-rust) ·
API reference: [zipbox.ai/openapi.json](https://zipbox.ai/openapi.json) ·
License: Apache-2.0
