//! Box lifecycle.

use std::time::Duration;

use reqwest::header::{HeaderMap, HeaderValue};
use reqwest::Method;
use serde::Deserialize;

use crate::backoff::Backoff;
use crate::client::{decode_json, encode, json_body, Body, Zipbox};
use crate::error::{Error, ErrorCode, Result};
use crate::sandbox::Sandbox;
use crate::types::{CreateOptions, SandboxInfo};

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
    /// claim carries an `Idempotency-Key`. Waits with backoff and no deadline.
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

    /// The running box named `name`, creating it if the account has none. A
    /// stopped box of that name is started.
    pub async fn get_or_create(&self, name: &str) -> Result<Sandbox> {
        let existing = self.list().await?.into_iter().find(|b| {
            b.name.as_deref() == Some(name) && !matches!(b.status.as_str(), "destroyed" | "stopped")
        });
        match existing {
            Some(info) => {
                if info.status == "archived" || info.is_archived == Some(true) {
                    self.start(&info.id).await?;
                }
                self.wait_ready(&info.id).await
            }
            None => {
                self.create(CreateOptions {
                    name: Some(name.to_string()),
                    ..CreateOptions::default()
                })
                .await
            }
        }
    }

    /// Starts a stopped box (`POST /sandbox/:id/restore`).
    pub async fn start(&self, id: &str) -> Result<()> {
        self.post(id, "restore").await
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
        self.forget(id);
        self.client
            .api_unit(
                Method::DELETE,
                &format!("/sandbox/{}", encode(id)),
                Body::None,
            )
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
    pub async fn wait_ready(&self, id: &str) -> Result<Sandbox> {
        let mut backoff = Backoff::new(self.client.inner.poll_interval, Duration::from_secs(10));
        loop {
            let detail = self.client.detail(id).await?;
            if detail.ready {
                return Ok(Sandbox::new(
                    self.client.clone(),
                    id.to_string(),
                    Some(detail),
                ));
            }
            if matches!(detail.info.status.as_str(), "destroyed" | "stopped") {
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
