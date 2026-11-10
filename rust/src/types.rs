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
    /// `provisioning`, `running`, `stopping`, `pausing` (paused: the VM is stopped and its
    /// disk is safe on its host; its backup is still uploading; a start boots it at once),
    /// `stopped`, `archived`, `deleting` (signing out of its tailnet before it is
    /// destroyed) or `destroyed`.
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
    /// Private ("tailnet only"): the box answers only devices on the owner's tailnet.
    #[serde(default)]
    pub private_mode: bool,
    /// The box's own P-256 public key (base64 SPKI DER) that box credentials are
    /// sealed to. `None` until the box has started; a claim re-mints it.
    #[serde(default)]
    pub agent_public_key: Option<String>,
    /// The box's tailnet. `None` when it never joined one and no key is pending.
    #[serde(default)]
    pub tailscale: Option<TailscaleStatus>,
    /// `Some(false)` when the box's ZipOS predates Tailscale: connecting fails with
    /// [`crate::ErrorCode::TailscaleRequiresUpdate`] until the ZipOS update is
    /// accepted. `None` until the box has reported since it booted.
    #[serde(default)]
    pub tailscale_supported: Option<bool>,
    /// The fork still making this box: its phase, source recovery, waits and error.
    /// `None` on every box not being forked into.
    #[serde(default)]
    pub fork: Option<SandboxFork>,
    /// What the box is doing now: `starting`, `running`, `stopping`, `paused`,
    /// `resizing`, `forking`, `deleting`, `delete_failed` (a delete was refused; the
    /// box still runs), `start_failed` or `gone`. `None` from an older control plane.
    #[serde(default)]
    pub phase: Option<String>,
    /// The lifecycle verbs the box accepts now (`start`, `stop`, `restart`, `delete`,
    /// `cancel_transition`, `fork`). A listed verb can still be refused for a reason
    /// outside the box: credit, capacity, an argument. `None` from an older control
    /// plane.
    #[serde(default)]
    pub allowed_actions: Option<Vec<String>>,
    /// Undocumented fields of the row.
    #[serde(flatten)]
    pub extra: Map<String, Value>,
}

/// A box's tailnet: its live status merged with the last known identity.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct TailscaleStatus {
    /// `joining`, `connected`, `needsApproval`, `needsLogin`, `stopped` or `failed`;
    /// `None` when only the identity is known (the box is stopped).
    #[serde(default)]
    pub state: Option<String>,
    /// The tailnet's name, e.g. `example.com`.
    #[serde(default)]
    pub tailnet: Option<String>,
    /// The box's tailnet IPv4 (`100.x`).
    #[serde(default)]
    pub ipv4: Option<String>,
    /// The box's tailnet IPv6.
    #[serde(default)]
    pub ipv6: Option<String>,
    /// The box's MagicDNS name.
    #[serde(default)]
    pub dns_name: Option<String>,
    /// When the node key expires (ISO-8601); `None` when expiry is off, which is
    /// the recommended setting.
    #[serde(default)]
    pub key_expiry: Option<String>,
    /// True while an auth key is on its way to the box. Dropping without the state
    /// advancing means the key never arrived: connect again with a new one.
    #[serde(default)]
    pub pending: bool,
    /// Your other devices on the tailnet (online first, at most 50); `None` while
    /// the box is not reporting.
    #[serde(default)]
    pub peers: Option<Vec<TailscalePeer>>,
    /// Undocumented fields.
    #[serde(flatten)]
    pub extra: Map<String, Value>,
}

/// One of your other devices on the tailnet, as the box sees it.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct TailscalePeer {
    /// The device's own name, e.g. `lucys-macbook`.
    #[serde(default)]
    pub name: String,
    /// Its MagicDNS name.
    #[serde(default)]
    pub dns_name: Option<String>,
    /// `macOS`, `linux`, `iOS`, `windows`, `android`, …
    #[serde(default)]
    pub os: Option<String>,
    /// Connected to Tailscale right now.
    #[serde(default)]
    pub online: bool,
    /// Its tailnet IPv4 (`100.x`).
    #[serde(default)]
    pub ipv4: Option<String>,
    /// `direct` (peer to peer), `relay` (through a Tailscale DERP relay), or `None`
    /// (no recent traffic). A string, so a later value never breaks parsing.
    #[serde(default)]
    pub connection: Option<String>,
    /// Undocumented fields.
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
    /// Whether the box will answer, from the same connection gate as `ready`: what
    /// every wait reads. `ready`, `settling`, `awaits_tailnet`, `paused`,
    /// `start_failed`, `deleting` or `gone`. `None` from an older control plane, where
    /// the client falls back to `ready` and `status`.
    #[serde(default)]
    pub readiness: Option<String>,
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
            .field("readiness", &self.readiness)
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

/// Options for [`crate::Sandboxes::create`]. [`crate::Sandboxes::get_or_create_with`]
/// takes the same options and applies them only when it creates the box.
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
    /// Create the box's mailbox now. `None` = created the first time the box uses mail.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub mailbox: Option<bool>,
    /// Create the box private ("tailnet only"): it is never public. A private box
    /// is not ready until it joins your tailnet, so `create` returns once it is
    /// running with its key; call [`crate::Tailscale::connect`] next.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub private: Option<bool>,
}

/// How a fork treats its source, for [`ForkOptions::source_policy`].
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ForkSourcePolicy {
    /// Refuse a running source ([`crate::ErrorCode::PauseRequired`]). A paused source
    /// stays paused.
    RequirePaused,
    /// Let a running source pause for its capture: its sessions disconnect and it
    /// resumes right after. A paused source stays paused.
    PauseAndResume,
}

impl ForkSourcePolicy {
    /// The wire value: `require_paused` or `pause_and_resume`.
    pub fn as_str(self) -> &'static str {
        match self {
            ForkSourcePolicy::RequirePaused => "require_paused",
            ForkSourcePolicy::PauseAndResume => "pause_and_resume",
        }
    }
}

/// Options for [`crate::Sandboxes::fork`]. Build them with [`ForkOptions::new`]:
/// it waits for the new box by default, like the other SDKs.
///
/// `Debug` redacts `tailscale_auth_key`, so printing the options cannot leak it.
#[derive(Clone)]
pub struct ForkOptions {
    /// The new box's permanent name. It is kept even if the fork is cancelled.
    pub name: String,
    /// What the fork may do to a running source.
    pub source_policy: ForkSourcePolicy,
    /// Start the new box private ("tailnet only"): never public, reachable only
    /// from your tailnet once it joins. `None` or `Some(false)` = public, whatever
    /// the source's privacy.
    pub private: Option<bool>,
    /// A Tailscale auth key to join the new box to your tailnet once it runs. It is
    /// sealed on this machine to the new box's own key and never sent to `/fork`.
    /// Needs `wait`.
    pub tailscale_auth_key: Option<String>,
    /// Wait until the new box is ready (a private one: running with its key, then
    /// enrolled when a key is given). `false` returns the accepted box at once.
    /// Default `true`. Dropping the future only stops the wait; the fork goes on.
    pub wait: bool,
}

impl ForkOptions {
    /// Fork into a box named `name`, waiting for it, with no Tailscale key.
    pub fn new(name: impl Into<String>, source_policy: ForkSourcePolicy) -> ForkOptions {
        ForkOptions {
            name: name.into(),
            source_policy,
            private: None,
            tailscale_auth_key: None,
            wait: true,
        }
    }
}

impl std::fmt::Debug for ForkOptions {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ForkOptions")
            .field("name", &self.name)
            .field("source_policy", &self.source_policy)
            .field("private", &self.private)
            .field(
                "tailscale_auth_key",
                &self.tailscale_auth_key.as_ref().map(|_| "<redacted>"),
            )
            .field("wait", &self.wait)
            .finish()
    }
}

/// Why a fork ended without a box.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct ForkError {
    /// `source_unsupported`, `source_gone`, `identity_reused`, `copy_failed` or
    /// `start_failed`. A string, so a later value never breaks parsing.
    pub code: String,
    /// Human-readable detail.
    pub message: String,
}

/// One fork operation's progress (`GET /sandbox/:id/fork` on the new box).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct SandboxFork {
    /// The durable operation's id.
    pub operation_id: String,
    /// The box being forked.
    pub source_sandbox_id: String,
    /// The new box.
    pub child_sandbox_id: String,
    /// `require_paused` or `pause_and_resume`.
    pub source_policy: String,
    /// `preparing_identity`, `queued`, `capturing`, `copying`, `starting`,
    /// `complete`, `cancelling`, `cancelled` or `failed`. `complete`: the new box has
    /// its own disk and serves.
    pub phase: String,
    /// `not_needed` (the source was paused and stays paused), `pending` (its resume
    /// is owed), `resumed` or `superseded` (a newer pause or delete replaced it).
    pub source_recovery: String,
    /// `reflink` or `copy`, once the source is captured.
    #[serde(default)]
    pub capture_mode: Option<String>,
    /// Why the current step waits (`software_install`, `legacy_chisel`, `busy`,
    /// `host_offline`, `capacity`); `None` while it moves.
    #[serde(default)]
    pub waiting_on: Option<String>,
    /// The retry ladder's position for the current step.
    #[serde(default)]
    pub attempt: u32,
    /// When the current step is retried next.
    #[serde(default)]
    pub next_attempt_at: Option<String>,
    /// When a cancel was asked for.
    #[serde(default)]
    pub cancel_requested_at: Option<String>,
    /// Why a `failed` fork ended.
    #[serde(default)]
    pub error: Option<ForkError>,
    /// When the fork was accepted.
    #[serde(default)]
    pub created_at: String,
    /// When the fork last changed.
    #[serde(default)]
    pub updated_at: String,
    /// When the fork reached `complete`, `cancelled` or `failed`.
    #[serde(default)]
    pub completed_at: Option<String>,
    /// Undocumented fields.
    #[serde(flatten)]
    pub extra: Map<String, Value>,
}

/// Options for [`crate::Sandboxes::delete_with`].
#[derive(Debug, Clone, Default)]
pub struct DeleteOptions {
    /// Keep a recoverable copy for 30 days. `None` = the server default (`true`).
    pub archive: Option<bool>,
}

/// Options for [`crate::PtyClient::create`] and [`crate::PtyClient::attach`].
#[derive(Debug, Clone, Default)]
pub struct PtyOptions {
    /// Initial width. `None` (with `rows`) = the box's default size.
    pub cols: Option<u16>,
    /// Initial height.
    pub rows: Option<u16>,
    /// Names this client in the box's session list. `None` = `zipbox-sdk`.
    pub client_label: Option<String>,
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

/// What [`crate::Sandboxes::cancel_transition`] did. A change past its point of no
/// return is refused with [`crate::ErrorCode::Conflict`] carrying the reason.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum TransitionCancelOutcome {
    /// Nothing was executing on a host: the queued change was dropped and the box is
    /// already back where it was.
    Cleared,
    /// The host is executing the change; its abort is on the way.
    Cancelling,
    /// The box had no change in flight.
    None,
}

impl TransitionCancelOutcome {
    /// The wire value: `cleared`, `cancelling` or `none`.
    pub fn as_str(self) -> &'static str {
        match self {
            TransitionCancelOutcome::Cleared => "cleared",
            TransitionCancelOutcome::Cancelling => "cancelling",
            TransitionCancelOutcome::None => "none",
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
    /// Only this systemd unit (`journalctl -u`).
    pub unit: Option<String>,
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

    #[test]
    fn fork_options_wait_by_default_and_never_print_the_auth_key() {
        let mut options = ForkOptions::new("demo-2", ForkSourcePolicy::PauseAndResume);
        assert!(options.wait);
        assert!(options.tailscale_auth_key.is_none());
        assert!(options.private.is_none());
        assert!(format!("{options:?}").contains("private: None"));
        options.tailscale_auth_key = Some("tskey-auth-secret".into());
        options.private = Some(true);
        let debug = format!("{options:?} {options:#?}");
        assert!(
            !debug.contains("tskey-auth-secret") && debug.contains("<redacted>"),
            "{debug}"
        );
        assert!(debug.contains("demo-2"), "{debug}");
        assert!(debug.contains("private: Some(true)"), "{debug}");
        assert_eq!(
            serde_json::to_value(ForkSourcePolicy::PauseAndResume).unwrap(),
            "pause_and_resume"
        );
    }

    #[test]
    fn fork_projection_parses_values_this_sdk_does_not_know() {
        let fork: SandboxFork = serde_json::from_str(
            r#"{"operationId":"fork_1","sourceSandboxId":"sbx_1","childSandboxId":"sbx_2","sourcePolicy":"require_paused","phase":"later_phase","sourceRecovery":"later_recovery","waitingOn":"later_wait","attempt":2,"createdAt":"t","updatedAt":"t"}"#,
        )
        .unwrap();
        assert_eq!(fork.phase, "later_phase");
        assert_eq!(fork.waiting_on.as_deref(), Some("later_wait"));
        assert!(fork.error.is_none());
    }
}
