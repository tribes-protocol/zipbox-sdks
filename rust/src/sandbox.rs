//! One box: commands, code, logs, SSH, previews and screenshots.

use std::time::Duration;

use crate::backoff::Backoff;
use base64::Engine;
use futures::stream::BoxStream;
use futures::{Stream, StreamExt};
use reqwest::Method;

use crate::client::{encode, json_body, Zipbox};
use crate::commands::{exec_events, Command, Commands};
use crate::error::{Error, ErrorCode, Result};
use crate::files::{resolve_path, Files};
use crate::pty::PtyClient;
use crate::tailscale::Tailscale;
use crate::types::{
    DeleteOptions, ExecEvent, ExecOptions, ExecRecord, ExecRequestBody, ExecResult, ForkOptions,
    Language, LogOptions, OutputStream, Preview, SandboxDetail, SandboxFork, SshInfo,
    TransitionCancelOutcome,
};

/// Captures the desktop (`:10`) with ffmpeg's x11grab, which the platform image
/// ships (it has no ImageMagick). Under pipefail a capture that fails is a
/// non-zero exit, not an empty PNG.
const SCREENSHOT_COMMAND: &str = "set -o pipefail; ffmpeg -loglevel error -f x11grab -i :10 \
     -frames:v 1 -f image2pipe -vcodec png - | base64 -w0";

/// A handle to one box, from [`Zipbox::sandbox`] or the lifecycle calls.
///
/// The box token is fetched on first use and cached for the client's lifetime.
#[derive(Debug, Clone)]
pub struct Sandbox {
    pub(crate) client: Zipbox,
    id: String,
    detail: Option<SandboxDetail>,
}

impl serde::Serialize for Sandbox {
    fn serialize<S: serde::Serializer>(&self, s: S) -> std::result::Result<S::Ok, S::Error> {
        match &self.detail {
            Some(detail) => detail.serialize(s),
            None => serde_json::json!({ "id": self.id }).serialize(s),
        }
    }
}

impl Sandbox {
    pub(crate) fn new(client: Zipbox, id: String, detail: Option<SandboxDetail>) -> Sandbox {
        Sandbox { client, id, detail }
    }

    /// The box id.
    pub fn id(&self) -> &str {
        &self.id
    }

    /// The box as last read, when this handle came from a lifecycle call.
    pub fn info(&self) -> Option<&SandboxDetail> {
        self.detail.as_ref()
    }

    /// Re-reads the box from the control plane.
    pub async fn refresh(&mut self) -> Result<&SandboxDetail> {
        let detail = self.client.detail(&self.id).await?;
        Ok(self.detail.insert(detail))
    }

    /// Waits until the box is ready (see [`crate::Sandboxes::wait_ready`]).
    pub async fn wait_ready(&mut self) -> Result<&SandboxDetail> {
        let ready = self.client.sandboxes().wait_ready(&self.id).await?;
        let detail = ready.detail.expect("wait_ready returns the detail it read");
        Ok(self.detail.insert(detail))
    }

    /// Pauses the box (nothing billed while paused) and returns its detail, paused
    /// (see [`crate::Sandboxes::pause`]).
    pub async fn pause(&mut self) -> Result<&SandboxDetail> {
        let paused = self.client.sandboxes().pause(&self.id).await?;
        let detail = paused.detail.expect("pause returns the detail it read");
        Ok(self.detail.insert(detail))
    }

    /// Resumes the paused box and returns its detail, ready. A box still `stopping`
    /// is resumed once its Pause lands (see [`crate::Sandboxes::resume`]).
    pub async fn resume(&mut self) -> Result<&SandboxDetail> {
        let ready = self.client.sandboxes().resume(&self.id).await?;
        let detail = ready.detail.expect("resume returns the detail it read");
        Ok(self.detail.insert(detail))
    }

    /// Alias of [`Sandbox::pause`]: the same request and the same result.
    pub async fn stop(&mut self) -> Result<&SandboxDetail> {
        self.pause().await
    }

    /// Alias of [`Sandbox::resume`]: the same request and the same result.
    pub async fn start(&mut self) -> Result<&SandboxDetail> {
        self.resume().await
    }

    /// Hard-restarts the box.
    pub async fn restart(&self) -> Result<()> {
        self.client.sandboxes().restart(&self.id).await
    }

    /// Cancels the box's state change in flight (see
    /// [`crate::Sandboxes::cancel_transition`]).
    pub async fn cancel_transition(&self) -> Result<TransitionCancelOutcome> {
        self.client.sandboxes().cancel_transition(&self.id).await
    }

    /// Forks this box into a new box with the same files and a new identity, and
    /// returns a handle on the new box (see [`crate::Sandboxes::fork`]).
    pub async fn fork(&self, options: ForkOptions) -> Result<Sandbox> {
        self.client.sandboxes().fork(&self.id, options).await
    }

    /// The fork that made (or is making) this box (see
    /// [`crate::Sandboxes::get_fork`]).
    pub async fn fork_status(&self) -> Result<SandboxFork> {
        self.client.sandboxes().get_fork(&self.id).await
    }

    /// Deletes the box, keeping a recoverable copy for 30 days.
    pub async fn delete(&self) -> Result<()> {
        self.client.sandboxes().delete(&self.id).await
    }

    /// [`Sandbox::delete`] with options (`archive: Some(false)` skips the copy).
    pub async fn delete_with(&self, options: DeleteOptions) -> Result<()> {
        self.client.sandboxes().delete_with(&self.id, options).await
    }

    /// Moves the box to another tier (`xs` … `xxl`).
    pub async fn set_tier(&self, tier: &str) -> Result<()> {
        self.client.sandboxes().set_tier(&self.id, tier).await
    }

    /// The box's tailnet: connect, status, disconnect.
    pub fn tailscale(&self) -> Tailscale {
        Tailscale {
            client: self.client.clone(),
            id: self.id.clone(),
        }
    }

    /// Makes the box private ("tailnet only") or public again.
    ///
    /// Turning it on needs the box connected to your tailnet
    /// ([`ErrorCode::TailnetNotConnected`] otherwise). A private box answers only
    /// devices on your tailnet; box calls from anywhere else fail with
    /// [`ErrorCode::BoxPrivateUnreachable`].
    pub async fn set_private(&self, enabled: bool) -> Result<()> {
        self.client
            .inner
            .boxes
            .lock()
            .expect("box cache")
            .remove(&self.id);
        self.client
            .api_unit(
                Method::PUT,
                &format!("/sandbox/{}/private", encode(&self.id)),
                json_body(&serde_json::json!({ "enabled": enabled })),
            )
            .await
    }

    // ---- exec ------------------------------------------------------------

    /// Runs `cmd` through `bash -lc` and collects its output. A non-zero exit
    /// status is a result, not an error. No deadline unless you set one with
    /// [`Sandbox::exec_with`].
    pub async fn exec(&self, cmd: &str) -> Result<ExecResult> {
        self.exec_with(cmd, ExecOptions::default()).await
    }

    /// [`Sandbox::exec`] with options. With `options.argv` set, `cmd` is ignored
    /// and the argv is spawned directly.
    pub async fn exec_with(&self, cmd: &str, options: ExecOptions) -> Result<ExecResult> {
        collect(self.exec_stream_with(cmd, options).await?).await
    }

    /// Runs `cmd` and streams its events as they happen. A stream that drops after
    /// the command started re-attaches from its last cursor (see
    /// [`Command::events`]). Dropping the stream before the exit event ends the
    /// command; use [`Sandbox::exec_background`] for anything that must outlive
    /// the connection.
    pub async fn exec_stream(
        &self,
        cmd: &str,
    ) -> Result<impl Stream<Item = Result<ExecEvent>> + Send + 'static> {
        self.exec_stream_with(cmd, ExecOptions::default()).await
    }

    /// [`Sandbox::exec_stream`] with options.
    pub async fn exec_stream_with(
        &self,
        cmd: &str,
        options: ExecOptions,
    ) -> Result<impl Stream<Item = Result<ExecEvent>> + Send + 'static> {
        let body = request_body(cmd, &options, false);
        let resp = self
            .client
            .box_request(
                &self.id,
                Method::POST,
                "/exec",
                || json_body(&body),
                Some("text/event-stream"),
            )
            .await?;
        Ok(resume_on_drop(self.clone(), exec_events(resp).boxed()))
    }

    pub(crate) async fn exec_argv(&self, argv: Vec<String>) -> Result<ExecResult> {
        let options = ExecOptions {
            argv: Some(argv),
            ..ExecOptions::default()
        };
        self.exec_with("", options).await
    }

    /// Starts `cmd` in the background and returns at once. The command keeps
    /// running when this process exits, and survives the box daemon restarting.
    pub async fn exec_background(&self, cmd: &str) -> Result<Command> {
        self.exec_background_with(cmd, ExecOptions::default()).await
    }

    /// [`Sandbox::exec_background`] with options.
    pub async fn exec_background_with(&self, cmd: &str, options: ExecOptions) -> Result<Command> {
        let body = request_body(cmd, &options, true);
        let record: ExecRecord = self
            .client
            .box_json(&self.id, Method::POST, "/exec", || json_body(&body))
            .await?;
        Ok(Command::new(self.clone(), record.id.clone(), Some(record)))
    }

    /// Runs a snippet of code: writes it to a temp file, runs `python3`, `node`
    /// (as an ES module) or `bash` on it through the login shell, then removes it.
    pub async fn run_code(&self, language: Language, code: &str) -> Result<ExecResult> {
        self.run_code_with(language, code, ExecOptions::default())
            .await
    }

    /// [`Sandbox::run_code`] with options (`cwd`, `env`, `timeout_ms`, `user`;
    /// `argv` and `stdin` are ignored).
    pub async fn run_code_with(
        &self,
        language: Language,
        code: &str,
        options: ExecOptions,
    ) -> Result<ExecResult> {
        let path = format!(
            "/tmp/zipbox-run-{}.{}",
            uuid::Uuid::new_v4(),
            language.extension()
        );
        self.files().write(&path, code.as_bytes().to_vec()).await?;
        let script = format!(
            "{} '{path}'; status=$?; rm -f '{path}'; exit $status",
            language.interpreter()
        );
        let options = ExecOptions {
            argv: None,
            stdin: false,
            ..options
        };
        self.exec_with(&script, options).await
    }

    /// Commands on this box, by id.
    pub fn commands(&self) -> Commands {
        Commands {
            sandbox: self.clone(),
        }
    }

    /// Files on this box.
    pub fn files(&self) -> Files {
        Files {
            sandbox: self.clone(),
        }
    }

    /// Terminal sessions on this box.
    pub fn pty(&self) -> PtyClient {
        PtyClient {
            sandbox: self.clone(),
        }
    }

    /// Public URLs for ports on this box.
    pub fn preview(&self) -> Previews {
        Previews {
            sandbox: self.clone(),
        }
    }

    /// The box's system log (`journalctl`). A failed journalctl is a
    /// [`ErrorCode::Conflict`].
    pub async fn logs(&self, options: LogOptions) -> Result<String> {
        let mut argv: Vec<String> = ["journalctl", "-o", "short-iso", "--no-pager"]
            .iter()
            .map(|s| s.to_string())
            .collect();
        if let Some(since) = options.since {
            argv.push("--since".into());
            argv.push(since);
        }
        if let Some(lines) = options.lines {
            argv.push("-n".into());
            argv.push(lines.to_string());
        }
        if let Some(unit) = options.unit {
            argv.push("-u".into());
            argv.push(unit);
        }
        let out = self.exec_argv(argv).await?;
        check_ok(&out, "journalctl")?;
        Ok(out.stdout)
    }

    /// How to SSH in: `ssh root@<ipv6>` (IPv6 only).
    pub async fn ssh_info(&self) -> Result<SshInfo> {
        let detail = self.client.detail(&self.id).await?;
        let host = detail.info.ipv6.clone().ok_or_else(|| {
            Error::new(
                ErrorCode::BoxNotRunning,
                format!("box {} has no address yet", self.id),
            )
        })?;
        Ok(SshInfo {
            command: format!("ssh root@{host}"),
            host,
            user: "root".to_string(),
        })
    }

    /// A PNG of the box's desktop.
    pub async fn screenshot(&self) -> Result<Vec<u8>> {
        let out = self.exec(SCREENSHOT_COMMAND).await?;
        if out.exit_code != Some(0) {
            return Err(Error::new(
                ErrorCode::Conflict,
                format!("screenshot failed: {}", out.stderr.trim()),
            ));
        }
        base64::engine::general_purpose::STANDARD
            .decode(out.stdout.trim())
            .map_err(Error::decode)
    }

    pub(crate) async fn box_host(&self) -> Result<String> {
        let access = self.client.box_access(&self.id, false).await?;
        url::Url::parse(&access.base_url)
            .ok()
            .and_then(|u| {
                u.host_str()
                    // `box--` on a public box, `box.` on a private one (it serves
                    // itself on box.<apex>); only one prefix is ever dropped.
                    .map(|host| {
                        host.strip_prefix("box--")
                            .or_else(|| host.strip_prefix("box."))
                            .unwrap_or(host)
                            .to_string()
                    })
            })
            .ok_or_else(|| Error::new(ErrorCode::Unavailable, "box origin has no host"))
    }
}

/// Public URLs for ports on a box, from [`Sandbox::preview`]. Previews are
/// public: anyone with the URL reaches the port.
#[derive(Debug, Clone)]
pub struct Previews {
    sandbox: Sandbox,
}

impl Previews {
    /// Serves `127.0.0.1:<port>` on the box at `https://<label>.<box host>`.
    /// Default label: `p<port>`.
    pub async fn create(&self, port: u16, label: Option<&str>) -> Result<Preview> {
        let label = label
            .map(str::to_string)
            .unwrap_or_else(|| format!("p{port}"));
        let host = format!("{label}.{}", self.sandbox.box_host().await?);
        let mut backoff = Backoff::new(
            self.sandbox.client.inner.retry_base_delay,
            self.sandbox.client.inner.retry_max_delay,
        );
        loop {
            let retry_after = match self
                .sandbox
                .exec_argv(vec![
                    "tribes-caddy".into(),
                    "add".into(),
                    host.clone(),
                    format!("127.0.0.1:{port}"),
                    "--no-wait".into(),
                ])
                .await
            {
                Ok(out) => {
                    if out.exit_code == Some(0) {
                        break;
                    }
                    if out.exit_code != Some(75) {
                        check_ok(&out, "tribes-caddy add")?;
                    }
                    let pending: serde_json::Value =
                        serde_json::from_str(out.stdout.trim().lines().last().unwrap_or(""))
                            .map_err(|_| {
                                Error::new(
                                    ErrorCode::Conflict,
                                    "preview returned invalid pending status",
                                )
                            })?;
                    let delay = pending["retryAfterMs"]
                        .as_u64()
                        .filter(|_| pending["status"] == "pending")
                        .ok_or_else(|| {
                            Error::new(
                                ErrorCode::Conflict,
                                "preview returned invalid pending status",
                            )
                        })?;
                    Duration::from_millis(delay)
                }
                Err(error) if error.retryable => error.retry_after.unwrap_or_default(),
                Err(error) => return Err(error),
            };
            tokio::time::sleep(retry_after.max(backoff.next_delay())).await;
        }
        Ok(Preview {
            url: format!("https://{host}"),
            label,
        })
    }

    /// Stops serving a preview, by its label (or full host).
    pub async fn remove(&self, label: &str) -> Result<()> {
        let box_host = self.sandbox.box_host().await?;
        let host = if label.ends_with(&format!(".{box_host}")) {
            label.to_string()
        } else {
            format!("{label}.{box_host}")
        };
        let out = self
            .sandbox
            .exec_argv(vec!["tribes-caddy".into(), "rm".into(), host])
            .await?;
        check_ok(&out, "tribes-caddy rm")
    }
}

fn check_ok(out: &ExecResult, what: &str) -> Result<()> {
    if out.exit_code == Some(0) {
        return Ok(());
    }
    Err(Error::new(
        ErrorCode::Conflict,
        format!("{what} failed: {}", out.stderr.trim()),
    ))
}

fn request_body(cmd: &str, options: &ExecOptions, background: bool) -> ExecRequestBody {
    let argv = options.argv.clone();
    ExecRequestBody {
        cmd: if argv.is_some() {
            None
        } else {
            Some(cmd.to_string())
        },
        argv,
        cwd: options.cwd.as_deref().map(resolve_path),
        env: if options.env.is_empty() {
            None
        } else {
            Some(options.env.clone())
        },
        user: options.user.clone(),
        timeout_ms: options.timeout_ms,
        background: if background { Some(true) } else { None },
        stdin: if options.stdin { Some(true) } else { None },
    }
}

/// A foreground command's events: the `POST /exec` stream, then, if it drops after
/// the command started, the command's followed stream from the last cursor.
fn resume_on_drop(
    sandbox: Sandbox,
    first: BoxStream<'static, Result<ExecEvent>>,
) -> impl Stream<Item = Result<ExecEvent>> + Send + 'static {
    struct Foreground {
        sandbox: Sandbox,
        stream: BoxStream<'static, Result<ExecEvent>>,
        command_id: Option<String>,
        cursor: String,
        resumed: bool,
        done: bool,
    }
    let state = Foreground {
        sandbox,
        stream: first,
        command_id: None,
        cursor: "0-0".to_string(),
        resumed: false,
        done: false,
    };
    futures::stream::unfold(state, |mut st| async move {
        if st.done {
            return None;
        }
        let lost = match st.stream.next().await {
            Some(Ok(event)) => {
                st.cursor = event.cursor().to_string();
                if let ExecEvent::Start { id, .. } = &event {
                    st.command_id = Some(id.clone());
                }
                st.done = matches!(event, ExecEvent::Exit { .. });
                return Some((Ok(event), st));
            }
            Some(Err(err)) => err,
            None => Error::new(
                ErrorCode::Unavailable,
                "the box closed the command stream before the command exited",
            ),
        };
        // The followed stream reconnects on its own; its error is final.
        let id = match (&st.command_id, st.resumed) {
            (Some(id), false) if lost.retryable => id.clone(),
            _ => {
                st.done = true;
                return Some((Err(lost), st));
            }
        };
        let followed = st
            .sandbox
            .commands()
            .command(&id)
            .events(Some(&st.cursor))
            .await;
        match followed {
            Ok(stream) => {
                st.stream = stream.boxed();
                st.resumed = true;
                match st.stream.next().await {
                    Some(Ok(event)) => {
                        st.cursor = event.cursor().to_string();
                        st.done = matches!(event, ExecEvent::Exit { .. });
                        Some((Ok(event), st))
                    }
                    Some(Err(err)) => {
                        st.done = true;
                        Some((Err(err), st))
                    }
                    None => None,
                }
            }
            Err(err) => {
                st.done = true;
                Some((Err(err), st))
            }
        }
    })
}

pub(crate) async fn collect(
    stream: impl Stream<Item = Result<ExecEvent>> + Send + 'static,
) -> Result<ExecResult> {
    let mut stream = Box::pin(stream);
    let mut result = ExecResult {
        id: String::new(),
        exit_code: None,
        stdout: String::new(),
        stderr: String::new(),
    };
    while let Some(event) = stream.next().await {
        match event? {
            ExecEvent::Start { id, .. } => result.id = id,
            ExecEvent::Output { stream, data, .. } => match stream {
                OutputStream::Stdout => result.stdout.push_str(&data),
                OutputStream::Stderr => result.stderr.push_str(&data),
            },
            ExecEvent::Gap { .. } => {}
            ExecEvent::Exit { exit_code, .. } => {
                result.exit_code = exit_code;
                return Ok(result);
            }
        }
    }
    Err(Error::new(
        ErrorCode::Unavailable,
        "the box closed the command stream before the command exited",
    ))
}

#[cfg(test)]
mod tests {
    use super::SCREENSHOT_COMMAND;
    use crate::client::{BoxAccess, Zipbox};

    #[tokio::test]
    async fn preview_apex_preserves_transport_origin() {
        for (origin, apex) in [
            ("https://box--demo.zbox.sh", "demo.zbox.sh"),
            ("https://box--box--demo.zbox.sh", "box--demo.zbox.sh"),
            ("https://box.demo.zbox.sh", "demo.zbox.sh"),
            ("https://box--box.zbox.sh", "box.zbox.sh"),
            ("https://demo.zbox.sh", "demo.zbox.sh"),
            ("https://mailbox--demo.zbox.sh", "mailbox--demo.zbox.sh"),
            ("http://127.0.0.1:9100", "127.0.0.1"),
        ] {
            let client = Zipbox::new("key").unwrap();
            client.inner.boxes.lock().unwrap().insert(
                "box".into(),
                BoxAccess {
                    base_url: origin.into(),
                    token: "bt_test".into(),
                    name: "box".into(),
                    private: false,
                    reachable: false,
                },
            );
            assert_eq!(client.sandbox("box").box_host().await.unwrap(), apex);
            assert_eq!(
                client.box_access("box", false).await.unwrap().base_url,
                origin
            );
        }
    }

    #[test]
    fn screenshot_uses_the_images_ffmpeg_and_fails_loudly() {
        assert!(SCREENSHOT_COMMAND.starts_with("set -o pipefail;"));
        assert!(SCREENSHOT_COMMAND.contains("ffmpeg -loglevel error -f x11grab -i :10 -frames:v 1"));
        assert!(!SCREENSHOT_COMMAND.contains("import "));
    }
}
