//! Terminal sessions on a box.
//!
//! A session is a PTY on the box that outlives every connection to it. A [`Pty`]
//! keeps its connection alive on its own: a dropped socket re-attaches with
//! exponential backoff (cap 30 s, jitter) and no attempt limit, and the ladder
//! resets only when a frame actually arrives. Re-attaching replays the screen.

use std::collections::VecDeque;
use std::pin::Pin;
use std::task::{Context, Poll};
use std::time::Duration;

use futures::{SinkExt, Stream, StreamExt};
use reqwest::Method;
use serde::Deserialize;
use tokio::sync::mpsc;
use tokio_tungstenite::tungstenite::client::IntoClientRequest;
use tokio_tungstenite::tungstenite::http::HeaderValue;
use tokio_tungstenite::tungstenite::Message;

use crate::backoff::Backoff;
use crate::client::{encode, Body};
use crate::error::{Error, ErrorCode, Result};
use crate::sandbox::Sandbox;

/// The websocket subprotocol marker the box expects before the token.
pub const PTY_SUBPROTOCOL: &str = "tribes.session";
const RECONNECT_CAP: Duration = Duration::from_secs(30);
const CLOSE_REVOKED: u16 = 4401;
const CLOSE_UNKNOWN_SESSION: u16 = 4404;

/// Terminal sessions on a box, from [`Sandbox::pty`].
#[derive(Debug, Clone)]
pub struct PtyClient {
    pub(crate) sandbox: Sandbox,
}

impl PtyClient {
    /// Opens a new shell session (`bash`) sized `cols` × `rows`.
    pub async fn create(&self, cols: u16, rows: u16) -> Result<Pty> {
        let session = format!("bash-{}", uuid::Uuid::new_v4());
        Pty::open(self.sandbox.clone(), session, Some((cols, rows))).await
    }

    /// Re-attaches to an existing session; its screen is replayed.
    pub async fn attach(&self, session_id: &str) -> Result<Pty> {
        Pty::open(self.sandbox.clone(), session_id.to_string(), None).await
    }
}

/// What a [`Pty`] yields.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum PtyEvent {
    /// Terminal output.
    Output(String),
    /// The screen replay that follows a (re-)attach is complete.
    ReplayComplete,
    /// The connection dropped; the next attempt starts after `delay`.
    Reconnecting {
        /// How many attempts in a row have failed so far.
        attempt: u32,
        /// How long until the next one.
        delay: Duration,
    },
    /// Another client took this session over. The `Pty` stops here.
    Taken,
    /// The shell exited. The `Pty` stops here.
    Exit(Option<i32>),
    /// The box ended the session: the session no longer exists, the box is
    /// gone, or the credential was refused after one re-fetch. The `Pty` stops
    /// here.
    Closed(String),
}

enum Control {
    Input(String),
    Resize(u16, u16),
    Stop,
}

/// A live terminal session. Read it as a [`Stream`] of [`PtyEvent`]s.
pub struct Pty {
    sandbox: Sandbox,
    session_id: String,
    client_id: String,
    control: mpsc::UnboundedSender<Control>,
    events: mpsc::UnboundedReceiver<PtyEvent>,
}

impl std::fmt::Debug for Pty {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Pty")
            .field("session_id", &self.session_id)
            .finish_non_exhaustive()
    }
}

type Socket =
    tokio_tungstenite::WebSocketStream<tokio_tungstenite::MaybeTlsStream<tokio::net::TcpStream>>;

#[derive(Deserialize)]
struct ServerFrame {
    t: String,
    #[serde(default)]
    d: Option<String>,
    #[serde(default)]
    code: Option<i32>,
}

impl Pty {
    async fn open(sandbox: Sandbox, session_id: String, size: Option<(u16, u16)>) -> Result<Pty> {
        let client_id = format!("sdk-{}", uuid::Uuid::new_v4().simple());
        // The first connection's failure is the caller's to see (after one token
        // re-fetch on a 401); every later drop is recovered automatically.
        let socket = match connect(&sandbox, &session_id, &client_id, false, false).await {
            Err(err) if err.status == Some(401) => {
                connect(&sandbox, &session_id, &client_id, false, true).await?
            }
            other => other?,
        };
        let (control, control_rx) = mpsc::unbounded_channel();
        let (events_tx, events) = mpsc::unbounded_channel();
        let task = Session {
            sandbox: sandbox.clone(),
            session_id: session_id.clone(),
            client_id: client_id.clone(),
            size,
            control: control_rx,
            events: events_tx,
            backoff: Backoff::new(sandbox.client.inner.reconnect_base_delay, RECONNECT_CAP),
            owned: false,
            pending: VecDeque::new(),
            frames: 0,
        };
        tokio::spawn(task.run(socket));
        Ok(Pty {
            sandbox,
            session_id,
            client_id,
            control,
            events,
        })
    }

    /// The session id; pass it to [`PtyClient::attach`] to come back later.
    pub fn session_id(&self) -> &str {
        &self.session_id
    }

    /// Types `data` into the terminal. Queued while reconnecting.
    pub fn write(&self, data: impl Into<String>) -> Result<()> {
        self.send(Control::Input(data.into()))
    }

    /// Resizes the terminal. Re-applied after every reconnect.
    pub fn resize(&self, cols: u16, rows: u16) -> Result<()> {
        self.send(Control::Resize(cols, rows))
    }

    /// Disconnects and leaves the session running on the box.
    pub fn detach(self) {
        let _ = self.control.send(Control::Stop);
    }

    /// Kills the session on the box and disconnects.
    pub async fn close(self) -> Result<()> {
        let _ = self.control.send(Control::Stop);
        let path = format!(
            "/session/close?session={}&clientId={}",
            encode(&self.session_id),
            encode(&self.client_id)
        );
        self.sandbox
            .client
            .box_request(self.sandbox.id(), Method::POST, &path, || Body::None, None)
            .await?;
        Ok(())
    }

    fn send(&self, control: Control) -> Result<()> {
        self.control
            .send(control)
            .map_err(|_| Error::new(ErrorCode::Conflict, "the terminal session has ended"))
    }
}

impl Stream for Pty {
    type Item = PtyEvent;
    fn poll_next(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Option<PtyEvent>> {
        self.events.poll_recv(cx)
    }
}

struct Session {
    sandbox: Sandbox,
    session_id: String,
    client_id: String,
    size: Option<(u16, u16)>,
    control: mpsc::UnboundedReceiver<Control>,
    events: mpsc::UnboundedSender<PtyEvent>,
    backoff: Backoff,
    owned: bool,
    pending: VecDeque<String>,
    /// Text frames received over the session's life.
    frames: u64,
}

enum Outcome {
    /// The socket dropped; reconnect.
    Dropped,
    /// The box revoked this socket's token; re-fetch it once, then reconnect.
    Revoked,
    /// The session ended or the caller stopped; finish.
    Done,
}

impl Session {
    // Reconnects forever on a transient failure. It stops only on an ANSWERED
    // verdict: the box or the control plane refusing the credential after one
    // re-fetch, or saying the box no longer exists.
    async fn run(mut self, first: Socket) {
        let mut socket = Some(first);
        let mut refresh = false;
        // A re-fetched token was refused again with no frame in between.
        let mut refetched = false;
        loop {
            if let Some(ws) = socket.take() {
                let frames_before = self.frames;
                match self.serve(ws).await {
                    Outcome::Done => return,
                    Outcome::Dropped => {}
                    Outcome::Revoked => {
                        if refetched && self.frames == frames_before {
                            self.close("the box revoked this client's access");
                            return;
                        }
                        refresh = true;
                    }
                }
                if self.frames != frames_before {
                    refetched = false;
                }
            }
            let delay = self.backoff.next_delay();
            let _ = self.events.send(PtyEvent::Reconnecting {
                attempt: self.backoff.attempt(),
                delay,
            });
            // Input typed while disconnected stays queued in the channel; only a
            // stop is honoured during the wait.
            tokio::time::sleep(delay).await;
            if self.control.is_closed() {
                return;
            }
            match connect(
                &self.sandbox,
                &self.session_id,
                &self.client_id,
                true,
                refresh,
            )
            .await
            {
                Ok(ws) => {
                    refetched = refresh;
                    refresh = false;
                    socket = Some(ws);
                }
                Err(err) if err.code == ErrorCode::Unauthorized && !refresh => refresh = true,
                Err(err)
                    if matches!(
                        err.code,
                        ErrorCode::Unauthorized | ErrorCode::NotFound | ErrorCode::Forbidden
                    ) =>
                {
                    self.close(&err.message);
                    return;
                }
                Err(_) => {}
            }
        }
    }

    fn close(&self, reason: &str) {
        let _ = self.events.send(PtyEvent::Closed(reason.to_string()));
    }

    // Input and resizes reach the box only once it says this socket OWNS the
    // session (`owned`, after the replay). Until then they wait in `pending`: a
    // frame sent by a socket that is still `blocked` would be dropped by the box.
    async fn serve(&mut self, mut ws: Socket) -> Outcome {
        self.owned = false;
        loop {
            tokio::select! {
                control = self.control.recv() => {
                    let frame = match control {
                        Some(Control::Input(data)) => {
                            serde_json::json!({ "t": "i", "d": data }).to_string()
                        }
                        Some(Control::Resize(cols, rows)) => {
                            self.size = Some((cols, rows));
                            resize_frame(cols, rows)
                        }
                        Some(Control::Stop) | None => {
                            let _ = ws.close(None).await;
                            return Outcome::Done;
                        }
                    };
                    if !self.owned {
                        self.pending.push_back(frame);
                    } else if send_frame(&mut ws, frame).await.is_err() {
                        return Outcome::Dropped;
                    }
                }
                message = ws.next() => match message {
                    Some(Ok(Message::Text(text))) => {
                        self.backoff.reset();
                        self.frames += 1;
                        if let Some(outcome) = self.frame(&mut ws, text.as_str()).await {
                            return outcome;
                        }
                    }
                    Some(Ok(Message::Close(frame))) => {
                        let code = frame.as_ref().map(|f| u16::from(f.code));
                        if code == Some(CLOSE_UNKNOWN_SESSION) {
                            let reason = frame.map(|f| f.reason.to_string()).unwrap_or_default();
                            self.close(&reason);
                            return Outcome::Done;
                        }
                        if code == Some(CLOSE_REVOKED) {
                            return Outcome::Revoked;
                        }
                        return Outcome::Dropped;
                    }
                    Some(Ok(_)) => self.backoff.reset(),
                    Some(Err(_)) | None => return Outcome::Dropped,
                }
            }
        }
    }

    async fn on_owned(&mut self, ws: &mut Socket) -> Option<Outcome> {
        self.owned = true;
        if let Some((cols, rows)) = self.size {
            if send_frame(ws, resize_frame(cols, rows)).await.is_err() {
                return Some(Outcome::Dropped);
            }
        }
        while let Some(frame) = self.pending.pop_front() {
            if send_frame(ws, frame.clone()).await.is_err() {
                self.pending.push_front(frame);
                return Some(Outcome::Dropped);
            }
        }
        None
    }

    async fn frame(&mut self, ws: &mut Socket, text: &str) -> Option<Outcome> {
        let frame: ServerFrame = serde_json::from_str(text).ok()?;
        match frame.t.as_str() {
            "o" => {
                let _ = self
                    .events
                    .send(PtyEvent::Output(frame.d.unwrap_or_default()));
                None
            }
            "replay-complete" => {
                let _ = self.events.send(PtyEvent::ReplayComplete);
                None
            }
            "owned" => self.on_owned(ws).await,
            // Our own session, held by a ghost of our previous socket or another
            // client: take it back.
            "blocked" => {
                let takeover = serde_json::json!({ "t": "takeover" }).to_string();
                if send_frame(ws, takeover).await.is_err() {
                    return Some(Outcome::Dropped);
                }
                None
            }
            "taken" => {
                let _ = self.events.send(PtyEvent::Taken);
                let _ = ws.close(None).await;
                Some(Outcome::Done)
            }
            "x" => {
                let _ = self.events.send(PtyEvent::Exit(frame.code));
                let _ = ws.close(None).await;
                Some(Outcome::Done)
            }
            _ => None,
        }
    }
}

fn resize_frame(cols: u16, rows: u16) -> String {
    serde_json::json!({ "t": "r", "c": cols, "r": rows }).to_string()
}

async fn send_frame(ws: &mut Socket, frame: String) -> std::result::Result<(), ()> {
    ws.send(Message::text(frame)).await.map_err(|_| ())
}

async fn connect(
    sandbox: &Sandbox,
    session_id: &str,
    client_id: &str,
    resume: bool,
    refresh: bool,
) -> Result<Socket> {
    let access = sandbox.client.box_access(sandbox.id(), refresh).await?;
    let ws_origin = if let Some(rest) = access.base_url.strip_prefix("https://") {
        format!("wss://{rest}")
    } else if let Some(rest) = access.base_url.strip_prefix("http://") {
        format!("ws://{rest}")
    } else {
        access.base_url.clone()
    };
    let mut url = format!(
        "{ws_origin}/?session={}&clientId={}&clientLabel=zipbox-sdk",
        encode(session_id),
        encode(client_id)
    );
    if resume {
        url.push_str("&resume=1");
    }
    let mut request = url
        .into_client_request()
        .map_err(|err| Error::new(ErrorCode::InvalidRequest, err.to_string()))?;
    let protocol = HeaderValue::from_str(&format!("{PTY_SUBPROTOCOL}, {}", access.token))
        .map_err(|err| Error::new(ErrorCode::InvalidRequest, err.to_string()))?;
    request
        .headers_mut()
        .insert("sec-websocket-protocol", protocol);
    match tokio_tungstenite::connect_async(request).await {
        Ok((socket, _)) => Ok(socket),
        Err(tokio_tungstenite::tungstenite::Error::Http(response)) => {
            let status = response.status().as_u16();
            let body = response.body().clone().unwrap_or_default();
            if status == 401 {
                sandbox
                    .client
                    .inner
                    .boxes
                    .lock()
                    .expect("box cache")
                    .remove(sandbox.id());
            }
            Err(Error::from_response(status, None, &body))
        }
        Err(err) => Err(Error::transport(err)),
    }
}
