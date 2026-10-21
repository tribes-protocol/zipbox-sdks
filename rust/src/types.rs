//! The data types the API returns and accepts.

use std::collections::BTreeMap;

use serde::{Deserialize, Serialize};
use serde_json::{Map, Value};

/// A box as `GET /sandbox` lists it. Only the documented fields are named; the
/// rest of the row is kept in `extra` and may change without notice.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct SandboxInfo {
    /// The box id.
    pub id: String,
    /// The box's permanent subdomain.
    #[serde(default)]
    pub name: Option<String>,
    /// The domain the box is served under (`<name>.<domain>`).
    #[serde(default)]
    pub domain: Option<String>,
    /// The box's public IPv6 address (SSH goes here directly).
    #[serde(default)]
    pub ipv6: Option<String>,
    /// `provisioning`, `running`, `stopping`, `stopped`, `archived` or `destroyed`.
    pub status: String,
    /// The machine tier (`xs` … `xxl`).
    #[serde(default)]
    pub tier: Option<String>,
    /// The tier an in-flight upgrade is moving to.
    #[serde(default)]
    pub pending_tier: Option<String>,
    /// vCPUs, memory and disk the box runs with.
    #[serde(default)]
    pub machine_size: Option<Value>,
    /// True while the disk is archived (a stopped box) or being archived.
    #[serde(default)]
    pub is_archived: Option<bool>,
    /// CPU use, percent of the box's vCPUs.
    #[serde(default)]
    pub cpu_pct: Option<f64>,
    /// Memory use, percent.
    #[serde(default)]
    pub mem_pct: Option<f64>,
    /// Disk use, percent.
    #[serde(default)]
    pub disk_pct: Option<f64>,
    /// Network receive rate, bytes per second.
    #[serde(default)]
    pub net_rx_bps: Option<f64>,
    /// Network transmit rate, bytes per second.
    #[serde(default)]
    pub net_tx_bps: Option<f64>,
    /// When the stats above were sampled.
    #[serde(default)]
    pub stats_updated_at: Option<String>,
    /// Set when the last start failed.
    #[serde(default)]
    pub start_failed_at: Option<String>,
    /// When the box was claimed.
    #[serde(default)]
    pub claimed_at: Option<String>,
    /// When the row was created.
    #[serde(default)]
    pub created_at: Option<String>,
    /// When the row last changed.
    #[serde(default)]
    pub updated_at: Option<String>,
    /// Undocumented fields of the row.
    #[serde(flatten)]
    pub extra: Map<String, Value>,
}

/// A box as `GET /sandbox/:id` returns it.
///
/// `box_token` is a credential: it is never serialized and `Debug` redacts it, so
/// logging or printing a detail (or a [`crate::Sandbox`]) cannot leak it.
#[derive(Clone, Serialize, Deserialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct SandboxDetail {
    /// The row.
    #[serde(flatten)]
    pub info: SandboxInfo,
    /// Running and serving the owner's generation: the box answers requests.
    #[serde(default)]
    pub ready: bool,
    /// Whether `base_url` completes a TLS handshake with a trusted certificate yet.
    /// A fresh name's certificate can trail `ready`; the client's box calls retry the
    /// handshake meanwhile.
    #[serde(default)]
    pub https_ready: bool,
    /// The box's HTTPS origin, once ready.
    #[serde(default)]
    pub base_url: Option<String>,
    /// The per-key, per-box token the box accepts. Managed by the client.
    #[serde(default, skip_serializing)]
    pub box_token: Option<String>,
}

impl std::fmt::Debug for SandboxDetail {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("SandboxDetail")
            .field("info", &self.info)
            .field("ready", &self.ready)
            .field("https_ready", &self.https_ready)
            .field("base_url", &self.base_url)
            .field("box_token", &self.box_token.as_ref().map(|_| "<redacted>"))
            .finish()
    }
}

impl std::ops::Deref for SandboxDetail {
    type Target = SandboxInfo;
    fn deref(&self) -> &SandboxInfo {
        &self.info
    }
}

/// Options for [`crate::Sandboxes::create`].
#[derive(Debug, Clone, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct CreateOptions {
    /// The box's permanent subdomain. `None` = the account's default box.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub name: Option<String>,
    /// The machine tier (`xs` … `xxl`).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub tier: Option<String>,
    /// An OpenSSH public key to authorize for `root`.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub ssh_public_key: Option<String>,
}

/// Options for a command beyond the command line itself.
#[derive(Debug, Clone, Default)]
pub struct ExecOptions {
    /// Spawn this argv directly instead of running `cmd` through `bash -lc`.
    pub argv: Option<Vec<String>>,
    /// Working directory (absolute, or relative to `/root/workspace`).
    pub cwd: Option<String>,
    /// Extra environment variables.
    pub env: BTreeMap<String, String>,
    /// Kill the command after this many milliseconds. Default: no deadline.
    pub timeout_ms: Option<u64>,
    /// Run as another local user. Default: the box user.
    pub user: Option<String>,
    /// Open a stdin pipe (background commands; see [`crate::Command::stdin`]).
    pub stdin: bool,
}

#[derive(Debug, Clone, Default, Serialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct ExecRequestBody {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub argv: Option<Vec<String>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub cmd: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub cwd: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub env: Option<BTreeMap<String, String>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub user: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub timeout_ms: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub background: Option<bool>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub stdin: Option<bool>,
}

/// A finished foreground command.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct ExecResult {
    /// The command id on the box.
    pub id: String,
    /// Shell-style exit status (`128 + N` when killed by signal N). A non-zero
    /// status is a result, not an error.
    pub exit_code: Option<i32>,
    /// Everything the command wrote to stdout (lossy UTF-8).
    pub stdout: String,
    /// Everything the command wrote to stderr (lossy UTF-8).
    pub stderr: String,
}

/// A command as the box records it.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct ExecRecord {
    /// The command id.
    pub id: String,
    /// `running`, `exited` or `unknown`.
    pub state: String,
    /// What was spawned.
    #[serde(default)]
    pub argv: Vec<String>,
    /// Where it ran.
    #[serde(default)]
    pub cwd: String,
    /// Whether it survives the client disconnecting.
    #[serde(default)]
    pub background: bool,
    /// The exit status, once exited.
    #[serde(default)]
    pub exit_code: Option<i32>,
    /// The signal the box delivered, if it delivered one.
    #[serde(default)]
    pub signal: Option<String>,
    /// Whether `timeout_ms` ended it.
    #[serde(default)]
    pub timed_out: bool,
    /// ISO-8601 start time.
    #[serde(default)]
    pub started_at: String,
    /// ISO-8601 end time, once exited.
    #[serde(default)]
    pub ended_at: Option<String>,
    /// Bytes of stdout recorded.
    #[serde(default)]
    pub stdout_bytes: u64,
    /// Bytes of stderr recorded.
    #[serde(default)]
    pub stderr_bytes: u64,
}

/// Which output stream.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum OutputStream {
    /// Standard output.
    Stdout,
    /// Standard error.
    Stderr,
}

/// One event of a command's output stream. `cursor` is the resume point for
/// [`crate::Command::events`].
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ExecEvent {
    /// The command started.
    Start {
        /// The command id.
        id: String,
        /// Resume cursor.
        cursor: String,
    },
    /// Output on stdout or stderr.
    Output {
        /// Which stream.
        stream: OutputStream,
        /// The text.
        data: String,
        /// Resume cursor.
        cursor: String,
    },
    /// The stream hit the box's 64 MiB per-stream cap; later output was discarded.
    Gap {
        /// Which stream.
        stream: OutputStream,
        /// Why.
        message: String,
        /// Resume cursor.
        cursor: String,
    },
    /// The command ended.
    Exit {
        /// `exited` or `unknown`.
        state: String,
        /// The exit status.
        exit_code: Option<i32>,
        /// The signal the box delivered, if any.
        signal: Option<String>,
        /// Whether a timeout ended it.
        timed_out: bool,
        /// Resume cursor.
        cursor: String,
    },
}

impl ExecEvent {
    /// The resume cursor of this event.
    pub fn cursor(&self) -> &str {
        match self {
            ExecEvent::Start { cursor, .. }
            | ExecEvent::Output { cursor, .. }
            | ExecEvent::Gap { cursor, .. }
            | ExecEvent::Exit { cursor, .. } => cursor,
        }
    }
}

/// A signal for [`crate::Command::signal`].
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "UPPERCASE")]
pub enum Signal {
    /// SIGTERM.
    Term,
    /// SIGKILL.
    Kill,
    /// SIGINT.
    Int,
    /// SIGHUP.
    Hup,
    /// SIGQUIT.
    Quit,
}

impl std::str::FromStr for Signal {
    type Err = crate::Error;
    fn from_str(s: &str) -> crate::Result<Signal> {
        match s.trim_start_matches("SIG").to_ascii_uppercase().as_str() {
            "TERM" => Ok(Signal::Term),
            "KILL" => Ok(Signal::Kill),
            "INT" => Ok(Signal::Int),
            "HUP" => Ok(Signal::Hup),
            "QUIT" => Ok(Signal::Quit),
            other => Err(crate::Error::new(
                crate::ErrorCode::InvalidRequest,
                format!("unknown signal {other}"),
            )),
        }
    }
}

/// The language for [`crate::Sandbox::run_code`].
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Language {
    /// `python3`.
    Python,
    /// `node`.
    Node,
    /// `bash`.
    Bash,
}

impl Language {
    pub(crate) fn interpreter(self) -> &'static str {
        match self {
            Language::Python => "python3",
            Language::Node => "node",
            Language::Bash => "bash",
        }
    }

    pub(crate) fn extension(self) -> &'static str {
        match self {
            Language::Python => "py",
            Language::Node => "mjs",
            Language::Bash => "sh",
        }
    }
}

/// What a directory entry is.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum FileKind {
    /// A directory.
    Directory,
    /// A regular file.
    File,
    /// A symbolic link.
    Symlink,
    /// Anything else.
    Other,
}

/// One directory entry.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct FileEntry {
    /// The entry's name (not its path).
    pub name: String,
    /// What it is.
    pub kind: FileKind,
    /// Size in bytes.
    pub size_bytes: u64,
    /// Modification time, epoch milliseconds.
    pub modified_at_ms: f64,
}

#[derive(Debug, Clone, Deserialize)]
pub(crate) struct FileListing {
    pub entries: Vec<FileEntry>,
}

/// A public URL serving a port on the box.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct Preview {
    /// `https://<label>.<box host>`.
    pub url: String,
    /// The label (the leftmost DNS label of the URL).
    pub label: String,
}

/// Options for [`crate::Sandbox::logs`].
#[derive(Debug, Clone, Default)]
pub struct LogOptions {
    /// Anything `journalctl --since` accepts (`"10 min ago"`, an ISO time).
    pub since: Option<String>,
    /// At most this many lines.
    pub lines: Option<u32>,
}

/// How to SSH into the box.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct SshInfo {
    /// The box's IPv6 address.
    pub host: String,
    /// The login user.
    pub user: String,
    /// A ready-to-run command line.
    pub command: String,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn box_token_is_read_but_never_written_or_printed() {
        let detail: SandboxDetail = serde_json::from_str(
            r#"{"id":"sbx_1","status":"running","ready":true,"baseUrl":"https://d.zbox.sh","boxToken":"bt_secret"}"#,
        )
        .unwrap();
        assert_eq!(detail.box_token.as_deref(), Some("bt_secret"));
        let json = serde_json::to_string(&detail).unwrap();
        assert!(
            !json.contains("bt_secret") && !json.contains("boxToken"),
            "{json}"
        );
        assert!(json.contains(r#""baseUrl":"https://d.zbox.sh""#), "{json}");
        let debug = format!("{detail:?}");
        assert!(
            !debug.contains("bt_secret") && debug.contains("<redacted>"),
            "{debug}"
        );
    }
}
