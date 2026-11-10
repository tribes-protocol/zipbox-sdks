//! Box lifecycle.

use reqwest::header::{HeaderMap, HeaderValue};
use reqwest::Method;
use serde::Deserialize;

use crate::backoff::Backoff;
use crate::client::{decode_json, encode, json_body, Body, Zipbox};
use crate::error::{Error, ErrorCode, Result};
use crate::sandbox::Sandbox;
use crate::types::{
    CreateOptions, DeleteOptions, ForkOptions, SandboxDetail, SandboxFork, SandboxInfo,
    TransitionCancelOutcome,
};

const READINESS: [&str; 7] = [
    "ready",
    "settling",
    "awaits_tailnet",
    "paused",
    "start_failed",
    "deleting",
    "gone",
];

/// The rule before `readiness`, for a control plane that does not send it: ready,
/// private and waiting for a tailnet, ended (`destroyed`, `stopped`, `deleting`), or
/// still settling. Remove once every control plane sends `readiness`.
fn fallback_readiness(detail: &SandboxDetail) -> &'static str {
    if detail.ready {
        return "ready";
    }
    let joined = detail
        .info
        .tailscale
        .as_ref()
        .is_some_and(|t| t.ipv4.as_deref().is_some_and(|ip| !ip.is_empty()));
    if detail.info.private_mode
        && !joined
        && detail.info.status == "running"
        && detail
            .info
            .agent_public_key
            .as_deref()
            .is_some_and(|k| !k.is_empty())
    {
        return "awaits_tailnet";
    }
    match detail.info.status.as_str() {
        "destroyed" | "stopped" => "gone",
        "deleting" => "deleting",
        _ => "settling",
    }
}

/// The readiness the server stated, when it is one this client knows.
fn stated_readiness(detail: &SandboxDetail) -> Option<&str> {
    detail
        .readiness
        .as_deref()
        .filter(|value| READINESS.contains(value))
}

/// Whether the box will answer, as the server decided it (`readiness`).
pub(crate) fn readiness_of(detail: &SandboxDetail) -> &str {
    stated_readiness(detail).unwrap_or_else(|| fallback_readiness(detail))
}

/// The error a wait returns for a box that will not become ready by itself, or
/// `None` while it may. Each is an answered verdict, never retryable.
fn ended_error(id: &str, detail: &SandboxDetail) -> Option<Error> {
    let mut err = match readiness_of(detail) {
        "paused" => {
            // Start it, unless the server says it cannot start now.
            let remedy = match &detail.info.allowed_actions {
                Some(actions) if !actions.iter().any(|action| action == "start") => {
                    "it cannot be started now; see its allowed_actions"
                }
                _ => "start it first",
            };
            Error::new(
                ErrorCode::BoxNotRunning,
                format!("box {id} is paused and nothing will start it; {remedy}"),
            )
        }
        "start_failed" => {
            let remedy = if detail.info.status == "stopped" {
                "its name was released; create it again"
            } else {
                "start it again"
            };
            Error::new(
                ErrorCode::StartFailed,
                format!("box {id} failed to start; {remedy}"),
            )
        }
        "deleting" | "gone" => Error::new(
            ErrorCode::Conflict,
            format!(
                "box {id} is {} and will not become ready",
                detail.info.status
            ),
        ),
        _ => return None,
    };
    err.retryable = false;
    Some(err)
}

/// The error a box call returns on a detail it cannot call: a private box that has
/// not joined a tailnet answers only once the owner's auth key joins it
/// ([`ErrorCode::TailnetNotConnected`]); anything else is
/// [`ErrorCode::BoxNotRunning`], retryable only while the box is settling.
pub(crate) fn box_not_ready_error(id: &str, detail: &SandboxDetail) -> Error {
    let stated = stated_readiness(detail);
    if stated.unwrap_or_else(|| fallback_readiness(detail)) == "awaits_tailnet" {
        let mut err = Error::new(
            ErrorCode::TailnetNotConnected,
            format!(
                "box {id} is private and has not joined a tailnet yet; connect it with \
                 tailscale().connect"
            ),
        );
        err.retryable = false;
        return err;
    }
    let mut err = Error::new(
        ErrorCode::BoxNotRunning,
        format!(
            "box {id} is not running (status {}); start it and wait until it is ready",
            detail.info.status
        ),
    );
    err.retryable = match stated {
        Some(readiness) => readiness == "settling",
        // Without readiness (an older control plane), the rule before it: booting or
        // restarting comes back by itself, and nothing else does.
        None => matches!(detail.info.status.as_str(), "provisioning" | "running"),
    };
    err
}

/// Box lifecycle, from [`Zipbox::sandboxes`].
#[derive(Debug, Clone)]
pub struct Sandboxes {
    pub(crate) client: Zipbox,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct ClaimResponse {
    sandbox_id: String,
}

/// The 202 of `POST /sandbox/:id/fork`: the new box, already on the account, and
/// the fork making it (which carries the operation id).
#[derive(Deserialize)]
struct ForkAccepted {
    /// None once a cancelled or failed fork removed the box (a replay of the same
    /// Idempotency-Key answers its tombstone).
    sandbox: Option<SandboxInfo>,
    fork: SandboxFork,
}

/// The error for a fork's own failure. Every fork error code is an answered
/// verdict; one this SDK does not know yet reads as `conflict`.
fn fork_failed(code: &str, message: String) -> Error {
    let code = ErrorCode::known(code).unwrap_or(ErrorCode::Conflict);
    let mut err = Error::new(code, message);
    err.retryable = false;
    err
}

/// The error a fork that ended without its box answers: its own failure, or
/// `conflict` for a cancel. None while it has not ended that way.
fn fork_ended_error(id: &str, fork: &SandboxFork) -> Option<Error> {
    if let Some(error) = &fork.error {
        return Some(fork_failed(&error.code, error.message.clone()));
    }
    if fork.phase == "cancelled" {
        let mut err = Error::new(
            ErrorCode::Conflict,
            format!("the fork making box {id} was cancelled"),
        );
        err.retryable = false;
        return Some(err);
    }
    None
}

impl Sandboxes {
    /// Every box on the account.
    pub async fn list(&self) -> Result<Vec<SandboxInfo>> {
        self.client
            .api_json(Method::GET, "/sandbox", Body::None)
            .await
    }

    /// One box, with its readiness and origin.
    pub async fn get(&self, id: &str) -> Result<Sandbox> {
        let detail = self.client.detail(id).await?;
        Ok(Sandbox::new(
            self.client.clone(),
            id.to_string(),
            Some(detail),
        ))
    }

    /// Creates a box and waits until it answers requests. Safe to retry: the
    /// claim carries an `Idempotency-Key`. Waits with backoff and no deadline. A
    /// box created with `private: Some(true)` returns once it is running with its
    /// key (see [`Sandboxes::wait_ready`]).
    pub async fn create(&self, options: CreateOptions) -> Result<Sandbox> {
        let mut headers = HeaderMap::new();
        let key = uuid::Uuid::new_v4().to_string();
        headers.insert(
            "idempotency-key",
            HeaderValue::from_str(&key).expect("uuid is a valid header"),
        );
        let resp = self
            .client
            .api(
                Method::POST,
                "/sandbox/identity/claim",
                json_body(&options),
                headers,
                true,
            )
            .await?;
        let claim: ClaimResponse = decode_json(resp).await?;
        self.wait_ready(&claim.sandbox_id).await
    }

    /// The running box named `name`, creating it with the default options if the
    /// account has none. A stopped box of that name is started. See
    /// [`Sandboxes::get_or_create_with`] to create it with a tier, SSH key, mailbox
    /// or private mode.
    pub async fn get_or_create(&self, name: &str) -> Result<Sandbox> {
        self.get_or_create_with(name, CreateOptions::default())
            .await
    }

    /// [`Sandboxes::get_or_create`], creating the box with `options` when there is
    /// none. `options.name` must be `None` or `name`
    /// ([`ErrorCode::InvalidRequest`] otherwise).
    ///
    /// The options apply only when a box is created. A box that already has this
    /// name is returned as it is, even when it differs (another tier, not
    /// private): that is not an error and nothing is resized. Check its tier and
    /// call [`Sandboxes::set_tier`] if it matters.
    pub async fn get_or_create_with(&self, name: &str, options: CreateOptions) -> Result<Sandbox> {
        if options.name.as_deref().is_some_and(|n| n != name) {
            return Err(Error::new(
                ErrorCode::InvalidRequest,
                format!(
                    "get_or_create_with({name:?}) was given options.name {:?}",
                    options.name
                ),
            ));
        }
        let existing = self.list().await?.into_iter().find(|b| {
            b.name.as_deref() == Some(name)
                && !matches!(b.status.as_str(), "destroyed" | "stopped" | "deleting")
        });
        let create = CreateOptions {
            name: Some(name.to_string()),
            ..options
        };
        let Some(info) = existing else {
            return self.create(create).await;
        };
        // "stopping": a stop is still landing; start waits it out, then starts.
        // "pausing": paused while its backup uploads; the start boots it at once.
        if matches!(info.status.as_str(), "archived" | "stopping" | "pausing") {
            match self.start(&info.id).await {
                Ok(()) => {}
                // The name was being deleted: it is gone now, so make it.
                Err(err) if err.code == ErrorCode::NotFound => return self.create(create).await,
                // Another caller started it first: the server's idempotent answer to
                // a second restore. A box now coming up is waited on like any other.
                Err(err) if err.code == ErrorCode::Conflict => {
                    let detail = self.detail_in_wait(&info.id).await?;
                    if !matches!(detail.info.status.as_str(), "provisioning" | "running") {
                        return Err(err);
                    }
                }
                Err(err) => return Err(err),
            }
        }
        self.wait_ready(&info.id).await
    }

    /// Starts a stopped box (`POST /sandbox/:id/restore`).
    ///
    /// A stop is accepted at once but its disk takes seconds to minutes to land;
    /// meanwhile the box reads `stopping` and the restore answers `conflict`. This
    /// waits that out with backoff and starts the box once it reads `archived`. A
    /// box that comes out of the wait already starting or running (a resize
    /// restores itself, a failed archive leaves it running) is started. Any other
    /// `conflict` is returned as it came.
    pub async fn start(&self, id: &str) -> Result<()> {
        self.forget(id);
        let mut waited = false;
        loop {
            let err = match self.post(id, "restore").await {
                Err(err) if err.code == ErrorCode::Conflict => err,
                other => return other,
            };
            let mut detail = self.detail_in_wait(id).await?;
            if detail.info.status != "stopping" {
                // A second conflict after a stop was waited out: the box restored
                // itself, or it is archived and will not restore (that is final).
                if waited && matches!(detail.info.status.as_str(), "provisioning" | "running") {
                    return Ok(());
                }
                return Err(err);
            }
            let mut backoff = Backoff::poll(
                self.client.inner.poll_interval,
                self.client.inner.poll_max_interval,
            );
            while detail.info.status == "stopping" {
                tokio::time::sleep(backoff.next_delay()).await;
                detail = self.detail_in_wait(id).await?;
            }
            waited = true;
            match detail.info.status.as_str() {
                "archived" => {}
                "provisioning" | "running" => return Ok(()),
                _ => return Err(err),
            }
        }
    }

    /// Stops a box: its disk is archived and nothing is billed while stopped.
    pub async fn stop(&self, id: &str) -> Result<()> {
        self.forget(id);
        self.post(id, "pause").await
    }

    /// Hard-restarts a running box. Files survive; running processes do not.
    pub async fn restart(&self, id: &str) -> Result<()> {
        self.forget(id);
        self.post(id, "reset").await
    }

    /// Cancels the box's state change in flight (a stop, a start, a restart, a tier
    /// change) and returns it to where it was. Past the point of no return this fails
    /// with [`ErrorCode::Conflict`] carrying the server's reason. Not retried.
    pub async fn cancel_transition(&self, id: &str) -> Result<TransitionCancelOutcome> {
        #[derive(Deserialize)]
        struct CancelResult {
            outcome: TransitionCancelOutcome,
        }
        self.forget(id);
        let result: CancelResult = self
            .client
            .api_json(
                Method::POST,
                &format!("/sandbox/{}/transition/cancel", encode(id)),
                Body::None,
            )
            .await?;
        Ok(result.outcome)
    }

    /// Deletes a box, keeping a recoverable copy for 30 days.
    pub async fn delete(&self, id: &str) -> Result<()> {
        self.delete_with(id, DeleteOptions::default()).await
    }

    /// [`Sandboxes::delete`] with options: `archive: Some(false)` skips the
    /// recoverable copy.
    pub async fn delete_with(&self, id: &str, options: DeleteOptions) -> Result<()> {
        self.forget(id);
        let body = match options.archive {
            Some(archive) => json_body(&serde_json::json!({ "archiveBeforeDestroy": archive })),
            None => Body::None,
        };
        self.client
            .api_unit(Method::DELETE, &format!("/sandbox/{}", encode(id)), body)
            .await
    }

    /// Moves a box to another tier (`xs` … `xxl`).
    pub async fn set_tier(&self, id: &str, tier: &str) -> Result<()> {
        self.forget(id);
        self.client
            .api_unit(
                Method::POST,
                &format!("/sandbox/{}/tier", encode(id)),
                json_body(&serde_json::json!({ "tier": tier })),
            )
            .await
    }

    /// Forks a box into a new box owned by the same account: the same files,
    /// installed software and settings, the same size, and a new identity (name,
    /// address, keys). It starts public unless `private: Some(true)`. Returns a
    /// handle on the NEW box.
    ///
    /// `POST /sandbox/:id/fork` carries one `Idempotency-Key` for the whole call, so
    /// a retry never makes two boxes, and a body of exactly the name and source
    /// policy, plus `private: true` when `private` is `Some(true)`. With `wait` (the default) this then waits for the new box like
    /// [`Sandboxes::wait_ready`]; a fork that ends without a box fails with the
    /// fork's own error code (`copy_failed`, `source_gone`, …), or `conflict` when
    /// it was cancelled. With a `tailscale_auth_key` it then joins the new box to
    /// your tailnet as [`crate::Tailscale::connect`] does: the key is sealed here to
    /// the new box's own key and never sent to `/fork`.
    ///
    /// `wait: false` returns the accepted box (`provisioning`, not ready, carrying
    /// its fork) at once; follow it with [`Sandboxes::get_fork`]. Dropping the future
    /// only stops the wait: the fork goes on. Cancel it with
    /// [`Sandboxes::cancel_transition`] on the new box's id.
    pub async fn fork(&self, source_id: &str, options: ForkOptions) -> Result<Sandbox> {
        if let Some(key) = &options.tailscale_auth_key {
            if key.trim().is_empty() {
                return Err(Error::new(
                    ErrorCode::InvalidRequest,
                    "the Tailscale auth key is empty",
                ));
            }
            if !options.wait {
                return Err(Error::new(
                    ErrorCode::InvalidRequest,
                    "tailscale_auth_key needs wait: the key is sealed to the new box once it is running",
                ));
            }
        }
        let mut headers = HeaderMap::new();
        let key = uuid::Uuid::new_v4().to_string();
        headers.insert(
            "idempotency-key",
            HeaderValue::from_str(&key).expect("uuid is a valid header"),
        );
        // A running source pauses for its capture: its cached address goes with it.
        self.forget(source_id);
        let mut fields = serde_json::json!({
            "name": options.name,
            "sourcePolicy": options.source_policy,
        });
        if options.private == Some(true) {
            fields["private"] = serde_json::Value::Bool(true);
        }
        let body = json_body(&fields);
        let resp = self
            .client
            .api(
                Method::POST,
                &format!("/sandbox/{}/fork", encode(source_id)),
                body,
                headers,
                true,
            )
            .await?;
        let accepted: ForkAccepted = decode_json(resp).await?;
        let child_id = accepted.fork.child_sandbox_id.clone();
        let Some(sandbox) = accepted.sandbox else {
            // The key found a fork that already ended and removed its box: that
            // fork's verdict, never an "unexpected response".
            return Err(
                fork_ended_error(&child_id, &accepted.fork).unwrap_or_else(|| {
                    let mut err = Error::new(
                        ErrorCode::Conflict,
                        format!("the fork making box {child_id} ended"),
                    );
                    err.retryable = false;
                    err
                }),
            );
        };
        if !options.wait {
            let mut info = sandbox;
            info.fork = Some(accepted.fork);
            let detail = SandboxDetail {
                info,
                ready: false,
                readiness: None,
                https_ready: false,
                base_url: None,
                box_token: None,
            };
            return Ok(Sandbox::new(self.client.clone(), child_id, Some(detail)));
        }
        let child = match self.settle(&child_id).await {
            Ok(child) => child,
            // A cancelled or failed fork removes its box: the box reads not_found
            // while the fork still says why.
            Err(error) if error.code == ErrorCode::NotFound => {
                return Err(self.fork_vanished(&child_id, error).await);
            }
            Err(error) => return Err(error),
        };
        if let Some(auth_key) = &options.tailscale_auth_key {
            child.tailscale().connect(auth_key.trim()).await?;
        }
        Ok(child)
    }

    /// The fork that made (or is making) the box `child_id`
    /// (`GET /sandbox/:id/fork`): its phase, source recovery, waits and error.
    /// [`ErrorCode::NotFound`] when that box was not made by a fork.
    pub async fn get_fork(&self, child_id: &str) -> Result<SandboxFork> {
        self.client
            .api_json(
                Method::GET,
                &format!("/sandbox/{}/fork", encode(child_id)),
                Body::None,
            )
            .await
    }

    /// The error for a new box its fork removed while it was waited on: the fork,
    /// read once, gives its own error (or `conflict` for a cancel); otherwise the
    /// box's `not_found` stands. A fork that could not be read (a retryable error)
    /// is no verdict: that error is returned.
    async fn fork_vanished(&self, id: &str, not_found: Error) -> Error {
        match self.get_fork(id).await {
            Ok(fork) => fork_ended_error(id, &fork).unwrap_or(not_found),
            Err(error) if error.retryable => error,
            Err(_) => not_found,
        }
    }

    /// Re-reads the box with backoff until it is ready.
    ///
    /// A private box that has not joined a tailnet (no tailnet IPv4 yet) cannot be
    /// ready, so for one this returns as soon as it is running with its
    /// `agent_public_key`: the point at which a Tailscale auth key can be sealed to
    /// it. A private box that has joined waits for ready like any other. A box that
    /// will not become ready by itself is an error: paused
    /// [`ErrorCode::BoxNotRunning`], a failed start [`ErrorCode::StartFailed`],
    /// deleting or gone [`ErrorCode::Conflict`], none of them retryable.
    pub async fn wait_ready(&self, id: &str) -> Result<Sandbox> {
        self.settle(id).await
    }

    /// The wait behind [`Sandboxes::wait_ready`] and [`Sandboxes::fork`]: re-reads
    /// the box with backoff, and no deadline, until it is ready or awaits its
    /// tailnet. The server's `readiness` decides: `settling` keeps reading. A read
    /// that fails with a retryable error goes on the same ladder, never sooner than
    /// its `retry_after`.
    async fn settle(&self, id: &str) -> Result<Sandbox> {
        let mut backoff = Backoff::poll(
            self.client.inner.poll_interval,
            self.client.inner.poll_max_interval,
        );
        loop {
            let mut retry_after = std::time::Duration::ZERO;
            match self.client.detail(id).await {
                Ok(detail) => {
                    if let Some(err) = ended_error(id, &detail) {
                        return Err(err);
                    }
                    if readiness_of(&detail) != "settling" {
                        return Ok(Sandbox::new(
                            self.client.clone(),
                            id.to_string(),
                            Some(detail),
                        ));
                    }
                }
                Err(err) if err.retryable => retry_after = err.retry_after.unwrap_or_default(),
                Err(err) => return Err(err),
            }
            tokio::time::sleep(backoff.next_delay().max(retry_after)).await;
        }
    }

    /// One read of the box inside a wait. A read that fails with a retryable error
    /// (a control-plane deploy) is read again on the poll ladder, never sooner than
    /// its `retry_after`, with no ceiling; any other error is returned.
    async fn detail_in_wait(&self, id: &str) -> Result<SandboxDetail> {
        let mut backoff = Backoff::poll(
            self.client.inner.poll_interval,
            self.client.inner.poll_max_interval,
        );
        loop {
            match self.client.detail(id).await {
                Ok(detail) => return Ok(detail),
                Err(err) if err.retryable => {
                    let wait = backoff
                        .next_delay()
                        .max(err.retry_after.unwrap_or_default());
                    tokio::time::sleep(wait).await;
                }
                Err(err) => return Err(err),
            }
        }
    }

    async fn post(&self, id: &str, action: &str) -> Result<()> {
        self.client
            .api_unit(
                Method::POST,
                &format!("/sandbox/{}/{action}", encode(id)),
                Body::None,
            )
            .await
    }

    fn forget(&self, id: &str) {
        self.client
            .inner
            .boxes
            .lock()
            .expect("box cache")
            .remove(id);
    }
}
