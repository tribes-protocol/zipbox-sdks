//! A box call whose request never reached the box re-reads the box and retries
//! where it now answers: a created box's address moves once, to box--<name>.

mod common;

use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use serde_json::{json, Value};
use zipbox::{ErrorCode, Zipbox};

use common::MockServer;

fn detail(base_url: &str) -> Value {
    json!({
        "request": { "method": "GET", "path": "/sandbox/sbx_1" },
        "response": { "status": 200, "body": {
            "id": "sbx_1", "status": "running", "ready": true, "readiness": "ready",
            "baseUrl": base_url, "boxToken": "bt_test"
        }}
    })
}

fn paused() -> Value {
    json!({
        "request": { "method": "GET", "path": "/sandbox/sbx_1" },
        "response": { "status": 200, "body": {
            "id": "sbx_1", "status": "archived", "ready": false, "readiness": "paused"
        }}
    })
}

fn exec() -> Value {
    json!({
        "request": { "method": "POST", "path": "/exec" },
        "response": {
            "status": 200,
            "headers": { "content-type": "text/event-stream" },
            "bodyText": "id: 0-0\nevent: start\ndata: {\"id\":\"c\"}\n\nid: 1-0\nevent: exit\ndata: {\"state\":\"exited\",\"exitCode\":0}\n\n"
        }
    })
}

fn client(api: &MockServer, base_delay: Duration) -> Zipbox {
    Zipbox::builder()
        .api_key("zb_TTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTT")
        .base_url(&api.base)
        .retry_base_delay(base_delay)
        .retry_max_delay(Duration::from_secs(10))
        .poll_interval(Duration::from_millis(1))
        .build()
        .unwrap()
}

// The failure a retired boot route produced in production: the TCP connect
// succeeds and the peer ends the TLS handshake. No byte of the POST left the
// client, so it is unsent: the call re-reads the box, which here is paused, and
// ends with the wait's own verdict. Classified as sent, it would end with
// `unavailable` and never read the box again.
#[tokio::test(flavor = "multi_thread")]
async fn a_tls_handshake_the_box_ends_is_unsent() {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = listener.local_addr().unwrap().port();
    let accepted = Arc::new(AtomicUsize::new(0));
    let counter = accepted.clone();
    tokio::spawn(async move {
        while let Ok((socket, _)) = listener.accept().await {
            counter.fetch_add(1, Ordering::SeqCst);
            drop(socket);
        }
    });
    let (api, api_listener) = MockServer::bind().await;
    api.state.lock().unwrap().exchanges =
        vec![detail(&format!("https://127.0.0.1:{port}")), paused()];
    api.serve(api_listener);

    let err = tokio::time::timeout(
        Duration::from_secs(10),
        client(&api, Duration::from_millis(1))
            .sandbox("sbx_1")
            .exec("true"),
    )
    .await
    .expect("the paused verdict ends the call")
    .unwrap_err();
    assert_eq!(err.code, ErrorCode::BoxNotRunning, "{err:?}");
    assert!(!err.retryable);
    assert!(
        accepted.load(Ordering::SeqCst) >= 1,
        "the connect never landed"
    );
    assert!(api.finish().is_empty(), "{:?}", api.finish());
}

// One ladder per call: each refused dial waits a longer rung, and the box reads
// that succeed in between do not reset it. Four refusals on a 40 ms base wait at
// least 20 + 40 + 80 + 160 ms; a ladder reset by each read would wait at most
// 4 × 40 ms.
#[tokio::test(flavor = "multi_thread")]
async fn the_unsent_ladder_grows_across_rereads() {
    // Bound but not listening: every dial is refused, and no other test can take
    // the port meanwhile.
    let dead = tokio::net::TcpSocket::new_v4().unwrap();
    dead.bind("127.0.0.1:0".parse().unwrap()).unwrap();
    let dead_origin = format!("http://127.0.0.1:{}", dead.local_addr().unwrap().port());
    let (api, api_listener) = MockServer::bind().await;
    let mut exchanges: Vec<Value> = (0..4).map(|_| detail(&dead_origin)).collect();
    exchanges.push(detail(&api.base));
    exchanges.push(exec());
    api.state.lock().unwrap().exchanges = exchanges;
    api.serve(api_listener);

    let started = Instant::now();
    let result = tokio::time::timeout(
        Duration::from_secs(10),
        client(&api, Duration::from_millis(40))
            .sandbox("sbx_1")
            .exec("true"),
    )
    .await
    .expect("the call ends once the box answers")
    .unwrap();
    let elapsed = started.elapsed();
    assert_eq!(result.exit_code, Some(0));
    assert!(
        elapsed >= Duration::from_millis(300),
        "the ladder was reset: {elapsed:?}"
    );
    assert!(api.finish().is_empty(), "{:?}", api.finish());
    drop(dead);
}
