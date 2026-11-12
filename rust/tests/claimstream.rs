//! Streamed create (sdks/conformance/README.md, "Streamed create"): create asks for its
//! answer streamed and reads the last line's answer exactly like an unstreamed one. The
//! placed line is informational: its origin is handed out before its DNS record is known
//! to be live, so create never dials it. An unstreamed answer reads as before.

use std::sync::{Arc, Mutex};
use std::time::Duration;

use serde_json::{json, Value};
use tokio::io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader};
use tokio::net::{TcpListener, TcpStream};
use zipbox::{ErrorCode, Zipbox};

const CLAIM_PATH: &str = "/sandbox/identity/claim";

const EXEC_EVENTS: &str = "event: start\ndata: {\"id\":\"cmd_1\"}\n\n\
    event: exit\ndata: {\"state\":\"exited\",\"exitCode\":0,\"signal\":null,\"timedOut\":false}\n\n";

/// How one POST /sandbox/identity/claim answers.
#[derive(Clone, Copy)]
enum Claim {
    /// The placed line, then the answer a moment later (the box binding).
    Streamed,
    /// The placed line, then a 409 conflict in the last line.
    Conflict,
    /// The placed line, then the stream ends.
    Broken,
    /// Plain JSON, unstreamed.
    Plain,
}

/// What the server saw: (path, idempotency-key, accept) per request.
type Seen = Arc<Mutex<Vec<(String, Option<String>, Option<String>)>>>;

struct Server {
    base: String,
    seen: Seen,
}

async fn serve(claims: Vec<Claim>) -> Server {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let base = format!("http://{}", listener.local_addr().unwrap());
    let seen: Seen = Arc::default();
    let claims = Arc::new(Mutex::new(claims));
    let (seen_loop, base_loop) = (seen.clone(), base.clone());
    tokio::spawn(async move {
        while let Ok((stream, _)) = listener.accept().await {
            let seen = seen_loop.clone();
            let claims = claims.clone();
            let base = base_loop.clone();
            tokio::spawn(async move {
                let _ = connection(stream, base, seen, claims).await;
            });
        }
    });
    Server { base, seen }
}

fn detail(base: &str) -> Value {
    json!({
        "sandboxId": "sbx_1", "id": "sbx_1", "name": "demo", "status": "running",
        "created": true, "ready": true, "readiness": "ready", "baseUrl": base,
        "boxToken": "bt"
    })
}

fn conflict() -> Value {
    json!({"error": {
        "code": "conflict", "message": "Sandbox name is already taken",
        "requestId": "req_1", "retryable": false, "retryAfterMs": null,
        "docsUrl": "https://zipbox.ai/docs/errors#conflict"
    }})
}

async fn write_answer(
    stream: &mut TcpStream,
    content_type: &str,
    body: &str,
) -> std::io::Result<()> {
    let head = format!(
        "HTTP/1.1 200 OK\r\ncontent-type: {content_type}\r\ncontent-length: {}\r\n\r\n",
        body.len()
    );
    stream.write_all(head.as_bytes()).await?;
    stream.write_all(body.as_bytes()).await
}

async fn connection(
    stream: TcpStream,
    base: String,
    seen: Seen,
    claims: Arc<Mutex<Vec<Claim>>>,
) -> std::io::Result<()> {
    let mut reader = BufReader::new(stream);
    loop {
        let mut line = String::new();
        if reader.read_line(&mut line).await? == 0 {
            return Ok(());
        }
        let path = line.split_whitespace().nth(1).unwrap_or_default();
        let path = path.to_string();
        let (mut length, mut key, mut accept) = (0usize, None, None);
        loop {
            let mut header = String::new();
            reader.read_line(&mut header).await?;
            let header = header.trim_end();
            if header.is_empty() {
                break;
            }
            if let Some((name, value)) = header.split_once(':') {
                let value = value.trim().to_string();
                match name.to_ascii_lowercase().as_str() {
                    "content-length" => length = value.parse().unwrap_or(0),
                    "idempotency-key" => key = Some(value),
                    "accept" => accept = Some(value),
                    _ => {}
                }
            }
        }
        let mut body = vec![0; length];
        reader.read_exact(&mut body).await?;
        seen.lock().unwrap().push((path.clone(), key, accept));
        let stream = reader.get_mut();
        match path.as_str() {
            "/healthz" => write_answer(stream, "application/json", "{\"ok\":true}").await?,
            "/exec" => write_answer(stream, "text/event-stream", EXEC_EVENTS).await?,
            CLAIM_PATH => {
                let claim = claims.lock().unwrap().remove(0);
                if let Claim::Plain = claim {
                    let body = detail(&base).to_string();
                    write_answer(stream, "application/json", &body).await?;
                    continue;
                }
                let head = "HTTP/1.1 200 OK\r\ncontent-type: application/x-ndjson\r\n\
                    connection: close\r\n\r\n";
                let placed = json!({"type": "placed", "sandboxId": "sbx_1", "baseUrl": base});
                stream.write_all(head.as_bytes()).await?;
                stream.write_all(format!("{placed}\n").as_bytes()).await?;
                stream.flush().await?;
                let (status, body) = match claim {
                    Claim::Streamed => {
                        // While the box binds, nothing may reach its origin.
                        tokio::time::sleep(Duration::from_millis(100)).await;
                        (200, detail(&base))
                    }
                    Claim::Conflict => (409, conflict()),
                    Claim::Broken | Claim::Plain => return Ok(()),
                };
                let last = json!({"type": "answer", "status": status, "headers": {}, "body": body});
                stream.write_all(format!("{last}\n").as_bytes()).await?;
                return Ok(());
            }
            _ => {
                let body = detail(&base).to_string();
                write_answer(stream, "application/json", &body).await?;
            }
        }
    }
}

fn client(base: &str) -> Zipbox {
    Zipbox::builder()
        .api_key("zb_TTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTT")
        .base_url(base)
        .retry_base_delay(Duration::from_millis(1))
        .retry_max_delay(Duration::from_millis(1))
        .poll_interval(Duration::from_millis(1))
        .build()
        .unwrap()
}

fn paths(server: &Server) -> Vec<String> {
    let seen = server.seen.lock().unwrap();
    seen.iter().map(|request| request.0.clone()).collect()
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn create_reads_the_last_line_and_never_dials_the_placed_origin() {
    let server = serve(vec![Claim::Streamed]).await;
    let zb = client(&server.base);

    let sandbox = zb.sandboxes().create(Default::default()).await.unwrap();
    sandbox.exec("true").await.unwrap();

    let seen = server.seen.lock().unwrap().clone();
    let accept = seen[0].2.clone().unwrap_or_default();
    assert!(accept.contains("application/x-ndjson"), "{accept}");
    let order: Vec<&str> = seen.iter().map(|request| request.0.as_str()).collect();
    assert_eq!(order, [CLAIM_PATH, "/exec"]);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn create_reads_an_unstreamed_answer_as_before() {
    let server = serve(vec![Claim::Plain]).await;
    let zb = client(&server.base);

    let sandbox = zb.sandboxes().create(Default::default()).await.unwrap();

    assert_eq!(sandbox.id(), "sbx_1");
    assert_eq!(paths(&server), [CLAIM_PATH]);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn create_raises_the_error_in_the_last_line() {
    let server = serve(vec![Claim::Conflict]).await;
    let zb = client(&server.base);

    let created = zb.sandboxes().create(Default::default()).await;
    let error = created.err().unwrap();

    assert_eq!(error.code, ErrorCode::Conflict);
    assert_eq!(error.status, Some(409));
    assert_eq!(error.request_id.as_deref(), Some("req_1"));
    assert_eq!(paths(&server), [CLAIM_PATH]);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn create_retries_a_stream_that_ends_before_its_answer_with_the_same_key() {
    let server = serve(vec![Claim::Broken, Claim::Plain]).await;
    let zb = client(&server.base);

    let sandbox = zb.sandboxes().create(Default::default()).await.unwrap();

    assert_eq!(sandbox.id(), "sbx_1");
    let seen = server.seen.lock().unwrap().clone();
    let claims: Vec<_> = seen.iter().filter(|r| r.0 == CLAIM_PATH).collect();
    assert_eq!(claims.len(), 2);
    assert_eq!(claims[0].1, claims[1].1);
    assert_eq!(paths(&server), [CLAIM_PATH, CLAIM_PATH]);
}
