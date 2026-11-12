//! PTY: subprotocol credential, resize on attach, reconnect with resume=1 and the
//! same clientId after a drop, takeover on `blocked`, queued input, exit.

mod common;

use std::sync::{Arc, Mutex};
use std::time::Duration;

use futures::{SinkExt, StreamExt};
use serde_json::{json, Value};
use tokio::net::TcpListener;
use tokio_tungstenite::tungstenite::handshake::server::{Request, Response};
use tokio_tungstenite::tungstenite::http::HeaderValue;
use tokio_tungstenite::tungstenite::Message;
use zipbox::{PtyEvent, Zipbox};

use common::MockServer;

const OWNED: &str = r#"{"t":"owned","holder":{"clientId":"me","label":"zipbox-sdk"}}"#;
const API_KEY: &str = "zb_TTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTT";

#[derive(Default, Clone)]
struct Seen {
    uris: Arc<Mutex<Vec<String>>>,
    protocols: Arc<Mutex<Vec<String>>>,
    frames: Arc<Mutex<Vec<Value>>>,
}

// The handshake callback's error type is tungstenite's own `ErrorResponse`.
#[allow(clippy::result_large_err)]
async fn accept(
    listener: &TcpListener,
    seen: &Seen,
) -> tokio_tungstenite::WebSocketStream<tokio::net::TcpStream> {
    let (stream, _) = listener.accept().await.unwrap();
    let seen = seen.clone();
    tokio_tungstenite::accept_hdr_async(stream, move |req: &Request, mut resp: Response| {
        seen.uris.lock().unwrap().push(req.uri().to_string());
        let protocol = req
            .headers()
            .get("sec-websocket-protocol")
            .and_then(|v| v.to_str().ok())
            .unwrap_or("")
            .to_string();
        seen.protocols.lock().unwrap().push(protocol);
        resp.headers_mut().insert(
            "sec-websocket-protocol",
            HeaderValue::from_static("tribes.session"),
        );
        Ok(resp)
    })
    .await
    .unwrap()
}

async fn next_frame(
    ws: &mut tokio_tungstenite::WebSocketStream<tokio::net::TcpStream>,
    seen: &Seen,
) -> Value {
    loop {
        match ws.next().await.unwrap().unwrap() {
            Message::Text(text) => {
                let v: Value = serde_json::from_str(text.as_str()).unwrap();
                seen.frames.lock().unwrap().push(v.clone());
                return v;
            }
            _ => continue,
        }
    }
}

fn query(uri: &str, key: &str) -> Option<String> {
    uri.split_once('?')?
        .1
        .split('&')
        .find_map(|kv| kv.strip_prefix(&format!("{key}=")).map(str::to_string))
}

#[tokio::test(flavor = "multi_thread")]
async fn reconnects_resumes_and_takes_over() {
    let ws_listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let ws_origin = format!(
        "http://127.0.0.1:{}",
        ws_listener.local_addr().unwrap().port()
    );
    let (api, api_listener) = MockServer::bind().await;
    api.state.lock().unwrap().exchanges = vec![json!({
        "request": { "method": "GET", "path": "/sandbox/sbx_1" },
        "response": { "status": 200, "body": {
            "id": "sbx_1", "status": "running", "ready": true,
            "baseUrl": ws_origin, "boxToken": "bt_pty"
        }}
    })];
    api.serve(api_listener);

    let seen = Seen::default();
    let server_seen = seen.clone();
    let server = tokio::spawn(async move {
        // First connection: resize arrives, output goes out, then the socket dies.
        let mut ws = accept(&ws_listener, &server_seen).await;
        ws.send(Message::text(OWNED)).await.unwrap();
        assert_eq!(
            next_frame(&mut ws, &server_seen).await,
            json!({"t":"r","c":80,"r":24})
        );
        ws.send(Message::text(r#"{"t":"o","d":"hi"}"#))
            .await
            .unwrap();
        let _ = next_frame(&mut ws, &server_seen).await; // the "ls" typed before the drop
        drop(ws);
        // Second connection: resumed. The session is held by our ghost: blocked.
        let mut ws = accept(&ws_listener, &server_seen).await;
        ws.send(Message::text(
            r#"{"t":"blocked","holder":{"clientId":"old","label":"x"}}"#,
        ))
        .await
        .unwrap();
        assert_eq!(
            next_frame(&mut ws, &server_seen).await,
            json!({"t":"takeover"})
        );
        // Nothing else may arrive before the box says this socket owns the session.
        ws.send(Message::text(OWNED)).await.unwrap();
        assert_eq!(
            next_frame(&mut ws, &server_seen).await,
            json!({"t":"r","c":80,"r":24})
        );
        assert_eq!(
            next_frame(&mut ws, &server_seen).await,
            json!({"t":"i","d":"pwd\n"})
        );
        ws.send(Message::text(r#"{"t":"x","code":0}"#))
            .await
            .unwrap();
        let _ = ws.next().await;
    });

    let zb = Zipbox::builder()
        .api_key(API_KEY)
        .base_url(&api.base)
        .pty_base_delay(Duration::from_millis(1))
        .build()
        .unwrap();
    let mut pty = zb
        .sandbox("sbx_1")
        .pty()
        .create(zipbox::PtyOptions {
            cols: Some(80),
            rows: Some(24),
            ..Default::default()
        })
        .await
        .unwrap();
    assert!(pty.session_id().starts_with("bash-"));

    let mut events = Vec::new();
    let mut typed_after_drop = false;
    let run = async {
        while let Some(event) = pty.next().await {
            match &event {
                PtyEvent::Output(_) => pty.write("ls\n").unwrap(),
                PtyEvent::Reconnecting { .. } if !typed_after_drop => {
                    typed_after_drop = true;
                    pty.write("pwd\n").unwrap();
                }
                _ => {}
            }
            let done = matches!(event, PtyEvent::Exit(_));
            events.push(event);
            if done {
                break;
            }
        }
    };
    tokio::time::timeout(Duration::from_secs(10), run)
        .await
        .expect("pty finished");
    server.await.unwrap();

    assert_eq!(events.first(), Some(&PtyEvent::Output("hi".into())));
    assert!(events
        .iter()
        .any(|e| matches!(e, PtyEvent::Reconnecting { attempt: 1, .. })));
    assert_eq!(events.last(), Some(&PtyEvent::Exit(Some(0))));

    let uris = seen.uris.lock().unwrap().clone();
    assert_eq!(uris.len(), 2);
    assert_eq!(query(&uris[0], "resume"), None);
    assert_eq!(query(&uris[1], "resume").as_deref(), Some("1"));
    assert_eq!(query(&uris[0], "clientId"), query(&uris[1], "clientId"));
    assert_eq!(query(&uris[0], "session"), query(&uris[1], "session"));
    for protocol in seen.protocols.lock().unwrap().iter() {
        assert_eq!(protocol, "tribes.session, bt_pty");
    }
    assert!(api.finish().is_empty(), "{:?}", api.finish());
}

// A handshake answered 401 re-fetches the token once; a box that the control
// plane then says is gone (404) ends the session instead of redialling forever.
#[tokio::test(flavor = "multi_thread")]
async fn refetches_once_on_401_and_stops_on_an_answered_verdict() {
    let ws_listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let ws_origin = format!(
        "http://127.0.0.1:{}",
        ws_listener.local_addr().unwrap().port()
    );
    let detail = json!({
        "request": { "method": "GET", "path": "/sandbox/sbx_1" },
        "response": { "status": 200, "body": {
            "id": "sbx_1", "status": "running", "ready": true,
            "baseUrl": ws_origin, "boxToken": "bt_pty"
        }}
    });
    let gone = json!({
        "request": { "method": "GET", "path": "/sandbox/sbx_1" },
        "response": { "status": 404, "body": { "error": {
            "code": "not_found", "message": "no such box", "requestId": "r",
            "retryable": false, "docsUrl": "https://zipbox.ai/docs/errors#not_found"
        }}}
    });
    let (api, api_listener) = MockServer::bind().await;
    api.state.lock().unwrap().exchanges = vec![detail.clone(), detail, gone];
    api.serve(api_listener);

    let server = tokio::spawn(async move {
        // 1: refuse the handshake (token not pushed yet).
        reject(&ws_listener).await;
        // 2: accept after the re-fetch, then drop the socket.
        let mut ws = accept(&ws_listener, &Seen::default()).await;
        ws.send(Message::text(OWNED)).await.unwrap();
        drop(ws);
        // 3: the reconnect is refused too; the re-fetch then finds the box gone.
        reject(&ws_listener).await;
    });

    let zb = Zipbox::builder()
        .api_key(API_KEY)
        .base_url(&api.base)
        .pty_base_delay(Duration::from_millis(1))
        .build()
        .unwrap();
    let mut pty = zb
        .sandbox("sbx_1")
        .pty()
        .attach("bash-x", zipbox::PtyOptions::default())
        .await
        .unwrap();
    let mut events = Vec::new();
    tokio::time::timeout(Duration::from_secs(10), async {
        while let Some(event) = pty.next().await {
            events.push(event);
        }
    })
    .await
    .expect("the pty ends on the 404 instead of reconnecting forever");
    server.await.unwrap();
    assert!(
        matches!(events.last(), Some(PtyEvent::Closed(reason)) if reason == "no such box"),
        "{events:?}"
    );
    assert!(api.finish().is_empty(), "{:?}", api.finish());
}

fn ready(ws_origin: &str) -> Value {
    ready_at("/sandbox/sbx_1", ws_origin)
}

// The re-read after a dial nobody answered is the readiness wait.
fn ready_wait(ws_origin: &str) -> Value {
    ready_at("/sandbox/sbx_1/wait?until=ready", ws_origin)
}

fn ready_at(path: &str, ws_origin: &str) -> Value {
    json!({
        "request": { "method": "GET", "path": path },
        "response": { "status": 200, "body": {
            "id": "sbx_1", "status": "running", "ready": true,
            "baseUrl": ws_origin, "boxToken": "bt_pty"
        }}
    })
}

fn client(api: &MockServer) -> Zipbox {
    Zipbox::builder()
        .api_key(API_KEY)
        .base_url(&api.base)
        .pty_base_delay(Duration::from_millis(1))
        .pty_max_delay(Duration::from_millis(20))
        .build()
        .unwrap()
}

async fn events_until_end(pty: &mut zipbox::Pty) -> Vec<PtyEvent> {
    let mut events = Vec::new();
    tokio::time::timeout(Duration::from_secs(10), async {
        while let Some(event) = pty.next().await {
            let end = matches!(
                event,
                PtyEvent::Exit(_) | PtyEvent::Closed(_) | PtyEvent::Taken
            );
            events.push(event);
            if end {
                break;
            }
        }
    })
    .await
    .expect("the pty ends");
    events
}

// AX-07: a box is ready before it answers on its terminal address (its certificate
// can trail its readiness), and a created box's address moves once, to box--<name>.
// Nothing was sent, so the first dial waits on the PTY ladder like a reconnect and
// re-reads the box before each new dial. Here the old address is bound but not
// listening: the connect is refused, and no other test can take the port meanwhile.
#[tokio::test(flavor = "multi_thread")]
async fn the_first_dial_waits_out_a_box_that_does_not_answer_yet() {
    let dead = tokio::net::TcpSocket::new_v4().unwrap();
    dead.bind("127.0.0.1:0".parse().unwrap()).unwrap();
    let dead_origin = format!("http://127.0.0.1:{}", dead.local_addr().unwrap().port());
    let ws_listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let ws_origin = format!(
        "http://127.0.0.1:{}",
        ws_listener.local_addr().unwrap().port()
    );
    let (api, api_listener) = MockServer::bind().await;
    api.state.lock().unwrap().exchanges = vec![
        ready(&dead_origin),
        ready_wait(&dead_origin),
        ready_wait(&ws_origin),
    ];
    api.serve(api_listener);
    let server = tokio::spawn(async move {
        let mut ws = accept(&ws_listener, &Seen::default()).await;
        ws.send(Message::text(r#"{"t":"x","code":0}"#))
            .await
            .unwrap();
        let _ = ws.next().await;
    });

    let zb = client(&api);
    let mut pty = tokio::time::timeout(
        Duration::from_secs(10),
        zb.sandbox("sbx_1")
            .pty()
            .create(zipbox::PtyOptions::default()),
    )
    .await
    .expect("the wait ends once the box answers")
    .expect("the first dial waits instead of failing");
    let events = events_until_end(&mut pty).await;
    assert_eq!(events.last(), Some(&PtyEvent::Exit(Some(0))), "{events:?}");
    server.await.unwrap();
    assert!(api.finish().is_empty(), "{:?}", api.finish());
    drop(dead);
}

// A reconnect whose dial is refused re-reads the box before the next dial, instead
// of redialling the address it cached: a created box's address moves once, to
// box--<name>. Here a read in between caches the old address, now refused.
#[tokio::test(flavor = "multi_thread")]
async fn a_reconnect_rereads_a_box_whose_dial_is_refused() {
    let dead = tokio::net::TcpSocket::new_v4().unwrap();
    dead.bind("127.0.0.1:0".parse().unwrap()).unwrap();
    let dead_origin = format!("http://127.0.0.1:{}", dead.local_addr().unwrap().port());
    let ws_listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let ws_origin = format!(
        "http://127.0.0.1:{}",
        ws_listener.local_addr().unwrap().port()
    );
    let (api, api_listener) = MockServer::bind().await;
    api.state.lock().unwrap().exchanges = vec![
        ready(&ws_origin),
        ready(&dead_origin),
        ready_wait(&ws_origin),
    ];
    api.serve(api_listener);
    let (cached_tx, cached_rx) = tokio::sync::oneshot::channel::<()>();
    let seen = Seen::default();
    let server_seen = seen.clone();
    let server = tokio::spawn(async move {
        let mut ws = accept(&ws_listener, &server_seen).await;
        ws.send(Message::text(r#"{"t":"o","d":"a"}"#))
            .await
            .unwrap();
        cached_rx.await.unwrap();
        drop(ws);
        let mut ws = accept(&ws_listener, &server_seen).await;
        ws.send(Message::text(r#"{"t":"x","code":0}"#))
            .await
            .unwrap();
        let _ = ws.next().await;
    });

    let zb = client(&api);
    let mut pty = zb
        .sandbox("sbx_1")
        .pty()
        .create(zipbox::PtyOptions::default())
        .await
        .unwrap();
    assert_eq!(pty.next().await, Some(PtyEvent::Output("a".into())));
    // The client now caches the refused address.
    zb.sandboxes().get("sbx_1").await.unwrap();
    cached_tx.send(()).unwrap();
    let events = events_until_end(&mut pty).await;
    assert_eq!(events.last(), Some(&PtyEvent::Exit(Some(0))), "{events:?}");
    // The drop, then the refused dial: the ladder grew across the re-read.
    assert!(
        events
            .iter()
            .any(|e| matches!(e, PtyEvent::Reconnecting { attempt: 2, .. })),
        "{events:?}"
    );
    server.await.unwrap();
    let uris = seen.uris.lock().unwrap().clone();
    assert_eq!(uris.len(), 2);
    assert_eq!(query(&uris[1], "resume").as_deref(), Some("1"));
    assert!(api.finish().is_empty(), "{:?}", api.finish());
    drop(dead);
}

// AX-14: a box that answers 401 after the control plane re-read accepted the key has
// not received a key created moments ago. No verdict: the first dial waits for it,
// re-reading the token each time.
#[tokio::test(flavor = "multi_thread")]
async fn the_first_dial_waits_for_the_box_to_receive_a_live_key() {
    let ws_listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let ws_origin = format!(
        "http://127.0.0.1:{}",
        ws_listener.local_addr().unwrap().port()
    );
    let (api, api_listener) = MockServer::bind().await;
    api.state.lock().unwrap().exchanges =
        vec![ready(&ws_origin), ready(&ws_origin), ready(&ws_origin)];
    api.serve(api_listener);
    let server = tokio::spawn(async move {
        reject(&ws_listener).await;
        reject(&ws_listener).await;
        let mut ws = accept(&ws_listener, &Seen::default()).await;
        ws.send(Message::text(r#"{"t":"x","code":0}"#))
            .await
            .unwrap();
        let _ = ws.next().await;
    });

    let zb = client(&api);
    let mut pty = zb
        .sandbox("sbx_1")
        .pty()
        .create(zipbox::PtyOptions::default())
        .await
        .expect("a key the box has not received yet is no verdict");
    let events = events_until_end(&mut pty).await;
    assert_eq!(events.last(), Some(&PtyEvent::Exit(Some(0))), "{events:?}");
    server.await.unwrap();
    assert!(api.finish().is_empty(), "{:?}", api.finish());
}

// AX-14: the same on a reconnect. The session goes on once the box has the key.
#[tokio::test(flavor = "multi_thread")]
async fn a_reconnect_waits_for_the_box_to_receive_a_live_key() {
    let ws_listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let ws_origin = format!(
        "http://127.0.0.1:{}",
        ws_listener.local_addr().unwrap().port()
    );
    let (api, api_listener) = MockServer::bind().await;
    api.state.lock().unwrap().exchanges =
        vec![ready(&ws_origin), ready(&ws_origin), ready(&ws_origin)];
    api.serve(api_listener);
    let server = tokio::spawn(async move {
        let mut ws = accept(&ws_listener, &Seen::default()).await;
        ws.send(Message::text(r#"{"t":"o","d":"a"}"#))
            .await
            .unwrap();
        drop(ws);
        // The cached token, then the re-read one: both refused.
        reject(&ws_listener).await;
        reject(&ws_listener).await;
        let mut ws = accept(&ws_listener, &Seen::default()).await;
        ws.send(Message::text(r#"{"t":"x","code":0}"#))
            .await
            .unwrap();
        let _ = ws.next().await;
    });

    let zb = client(&api);
    let mut pty = zb
        .sandbox("sbx_1")
        .pty()
        .create(zipbox::PtyOptions::default())
        .await
        .unwrap();
    let events = events_until_end(&mut pty).await;
    assert_eq!(events.first(), Some(&PtyEvent::Output("a".into())));
    assert_eq!(events.last(), Some(&PtyEvent::Exit(Some(0))), "{events:?}");
    server.await.unwrap();
    assert!(api.finish().is_empty(), "{:?}", api.finish());
}

// AX-14: a deleted key fails the re-read at the control plane. That is the answered
// verdict, and it ends the session.
#[tokio::test(flavor = "multi_thread")]
async fn a_key_the_control_plane_refuses_ends_the_session() {
    let ws_listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let ws_origin = format!(
        "http://127.0.0.1:{}",
        ws_listener.local_addr().unwrap().port()
    );
    let deleted = json!({
        "request": { "method": "GET", "path": "/sandbox/sbx_1" },
        "response": { "status": 401, "body": { "error": {
            "code": "unauthorized", "message": "Invalid or deleted API key", "requestId": "r",
            "retryable": false, "docsUrl": "https://zipbox.ai/docs/errors#unauthorized"
        }}}
    });
    let (api, api_listener) = MockServer::bind().await;
    api.state.lock().unwrap().exchanges = vec![ready(&ws_origin), deleted];
    api.serve(api_listener);
    let server = tokio::spawn(async move {
        let mut ws = accept(&ws_listener, &Seen::default()).await;
        ws.send(Message::text(r#"{"t":"o","d":"a"}"#))
            .await
            .unwrap();
        drop(ws);
        reject(&ws_listener).await;
    });

    let zb = client(&api);
    let mut pty = zb
        .sandbox("sbx_1")
        .pty()
        .create(zipbox::PtyOptions::default())
        .await
        .unwrap();
    let events = events_until_end(&mut pty).await;
    server.await.unwrap();
    assert!(
        matches!(events.last(), Some(PtyEvent::Closed(reason)) if reason == "Invalid or deleted API key"),
        "{events:?}"
    );
    assert!(api.finish().is_empty(), "{:?}", api.finish());
}

#[allow(clippy::result_large_err)]
async fn reject(listener: &TcpListener) {
    let (stream, _) = listener.accept().await.unwrap();
    let _ = tokio_tungstenite::accept_hdr_async(stream, |_: &Request, _: Response| {
        let mut refused = tokio_tungstenite::tungstenite::handshake::server::ErrorResponse::new(
            Some("unauthorized".to_string()),
        );
        *refused.status_mut() = tokio_tungstenite::tungstenite::http::StatusCode::UNAUTHORIZED;
        Err(refused)
    })
    .await;
}
