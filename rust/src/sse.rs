//! A minimal server-sent-events parser for the box's exec streams.

use std::collections::VecDeque;
use std::pin::Pin;

use bytes::Bytes;
use futures::{Stream, StreamExt};

use crate::error::{Error, Result};

/// One SSE event as it came off the wire.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct SseEvent {
    pub id: Option<String>,
    pub event: String,
    pub data: String,
}

/// Incremental parser. Bytes are buffered until a full line arrives, so a chunk
/// boundary inside a multi-byte UTF-8 sequence never corrupts the text: a line
/// break byte can never sit inside one.
#[derive(Debug, Default)]
pub(crate) struct SseParser {
    buf: Vec<u8>,
    id: Option<String>,
    event: Option<String>,
    data: Vec<String>,
}

impl SseParser {
    pub fn feed(&mut self, chunk: &[u8]) -> Vec<SseEvent> {
        self.buf.extend_from_slice(chunk);
        let mut out = Vec::new();
        while let Some(pos) = self.buf.iter().position(|b| *b == b'\n') {
            let mut line: Vec<u8> = self.buf.drain(..=pos).collect();
            line.pop();
            if line.last() == Some(&b'\r') {
                line.pop();
            }
            let line = String::from_utf8_lossy(&line).into_owned();
            if let Some(event) = self.line(&line) {
                out.push(event);
            }
        }
        out
    }

    /// Ends the stream. An event the stream cut off before its blank line is
    /// discarded (WHATWG): a dropped connection can end mid-`data:`, and a
    /// half-read event is not an event. The caller resumes from the last cursor.
    pub fn finish(&mut self) {
        self.buf.clear();
        self.event = None;
        self.data.clear();
    }

    fn line(&mut self, line: &str) -> Option<SseEvent> {
        if line.is_empty() {
            return self.dispatch();
        }
        if line.starts_with(':') {
            return None;
        }
        let (field, value) = match line.split_once(':') {
            Some((f, v)) => (f, v.strip_prefix(' ').unwrap_or(v)),
            None => (line, ""),
        };
        match field {
            "id" => self.id = Some(value.to_string()),
            "event" => self.event = Some(value.to_string()),
            "data" => self.data.push(value.to_string()),
            _ => {}
        }
        None
    }

    fn dispatch(&mut self) -> Option<SseEvent> {
        if self.data.is_empty() && self.event.is_none() {
            return None;
        }
        let event = SseEvent {
            id: self.id.clone(),
            event: self.event.take().unwrap_or_else(|| "message".to_string()),
            data: std::mem::take(&mut self.data).join("\n"),
        };
        Some(event)
    }
}

type ByteStream = Pin<Box<dyn Stream<Item = reqwest::Result<Bytes>> + Send>>;

struct State {
    bytes: ByteStream,
    parser: SseParser,
    pending: VecDeque<SseEvent>,
    done: bool,
}

/// Turns a streaming response body into a stream of SSE events.
pub(crate) fn sse_stream(
    response: reqwest::Response,
) -> impl Stream<Item = Result<SseEvent>> + Send + 'static {
    let state = State {
        bytes: Box::pin(response.bytes_stream()),
        parser: SseParser::default(),
        pending: VecDeque::new(),
        done: false,
    };
    futures::stream::unfold(state, |mut st| async move {
        loop {
            if let Some(event) = st.pending.pop_front() {
                return Some((Ok(event), st));
            }
            if st.done {
                return None;
            }
            match st.bytes.next().await {
                Some(Ok(chunk)) => st.pending.extend(st.parser.feed(&chunk)),
                Some(Err(err)) => {
                    st.done = true;
                    return Some((Err(Error::transport(err)), st));
                }
                None => {
                    st.done = true;
                    st.parser.finish();
                }
            }
        }
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    const WIRE: &str = "id: 0-0\nevent: start\ndata: {\"id\":\"c\"}\n\nid: 9-0\r\nevent: stdout\r\ndata: {\"data\":\"h\u{e9}llo \u{1f600}\\n\"}\r\n\r\n: keepalive\n\n";

    fn parse_in_chunks(size: usize) -> Vec<SseEvent> {
        let mut parser = SseParser::default();
        let mut out = Vec::new();
        for chunk in WIRE.as_bytes().chunks(size) {
            out.extend(parser.feed(chunk));
        }
        parser.finish();
        out
    }

    #[test]
    fn parses_regardless_of_chunk_boundaries() {
        let whole = parse_in_chunks(WIRE.len());
        assert_eq!(whole.len(), 2);
        assert_eq!(whole[0].event, "start");
        assert_eq!(whole[1].id.as_deref(), Some("9-0"));
        assert!(whole[1].data.contains("h\u{e9}llo \u{1f600}"));
        for size in 1..12 {
            assert_eq!(parse_in_chunks(size), whole, "chunk size {size}");
        }
    }

    #[test]
    fn joins_multiline_data_and_drops_a_cut_off_event() {
        let mut parser = SseParser::default();
        // The connection drops mid-event: the half `exit` must not surface (its
        // truncated JSON would read as a fatal decode error, not a drop).
        let events =
            parser.feed(b"event: stdout\ndata: a\ndata: b\n\nevent: exit\ndata: {\"exitCo");
        parser.finish();
        assert_eq!(events.len(), 1);
        assert_eq!(events[0].data, "a\nb");
        assert!(
            parser.feed(b"\n\n").is_empty(),
            "finish leaves nothing behind"
        );
    }
}
