//! # zipbox
//!
//! Drive [Zipbox](https://zipbox.ai) boxes from Rust: create a box, run commands,
//! move files, open a terminal, publish a port, stop it when you are done. Nothing
//! on a box stops on a clock.
//!
//! ```no_run
//! # async fn run() -> zipbox::Result<()> {
//! let zb = zipbox::Zipbox::from_env()?; // reads ZIPBOX_API_KEY
//! let sandbox = zb.sandboxes().get_or_create("demo").await?;
//! // Create options apply only when the box is created:
//! let ci = zb
//!     .sandboxes()
//!     .get_or_create_with("ci-runner", zipbox::CreateOptions { tier: Some("l".into()), ..Default::default() })
//!     .await?;
//! # let _ = ci;
//! let out = sandbox.exec("echo hello").await?;
//! assert_eq!(out.stdout, "hello\n");
//! # Ok(()) }
//! ```
//!
//! Every failure is one [`Error`] carrying a stable [`ErrorCode`]; a command that
//! exits non-zero is a result ([`ExecResult::exit_code`]), never an error. Box
//! tokens are fetched and cached by the client. Docs:
//! <https://zipbox.ai/docs/sdk-rust>.

#![deny(missing_docs)]

mod account;
mod backoff;
mod client;
mod commands;
mod envelope;
mod error;
mod files;
mod pty;
mod sandbox;
mod sandboxes;
mod sse;
mod tailscale;
mod types;

pub use account::Account;
pub use client::{Zipbox, ZipboxBuilder, DEFAULT_API_URL};
pub use commands::{Command, Commands};
pub use error::{Error, ErrorCode, Result, DOCS_ERRORS_URL};
pub use files::{Files, MAX_FILE_BYTES, WORKSPACE_DIR};
pub use pty::{Pty, PtyClient, PtyEvent, PTY_SUBPROTOCOL};
pub use sandbox::{Previews, Sandbox};
pub use sandboxes::Sandboxes;
pub use tailscale::Tailscale;
pub use types::{
    CreateOptions, DeleteOptions, ExecEvent, ExecOptions, ExecRecord, ExecResult, FileEntry,
    FileKind, Language, LogOptions, OutputStream, Preview, PtyOptions, SandboxDetail, SandboxInfo,
    Signal, SshInfo, TailscalePeer, TailscaleStatus,
};
