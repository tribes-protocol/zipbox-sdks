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
    let sandbox = zb.sandboxes().get_or_create("demo").await?;
    let out = sandbox.exec("echo hello").await?;
    print!("{}", out.stdout);
    Ok(())
}
```

## What you can do

| Area      | Calls                                                                                                                                                                         |
| --------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Lifecycle | `zb.sandboxes().create / get_or_create / list / get / start / stop / restart / delete / set_tier`                                                                             |
| Commands  | `sandbox.exec`, `exec_with`, `exec_stream` (`Stream` of events), `exec_background` → `Command` (`get`, `events(since)`, `wait`, `stdin`, `close_stdin`, `signal`), `run_code` |
| Files     | `sandbox.files().read / read_text / write / list / stat / mkdir / move_to / delete / upload / download`                                                                       |
| Terminal  | `sandbox.pty().create(cols, rows)` / `attach(session_id)` → `Pty` (`Stream` of `PtyEvent`, `write`, `resize`, `detach`, `close`)                                              |
| Previews  | `sandbox.preview().create(port, label)` → `https://<label>.<box host>`; `remove(label)`                                                                                       |
| Other     | `sandbox.logs`, `ssh_info`, `screenshot`; `zb.account().balance / usage`                                                                                                      |

- A command that exits non-zero is a result (`ExecResult::exit_code`), never an error.
- Every failure is a `zipbox::Error` with a stable `code` (`not_found`, `rate_limited`, …)
  and a `docs_url` (`https://zipbox.ai/docs/errors#<code>`).
- Relative paths resolve against `/root/workspace`.
- No request or command has a deadline unless you set one (`ExecOptions::timeout_ms`).
- A `Pty` reconnects by itself, forever, with backoff, and re-attaches its session.
- Box tokens are fetched from the control plane and cached; you never handle them.

Configuration: `Zipbox::builder()` (`api_key`, `base_url`, `max_retries`,
`http_client`, …), or the `ZIPBOX_API_KEY` / `ZIPBOX_API_URL` environment variables.

Docs: [zipbox.ai/docs/sdk-rust](https://zipbox.ai/docs/sdk-rust) ·
API reference: [zipbox.ai/openapi.json](https://zipbox.ai/openapi.json) ·
License: Apache-2.0
