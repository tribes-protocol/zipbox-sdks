//! A box's tailnet: connect with a one-off auth key, read the status, disconnect.

use reqwest::Method;
use serde::Deserialize;

use crate::client::{encode, json_body, Body, Zipbox};
use crate::envelope::{seal, tailscale_info};
use crate::error::{Error, ErrorCode, Result};
use crate::types::TailscaleStatus;

/// The box's tailnet, from [`crate::Sandbox::tailscale`].
///
/// The auth key is sealed on this machine to the box's own key and opened only
/// inside the box; Zipbox relays ciphertext and never sees the key.
#[derive(Debug, Clone)]
pub struct Tailscale {
    pub(crate) client: Zipbox,
    pub(crate) id: String,
}

#[derive(Deserialize)]
struct StatusResponse {
    #[serde(default)]
    tailscale: Option<TailscaleStatus>,
}

impl Tailscale {
    fn path(&self) -> String {
        format!("/sandbox/{}/tailscale", encode(&self.id))
    }

    /// The tailnet status, or `None` when the box never joined one.
    pub async fn status(&self) -> Result<Option<TailscaleStatus>> {
        let response: StatusResponse = self
            .client
            .api_json(Method::GET, &self.path(), Body::None)
            .await?;
        Ok(response.tailscale)
    }

    /// Joins your tailnet with a one-off auth key (Tailscale admin → Settings →
    /// Keys). Read the key from a file or the environment, never from argv.
    ///
    /// The box's key is read fresh before sealing, because a claim re-mints it;
    /// [`ErrorCode::BoxKeyChanged`] re-reads and re-seals once. Returns once the
    /// box has accepted the key; follow [`Tailscale::status`] for the join.
    pub async fn connect(&self, auth_key: &str) -> Result<()> {
        let mut resealed = false;
        loop {
            let detail = self.client.detail(&self.id).await?;
            let box_key = match (&detail.info.agent_public_key, detail.info.status.as_str()) {
                (Some(key), "provisioning" | "running") if !key.is_empty() => key.clone(),
                _ => {
                    let mut err = Error::new(
                        ErrorCode::BoxNotRunning,
                        format!(
                            "box {} is {}; start it before connecting Tailscale",
                            self.id, detail.info.status
                        ),
                    );
                    // A box that is settling grows its key by itself; one that is
                    // paused or gone does not.
                    err.retryable = crate::sandboxes::readiness_of(&detail) == "settling";
                    return Err(err);
                }
            };
            let envelope = seal(&box_key, &tailscale_info(&self.id), auth_key)?;
            let body = json_body(&serde_json::json!({
                "envelope": envelope,
                "agentPublicKey": box_key,
            }));
            match self.client.api_unit(Method::PUT, &self.path(), body).await {
                Err(err) if err.code == ErrorCode::BoxKeyChanged && !resealed => resealed = true,
                other => return other,
            }
        }
    }

    /// Leaves the tailnet: the box logs out and forgets its node.
    pub async fn disconnect(&self) -> Result<()> {
        self.client
            .api_unit(Method::DELETE, &self.path(), Body::None)
            .await
    }
}
