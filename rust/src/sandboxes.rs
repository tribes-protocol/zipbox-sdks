//! Box lifecycle.

use reqwest::header::{HeaderMap, HeaderValue};
use reqwest::Method;
use serde::Deserialize;

use crate::backoff::Backoff;
use crate::client::{decode_json, encode, json_body, Body, Zipbox};
use crate::error::{Error, ErrorCode, Result};
use crate::sandbox::Sandbox;
use crate::types::{CreateOptions, DeleteOptions, SandboxDetail, SandboxInfo};

/// A private box that is running with its key but has not joined a tailnet yet.
fn awaits_tailnet(detail: &SandboxDetail) -> bool {
    let joined = detail
        .info
        .tailscale
        .as_ref()
        .is_some_and(|t| t.ipv4.as_deref().is_some_and(|ip| !ip.is_empty()));
    detail.info.private_mode
        && !joined
        && detail.info.status == "running"
        && detail
            .info
            .agent_public_key
            .as_deref()
            .is_some_and(|k| !k.is_empty())
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
        match existing {
            Some(info) => {
                // "stopping": a stop is still landing; start waits it out, then starts.
                if info.status == "archived" || info.status == "stopping" {
                    self.start(&info.id).await?;
                }
                self.wait_ready(&info.id).await
            }
            None => {
                self.create(CreateOptions {
                    name: Some(name.to_string()),
                    ..options
                })
                .await
            }
        }
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
        let mut waited = false;
        loop {
            let err = match self.post(id, "restore").await {
                Err(err) if err.code == ErrorCode::Conflict => err,
                other => return other,
            };
            let mut detail = self.client.detail(id).await?;
            if detail.info.status != "stopping" {
                // A second conflict after a stop was waited out: the box restored
                // itself, or it is archived and will not restore (that is final).
                if waited && matches!(detail.info.status.as_str(), "provisioning" | "running") {
                    return Ok(());
                }
                return Err(err);
            }
            let mut backoff = Backoff::new(
                self.client.inner.poll_interval,
                self.client.inner.poll_max_interval,
            );
            while detail.info.status == "stopping" {
                tokio::time::sleep(backoff.next_delay()).await;
                detail = self.client.detail(id).await?;
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
        self.post(id, "reset").await
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
        self.client
            .api_unit(
                Method::POST,
                &format!("/sandbox/{}/tier", encode(id)),
                json_body(&serde_json::json!({ "tier": tier })),
            )
            .await
    }

    /// Re-reads the box with backoff until it is ready.
    ///
    /// A private box that has not joined a tailnet (no tailnet IPv4 yet) cannot be
    /// ready, so for one this returns as soon as it is running with its
    /// `agent_public_key`: the point at which a Tailscale auth key can be sealed to
    /// it. A private box that has joined waits for ready like any other.
    pub async fn wait_ready(&self, id: &str) -> Result<Sandbox> {
        let mut backoff = Backoff::new(
            self.client.inner.poll_interval,
            self.client.inner.poll_max_interval,
        );
        loop {
            let detail = self.client.detail(id).await?;
            if detail.ready || awaits_tailnet(&detail) {
                return Ok(Sandbox::new(
                    self.client.clone(),
                    id.to_string(),
                    Some(detail),
                ));
            }
            if matches!(
                detail.info.status.as_str(),
                "destroyed" | "stopped" | "deleting"
            ) {
                return Err(Error::new(
                    ErrorCode::Conflict,
                    format!(
                        "box {id} is {} and will not become ready",
                        detail.info.status
                    ),
                ));
            }
            tokio::time::sleep(backoff.next_delay()).await;
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
