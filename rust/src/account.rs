//! Account balance and usage.

use reqwest::Method;

use crate::client::{Body, Zipbox};
use crate::error::Result;

/// Balance and usage, from [`Zipbox::account`]. Returned untyped: these are the
/// dashboard's own shapes.
#[derive(Debug, Clone)]
pub struct Account {
    pub(crate) client: Zipbox,
}

impl Account {
    /// Who the key belongs to: `{userId, apiKey}` (`GET /account/whoami`).
    pub async fn whoami(&self) -> Result<serde_json::Value> {
        self.client
            .api_json(Method::GET, "/account/whoami", Body::None)
            .await
    }

    /// Credit balance and auto top-up state (`GET /billing/summary`).
    pub async fn balance(&self) -> Result<serde_json::Value> {
        self.client
            .api_json(Method::GET, "/billing/summary", Body::None)
            .await
    }

    /// Recent metered usage (`GET /billing/usage`).
    pub async fn usage(&self) -> Result<serde_json::Value> {
        self.client
            .api_json(Method::GET, "/billing/usage", Body::None)
            .await
    }
}
