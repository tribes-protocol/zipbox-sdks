//! The client, its configuration and the request machinery shared by every call.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use reqwest::header::{HeaderMap, HeaderValue, ACCEPT, AUTHORIZATION, CONTENT_TYPE, RETRY_AFTER};
use reqwest::{Method, Response};
use serde::de::DeserializeOwned;
use serde::Serialize;

use crate::account::Account;
use crate::backoff::Backoff;
use crate::error::{Error, ErrorCode, Result};
use crate::sandbox::Sandbox;
use crate::sandboxes::Sandboxes;
use crate::types::SandboxDetail;

/// The control plane every client talks to unless told otherwise.
pub const DEFAULT_API_URL: &str = "https://api.tribes.xyz";

const USER_AGENT: &str = concat!("zipbox-sdk-rust/", env!("CARGO_PKG_VERSION"));

/// The Zipbox client. Cheap to clone; clones share the box-token cache.
///
/// ```no_run
/// # async fn run() -> zipbox::Result<()> {
/// let zb = zipbox::Zipbox::from_env()?;
/// let sandbox = zb.sandboxes().get_or_create("demo").await?;
/// let out = sandbox.exec("uname -a").await?;
/// println!("{}", out.stdout);
/// # Ok(()) }
/// ```
#[derive(Clone)]
pub struct Zipbox {
    pub(crate) inner: Arc<Inner>,
}

impl std::fmt::Debug for Zipbox {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Zipbox")
            .field("base_url", &self.inner.base_url)
            .finish_non_exhaustive()
    }
}

pub(crate) struct Inner {
    pub http: reqwest::Client,
    pub base_url: String,
    pub api_key: String,
    pub max_retries: u32,
    pub retry_base_delay: Duration,
    pub poll_interval: Duration,
    pub reconnect_base_delay: Duration,
    pub boxes: Mutex<HashMap<String, BoxAccess>>,
}

/// What a box needs: its origin and the token it accepts.
#[derive(Debug, Clone)]
pub(crate) struct BoxAccess {
    pub base_url: String,
    pub token: String,
}

/// A request body.
pub(crate) enum Body {
    None,
    Json(serde_json::Value),
    Bytes(Vec<u8>),
}

/// Configures a [`Zipbox`] client.
#[derive(Debug, Clone)]
pub struct ZipboxBuilder {
    api_key: Option<String>,
    base_url: Option<String>,
    max_retries: u32,
    retry_base_delay: Duration,
    poll_interval: Duration,
    reconnect_base_delay: Duration,
    http: Option<reqwest::Client>,
}

impl Default for ZipboxBuilder {
    fn default() -> Self {
        ZipboxBuilder {
            api_key: None,
            base_url: None,
            max_retries: 3,
            retry_base_delay: Duration::from_millis(500),
            poll_interval: Duration::from_secs(1),
            reconnect_base_delay: Duration::from_millis(500),
            http: None,
        }
    }
}

impl ZipboxBuilder {
    /// The account API key (`zb_…`). Default: `ZIPBOX_API_KEY`.
    pub fn api_key(mut self, key: impl Into<String>) -> Self {
        self.api_key = Some(key.into());
        self
    }

    /// The control plane URL. Default: `ZIPBOX_API_URL`, else [`DEFAULT_API_URL`].
    pub fn base_url(mut self, url: impl Into<String>) -> Self {
        self.base_url = Some(url.into());
        self
    }

    /// How many times a retryable failure of a safe request is retried. Default 3.
    pub fn max_retries(mut self, n: u32) -> Self {
        self.max_retries = n;
        self
    }

    /// The first rung of the retry backoff. Default 500 ms.
    pub fn retry_base_delay(mut self, d: Duration) -> Self {
        self.retry_base_delay = d;
        self
    }

    /// The first rung of the poll that waits for a new box to be ready. Default 1 s.
    pub fn poll_interval(mut self, d: Duration) -> Self {
        self.poll_interval = d;
        self
    }

    /// The first rung of the PTY reconnect ladder. Default 500 ms (cap 30 s).
    pub fn reconnect_base_delay(mut self, d: Duration) -> Self {
        self.reconnect_base_delay = d;
        self
    }

    /// Use this `reqwest::Client` (proxies, custom TLS). No timeout is set by
    /// default and none should be: a box runs until someone stops it.
    pub fn http_client(mut self, client: reqwest::Client) -> Self {
        self.http = Some(client);
        self
    }

    /// Builds the client. Fails when no API key was given or found in the env.
    pub fn build(self) -> Result<Zipbox> {
        let api_key = self
            .api_key
            .or_else(|| std::env::var("ZIPBOX_API_KEY").ok())
            .filter(|k| !k.is_empty())
            .ok_or_else(|| {
                Error::new(
                    ErrorCode::Unauthorized,
                    "no API key: pass one or set ZIPBOX_API_KEY (zipbox.ai → Settings → API keys)",
                )
            })?;
        let base_url = self
            .base_url
            .or_else(|| std::env::var("ZIPBOX_API_URL").ok())
            .filter(|u| !u.is_empty())
            .unwrap_or_else(|| DEFAULT_API_URL.to_string());
        let http = match self.http {
            Some(http) => http,
            None => reqwest::Client::builder()
                .user_agent(USER_AGENT)
                .build()
                .map_err(Error::transport)?,
        };
        Ok(Zipbox {
            inner: Arc::new(Inner {
                http,
                base_url: base_url.trim_end_matches('/').to_string(),
                api_key,
                max_retries: self.max_retries,
                retry_base_delay: self.retry_base_delay,
                poll_interval: self.poll_interval,
                reconnect_base_delay: self.reconnect_base_delay,
                boxes: Mutex::new(HashMap::new()),
            }),
        })
    }
}

impl Zipbox {
    /// A client with this API key and every other setting at its default.
    pub fn new(api_key: impl Into<String>) -> Result<Zipbox> {
        ZipboxBuilder::default().api_key(api_key).build()
    }

    /// A client configured from `ZIPBOX_API_KEY` and `ZIPBOX_API_URL`.
    pub fn from_env() -> Result<Zipbox> {
        ZipboxBuilder::default().build()
    }

    /// A builder for every setting.
    pub fn builder() -> ZipboxBuilder {
        ZipboxBuilder::default()
    }

    /// Box lifecycle: create, list, get, start, stop, restart, delete, tier.
    pub fn sandboxes(&self) -> Sandboxes {
        Sandboxes {
            client: self.clone(),
        }
    }

    /// A handle to one box by id. Makes no request.
    pub fn sandbox(&self, id: impl Into<String>) -> Sandbox {
        Sandbox::new(self.clone(), id.into(), None)
    }

    /// Balance and usage.
    pub fn account(&self) -> Account {
        Account {
            client: self.clone(),
        }
    }

    // ---- control plane -------------------------------------------------

    pub(crate) async fn api(
        &self,
        method: Method,
        path: &str,
        body: Body,
        extra: HeaderMap,
        retry: bool,
    ) -> Result<Response> {
        let url = format!("{}{}", self.inner.base_url, path);
        let auth = format!("Bearer {}", self.inner.api_key);
        self.send(method, &url, &auth, body, extra, retry).await
    }

    pub(crate) async fn api_json<T: DeserializeOwned>(
        &self,
        method: Method,
        path: &str,
        body: Body,
    ) -> Result<T> {
        let retry = method == Method::GET;
        let resp = self
            .api(method, path, body, HeaderMap::new(), retry)
            .await?;
        decode_json(resp).await
    }

    pub(crate) async fn api_unit(&self, method: Method, path: &str, body: Body) -> Result<()> {
        let retry = method == Method::GET;
        self.api(method, path, body, HeaderMap::new(), retry)
            .await?;
        Ok(())
    }

    pub(crate) async fn detail(&self, id: &str) -> Result<SandboxDetail> {
        let detail: SandboxDetail = self
            .api_json(Method::GET, &format!("/sandbox/{}", encode(id)), Body::None)
            .await?;
        self.remember(&detail);
        Ok(detail)
    }

    pub(crate) fn remember(&self, detail: &SandboxDetail) {
        if let (true, Some(base), Some(token)) = (detail.ready, &detail.base_url, &detail.box_token)
        {
            self.inner.boxes.lock().expect("box cache").insert(
                detail.info.id.clone(),
                BoxAccess {
                    base_url: base.trim_end_matches('/').to_string(),
                    token: token.clone(),
                },
            );
        }
    }

    // ---- box data plane ------------------------------------------------

    pub(crate) async fn box_access(&self, id: &str, refresh: bool) -> Result<BoxAccess> {
        if !refresh {
            if let Some(access) = self.inner.boxes.lock().expect("box cache").get(id) {
                return Ok(access.clone());
            }
        }
        self.inner.boxes.lock().expect("box cache").remove(id);
        let detail = self.detail(id).await?;
        match (detail.ready, detail.base_url, detail.box_token) {
            (true, Some(base_url), Some(token)) => Ok(BoxAccess {
                base_url: base_url.trim_end_matches('/').to_string(),
                token,
            }),
            _ => Err(Error::new(
                ErrorCode::BoxNotRunning,
                format!(
                    "box {id} is not running (status {}); start it and wait until it is ready",
                    detail.info.status
                ),
            )),
        }
    }

    /// One request to a box. A 401 re-fetches the box token once and retries once.
    pub(crate) async fn box_request(
        &self,
        id: &str,
        method: Method,
        path: &str,
        body: impl Fn() -> Body,
        accept: Option<&'static str>,
    ) -> Result<Response> {
        let retry = method == Method::GET;
        let mut headers = HeaderMap::new();
        if let Some(accept) = accept {
            headers.insert(ACCEPT, HeaderValue::from_static(accept));
        }
        let access = self.box_access(id, false).await?;
        let first = self
            .send(
                method.clone(),
                &format!("{}{}", access.base_url, path),
                &format!("Bearer {}", access.token),
                body(),
                headers.clone(),
                retry,
            )
            .await;
        match first {
            Err(err) if err.status == Some(401) => {
                let access = self.box_access(id, true).await?;
                self.send(
                    method,
                    &format!("{}{}", access.base_url, path),
                    &format!("Bearer {}", access.token),
                    body(),
                    headers,
                    retry,
                )
                .await
            }
            other => other,
        }
    }

    pub(crate) async fn box_json<T: DeserializeOwned>(
        &self,
        id: &str,
        method: Method,
        path: &str,
        body: impl Fn() -> Body,
    ) -> Result<T> {
        decode_json(self.box_request(id, method, path, body, None).await?).await
    }

    async fn send(
        &self,
        method: Method,
        url: &str,
        auth: &str,
        body: Body,
        extra: HeaderMap,
        retry: bool,
    ) -> Result<Response> {
        let mut backoff = Backoff::new(self.inner.retry_base_delay, Duration::from_secs(30));
        let mut attempt = 0u32;
        loop {
            let mut req = self
                .inner
                .http
                .request(method.clone(), url)
                .header(AUTHORIZATION, auth)
                .headers(extra.clone());
            req = match &body {
                Body::None => req,
                Body::Json(value) => req
                    .header(CONTENT_TYPE, "application/json")
                    .body(serde_json::to_vec(value).map_err(Error::decode)?),
                Body::Bytes(bytes) => req
                    .header(CONTENT_TYPE, "application/octet-stream")
                    .body(bytes.clone()),
            };
            let outcome = match req.send().await {
                Ok(resp) if resp.status().is_success() => return Ok(resp),
                Ok(resp) => {
                    let status = resp.status().as_u16();
                    let retry_after = resp
                        .headers()
                        .get(RETRY_AFTER)
                        .and_then(|v| v.to_str().ok())
                        .and_then(|v| v.trim().parse::<u64>().ok())
                        .map(Duration::from_secs);
                    let bytes = resp.bytes().await.map_err(Error::transport)?;
                    Error::from_response(status, retry_after, &bytes)
                }
                Err(err) => Error::transport(err),
            };
            let retryable = matches!(
                outcome.code,
                ErrorCode::RateLimited | ErrorCode::Unavailable
            ) && outcome.retryable;
            if !retry || !retryable || attempt >= self.inner.max_retries {
                return Err(outcome);
            }
            attempt += 1;
            let delay = outcome.retry_after.unwrap_or_else(|| backoff.next_delay());
            tokio::time::sleep(delay).await;
        }
    }
}

pub(crate) fn json_body(value: &impl Serialize) -> Body {
    Body::Json(serde_json::to_value(value).expect("request bodies serialize"))
}

pub(crate) async fn decode_json<T: DeserializeOwned>(resp: Response) -> Result<T> {
    let bytes = resp.bytes().await.map_err(Error::transport)?;
    serde_json::from_slice(&bytes).map_err(Error::decode)
}

/// Percent-encodes everything but RFC 3986 unreserved characters.
pub(crate) fn encode(value: &str) -> String {
    let mut out = String::with_capacity(value.len());
    for b in value.bytes() {
        match b {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'.' | b'_' | b'~' => {
                out.push(b as char)
            }
            _ => out.push_str(&format!("%{b:02X}")),
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::encode;

    #[test]
    fn encodes_paths_for_queries() {
        assert_eq!(encode("/root/workspace/a b"), "%2Froot%2Fworkspace%2Fa%20b");
        assert_eq!(encode("plain-name_1.txt"), "plain-name_1.txt");
    }
}
