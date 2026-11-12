//! Pre-connect on resume (sdks/conformance/README.md, "Pre-connect on resume"): the
//! box's last known origin is dialled in parallel with POST /resume on the client's
//! pool, so the first box call after it reuses the connection. Never waited for,
//! never retried; a different origin in the answer or a failure drops it.

use std::sync::{Arc, Mutex};
use std::time::Duration;

use serde_json::{json, Value};
use tokio::io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::Notify;
use zipbox::Zipbox;

const EXEC_EVENTS: &str = "event: start\ndata: {\"id\":\"cmd_1\"}\n\n\
    event: exit\ndata: {\"state\":\"exited\",\"exitCode\":0,\"signal\":null,\"timedOut\":false}\n\n";

#[derive(Default)]
struct Seen {
    // (peer address, method, path, authorization)
    requests: Vec<(String, String, String, Option<String>)>,
    connections: usize,
}

type Handler = Arc<dyn Fn(&str, &str) -> Option<(u16, &'static str, String)> + Send + Sync>;

/// A keep-alive HTTP/1.1 server that records every request with its connection.
struct Server {
    base: String,
    seen: Arc<Mutex<Seen>>,
    healthz: Arc<Notify>,
}

async fn serve(handler: Handler) -> Server {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let base = format!("http://{}", listener.local_addr().unwrap());
    let seen = Arc::new(Mutex::new(Seen::default()));
    let healthz = Arc::new(Notify::new());
    let (seen_loop, healthz_loop) = (seen.clone(), healthz.clone());
    tokio::spawn(async move {
        while let Ok((stream, peer)) = listener.accept().await {
            seen_loop.lock().unwrap().connections += 1;
            let (seen, healthz, handler) =
                (seen_loop.clone(), healthz_loop.clone(), handler.clone());
            tokio::spawn(async move {
                let _ = connection(stream, peer.to_string(), seen, healthz, handler).await;
            });
        }
    });
    Server {
        base,
        seen,
        healthz,
    }
}

async fn connection(
    stream: TcpStream,
    peer: String,
    seen: Arc<Mutex<Seen>>,
    healthz: Arc<Notify>,
    handler: Handler,
) -> std::io::Result<()> {
    let mut reader = BufReader::new(stream);
    loop {
        let mut line = String::new();
        if reader.read_line(&mut line).await? == 0 {
            return Ok(());
        }
        let mut parts = line.split_whitespace();
        let method = parts.next().unwrap_or_default().to_string();
        let path = parts.next().unwrap_or_default().to_string();
        let (mut length, mut authorization) = (0usize, None);
        loop {
            let mut header = String::new();
            reader.read_line(&mut header).await?;
            let header = header.trim_end();
            if header.is_empty() {
                break;
            }
            if let Some((name, value)) = header.split_once(':') {
                match name.to_ascii_lowercase().as_str() {
                    "content-length" => length = value.trim().parse().unwrap_or(0),
                    "authorization" => authorization = Some(value.trim().to_string()),
                    _ => {}
                }
            }
        }
        let mut body = vec![0; length];
        reader.read_exact(&mut body).await?;
        seen.lock().unwrap().requests.push((
            peer.clone(),
            method.clone(),
            path.clone(),
            authorization,
        ));
        if path == "/healthz" {
            healthz.notify_one();
        }
        let Some((status, content_type, payload)) = handler(&method, &path) else {
            // A failed pre-connect: the connection drops with no answer.
            return Ok(());
        };
        let response = format!(
            "HTTP/1.1 {status} OK\r\ncontent-type: {content_type}\r\ncontent-length: {}\r\n\r\n{payload}",
            payload.len()
        );
        reader.get_mut().write_all(response.as_bytes()).await?;
    }
}

fn detail(base_url: Option<&str>) -> Value {
    match base_url {
        Some(base) => json!({
            "id": "sbx_1", "name": "demo", "status": "running", "ready": true,
            "readiness": "ready", "baseUrl": base, "boxToken": "bt"
        }),
        None => json!({
            "id": "sbx_1", "name": "demo", "status": "paused", "ready": false,
            "readiness": "paused", "baseUrl": null, "boxToken": null
        }),
    }
}

/// A box origin: its health route (or a dropped connection when `fail`) and /exec.
async fn box_server(fail: bool) -> Server {
    serve(Arc::new(move |_method: &str, path: &str| match path {
        "/healthz" if fail => None,
        "/healthz" => Some((200, "application/json", "{\"ok\":true}".to_string())),
        "/exec" => Some((200, "text/event-stream", EXEC_EVENTS.to_string())),
        _ => Some((404, "text/plain", String::new())),
    }))
    .await
}

/// The control plane: the box ready on `first` until a resume, which answers ready on
/// `resumed` only once `preconnect_box` has seen the pre-connect (or after 5 s), so a
/// pre-connect sent after the resume fails the test instead of passing it. The wait
/// blocks this request's worker: the tests run on the multi-thread runtime.
async fn api_server(first: String, resumed: String, preconnect_box: Arc<Mutex<Seen>>) -> Server {
    let handler: Handler = Arc::new(move |_method: &str, path: &str| {
        let body = match path {
            "/sandbox/sbx_1/pause" => detail(None),
            "/sandbox/sbx_1/resume" => {
                for _ in 0..500 {
                    if preconnect_box
                        .lock()
                        .unwrap()
                        .requests
                        .iter()
                        .any(|r| r.2 == "/healthz")
                    {
                        break;
                    }
                    std::thread::sleep(Duration::from_millis(10));
                }
                detail(Some(&resumed))
            }
            _ => detail(Some(&first)),
        };
        Some((200, "application/json", body.to_string()))
    });
    serve(handler).await
}

fn client(base: &str) -> Zipbox {
    Zipbox::builder()
        .api_key("zb_TTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTT")
        .base_url(base)
        .retry_base_delay(Duration::from_millis(1))
        .poll_interval(Duration::from_millis(1))
        .build()
        .unwrap()
}

/// The healthz request reached the box before (or while) the resume was answered.
async fn preconnected(server: &Server) {
    tokio::time::timeout(Duration::from_secs(5), async {
        loop {
            if server
                .seen
                .lock()
                .unwrap()
                .requests
                .iter()
                .any(|r| r.2 == "/healthz")
            {
                return;
            }
            server.healthz.notified().await;
        }
    })
    .await
    .expect("no pre-connect");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn resume_preconnects_and_the_first_call_reuses_the_connection() {
    let boxed = box_server(false).await;
    let api = api_server(boxed.base.clone(), boxed.base.clone(), boxed.seen.clone()).await;
    let zb = client(&api.base);
    let mut sandbox = zb.sandboxes().get("sbx_1").await.unwrap();
    sandbox.pause().await.unwrap();
    sandbox.resume().await.unwrap();
    preconnected(&boxed).await;
    // Let the pre-connect's answer be read and its connection go back to the pool.
    tokio::time::sleep(Duration::from_millis(50)).await;
    let out = sandbox.exec("true").await.unwrap();
    assert_eq!(out.exit_code, Some(0));
    let seen = boxed.seen.lock().unwrap();
    let healthz = seen.requests.iter().find(|r| r.2 == "/healthz").unwrap();
    let exec = seen.requests.iter().find(|r| r.2 == "/exec").unwrap();
    assert_eq!(healthz.3, None, "the pre-connect carries no token");
    assert_eq!(exec.3.as_deref(), Some("Bearer bt"));
    assert_eq!(seen.connections, 1, "exec opened a new connection");
    assert_eq!(exec.0, healthz.0);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn start_preconnects_like_resume() {
    let boxed = box_server(false).await;
    let api = api_server(boxed.base.clone(), boxed.base.clone(), boxed.seen.clone()).await;
    let zb = client(&api.base);
    zb.sandboxes().get("sbx_1").await.unwrap();
    zb.sandboxes().start("sbx_1").await.unwrap();
    preconnected(&boxed).await;
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_moved_origin_is_reached_the_normal_way() {
    let boxed = box_server(false).await;
    let moved = box_server(false).await;
    let api = api_server(boxed.base.clone(), moved.base.clone(), boxed.seen.clone()).await;
    let zb = client(&api.base);
    let mut sandbox = zb.sandboxes().get("sbx_1").await.unwrap();
    sandbox.resume().await.unwrap();
    sandbox.exec("true").await.unwrap();
    let moved_seen = moved.seen.lock().unwrap();
    assert!(moved_seen.requests.iter().any(|r| r.2 == "/exec"));
    assert!(boxed
        .seen
        .lock()
        .unwrap()
        .requests
        .iter()
        .all(|r| r.2 != "/exec"));
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_failed_preconnect_changes_nothing() {
    let boxed = box_server(true).await;
    let api = api_server(boxed.base.clone(), boxed.base.clone(), boxed.seen.clone()).await;
    let zb = client(&api.base);
    let mut sandbox = zb.sandboxes().get("sbx_1").await.unwrap();
    sandbox.resume().await.unwrap();
    preconnected(&boxed).await;
    let out = sandbox.exec("true").await.unwrap();
    assert_eq!(out.exit_code, Some(0));
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_client_that_never_saw_the_box_ready_sends_none() {
    let boxed = box_server(false).await;
    let api = api_server(boxed.base.clone(), boxed.base.clone(), boxed.seen.clone()).await;
    let zb = client(&api.base);
    zb.sandboxes().resume("sbx_1").await.unwrap();
    tokio::time::sleep(Duration::from_millis(50)).await;
    assert!(boxed.seen.lock().unwrap().requests.is_empty());
}
