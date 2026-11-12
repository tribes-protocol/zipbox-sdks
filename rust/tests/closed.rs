//! A server that closes the connection ends the call with the usual network error,
//! never a future left pending. No timeout makes this true: the close itself does
//! (AGENTS.md §0.0 forbids a request deadline on these calls). An exec that may have
//! reached the box is never sent again: running a command twice is worse than an error.

use std::sync::{Arc, Mutex};

use serde_json::json;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
use zipbox::{ErrorCode, Zipbox};

const MID_BODY: &str = "HTTP/1.1 200 OK\r\ncontent-type: application/json\r\n\
    content-length: 400\r\n\r\n[{\"id\":";
const MID_EVENTS: &str = "HTTP/1.1 200 OK\r\ncontent-type: text/event-stream\r\n\
    transfer-encoding: chunked\r\n\r\n40\r\nevent: sta";

/// Reads one request head and returns its first line ("" when the peer went away).
async fn read_head(stream: &mut TcpStream) -> String {
    let mut head = Vec::new();
    let mut buf = [0u8; 4096];
    while !head.windows(4).any(|w| w == b"\r\n\r\n") {
        match stream.read(&mut buf).await {
            Ok(0) | Err(_) => break,
            Ok(n) => head.extend_from_slice(&buf[..n]),
        }
    }
    let head = String::from_utf8_lossy(&head);
    head.split("\r\n").next().unwrap_or_default().to_string()
}

struct ClosingServer {
    base: String,
    requests: Arc<Mutex<Vec<String>>>,
}

/// Accepts, reads the request head, writes `reply` (if any) and closes.
async fn closing_server(reply: &'static str) -> ClosingServer {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let base = format!("http://{}", listener.local_addr().unwrap());
    let requests: Arc<Mutex<Vec<String>>> = Arc::default();
    let seen = requests.clone();
    tokio::spawn(async move {
        while let Ok((mut stream, _)) = listener.accept().await {
            let seen = seen.clone();
            tokio::spawn(async move {
                let first = read_head(&mut stream).await;
                seen.lock().unwrap().push(first);
                if !reply.is_empty() {
                    let _ = stream.write_all(reply.as_bytes()).await;
                }
            });
        }
    });
    ClosingServer { base, requests }
}

/// A control plane that answers every request with the box ready on `box_base`.
async fn api_server(box_base: String) -> String {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let base = format!("http://{}", listener.local_addr().unwrap());
    tokio::spawn(async move {
        while let Ok((mut stream, _)) = listener.accept().await {
            let box_base = box_base.clone();
            tokio::spawn(async move {
                read_head(&mut stream).await;
                let body = json!({
                    "id": "sbx", "sandboxId": "sbx", "name": "demo", "status": "running",
                    "ready": true, "readiness": "ready", "httpsReady": true,
                    "baseUrl": box_base, "boxToken": "bt"
                })
                .to_string();
                let response = format!(
                    "HTTP/1.1 200 OK\r\ncontent-type: application/json\r\n\
                     content-length: {}\r\nconnection: close\r\n\r\n{body}",
                    body.len()
                );
                let _ = stream.write_all(response.as_bytes()).await;
            });
        }
    });
    base
}

fn client(base: &str, max_retries: u32) -> Zipbox {
    Zipbox::builder()
        .api_key("zb_TTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTT")
        .base_url(base)
        .max_retries(max_retries)
        .build()
        .unwrap()
}

#[tokio::test]
async fn a_connection_closed_with_no_response_is_a_retryable_error() {
    let server = closing_server("").await;

    let error = client(&server.base, 0)
        .sandboxes()
        .list()
        .await
        .err()
        .unwrap();

    assert_eq!(error.code, ErrorCode::Unavailable);
    assert!(error.retryable);
}

#[tokio::test]
async fn a_connection_closed_mid_body_is_a_retryable_error() {
    let server = closing_server(MID_BODY).await;

    let error = client(&server.base, 0)
        .sandboxes()
        .list()
        .await
        .err()
        .unwrap();

    assert_eq!(error.code, ErrorCode::Unavailable);
    assert!(error.retryable);
}

async fn exec_against(reply: &'static str) -> (ErrorCode, Vec<String>) {
    let box_server = closing_server(reply).await;
    let api = api_server(box_server.base.clone()).await;

    let error = client(&api, 3)
        .sandbox("sbx")
        .exec("true")
        .await
        .err()
        .unwrap();

    let seen = box_server.requests.lock().unwrap().clone();
    (error.code, seen)
}

#[tokio::test]
async fn an_exec_on_a_connection_closed_with_no_response_is_sent_once() {
    let (code, seen) = exec_against("").await;

    assert_eq!(code, ErrorCode::Unavailable);
    assert_eq!(seen, ["POST /exec HTTP/1.1"]);
}

#[tokio::test]
async fn an_exec_whose_events_closed_before_its_start_is_sent_once() {
    let (code, seen) = exec_against(MID_EVENTS).await;

    assert_eq!(code, ErrorCode::Unavailable);
    assert_eq!(seen, ["POST /exec HTTP/1.1"]);
}
