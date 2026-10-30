//! Lifecycle edge cases the shared fixtures do not pin.

mod common;

use std::time::Duration;

use serde_json::json;
use zipbox::{CreateOptions, ErrorCode, Zipbox};

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

fn private_detail(base_url: &str) -> serde_json::Value {
    json!({
        "id": "sbx_1", "name": "build", "status": "running", "privateMode": true,
        "agentPublicKey": "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAENj+qimupfFq/bb5ok8+RD0Bh7Opby1hPK28tSUvvLn58j9QLIPm1e1mfo7z5H/kbmtn7lKcNZp7cT+1Lf5Enog==",
        "ready": true, "baseUrl": base_url, "boxToken": "bt_test"
    })
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

// A private box's name resolves to its tailnet address; off the tailnet a box call
// must fail with a clear error instead of hanging.
#[tokio::test]
async fn a_private_box_off_the_tailnet_fails_fast() {
    // Port 1 (tcpmux): privileged, so no test server in this run can be listening on
    // it, and the connect is refused. A port freed with `bind(0)` + `drop` is NOT
    // safe: the OS hands it straight back to the next `bind(0)`, so a mock server in
    // a concurrent test took it and answered this box's calls (CI, 2026-10-29).
    let (api, listener) = MockServer::bind().await;
    api.state.lock().unwrap().exchanges = vec![json!({
        "request": { "method": "GET", "path": "/sandbox/sbx_1" },
        "response": { "status": 200, "body": private_detail("http://127.0.0.1:1") }
    })];
    api.serve(listener);
    let err = tokio::time::timeout(
        Duration::from_secs(10),
        client(&api.base)
            .sandbox("sbx_1")
            .files()
            .list("/root/workspace"),
    )
    .await
    .expect("fails within the probe deadline")
    .unwrap_err();
    assert_eq!(err.code, ErrorCode::BoxPrivateUnreachable);
    assert!(!err.retryable);
    assert_eq!(
        err.message,
        "build is private: it can only be reached from devices on your tailnet. \
         Turn on Tailscale on this device (https://tailscale.com/download)."
    );
    assert!(api.finish().is_empty(), "{:?}", api.finish());
}

// Any HTTP answer from /challenge, even a 404, means the box is reachable, and the
// probe is not repeated for the next call.
#[tokio::test]
async fn a_reachable_private_box_is_probed_once() {
    let (api, listener) = MockServer::bind().await;
    let listing = json!({
        "request": { "method": "GET", "path": "/files?path=%2Froot%2Fworkspace" },
        "response": { "status": 200, "body": { "path": "/root/workspace", "entries": [] } }
    });
    api.state.lock().unwrap().exchanges = vec![
        json!({
            "request": { "method": "GET", "path": "/sandbox/sbx_1" },
            "response": { "status": 200, "body": private_detail(&api.base) }
        }),
        json!({
            "request": { "method": "GET", "path": "/challenge" },
            "response": { "status": 404, "body": { "error": "not found" } }
        }),
        listing.clone(),
        listing,
    ];
    api.serve(listener);
    let sandbox = client(&api.base).sandbox("sbx_1");
    assert!(sandbox
        .files()
        .list("/root/workspace")
        .await
        .unwrap()
        .is_empty());
    assert!(sandbox
        .files()
        .list("/root/workspace")
        .await
        .unwrap()
        .is_empty());
    assert!(api.finish().is_empty(), "{:?}", api.finish());
}

fn private_row(ready: bool, ipv4: Option<&str>) -> serde_json::Value {
    let tailscale = ipv4.map(|ip| json!({ "state": "connected", "ipv4": ip, "pending": false }));
    let base_url = if ready {
        json!("https://box--build.zbox.sh")
    } else {
        json!(null)
    };
    let token = if ready { json!("bt_test") } else { json!(null) };
    json!({
        "id": "sbx_1", "name": "build", "status": "running", "privateMode": true,
        "agentPublicKey": "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAENj+qimupfFq/bb5ok8+RD0Bh7Opby1hPK28tSUvvLn58j9QLIPm1e1mfo7z5H/kbmtn7lKcNZp7cT+1Lf5Enog==",
        "tailscale": tailscale, "ready": ready,
        "baseUrl": base_url,
        "boxToken": token
    })
}

// A private box that has not joined a tailnet cannot become ready; waiting for it
// would poll forever. It is usable (an auth key can be sealed to it) once running.
#[tokio::test]
async fn get_or_create_returns_an_unjoined_private_box_once_running_with_its_key() {
    let (api, listener) = MockServer::bind().await;
    api.state.lock().unwrap().exchanges = vec![
        json!({
            "request": { "method": "GET", "path": "/sandbox" },
            "response": { "status": 200, "body": [private_row(false, None)] }
        }),
        json!({
            "request": { "method": "GET", "path": "/sandbox/sbx_1" },
            "response": { "status": 200, "body": private_row(false, None) }
        }),
    ];
    api.serve(listener);
    let sandbox = tokio::time::timeout(
        Duration::from_secs(5),
        client(&api.base).sandboxes().get_or_create("build"),
    )
    .await
    .expect("returns without ready")
    .unwrap();
    let info = sandbox.info().unwrap();
    assert!(!info.ready && info.private_mode);
    assert!(api.finish().is_empty(), "{:?}", api.finish());
}

// A private box that has joined is reachable over its tailnet and waits for ready.
#[tokio::test]
async fn get_or_create_waits_for_ready_on_a_joined_private_box() {
    let (api, listener) = MockServer::bind().await;
    api.state.lock().unwrap().exchanges = vec![
        json!({
            "request": { "method": "GET", "path": "/sandbox" },
            "response": { "status": 200, "body": [private_row(false, Some("100.101.32.7"))] }
        }),
        json!({
            "request": { "method": "GET", "path": "/sandbox/sbx_1" },
            "response": { "status": 200, "body": private_row(false, Some("100.101.32.7")) }
        }),
        json!({
            "request": { "method": "GET", "path": "/sandbox/sbx_1" },
            "response": { "status": 200, "body": private_row(true, Some("100.101.32.7")) }
        }),
    ];
    api.serve(listener);
    let sandbox = tokio::time::timeout(
        Duration::from_secs(5),
        client(&api.base).sandboxes().get_or_create("build"),
    )
    .await
    .expect("returns once ready")
    .unwrap();
    assert!(sandbox.info().unwrap().ready);
    assert!(api.finish().is_empty(), "{:?}", api.finish());
}

// get_or_create_with creates a missing box with every create option on the claim.
#[tokio::test]
async fn get_or_create_with_sends_every_create_option_on_the_claim() {
    let (api, listener) = MockServer::bind().await;
    api.state.lock().unwrap().exchanges = vec![
        json!({
            "request": { "method": "GET", "path": "/sandbox" },
            "response": { "status": 200, "body": [] }
        }),
        json!({
            "request": {
                "method": "POST",
                "path": "/sandbox/identity/claim",
                "body": {
                    "name": "build", "tier": "l", "sshPublicKey": "ssh-ed25519 AAAAtest",
                    "mailbox": true, "private": true
                }
            },
            "response": { "status": 200, "body": { "sandboxId": "sbx_1", "created": true } }
        }),
        json!({
            "request": { "method": "GET", "path": "/sandbox/sbx_1" },
            "response": { "status": 200, "body": private_row(false, None) }
        }),
    ];
    api.serve(listener);
    let options = CreateOptions {
        tier: Some("l".into()),
        ssh_public_key: Some("ssh-ed25519 AAAAtest".into()),
        mailbox: Some(true),
        private: Some(true),
        ..CreateOptions::default()
    };
    let sandbox = tokio::time::timeout(
        Duration::from_secs(5),
        client(&api.base)
            .sandboxes()
            .get_or_create_with("build", options),
    )
    .await
    .expect("returns once running with its key")
    .unwrap();
    assert_eq!(sandbox.id(), "sbx_1");
    assert!(api.finish().is_empty(), "{:?}", api.finish());
}

// The options apply only on create: a box that already has the name is returned
// as it is, with no claim and no tier change.
#[tokio::test]
async fn get_or_create_with_returns_an_existing_box_as_it_is() {
    let row = json!({
        "id": "sbx_1", "name": "build", "status": "running", "tier": "s",
        "ready": true, "baseUrl": "https://box--build.zbox.sh", "boxToken": "bt_test"
    });
    let (api, listener) = MockServer::bind().await;
    api.state.lock().unwrap().exchanges = vec![
        json!({
            "request": { "method": "GET", "path": "/sandbox" },
            "response": { "status": 200, "body": [row.clone()] }
        }),
        json!({
            "request": { "method": "GET", "path": "/sandbox/sbx_1" },
            "response": { "status": 200, "body": row }
        }),
    ];
    api.serve(listener);
    let options = CreateOptions {
        tier: Some("l".into()),
        ..CreateOptions::default()
    };
    let sandbox = client(&api.base)
        .sandboxes()
        .get_or_create_with("build", options)
        .await
        .unwrap();
    assert_eq!(sandbox.info().unwrap().tier.as_deref(), Some("s"));
    assert!(api.finish().is_empty(), "{:?}", api.finish());
}

// A name in the options that disagrees with the name looked up is a caller bug.
#[tokio::test]
async fn get_or_create_with_rejects_a_different_options_name() {
    let options = CreateOptions {
        name: Some("other".into()),
        ..CreateOptions::default()
    };
    let err = client("http://127.0.0.1:1")
        .sandboxes()
        .get_or_create_with("build", options)
        .await
        .unwrap_err();
    assert_eq!(err.code, ErrorCode::InvalidRequest);
}
