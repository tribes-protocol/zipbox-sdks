//! Box lifecycle.

use std::time::Duration;

use reqwest::header::{HeaderMap, HeaderValue, ACCEPT};
use reqwest::Method;
use serde::Deserialize;

use crate::backoff::Backoff;
use crate::claimstream::CLAIM_STREAM_ACCEPT;
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
/// private and waiting for a tailnet, ended (`destroyed`, `deleting`), or still
/// settling. Remove once every control plane sends `readiness`.
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
        "destroyed" => "gone",
        "paused" => "paused",
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

/// The remedy for a paused box: resume it, unless the server says it cannot resume now.
fn paused_remedy(detail: &SandboxDetail) -> &'static str {
    match &detail.info.allowed_actions {
        Some(actions) if !actions.iter().any(|action| action == "resume") => {
            "it cannot be resumed now; see its allowed_actions"
        }
        _ => "resume it with sandboxes().resume",
    }
}

/// The error a wait returns for a box that will not become ready by itself, or
/// `None` while it may. Each is an answered verdict, never retryable: nothing but
/// the caller resumes the box.
fn ended_error(id: &str, detail: &SandboxDetail) -> Option<Error> {
    let mut err = match readiness_of(detail) {
        "paused" => Error::new(
            ErrorCode::BoxNotRunning,
            format!(
                "box {id} is paused and nothing will resume it; {}",
                paused_remedy(detail)
            ),
        ),
        "start_failed" => Error::new(
            ErrorCode::StartFailed,
            format!("box {id} failed to start; {}", paused_remedy(detail)),
        ),
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

/// The error a wait for a Pause returns for a box that will not reach `paused` by
/// itself, or `None` while it may (`paused`, `settling`). A box that reads ready
/// again had its Pause cancelled or refused; a failed, deleting or gone box will not
/// pause. Each is an answered verdict, never retryable.
fn pause_ended_error(id: &str, detail: &SandboxDetail) -> Option<Error> {
    let mut err = match readiness_of(detail) {
        "paused" | "settling" => return None,
        "ready" | "awaits_tailnet" => Error::new(
            ErrorCode::Conflict,
            format!("box {id} is running: its pause was cancelled or did not take effect"),
        ),
        other => Error::new(
            ErrorCode::Conflict,
            format!("box {id} is {other} and will not pause"),
        ),
    };
    err.retryable = false;
    Some(err)
}

/// What a wait waits for: the box answering (`ready`), or its Pause taking effect
/// (`paused`). It is the `until` of `GET /sandbox/:id/wait`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum WaitTarget {
    Ready,
    Paused,
}

impl WaitTarget {
    fn as_str(self) -> &'static str {
        match self {
            WaitTarget::Ready => "ready",
            WaitTarget::Paused => "paused",
        }
    }
}

/// One read inside a wait: `Ok(true)` once the wait is over, `Ok(false)` while the
/// box may still get there, and the answered verdict of a box that will not get
/// there by itself. A wait for `ready` also ends on `awaits_tailnet` (a private box
/// running with its key: the caller seals an auth key to it).
fn settled(id: &str, detail: &SandboxDetail, until: WaitTarget) -> Result<bool> {
    let ended = match until {
        WaitTarget::Ready => ended_error(id, detail),
        WaitTarget::Paused => pause_ended_error(id, detail),
    };
    if let Some(err) = ended {
        return Err(err);
    }
    Ok(match until {
        WaitTarget::Ready => readiness_of(detail) != "settling",
        WaitTarget::Paused => readiness_of(detail) == "paused",
    })
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
            "box {id} is not running (status {}); resume it with sandboxes().resume",
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

    /// Creates a box and returns it once it answers requests: at once when the
    /// server bound a booted machine inside the request (`ready` in its answer),
    /// else after waiting as [`Sandboxes::wait_ready`] does, with no deadline. Safe
    /// to retry: the claim carries an `Idempotency-Key`. A box created with
    /// `private: Some(true)` returns once it is running with its key.
    pub async fn create(&self, options: CreateOptions) -> Result<Sandbox> {
        let mut headers = HeaderMap::new();
        let key = uuid::Uuid::new_v4().to_string();
        headers.insert(
            "idempotency-key",
            HeaderValue::from_str(&key).expect("uuid is a valid header"),
        );
        headers.insert(ACCEPT, HeaderValue::from_static(CLAIM_STREAM_ACCEPT));
        // The answer streamed (conformance README, streamed create); its placed line is
        // never dialled. An unstreamed answer reads as before.
        let resp = self.client.api_claim(json_body(&options), headers).await?;
        let mut answer: serde_json::Value = decode_json(resp).await?;
        let claim: ClaimResponse = serde_json::from_value(answer.clone()).map_err(Error::decode)?;
        // The answer carries the box's detail once the server bound it in the request.
        if let Some(fields) = answer.as_object_mut() {
            fields
                .entry("id")
                .or_insert_with(|| serde_json::Value::String(claim.sandbox_id.clone()));
        }
        if let Ok(detail) = serde_json::from_value::<SandboxDetail>(answer) {
            if readiness_of(&detail) == "ready" && detail.base_url.is_some() {
                self.client.adopt(&claim.sandbox_id, &detail);
                return Ok(Sandbox::new(
                    self.client.clone(),
                    claim.sandbox_id,
                    Some(detail),
                ));
            }
        }
        self.wait_ready(&claim.sandbox_id).await
    }

    /// The running box named `name`, creating it with the default options if the
    /// account has none. A paused box of that name is resumed. See
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
                && !matches!(b.status.as_str(), "destroyed" | "deleting")
        });
        let create = CreateOptions {
            name: Some(name.to_string()),
            ..options
        };
        let Some(info) = existing else {
            return self.create(create).await;
        };
        // "stopping": a Pause is still landing; request_resume waits it out, then resumes.
        if matches!(info.status.as_str(), "paused" | "stopping") {
            match self.request_resume(&info.id).await {
                Ok(Some(detail)) if readiness_of(&detail) == "ready" => {
                    return Ok(Sandbox::new(
                        self.client.clone(),
                        info.id.clone(),
                        Some(detail),
                    ));
                }
                Ok(_) => {}
                // The name was being deleted: it is gone now, so make it.
                Err(err) if err.code == ErrorCode::NotFound => return self.create(create).await,
                // Another caller resumed it first: the server's idempotent answer to
                // a second resume. A box now coming up is waited on like any other.
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

    /// Pauses a box (`POST /sandbox/:id/pause`) and returns it paused. Nothing is
    /// billed while it is paused, and [`Sandboxes::resume`] brings it back with its
    /// files and installed software. Returns at once when the server answers with
    /// the paused box (200), else waits for the Pause to take effect
    /// (`GET /sandbox/:id/wait?until=paused`, no deadline). A box that reads running
    /// again (the Pause was cancelled) is [`ErrorCode::Conflict`].
    pub async fn pause(&self, id: &str) -> Result<Sandbox> {
        let accepted = self.lifecycle(id, "pause").await?;
        self.finish(id, WaitTarget::Paused, accepted).await
    }

    /// Resumes a paused box (`POST /sandbox/:id/resume`) and returns it ready: at
    /// once when the server answers with the ready box (200), else after waiting as
    /// [`Sandboxes::wait_ready`] does. A box still `stopping` is resumed once its
    /// Pause lands. Credit-gated: [`ErrorCode::InsufficientCredits`] when the
    /// balance is dry.
    pub async fn resume(&self, id: &str) -> Result<Sandbox> {
        let accepted = self.request_resume(id).await?;
        self.finish(id, WaitTarget::Ready, accepted).await
    }

    /// Alias of [`Sandboxes::pause`]: the same request and the same result.
    pub async fn stop(&self, id: &str) -> Result<Sandbox> {
        self.pause(id).await
    }

    /// Alias of [`Sandboxes::resume`]: the same request and the same result.
    pub async fn start(&self, id: &str) -> Result<Sandbox> {
        self.resume(id).await
    }

    /// Sends Resume, and returns the detail it answered with (`None` when the box
    /// came back by itself, or the answer carried no detail).
    ///
    /// A Pause the host has not answered yet leaves the box `stopping`, and Resume
    /// answers `conflict` meanwhile. This waits that out with backoff and resumes
    /// the box once it reads `paused`. A box that comes out of the wait already
    /// starting or running (a resize restores itself, a failed pause leaves it
    /// running) is not resumed again. Any other `conflict` is returned as it came.
    ///
    /// In parallel with the resume, never waited for, the box's last known origin is
    /// pre-connected, so the first call after it skips TCP and TLS.
    async fn request_resume(&self, id: &str) -> Result<Option<SandboxDetail>> {
        let preconnect = self.client.preconnect(id);
        let accepted = self.send_resume(id).await;
        if let Some(preconnect) = preconnect {
            match &accepted {
                Ok(detail) => preconnect.settle(detail.as_ref()),
                Err(_) => preconnect.task.abort(),
            }
        }
        accepted
    }

    /// Sends Resume, waiting out a Pause still landing (see `request_resume`).
    async fn send_resume(&self, id: &str) -> Result<Option<SandboxDetail>> {
        let mut waited = false;
        loop {
            let err = match self.lifecycle(id, "resume").await {
                Err(err) if err.code == ErrorCode::Conflict => err,
                other => return other,
            };
            let mut detail = self.detail_in_wait(id).await?;
            if detail.info.status != "stopping" {
                // A second conflict after a pause was waited out: the box came back by
                // itself, or it is paused and will not resume (that is final).
                if waited && matches!(detail.info.status.as_str(), "provisioning" | "running") {
                    return Ok(None);
                }
                return Err(err);
            }
            let mut backoff = self.poll_backoff();
            while detail.info.status == "stopping" {
                tokio::time::sleep(backoff.next_delay()).await;
                detail = self.detail_in_wait(id).await?;
            }
            waited = true;
            match detail.info.status.as_str() {
                "paused" => {}
                "provisioning" | "running" => return Ok(None),
                _ => return Err(err),
            }
        }
    }

    /// The accepted detail when it already shows `until`, else the wait for it.
    async fn finish(
        &self,
        id: &str,
        until: WaitTarget,
        accepted: Option<SandboxDetail>,
    ) -> Result<Sandbox> {
        let detail = match accepted {
            Some(detail) if readiness_of(&detail) == until.as_str() => detail,
            _ => self.wait_until(id, until).await?,
        };
        Ok(Sandbox::new(
            self.client.clone(),
            id.to_string(),
            Some(detail),
        ))
    }

    /// Hard-restarts a running box. Files survive; running processes do not.
    pub async fn restart(&self, id: &str) -> Result<()> {
        self.lifecycle(id, "reset").await.map(|_| ())
    }

    /// Cancels the box's state change in flight (a pause, a resume, a restart, a tier
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
        let child = match self.wait_ready(&child_id).await {
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

    /// Waits until the box is ready, on `GET /sandbox/:id/wait?until=ready`: the
    /// server answers when it is (an older API is polled with backoff instead). No
    /// deadline; dropping the future stops the wait.
    ///
    /// A private box that has not joined a tailnet (no tailnet IPv4 yet) cannot be
    /// ready, so for one this returns as soon as it is running with its
    /// `agent_public_key`: the point at which a Tailscale auth key can be sealed to
    /// it. A private box that has joined waits for ready like any other. A box that
    /// will not become ready by itself is an error: paused
    /// [`ErrorCode::BoxNotRunning`], a failed start [`ErrorCode::StartFailed`],
    /// deleting or gone [`ErrorCode::Conflict`], none of them retryable.
    pub async fn wait_ready(&self, id: &str) -> Result<Sandbox> {
        let detail = self.wait_until(id, WaitTarget::Ready).await?;
        Ok(Sandbox::new(
            self.client.clone(),
            id.to_string(),
            Some(detail),
        ))
    }

    /// Waits until the box reaches `until`, on `GET /sandbox/:id/wait`: the server
    /// holds the request until the box gets there (pushed when the host answers, no
    /// polling), until it cannot get there by itself, or until its hold limit
    /// passes. A held answer that is still settling is asked again at once: the
    /// server did the waiting. A read that fails with a retryable error goes on the
    /// poll ladder, never sooner than its `retry_after`. An API that predates
    /// `/wait` answers it 404 on a box that exists: then this is the poll on
    /// `GET /sandbox/:id` it replaced, whose 404 is the box's own.
    async fn wait_until(&self, id: &str, until: WaitTarget) -> Result<SandboxDetail> {
        let mut backoff = self.poll_backoff();
        loop {
            let retry_after = match self.wait_read(id, until).await {
                Ok(detail) => {
                    if settled(id, &detail, until)? {
                        return Ok(detail);
                    }
                    backoff.reset();
                    continue;
                }
                Err(err) if err.code == ErrorCode::NotFound => {
                    return self.poll_until(id, until).await;
                }
                Err(err) if err.retryable => err.retry_after.unwrap_or_default(),
                Err(err) => return Err(err),
            };
            tokio::time::sleep(backoff.next_delay().max(retry_after)).await;
        }
    }

    async fn wait_read(&self, id: &str, until: WaitTarget) -> Result<SandboxDetail> {
        let detail: SandboxDetail = self
            .client
            .api_json(
                Method::GET,
                &format!("/sandbox/{}/wait?until={}", encode(id), until.as_str()),
                Body::None,
            )
            .await?;
        self.client.adopt(id, &detail);
        Ok(detail)
    }

    /// The wait for an API without `/wait`: re-reads `GET /sandbox/:id` with
    /// backoff, and no deadline, until the box reaches `until` or will not get there
    /// by itself. A read that fails with a retryable error goes on the same ladder,
    /// never sooner than its `retry_after`.
    async fn poll_until(&self, id: &str, until: WaitTarget) -> Result<SandboxDetail> {
        let mut backoff = self.poll_backoff();
        loop {
            let mut retry_after = Duration::ZERO;
            match self.client.detail(id).await {
                Ok(detail) => {
                    if settled(id, &detail, until)? {
                        return Ok(detail);
                    }
                }
                Err(err) if err.retryable => retry_after = err.retry_after.unwrap_or_default(),
                Err(err) => return Err(err),
            }
            tokio::time::sleep(backoff.next_delay().max(retry_after)).await;
        }
    }

    fn poll_backoff(&self) -> Backoff {
        Backoff::poll(
            self.client.inner.poll_interval,
            self.client.inner.poll_max_interval,
        )
    }

    /// One read of the box inside a wait. A read that fails with a retryable error
    /// (a control-plane deploy) is read again on the poll ladder, never sooner than
    /// its `retry_after`, with no ceiling; any other error is returned.
    async fn detail_in_wait(&self, id: &str) -> Result<SandboxDetail> {
        let mut backoff = self.poll_backoff();
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

    /// Sends a lifecycle verb. The answer is the box's detail: its final state
    /// (200) when the host answered within the server's budget, or its state in
    /// transition (202). `None` for an answer that carries no detail.
    async fn lifecycle(&self, id: &str, action: &str) -> Result<Option<SandboxDetail>> {
        self.forget(id);
        let resp = self
            .client
            .api(
                Method::POST,
                &format!("/sandbox/{}/{action}", encode(id)),
                Body::None,
                HeaderMap::new(),
                false,
            )
            .await?;
        let bytes = resp.bytes().await.map_err(Error::transport)?;
        let Ok(detail) = serde_json::from_slice::<SandboxDetail>(&bytes) else {
            return Ok(None);
        };
        self.client.adopt(id, &detail);
        Ok(Some(detail))
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
