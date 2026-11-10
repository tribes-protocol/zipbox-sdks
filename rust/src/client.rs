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
    pub retry_max_delay: Duration,
    pub poll_interval: Duration,
    pub poll_max_interval: Duration,
    pub pty_base_delay: Duration,
    pub pty_max_delay: Duration,
    pub boxes: Mutex<HashMap<String, BoxAccess>>,
}

/// How long a private box's reachability probe may take before the box counts as
/// off this device's tailnet. A probe is a one-off check, not a live surface.
pub(crate) const PRIVATE_PROBE_TIMEOUT: Duration = Duration::from_secs(5);

/// What a box needs: its origin and the token it accepts.
#[derive(Clone)]
pub(crate) struct BoxAccess {
    pub base_url: String,
    pub token: String,
    /// The box's name, for messages.
    pub name: String,
    /// Private ("tailnet only"): probed before the first call from this client.
    pub private: bool,
    /// A private box this client has reached at least once.
    pub reachable: bool,
}

impl std::fmt::Debug for BoxAccess {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("BoxAccess")
            .field("base_url", &self.base_url)
            .field("name", &self.name)
            .field("private", &self.private)
            .finish_non_exhaustive()
    }
}

impl BoxAccess {
    fn from_detail(detail: &SandboxDetail) -> Option<BoxAccess> {
        match (detail.ready, &detail.base_url, &detail.box_token) {
            (true, Some(base), Some(token)) => Some(BoxAccess {
                base_url: base.trim_end_matches('/').to_string(),
                token: token.clone(),
                name: detail
                    .info
                    .name
                    .clone()
                    .unwrap_or_else(|| detail.info.id.clone()),
                private: detail.info.private_mode,
                reachable: false,
            }),
            _ => None,
        }
    }
}

/// Whether a failed probe still shows the box answered. A TLS handshake that failed
/// is an answer: the box is reachable and only lacks its certificate yet, which the
/// box call itself waits out. Off the tailnet nothing answers: the connect times
/// out, or the OS refuses to route or connect it.
fn answered(err: &reqwest::Error) -> bool {
    use std::io::ErrorKind;
    if err.is_timeout() {
        return false;
    }
    let mut source = std::error::Error::source(err);
    while let Some(inner) = source {
        if let Some(io) = inner.downcast_ref::<std::io::Error>() {
            return !matches!(
                io.kind(),
                ErrorKind::ConnectionRefused
                    | ErrorKind::NetworkUnreachable
                    | ErrorKind::HostUnreachable
                    | ErrorKind::TimedOut
                    | ErrorKind::AddrNotAvailable
            );
        }
        source = inner.source();
    }
    false
}

/// What a box-direct call on a private box says when this device is off its tailnet.
pub(crate) fn private_unreachable_message(name: &str) -> String {
    format!(
        "{name} is private: it can only be reached from devices on your tailnet. \
         Turn on Tailscale on this device (https://tailscale.com/download)."
    )
}

/// A request body.
pub(crate) enum Body {
    None,
    Json(serde_json::Value),
    Bytes(Vec<u8>),
}

/// One request for `Zipbox::send`. A box call sets `box_unsent` to its unsent ladder,
/// so a request that never left this client comes back to it instead of being retried
/// in place.
struct SendRequest<'a> {
    method: Method,
    url: &'a str,
    auth: &'a str,
    body: Body,
    extra: HeaderMap,
    retry: bool,
    box_unsent: Option<&'a mut Backoff>,
}

/// Configures a [`Zipbox`] client.
#[derive(Debug, Clone)]
pub struct ZipboxBuilder {
    api_key: Option<String>,
    base_url: Option<String>,
    max_retries: u32,
    retry_base_delay: Duration,
    retry_max_delay: Duration,
    poll_interval: Duration,
    poll_max_interval: Duration,
    pty_base_delay: Duration,
    pty_max_delay: Duration,
    http: Option<reqwest::Client>,
}

impl Default for ZipboxBuilder {
    fn default() -> Self {
        ZipboxBuilder {
            api_key: None,
            base_url: None,
            max_retries: 3,
            retry_base_delay: Duration::from_millis(500),
            retry_max_delay: Duration::from_secs(30),
            poll_interval: Duration::from_secs(1),
            poll_max_interval: Duration::from_secs(10),
            pty_base_delay: Duration::from_millis(500),
            pty_max_delay: Duration::from_secs(30),
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

    /// The cap of the retry backoff. Default 30 s.
    pub fn retry_max_delay(mut self, d: Duration) -> Self {
        self.retry_max_delay = d;
        self
    }

    /// The first rung of the poll that waits for a new box to be ready; each rung
    /// grows 1.25×, with jitter. Default 1 s.
    pub fn poll_interval(mut self, d: Duration) -> Self {
        self.poll_interval = d;
        self
    }

    /// The cap of that poll. Default 10 s.
    pub fn poll_max_interval(mut self, d: Duration) -> Self {
        self.poll_max_interval = d;
        self
    }

    /// The first rung of the PTY reconnect ladder. Default 500 ms.
    pub fn pty_base_delay(mut self, d: Duration) -> Self {
        self.pty_base_delay = d;
        self
    }

    /// The cap of the PTY reconnect ladder. Default 30 s; there is no attempt limit.
    pub fn pty_max_delay(mut self, d: Duration) -> Self {
        self.pty_max_delay = d;
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
                retry_max_delay: self.retry_max_delay,
                poll_interval: self.poll_interval,
                poll_max_interval: self.poll_max_interval,
                pty_base_delay: self.pty_base_delay,
                pty_max_delay: self.pty_max_delay,
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
        self.send(SendRequest {
            method,
            url: &url,
            auth: &auth,
            body,
            extra,
            retry,
            box_unsent: None,
        })
        .await
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
        // A box that is not ready no longer answers where it did: its cached access
        // goes, and the next box call reads it afresh.
        if !detail.ready {
            self.inner.boxes.lock().expect("box cache").remove(id);
        }
        self.remember(&detail);
        Ok(detail)
    }

    pub(crate) fn remember(&self, detail: &SandboxDetail) {
        if let Some(mut access) = BoxAccess::from_detail(detail) {
            let mut boxes = self.inner.boxes.lock().expect("box cache");
            // Reachability survives a token refresh, never a move or a privacy change.
            access.reachable = boxes.get(&detail.info.id).is_some_and(|previous| {
                previous.reachable && previous.base_url == access.base_url && access.private
            });
            boxes.insert(detail.info.id.clone(), access);
        }
    }

    // ---- box data plane ------------------------------------------------

    fn mark_reachable(&self, id: &str, access: &BoxAccess) {
        if let Some(cached) = self.inner.boxes.lock().expect("box cache").get_mut(id) {
            if cached.base_url == access.base_url {
                cached.reachable = true;
            }
        }
    }

    pub(crate) async fn box_access(&self, id: &str, refresh: bool) -> Result<BoxAccess> {
        let cached = if refresh {
            None
        } else {
            self.inner.boxes.lock().expect("box cache").get(id).cloned()
        };
        let access = match cached {
            Some(access) => access,
            None => {
                self.inner.boxes.lock().expect("box cache").remove(id);
                let detail = self.detail(id).await?;
                BoxAccess::from_detail(&detail)
                    .ok_or_else(|| crate::sandboxes::box_not_ready_error(id, &detail))?
            }
        };
        self.reach(id, access).await
    }

    /// The box's access after a request to it never left this client: its origin
    /// may have moved (a created box moves once, from its boot address to
    /// `box--<name>`). Drops the cached access and runs the wait `create` uses; a
    /// paused, failed or gone box ends it with that wait's error.
    pub(crate) async fn reresolve(&self, id: &str) -> Result<BoxAccess> {
        self.inner.boxes.lock().expect("box cache").remove(id);
        let ready = self.sandboxes().wait_ready(id).await?;
        let detail = ready.info().expect("wait_ready returns the detail it read");
        let access = BoxAccess::from_detail(detail)
            .ok_or_else(|| crate::sandboxes::box_not_ready_error(id, detail))?;
        self.reach(id, access).await
    }

    async fn reach(&self, id: &str, access: BoxAccess) -> Result<BoxAccess> {
        if access.private && !access.reachable {
            self.probe_private(id, &access, PRIVATE_PROBE_TIMEOUT)
                .await?;
        }
        Ok(access)
    }

    /// Checks a private box answers from here before calling it.
    ///
    /// A private box's name resolves to its tailnet address, which off the tailnet
    /// does not refuse a connection, it swallows it. Any HTTP answer from
    /// `/challenge` means this device reaches the box, and so does a failed TLS
    /// handshake (see [`answered`]); a connection that was never made within the
    /// deadline means it does not, and the call fails now instead of hanging.
    async fn probe_private(&self, id: &str, access: &BoxAccess, deadline: Duration) -> Result<()> {
        let probe = self
            .inner
            .http
            .get(format!("{}/challenge", access.base_url))
            .timeout(deadline)
            .send()
            .await;
        match probe {
            Ok(_) => {
                self.mark_reachable(id, access);
                Ok(())
            }
            Err(err) if answered(&err) => {
                self.mark_reachable(id, access);
                Ok(())
            }
            Err(err) => {
                let mut out = Error::new(
                    ErrorCode::BoxPrivateUnreachable,
                    private_unreachable_message(&access.name),
                );
                out.source = Some(Box::new(err));
                Err(out)
            }
        }
    }

    /// One request to a box. A 401 re-fetches the box token once and retries once.
    ///
    /// A second 401 is not a deleted key: that fails the re-fetch itself, at the
    /// control plane. The control plane just vouched for the key, so the box has not
    /// received its token yet, and the error says so.
    ///
    /// A request that never reached the box waits one rung of the unsent ladder,
    /// re-reads the box (`reresolve`) and retries where it now answers,
    /// with no attempt ceiling: dropping the future is the caller's bound. The
    /// ladder is the call's own; it grows across re-reads and starts over only when
    /// the box answers.
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
        let mut unsent = Backoff::new(self.inner.retry_base_delay, self.inner.retry_max_delay);
        let mut access = self.box_access(id, false).await?;
        let mut refetched = false;
        loop {
            let sent = self
                .send(SendRequest {
                    method: method.clone(),
                    url: &format!("{}{}", access.base_url, path),
                    auth: &format!("Bearer {}", access.token),
                    body: body(),
                    extra: headers.clone(),
                    retry,
                    box_unsent: Some(&mut unsent),
                })
                .await;
            match sent {
                Err(err) if is_unsent(&err) => {
                    tokio::time::sleep(unsent.next_delay()).await;
                    access = self.reresolve(id).await?;
                }
                Err(err) if err.status == Some(401) && !refetched => {
                    refetched = true;
                    access = self.box_access(id, true).await?;
                }
                Err(err) if err.status == Some(401) => return Err(key_not_received(id, err)),
                other => return other,
            }
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

    /// Sends one request, retrying a retryable answer of a safe request. A request
    /// that never left this client (a failed connect or TLS handshake) is retried
    /// in place for the control plane, whose origin does not move. A box call
    /// passes its unsent ladder (`box_unsent`) and gets that failure back instead:
    /// only it can re-read the box. Any answer resets that ladder.
    async fn send(&self, request: SendRequest<'_>) -> Result<Response> {
        let SendRequest {
            method,
            url,
            auth,
            body,
            extra,
            retry,
            mut box_unsent,
        } = request;
        let mut backoff = Backoff::new(self.inner.retry_base_delay, self.inner.retry_max_delay);
        let mut unsent_backoff =
            Backoff::new(self.inner.retry_base_delay, self.inner.retry_max_delay);
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
                Ok(resp) => {
                    if let Some(ladder) = box_unsent.as_deref_mut() {
                        ladder.reset();
                    }
                    if resp.status().is_success() {
                        return Ok(resp);
                    }
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
                // A failure to connect or to complete the TLS handshake: nothing was
                // sent. A box call re-reads the box before it retries.
                Err(err) if err.is_connect() && box_unsent.is_some() => {
                    return Err(Error::transport(err))
                }
                // Retried for EVERY method with no attempt ceiling, on a capped,
                // jittered ladder; dropping the future is the caller's bound.
                Err(err) if err.is_connect() => {
                    tokio::time::sleep(unsent_backoff.next_delay()).await;
                    continue;
                }
                Err(err) => Error::transport(err),
            };
            if !retry || !outcome.retryable || attempt >= self.inner.max_retries {
                return Err(outcome);
            }
            attempt += 1;
            let delay = outcome.retry_after.unwrap_or_else(|| backoff.next_delay());
            tokio::time::sleep(delay).await;
        }
    }
}

/// Whether a request failed before anything reached the server: a failed connect,
/// DNS lookup or TLS handshake, including one the server ended early. reqwest
/// reports each as a connect error; a failure after the request was written is not.
fn is_unsent(err: &Error) -> bool {
    err.source
        .as_deref()
        .and_then(|source| source.downcast_ref::<reqwest::Error>())
        .is_some_and(reqwest::Error::is_connect)
}

/// The second 401 of `box_request`, worded for what it is.
pub(crate) fn key_not_received(id: &str, err: Error) -> Error {
    let mut out = Error::new(
        ErrorCode::Unauthorized,
        format!(
            "box {id} has not received this API key's token yet: the control plane accepts \
             the key, the box does not. A key created moments ago reaches its running boxes \
             within seconds; retry then"
        ),
    );
    out.status = Some(401);
    out.retryable = true;
    out.request_id = err.request_id;
    out
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
    use super::{encode, key_not_received, BoxAccess, Zipbox, PRIVATE_PROBE_TIMEOUT};
    use crate::error::{Error, ErrorCode};
    use std::time::{Duration, Instant};

    #[test]
    fn the_private_probe_is_bounded_at_five_seconds() {
        assert_eq!(PRIVATE_PROBE_TIMEOUT, Duration::from_secs(5));
    }

    // A tailnet address off the tailnet swallows the request: the deadline, not
    // the network, is what ends the probe. Shortened here; the production bound is
    // PRIVATE_PROBE_TIMEOUT.
    #[tokio::test]
    async fn the_deadline_ends_a_probe_that_never_answers() {
        let silent = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let port = silent.local_addr().unwrap().port();
        let zb = Zipbox::new("zb_TTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTT").unwrap();
        let access = BoxAccess {
            base_url: format!("http://127.0.0.1:{port}"),
            token: "bt".into(),
            name: "build".into(),
            private: true,
            reachable: false,
        };
        let deadline = Duration::from_millis(300);
        let started = Instant::now();
        let err = zb
            .probe_private("sbx_1", &access, deadline)
            .await
            .unwrap_err();
        let elapsed = started.elapsed();
        assert_eq!(err.code, ErrorCode::BoxPrivateUnreachable);
        assert!(
            elapsed >= deadline,
            "ended before the deadline: {elapsed:?}"
        );
        assert!(
            elapsed < Duration::from_secs(3),
            "the deadline did not bound it"
        );
        drop(silent);
    }

    // A box whose certificate is not installed yet fails the TLS handshake. It
    // answered, so it is reachable: a plain-HTTP listener behind an https:// URL
    // makes rustls fail the same way.
    #[tokio::test]
    async fn a_failed_tls_handshake_counts_as_an_answer() {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = listener.local_addr().unwrap().port();
        tokio::spawn(async move {
            while let Ok((mut socket, _)) = listener.accept().await {
                use tokio::io::AsyncWriteExt;
                let _ = socket.write_all(b"HTTP/1.1 400 Bad Request\r\n\r\n").await;
            }
        });
        let zb = Zipbox::new("zb_TTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTT").unwrap();
        let access = BoxAccess {
            base_url: format!("https://127.0.0.1:{port}"),
            token: "bt".into(),
            name: "build".into(),
            private: true,
            reachable: false,
        };
        zb.probe_private("sbx_1", &access, Duration::from_secs(5))
            .await
            .expect("a TLS failure is an answer, not an unreachable box");
    }

    // Nothing listens: the connect is refused, which is what "unreachable" means here.
    #[tokio::test]
    async fn a_refused_connection_is_unreachable() {
        // Port 1 (tcpmux): privileged, so no test server in this run can be listening on
        // it, and the connect is refused. A port freed with `bind(0)` + `drop` is NOT
        // safe: the OS hands it straight back to the next `bind(0)`, so a mock server in
        // a concurrent test took it and answered this box's calls (CI, 2026-10-29).
        let zb = Zipbox::new("zb_TTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTT").unwrap();
        let access = BoxAccess {
            base_url: "http://127.0.0.1:1".into(),
            token: "bt".into(),
            name: "build".into(),
            private: true,
            reachable: false,
        };
        let err = zb
            .probe_private("sbx_1", &access, Duration::from_secs(5))
            .await
            .unwrap_err();
        assert_eq!(err.code, ErrorCode::BoxPrivateUnreachable);
    }

    #[test]
    fn encodes_paths_for_queries() {
        assert_eq!(encode("/root/workspace/a b"), "%2Froot%2Fworkspace%2Fa%20b");
        assert_eq!(encode("plain-name_1.txt"), "plain-name_1.txt");
    }

    // #4137: the box's own 401 used to pass through as if the key were deleted.
    #[test]
    fn a_second_401_says_the_box_has_not_received_a_live_key() {
        let mut box_401 = Error::new(ErrorCode::Unauthorized, "unauthorized");
        box_401.status = Some(401);
        let err = key_not_received("sbx_1", box_401);
        assert!(matches!(err.code, ErrorCode::Unauthorized));
        assert_eq!(err.status, Some(401));
        assert!(err.retryable);
        assert!(err.message.contains("has not received"));
    }
}
