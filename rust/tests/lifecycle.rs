//! Lifecycle edge cases the shared fixtures do not pin.

mod common;

use std::time::Duration;

use serde_json::json;
use zipbox::{ErrorCode, Zipbox};

use common::MockServer;

// A legacy `stopped` row is dead: waiting for it to become ready would poll forever.
#[tokio::test]
async fn wait_ready_gives_up_on_a_dead_box() {
    let (api, listener) = MockServer::bind().await;
    api.state.lock().unwrap().exchanges = vec![json!({
        "request": { "method": "GET", "path": "/sandbox/sbx_1" },
        "response": { "status": 200, "body": { "id": "sbx_1", "status": "stopped", "ready": false } }
    })];
    api.serve(listener);
    let zb = Zipbox::builder()
        .api_key("zb_TTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTT")
        .base_url(&api.base)
        .poll_interval(Duration::from_millis(1))
        .build()
        .unwrap();
    let err = tokio::time::timeout(Duration::from_secs(5), zb.sandboxes().wait_ready("sbx_1"))
        .await
        .expect("wait_ready returns")
        .unwrap_err();
    assert_eq!(err.code, ErrorCode::Conflict);
    assert!(api.finish().is_empty(), "{:?}", api.finish());
}
