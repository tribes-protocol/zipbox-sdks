//! A create's answer, streamed (conformance README, streamed create).
//!
//! A server that streams it sends `{"type":"placed","baseUrl"}` the moment the box is
//! placed, then `{"type":"answer","status","headers","body"}`: the answer it would have
//! given unstreamed. The placed line is informational and never dialled: its origin is
//! handed out before its DNS record is known to be live. The last line is returned as
//! the response it stands for, so it is read exactly like an unstreamed answer.

use std::collections::HashMap;

use reqwest::header::CONTENT_TYPE;
use reqwest::Response;
use serde::Deserialize;

use crate::error::{Error, ErrorCode, Result};

/// The media type `create` asks for: its answer streamed once the box is placed.
pub(crate) const CLAIM_STREAM_MEDIA_TYPE: &str = "application/x-ndjson";

/// `create`'s Accept header: the stream, or plain JSON from a server that does not
/// stream.
pub(crate) const CLAIM_STREAM_ACCEPT: &str = "application/x-ndjson, application/json";

#[derive(Deserialize)]
struct ClaimStreamLine {
    #[serde(rename = "type")]
    kind: String,
    status: Option<u16>,
    #[serde(default)]
    headers: HashMap<String, serde_json::Value>,
    #[serde(default)]
    body: serde_json::Value,
}

fn is_claim_stream(resp: &Response) -> bool {
    let Some(value) = resp.headers().get(CONTENT_TYPE) else {
        return false;
    };
    let header = value.to_str().unwrap_or_default();
    let media = header.split(';').next().unwrap_or_default();
    media.trim().eq_ignore_ascii_case(CLAIM_STREAM_MEDIA_TYPE)
}

/// A stream that cannot be read is retried like a dropped connection.
fn unreadable(message: String) -> Error {
    Error::new(ErrorCode::Unavailable, message)
}

/// Reads a create's answer: the last line comes back as its response, and the placed
/// line is skipped. Any other response is returned as it is. A stream that ends before
/// its answer is a retryable `unavailable`, as a dropped connection is: create retries
/// it with its `Idempotency-Key` and gets the same box.
pub(crate) async fn read_claim_stream(mut resp: Response) -> Result<Response> {
    if !is_claim_stream(&resp) {
        return Ok(resp);
    }
    let mut buffer: Vec<u8> = Vec::new();
    loop {
        let chunk = resp.chunk().await.map_err(Error::transport)?;
        let done = chunk.is_none();
        if let Some(bytes) = chunk {
            buffer.extend_from_slice(&bytes);
        }
        while let Some(end) = buffer.iter().position(|byte| *byte == b'\n') {
            let raw: Vec<u8> = buffer.drain(..=end).collect();
            if raw.iter().all(u8::is_ascii_whitespace) {
                continue;
            }
            let line: ClaimStreamLine = serde_json::from_slice(&raw)
                .map_err(|err| unreadable(format!("unreadable create stream: {err}")))?;
            if let ("answer", Some(status)) = (line.kind.as_str(), line.status) {
                // Read to its end (the server closes right after): the connection goes
                // back to the pool.
                while resp.chunk().await.map_err(Error::transport)?.is_some() {}
                return answer_response(status, &line.headers, &line.body);
            }
        }
        if done {
            return Err(unreadable(
                "the create stream ended before its answer".to_string(),
            ));
        }
    }
}

fn answer_response(
    status: u16,
    headers: &HashMap<String, serde_json::Value>,
    body: &serde_json::Value,
) -> Result<Response> {
    let mut builder = http::Response::builder()
        .status(status)
        .header(CONTENT_TYPE, "application/json");
    for (name, value) in headers {
        if let Some(value) = value.as_str() {
            builder = builder.header(name.as_str(), value);
        }
    }
    let bytes = if body.is_null() {
        Vec::new()
    } else {
        serde_json::to_vec(body).map_err(Error::decode)?
    };
    let answer = builder
        .body(bytes)
        .map_err(|err| unreadable(format!("unreadable create answer: {err}")))?;
    Ok(Response::from(answer))
}
