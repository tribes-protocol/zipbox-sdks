//! One box: commands, code, logs, SSH, previews and screenshots.

use base64::Engine;
use futures::{Stream, StreamExt};
use reqwest::Method;

use crate::client::{json_body, Zipbox};
use crate::commands::{exec_events, Command, Commands};
use crate::error::{Error, ErrorCode, Result};
use crate::files::{resolve_path, Files};
use crate::pty::PtyClient;
use crate::types::{
    ExecEvent, ExecOptions, ExecRecord, ExecRequestBody, ExecResult, Language, LogOptions,
    OutputStream, Preview, SandboxDetail, SshInfo,
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

    /// Starts the box if it is stopped.
    pub async fn start(&self) -> Result<()> {
        self.client.sandboxes().start(&self.id).await
    }

    /// Stops the box.
    pub async fn stop(&self) -> Result<()> {
        self.client.sandboxes().stop(&self.id).await
    }

    /// Hard-restarts the box.
    pub async fn restart(&self) -> Result<()> {
        self.client.sandboxes().restart(&self.id).await
    }

    /// Deletes the box.
    pub async fn delete(&self) -> Result<()> {
        self.client.sandboxes().delete(&self.id).await
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

    /// Runs `cmd` and streams its events as they happen. Dropping the stream
    /// before the exit event ends the command; use [`Sandbox::exec_background`]
    /// for anything that must outlive the connection.
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
        Ok(exec_events(resp))
    }

    pub(crate) async fn exec_argv(&self, argv: Vec<String>) -> Result<ExecResult> {
        let body = ExecRequestBody {
            argv: Some(argv),
            ..ExecRequestBody::default()
        };
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
        collect(exec_events(resp)).await
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

    /// Runs a snippet of code: writes it to a temp file and runs `python3`,
    /// `node` or `bash` on it.
    pub async fn run_code(&self, language: Language, code: &str) -> Result<ExecResult> {
        let path = format!(
            "/tmp/zipbox-run-{}.{}",
            uuid::Uuid::new_v4(),
            language.extension()
        );
        self.files().write(&path, code.as_bytes().to_vec()).await?;
        self.exec_argv(vec![language.interpreter().to_string(), path])
            .await
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

    /// The box's system log (`journalctl`).
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
        let out = self.exec_argv(argv).await?;
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
            .and_then(|u| u.host_str().map(str::to_string))
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
        let out = self
            .sandbox
            .exec_argv(vec![
                "tribes-caddy".into(),
                "add".into(),
                host.clone(),
                format!("127.0.0.1:{port}"),
            ])
            .await?;
        check_ok(&out, "tribes-caddy add")?;
        Ok(Preview {
            url: format!("https://{host}"),
            label,
        })
    }

    /// Stops serving a preview.
    pub async fn remove(&self, label: &str) -> Result<()> {
        let host = format!("{label}.{}", self.sandbox.box_host().await?);
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

async fn collect(
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

    #[test]
    fn screenshot_uses_the_images_ffmpeg_and_fails_loudly() {
        assert!(SCREENSHOT_COMMAND.starts_with("set -o pipefail;"));
        assert!(SCREENSHOT_COMMAND.contains("ffmpeg -loglevel error -f x11grab -i :10 -frames:v 1"));
        assert!(!SCREENSHOT_COMMAND.contains("import "));
    }
}
