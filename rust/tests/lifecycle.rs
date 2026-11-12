//! Lifecycle edge cases the shared fixtures do not pin.

mod common;

use std::time::Duration;

use serde_json::json;
use zipbox::{CreateOptions, ErrorCode, ForkOptions, ForkSourcePolicy, Zipbox};

use common::MockServer;

// A destroyed row without readiness is dead: waiting for it to become ready would
// wait forever.
#[tokio::test]
async fn wait_ready_gives_up_on_a_dead_box() {
    let (api, listener) = MockServer::bind().await;
    api.state.lock().unwrap().exchanges = vec![json!({
        "request": { "method": "GET", "path": "/sandbox/sbx_1/wait?until=ready" },
        "response": { "status": 200, "body": { "id": "sbx_1", "status": "destroyed", "ready": false } }
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

const WAIT: &str = "/sandbox/sbx_1/wait?until=ready";

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
            "request": { "method": "GET", "path": WAIT },
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
            "request": { "method": "GET", "path": WAIT },
            "response": { "status": 200, "body": private_row(false, Some("100.101.32.7")) }
        }),
        json!({
            "request": { "method": "GET", "path": WAIT },
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
            "request": { "method": "GET", "path": WAIT },
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
            "request": { "method": "GET", "path": WAIT },
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

// A caller's deadline (dropping the future) stops only its own wait: the POST is the
// only write, nothing cancels or deletes the new box, and nothing is sent afterwards.
#[tokio::test]
async fn a_dropped_fork_wait_leaves_the_fork_running() {
    let (api, listener) = MockServer::bind().await;
    let accepted = json!({
        "operationId": "fork_1",
        "sandbox": { "id": "sbx_2", "name": "demo-2", "status": "provisioning" },
        "fork": {
            "operationId": "fork_1", "sourceSandboxId": "sbx_1", "childSandboxId": "sbx_2",
            "sourcePolicy": "require_paused", "phase": "queued", "sourceRecovery": "not_needed",
            "attempt": 0, "createdAt": "2026-11-05T00:00:00Z", "updatedAt": "2026-11-05T00:00:00Z"
        }
    });
    let mut exchanges = vec![json!({
        "request": { "method": "POST", "path": "/sandbox/sbx_1/fork" },
        "response": { "status": 202, "body": accepted }
    })];
    // An API without /wait, so the wait polls on the flat ladder below.
    exchanges.push(json!({
        "request": { "method": "GET", "path": "/sandbox/sbx_2/wait?until=ready" },
        "response": { "status": 404, "body": { "error": "Route not found" } }
    }));
    // The new box keeps provisioning for longer than the caller waits.
    exchanges.extend((0..10_000).map(|_| {
        json!({
            "request": { "method": "GET", "path": "/sandbox/sbx_2" },
            "response": { "status": 200, "body": { "id": "sbx_2", "name": "demo-2", "status": "provisioning", "ready": false } }
        })
    }));
    api.state.lock().unwrap().exchanges = exchanges;
    api.serve(listener);
    // A flat 1-2 ms poll, so a wait that went on after the deadline would show.
    let zb = Zipbox::builder()
        .api_key("zb_TTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTT")
        .base_url(&api.base)
        .poll_interval(Duration::from_millis(1))
        .poll_max_interval(Duration::from_millis(2))
        .build()
        .unwrap();
    let waited = tokio::time::timeout(
        Duration::from_millis(200),
        zb.sandboxes().fork(
            "sbx_1",
            ForkOptions::new("demo-2", ForkSourcePolicy::RequirePaused),
        ),
    )
    .await;
    assert!(waited.is_err(), "the deadline ends the wait");
    let served = api.state.lock().unwrap().next;
    tokio::time::sleep(Duration::from_millis(300)).await;
    let state = api.state.lock().unwrap();
    assert!(served >= 2, "the wait read the new box: {served}");
    // At most the one read already in flight when the deadline fired: no poll goes on.
    assert!(
        state.next <= served + 1,
        "sent after the wait ended: {} > {served}",
        state.next
    );
    // Every request past the POST matched a GET of the new box: no cancel, no delete.
    assert!(state.failures.is_empty(), "{:?}", state.failures);
}

fn ready_row(base: &str) -> serde_json::Value {
    json!({
        "id": "sbx_1", "status": "running", "ready": true, "readiness": "ready",
        "baseUrl": base, "boxToken": "bt_test"
    })
}

fn exchange(method: &str, path: &str, body: serde_json::Value) -> serde_json::Value {
    json!({
        "request": { "method": method, "path": path },
        "response": { "status": 200, "body": body }
    })
}

fn exec_exchange() -> serde_json::Value {
    json!({
        "request": { "method": "POST", "path": "/exec" },
        "response": {
            "status": 200,
            "headers": { "content-type": "text/event-stream" },
            "bodyText": "id: 0-0\nevent: start\ndata: {\"id\":\"c\"}\n\nid: 1-0\nevent: exit\ndata: {\"state\":\"exited\",\"exitCode\":0}\n\n"
        }
    })
}

// audit-ax AX-10: a read that shows the box not ready, and every lifecycle call,
// drop its cached access, so the next box call never dials the old address.
#[tokio::test]
async fn not_ready_reads_and_lifecycle_calls_drop_cached_access() {
    let (api, listener) = MockServer::bind().await;
    let paused = json!({
        "id": "sbx_1", "status": "paused", "ready": false, "readiness": "paused"
    });
    let mut exchanges = Vec::new();
    for (method, path) in [
        ("GET", "/sandbox/sbx_1"),
        ("POST", "/sandbox/sbx_1/pause"),
        ("POST", "/sandbox/sbx_1/reset"),
        ("POST", "/sandbox/sbx_1/tier"),
    ] {
        exchanges.push(exchange("GET", "/sandbox/sbx_1", ready_row(&api.base)));
        exchanges.push(exec_exchange());
        let answer = if method == "GET" || path.ends_with("/pause") {
            paused.clone()
        } else {
            json!({})
        };
        exchanges.push(exchange(method, path, answer));
        // The next box call reads the box afresh, finds it not ready, and stops.
        exchanges.push(exchange("GET", "/sandbox/sbx_1", paused.clone()));
    }
    api.state.lock().unwrap().exchanges = exchanges;
    api.serve(listener);
    let zb = client(&api.base);
    let mut sandbox = zb.sandbox("sbx_1");
    for step in ["refresh", "pause", "restart", "set_tier"] {
        sandbox.exec("true").await.unwrap();
        match step {
            "refresh" => sandbox.refresh().await.map(|_| ()).unwrap(),
            "pause" => sandbox.pause().await.map(|_| ()).unwrap(),
            "restart" => sandbox.restart().await.unwrap(),
            _ => sandbox.set_tier("m").await.unwrap(),
        }
        let err = sandbox.exec("true").await.unwrap_err();
        assert_eq!(err.code, ErrorCode::BoxNotRunning, "{step}");
        assert!(
            !err.retryable,
            "{step}: a paused box does not come back by itself"
        );
    }
    assert!(api.finish().is_empty(), "{:?}", api.finish());
}

// audit-ax AX-23: a follower's ladder never retries sooner than the server asked.
#[tokio::test]
async fn a_followed_command_waits_out_retry_after() {
    use futures::StreamExt;

    let (api, listener) = MockServer::bind().await;
    api.state.lock().unwrap().exchanges = vec![
        exchange("GET", "/sandbox/sbx_1", ready_row(&api.base)),
        json!({
            "request": { "method": "GET", "path": "/exec/cmd_1/events" },
            "response": {
                "status": 200,
                "headers": { "content-type": "text/event-stream" },
                "bodyText": "id: 4-0\nevent: stdout\ndata: {\"data\":\"one\"}\n\n"
            }
        }),
        json!({
            "request": { "method": "GET", "path": "/exec/cmd_1/events?since=4-0" },
            "response": {
                "status": 503,
                "headers": { "retry-after": "1" },
                "body": { "error": "busy" }
            }
        }),
        json!({
            "request": { "method": "GET", "path": "/exec/cmd_1/events?since=4-0" },
            "response": {
                "status": 200,
                "headers": { "content-type": "text/event-stream" },
                "bodyText": "id: 8-0\nevent: exit\ndata: {\"state\":\"exited\",\"exitCode\":0}\n\n"
            }
        }),
    ];
    api.serve(listener);
    let zb = Zipbox::builder()
        .api_key("zb_TTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTT")
        .base_url(&api.base)
        .max_retries(0)
        .retry_base_delay(Duration::from_millis(1))
        .build()
        .unwrap();
    let started = std::time::Instant::now();
    let events: Vec<_> = zb
        .sandbox("sbx_1")
        .commands()
        .events("cmd_1", None)
        .await
        .unwrap()
        .collect()
        .await;
    assert_eq!(events.len(), 2);
    assert!(
        started.elapsed() >= Duration::from_millis(900),
        "re-attached {:?} after a Retry-After of 1s",
        started.elapsed()
    );
    assert!(api.finish().is_empty(), "{:?}", api.finish());
}

// audit-ax AX-17: one rule in every SDK. A code this SDK does not know maps by status
// (here a 409 is a conflict, not a retried `unavailable`).
#[tokio::test]
async fn an_unknown_code_maps_by_status() {
    let (api, listener) = MockServer::bind().await;
    api.state.lock().unwrap().exchanges = vec![json!({
        "request": { "method": "GET", "path": "/sandbox" },
        "response": {
            "status": 409,
            "body": { "error": { "code": "a_code_from_later", "message": "no" } }
        }
    })];
    api.serve(listener);
    let err = client(&api.base).sandboxes().list().await.unwrap_err();
    assert_eq!(err.code, ErrorCode::Conflict);
    assert!(!err.retryable);
    assert!(api.finish().is_empty(), "{:?}", api.finish());
}

fn answer(method: &str, path: &str, status: u16, body: serde_json::Value) -> serde_json::Value {
    json!({
        "request": { "method": method, "path": path },
        "response": { "status": status, "body": body }
    })
}

fn settling_row() -> serde_json::Value {
    json!({ "id": "sbx_1", "status": "provisioning", "ready": false, "readiness": "settling" })
}

// The server holds /wait until the box gets there or its hold limit passes, so a
// settling answer is asked again at once: no poll rung in between. A 1 s poll base
// would put three settling answers well past 2 s if they slept.
#[tokio::test]
async fn a_settling_wait_answer_is_asked_again_at_once() {
    let (api, listener) = MockServer::bind().await;
    api.state.lock().unwrap().exchanges = vec![
        answer("GET", WAIT, 200, settling_row()),
        answer("GET", WAIT, 200, settling_row()),
        answer("GET", WAIT, 200, settling_row()),
        answer("GET", WAIT, 200, ready_row(&api.base)),
    ];
    api.serve(listener);
    let zb = Zipbox::builder()
        .api_key("zb_TTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTT")
        .base_url(&api.base)
        .poll_interval(Duration::from_secs(1))
        .build()
        .unwrap();
    let started = std::time::Instant::now();
    let sandbox = zb.sandboxes().wait_ready("sbx_1").await.unwrap();
    assert!(sandbox.info().unwrap().ready);
    assert!(
        started.elapsed() < Duration::from_millis(900),
        "slept between held answers: {:?}",
        started.elapsed()
    );
    assert!(api.finish().is_empty(), "{:?}", api.finish());
}

// An API that predates /wait answers it 404 on a box that exists: the wait reads
// GET /sandbox/:id and, finding the box, polls it on the poll ladder (the old wait).
#[tokio::test]
async fn a_wait_falls_back_to_polling_on_an_api_without_wait() {
    let (api, listener) = MockServer::bind().await;
    api.state.lock().unwrap().exchanges = vec![
        answer("GET", WAIT, 404, json!({ "error": "Route not found" })),
        answer("GET", "/sandbox/sbx_1", 200, settling_row()),
        answer("GET", "/sandbox/sbx_1", 200, settling_row()),
        answer("GET", "/sandbox/sbx_1", 200, ready_row(&api.base)),
    ];
    api.serve(listener);
    let zb = Zipbox::builder()
        .api_key("zb_TTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTT")
        .base_url(&api.base)
        .poll_interval(Duration::from_millis(40))
        .build()
        .unwrap();
    let started = std::time::Instant::now();
    let sandbox = zb.sandboxes().wait_ready("sbx_1").await.unwrap();
    assert!(sandbox.info().unwrap().ready);
    // Two poll rungs (40 ms, then 50 ms, each jittered no lower than half).
    assert!(
        started.elapsed() >= Duration::from_millis(40),
        "the fallback did not sleep between reads: {:?}",
        started.elapsed()
    );
    assert!(api.finish().is_empty(), "{:?}", api.finish());
}

// A create the server answered with the box already bound returns at once, and box
// calls use the token in that answer: no wait and no detail read follow.
#[tokio::test]
async fn a_ready_claim_returns_at_once_with_its_token() {
    let (api, listener) = MockServer::bind().await;
    api.state.lock().unwrap().exchanges = vec![
        answer(
            "POST",
            "/sandbox/identity/claim",
            200,
            json!({
                "sandboxId": "sbx_1", "name": "demo", "status": "running", "created": true,
                "ready": true, "readiness": "ready", "httpsReady": true,
                "baseUrl": api.base, "boxToken": "bt_claim",
                "phase": "running", "allowedActions": ["pause", "restart", "delete", "fork"]
            }),
        ),
        json!({
            "request": {
                "method": "POST", "path": "/exec",
                "headers": { "authorization": "Bearer bt_claim" }
            },
            "response": {
                "status": 200,
                "headers": { "content-type": "text/event-stream" },
                "bodyText": "id: 0-0\nevent: start\ndata: {\"id\":\"c\"}\n\nid: 1-0\nevent: exit\ndata: {\"state\":\"exited\",\"exitCode\":0}\n\n"
            }
        }),
    ];
    api.serve(listener);
    let sandbox = client(&api.base)
        .sandboxes()
        .create(CreateOptions {
            name: Some("demo".into()),
            ..CreateOptions::default()
        })
        .await
        .unwrap();
    assert_eq!(sandbox.id(), "sbx_1");
    assert!(sandbox.info().unwrap().ready);
    assert_eq!(sandbox.exec("true").await.unwrap().exit_code, Some(0));
    assert!(api.finish().is_empty(), "{:?}", api.finish());
}

// stop and start are aliases: the same requests as pause and resume, the same result.
#[tokio::test]
async fn stop_and_start_send_what_pause_and_resume_send() {
    let paused =
        json!({ "id": "sbx_1", "status": "paused", "ready": false, "readiness": "paused" });
    let (api, listener) = MockServer::bind().await;
    // The box answers nowhere: start's background pre-connect of the origin resume
    // left (tests/preconnect.rs) fails quietly instead of reaching this strict server.
    // Port 1 is refused in every run (a freed bind(0) port can be any server's next).
    let box_origin = "http://127.0.0.1:1".to_string();
    api.state.lock().unwrap().exchanges = vec![
        answer("POST", "/sandbox/sbx_1/pause", 202, settling_row()),
        answer(
            "GET",
            "/sandbox/sbx_1/wait?until=paused",
            200,
            paused.clone(),
        ),
        answer("POST", "/sandbox/sbx_1/pause", 202, settling_row()),
        answer("GET", "/sandbox/sbx_1/wait?until=paused", 200, paused),
        answer("POST", "/sandbox/sbx_1/resume", 202, settling_row()),
        answer("GET", WAIT, 200, ready_row(&box_origin)),
        answer("POST", "/sandbox/sbx_1/resume", 202, settling_row()),
        answer("GET", WAIT, 200, ready_row(&box_origin)),
    ];
    api.serve(listener);
    let zb = client(&api.base);
    let by_pause = serde_json::to_value(zb.sandboxes().pause("sbx_1").await.unwrap()).unwrap();
    let by_stop = serde_json::to_value(zb.sandboxes().stop("sbx_1").await.unwrap()).unwrap();
    assert_eq!(by_pause, by_stop);
    assert_eq!(by_pause["readiness"], "paused");
    let by_resume = serde_json::to_value(zb.sandboxes().resume("sbx_1").await.unwrap()).unwrap();
    let by_start = serde_json::to_value(zb.sandboxes().start("sbx_1").await.unwrap()).unwrap();
    assert_eq!(by_resume, by_start);
    assert_eq!(by_resume["readiness"], "ready");
    assert!(api.finish().is_empty(), "{:?}", api.finish());
}
