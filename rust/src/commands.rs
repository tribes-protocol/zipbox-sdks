//! Commands on a box: inspect, follow, feed stdin, signal.

use futures::stream::BoxStream;
use futures::{Stream, StreamExt};
use reqwest::Method;
use serde::Deserialize;

use crate::backoff::Backoff;
use crate::client::{encode, json_body, Body};
use crate::error::{Error, Result};
use crate::sandbox::{collect, Sandbox};
use crate::sse::sse_stream;
use crate::types::{ExecEvent, ExecRecord, ExecResult, OutputStream, Signal};

/// Commands on a box by id, from [`Sandbox::commands`].
#[derive(Debug, Clone)]
pub struct Commands {
    pub(crate) sandbox: Sandbox,
}

#[derive(Deserialize)]
struct ListResponse {
    commands: Vec<ExecRecord>,
}

impl Commands {
    /// Running and recently finished commands.
    pub async fn list(&self) -> Result<Vec<ExecRecord>> {
        let list: ListResponse = self
            .sandbox
            .client
            .box_json(self.sandbox.id(), Method::GET, "/exec", || Body::None)
            .await?;
        Ok(list.commands)
    }

    /// A handle to one command.
    pub fn command(&self, command_id: &str) -> Command {
        Command::new(self.sandbox.clone(), command_id.to_string(), None)
    }

    /// One command's record.
    pub async fn get(&self, command_id: &str) -> Result<ExecRecord> {
        self.command(command_id).get().await
    }

    /// Replays a command's events from `since` (a cursor from an earlier event;
    /// `None` = from the start) and follows it until it exits, re-attaching from the
    /// last cursor whenever the stream drops (see [`Command::events`]).
    pub async fn events(
        &self,
        command_id: &str,
        since: Option<&str>,
    ) -> Result<impl Stream<Item = Result<ExecEvent>> + Send + 'static> {
        self.command(command_id).events(since).await
    }

    /// Appends bytes to a command's stdin (started with `stdin: true`).
    pub async fn stdin(&self, command_id: &str, data: impl Into<Vec<u8>>) -> Result<()> {
        self.command(command_id).stdin(data).await
    }

    /// Closes a command's stdin (EOF).
    pub async fn close_stdin(&self, command_id: &str) -> Result<()> {
        self.command(command_id).close_stdin().await
    }

    /// Sends a signal.
    pub async fn signal(&self, command_id: &str, signal: Signal) -> Result<()> {
        self.command(command_id).signal(signal).await
    }
}

/// One command on a box.
#[derive(Debug, Clone)]
pub struct Command {
    sandbox: Sandbox,
    id: String,
    record: Option<ExecRecord>,
}

impl Command {
    pub(crate) fn new(sandbox: Sandbox, id: String, record: Option<ExecRecord>) -> Command {
        Command {
            sandbox,
            id,
            record,
        }
    }

    /// The command id.
    pub fn id(&self) -> &str {
        &self.id
    }

    /// The record as the start call returned it, if this handle came from one.
    pub fn record(&self) -> Option<&ExecRecord> {
        self.record.as_ref()
    }

    /// The current record.
    pub async fn get(&self) -> Result<ExecRecord> {
        self.sandbox
            .client
            .box_json(
                self.sandbox.id(),
                Method::GET,
                &format!("/exec/{}", encode(&self.id)),
                || Body::None,
            )
            .await
    }

    /// Replays events from `since` (`None` = from the start) and follows the
    /// command until it exits. A followed command is a live surface: a dropped
    /// stream re-attaches from the last cursor with backoff and no attempt limit,
    /// and the ladder resets only when an event arrives. The stream ends after
    /// `exit` or on an answered error (e.g. `not_found`), which is its last item.
    pub async fn events(
        &self,
        since: Option<&str>,
    ) -> Result<impl Stream<Item = Result<ExecEvent>> + Send + 'static> {
        // The first attach's answered error is the caller's; a transient one is
        // retried by the follow loop like any later drop.
        let current = match self.connect(since).await {
            Ok(stream) => Some(stream),
            Err(err) if err.retryable => None,
            Err(err) => return Err(err),
        };
        let inner = &self.sandbox.client.inner;
        let follow = Follow {
            command: self.clone(),
            cursor: since.map(str::to_string),
            backoff: Backoff::new(inner.retry_base_delay, inner.retry_max_delay),
            current,
            done: false,
        };
        Ok(futures::stream::unfold(follow, Follow::step))
    }

    async fn connect(&self, since: Option<&str>) -> Result<BoxStream<'static, Result<ExecEvent>>> {
        let path = match since {
            Some(cursor) => format!("/exec/{}/events?since={}", encode(&self.id), encode(cursor)),
            None => format!("/exec/{}/events", encode(&self.id)),
        };
        let resp = self
            .sandbox
            .client
            .box_request(
                self.sandbox.id(),
                Method::GET,
                &path,
                || Body::None,
                Some("text/event-stream"),
            )
            .await?;
        Ok(exec_events(resp).boxed())
    }

    /// Waits for the command to exit and returns everything it printed. A dropped
    /// connection re-attaches from the last cursor with backoff, without limit:
    /// a command runs until it ends.
    pub async fn wait(&self) -> Result<ExecResult> {
        let mut result = collect(self.events(None).await?).await?;
        result.id = self.id.clone();
        Ok(result)
    }

    /// Appends bytes to stdin (the command must have been started with
    /// `stdin: true`).
    pub async fn stdin(&self, data: impl Into<Vec<u8>>) -> Result<()> {
        let data = data.into();
        self.sandbox
            .client
            .box_request(
                self.sandbox.id(),
                Method::POST,
                &format!("/exec/{}/stdin", encode(&self.id)),
                || Body::Bytes(data.clone()),
                None,
            )
            .await?;
        Ok(())
    }

    /// Closes stdin (EOF).
    pub async fn close_stdin(&self) -> Result<()> {
        self.sandbox
            .client
            .box_request(
                self.sandbox.id(),
                Method::POST,
                &format!("/exec/{}/stdin?close=1", encode(&self.id)),
                || Body::None,
                None,
            )
            .await?;
        Ok(())
    }

    /// Sends a signal to the command's process group.
    pub async fn signal(&self, signal: Signal) -> Result<()> {
        self.sandbox
            .client
            .box_request(
                self.sandbox.id(),
                Method::POST,
                &format!("/exec/{}/signal", encode(&self.id)),
                || json_body(&serde_json::json!({ "signal": signal })),
                None,
            )
            .await?;
        Ok(())
    }
}

/// A followed command stream: the open connection, if any, and where to resume.
struct Follow {
    command: Command,
    cursor: Option<String>,
    backoff: Backoff,
    current: Option<BoxStream<'static, Result<ExecEvent>>>,
    done: bool,
}

impl Follow {
    async fn step(mut self) -> Option<(Result<ExecEvent>, Follow)> {
        if self.done {
            return None;
        }
        loop {
            if let Some(stream) = self.current.as_mut() {
                let next = stream.next().await;
                match next {
                    Some(Ok(event)) => {
                        self.backoff.reset();
                        self.cursor = Some(event.cursor().to_string());
                        self.done = matches!(event, ExecEvent::Exit { .. });
                        return Some((Ok(event), self));
                    }
                    Some(Err(err)) if !err.retryable => {
                        self.done = true;
                        return Some((Err(err), self));
                    }
                    // A drop, or a stream that ended without `exit`: resume.
                    Some(Err(_)) | None => self.current = None,
                }
                tokio::time::sleep(self.backoff.next_delay()).await;
            }
            let connected = self.command.connect(self.cursor.as_deref()).await;
            match connected {
                Ok(stream) => self.current = Some(stream),
                Err(err) if err.retryable => {
                    tokio::time::sleep(self.backoff.next_delay()).await;
                }
                Err(err) => {
                    self.done = true;
                    return Some((Err(err), self));
                }
            }
        }
    }
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct ExitPayload {
    #[serde(default)]
    state: Option<String>,
    #[serde(default)]
    exit_code: Option<i32>,
    #[serde(default)]
    signal: Option<String>,
    #[serde(default)]
    timed_out: bool,
}

#[derive(Deserialize)]
struct GapPayload {
    stream: OutputStream,
    #[serde(default)]
    message: String,
}

/// Maps an exec SSE response to typed events, ending after `exit`.
pub(crate) fn exec_events(
    resp: reqwest::Response,
) -> impl Stream<Item = Result<ExecEvent>> + Send + 'static {
    let events = sse_stream(resp).filter_map(|item| async move {
        let event = match item {
            Ok(event) => event,
            Err(err) => return Some(Err(err)),
        };
        let cursor = event.id.clone().unwrap_or_else(|| "0-0".to_string());
        let parsed: serde_json::Value = match serde_json::from_str(&event.data) {
            Ok(value) => value,
            Err(err) => return Some(Err(Error::decode(err))),
        };
        let text = |key: &str| {
            parsed
                .get(key)
                .and_then(|v| v.as_str())
                .unwrap_or("")
                .to_string()
        };
        match event.event.as_str() {
            "start" => Some(Ok(ExecEvent::Start {
                id: text("id"),
                cursor,
            })),
            "stdout" => Some(Ok(ExecEvent::Output {
                stream: OutputStream::Stdout,
                data: text("data"),
                cursor,
            })),
            "stderr" => Some(Ok(ExecEvent::Output {
                stream: OutputStream::Stderr,
                data: text("data"),
                cursor,
            })),
            "gap" => Some(
                serde_json::from_value::<GapPayload>(parsed)
                    .map(|gap| ExecEvent::Gap {
                        stream: gap.stream,
                        message: gap.message,
                        cursor,
                    })
                    .map_err(Error::decode),
            ),
            "exit" => Some(
                serde_json::from_value::<ExitPayload>(parsed)
                    .map(|exit| ExecEvent::Exit {
                        state: exit.state.unwrap_or_else(|| "exited".to_string()),
                        exit_code: exit.exit_code,
                        signal: exit.signal,
                        timed_out: exit.timed_out,
                        cursor,
                    })
                    .map_err(Error::decode),
            ),
            _ => None,
        }
    });
    // Stop after the exit event: nothing follows it.
    futures::stream::unfold((Box::pin(events), false), |(mut events, done)| async move {
        if done {
            return None;
        }
        let item = events.next().await?;
        let finished = matches!(item, Ok(ExecEvent::Exit { .. }));
        Some((item, (events, finished)))
    })
}
