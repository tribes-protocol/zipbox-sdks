//! Runs every fixture in sdks/conformance/fixtures against a mock server.

mod common;

use std::path::PathBuf;
use std::time::Duration;

use futures::StreamExt;
use serde_json::{json, Value};
use zipbox::{
    CreateOptions, DeleteOptions, Error, ExecEvent, ForkOptions, ForkSourcePolicy, LogOptions,
    OutputStream, Signal, Zipbox,
};

use common::{dead_origin, subset, MockServer};

const API_KEY: &str = "zb_TTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTT";

fn fixtures_dir() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../conformance/fixtures")
}

fn arg<'a>(args: &'a Value, key: &str) -> &'a str {
    args[key]
        .as_str()
        .unwrap_or_else(|| panic!("missing arg {key}"))
}

fn event_json(event: &ExecEvent) -> Value {
    match event {
        ExecEvent::Start { id, cursor } => json!({ "type": "start", "cursor": cursor, "id": id }),
        ExecEvent::Output {
            stream,
            data,
            cursor,
        } => json!({
            "type": match stream { OutputStream::Stdout => "stdout", OutputStream::Stderr => "stderr" },
            "cursor": cursor,
            "data": data,
        }),
        ExecEvent::Gap {
            stream,
            message,
            cursor,
        } => {
            json!({ "type": "gap", "cursor": cursor, "stream": stream, "message": message })
        }
        ExecEvent::Exit {
            state,
            exit_code,
            signal,
            timed_out,
            cursor,
        } => json!({
            "type": "exit",
            "cursor": cursor,
            "state": state,
            "exitCode": exit_code,
            "signal": signal,
            "timedOut": timed_out,
        }),
    }
}

fn fork_options(args: &Value) -> ForkOptions {
    let policy = match arg(args, "sourcePolicy") {
        "pause_and_resume" => ForkSourcePolicy::PauseAndResume,
        _ => ForkSourcePolicy::RequirePaused,
    };
    ForkOptions {
        private: args["private"].as_bool(),
        tailscale_auth_key: args["tailscaleAuthKey"].as_str().map(str::to_string),
        wait: args["wait"].as_bool().unwrap_or(true),
        ..ForkOptions::new(arg(args, "name"), policy)
    }
}

fn create_options(args: &Value) -> CreateOptions {
    CreateOptions {
        name: args["name"].as_str().map(str::to_string),
        tier: args["tier"].as_str().map(str::to_string),
        ssh_public_key: args["sshPublicKey"].as_str().map(str::to_string),
        mailbox: args["mailbox"].as_bool(),
        private: args["private"].as_bool(),
    }
}

async fn run_op(zb: &Zipbox, op: &str, args: &Value) -> Result<Value, Error> {
    let to = |v: serde_json::Result<Value>| v.expect("results serialize");
    match op {
        "sandboxes.list" => Ok(to(serde_json::to_value(zb.sandboxes().list().await?))),
        "sandboxes.get" => Ok(to(serde_json::to_value(
            zb.sandboxes().get(arg(args, "id")).await?,
        ))),
        "sandboxes.create" => Ok(to(serde_json::to_value(
            zb.sandboxes().create(create_options(args)).await?,
        ))),
        "sandboxes.getOrCreate" => {
            let opts = CreateOptions {
                name: None,
                ..create_options(args)
            };
            Ok(to(serde_json::to_value(
                zb.sandboxes()
                    .get_or_create_with(arg(args, "name"), opts)
                    .await?,
            )))
        }
        "sandboxes.waitReady" => Ok(to(serde_json::to_value(
            zb.sandboxes().wait_ready(arg(args, "id")).await?,
        ))),
        "sandboxes.start" => zb
            .sandboxes()
            .start(arg(args, "id"))
            .await
            .map(|_| Value::Null),
        "sandboxes.stop" => zb
            .sandboxes()
            .stop(arg(args, "id"))
            .await
            .map(|_| Value::Null),
        "sandboxes.restart" => zb
            .sandboxes()
            .restart(arg(args, "id"))
            .await
            .map(|_| Value::Null),
        "sandboxes.cancelTransition" => zb
            .sandboxes()
            .cancel_transition(arg(args, "id"))
            .await
            .map(|outcome| Value::String(outcome.as_str().to_string())),
        "sandboxes.fork" => Ok(to(serde_json::to_value(
            zb.sandboxes()
                .fork(arg(args, "id"), fork_options(args))
                .await?,
        ))),
        "sandboxes.getFork" => Ok(to(serde_json::to_value(
            zb.sandboxes().get_fork(arg(args, "id")).await?,
        ))),
        "sandboxes.delete" => zb
            .sandboxes()
            .delete_with(
                arg(args, "id"),
                DeleteOptions {
                    archive: args["archive"].as_bool(),
                },
            )
            .await
            .map(|_| Value::Null),
        "sandboxes.setTier" => zb
            .sandboxes()
            .set_tier(arg(args, "id"), arg(args, "tier"))
            .await
            .map(|_| Value::Null),
        "sandbox.exec" => {
            let out = zb.sandbox(arg(args, "id")).exec(arg(args, "cmd")).await?;
            Ok(to(serde_json::to_value(out)))
        }
        "sandbox.execBackground" => {
            let command = zb
                .sandbox(arg(args, "id"))
                .exec_background(arg(args, "cmd"))
                .await?;
            Ok(to(serde_json::to_value(command.record().expect("record"))))
        }
        "commands.events" => {
            let stream = zb
                .sandbox(arg(args, "id"))
                .commands()
                .events(arg(args, "commandId"), args["since"].as_str())
                .await?;
            let events: Vec<_> = stream.collect().await;
            let mut out = Vec::new();
            for e in events {
                out.push(event_json(&e?));
            }
            Ok(json!({ "events": out }))
        }
        "commands.closeStdin" => zb
            .sandbox(arg(args, "id"))
            .commands()
            .close_stdin(arg(args, "commandId"))
            .await
            .map(|_| Value::Null),
        "sandbox.logs" => {
            let options = LogOptions {
                since: args["since"].as_str().map(str::to_string),
                lines: args["lines"].as_u64().map(|n| n as u32),
                unit: args["unit"].as_str().map(str::to_string),
            };
            Ok(Value::String(
                zb.sandbox(arg(args, "id")).logs(options).await?,
            ))
        }
        "commands.signal" => {
            let signal: Signal = arg(args, "signal").parse()?;
            zb.sandbox(arg(args, "id"))
                .commands()
                .signal(arg(args, "commandId"), signal)
                .await
                .map(|_| Value::Null)
        }
        "files.read" => Ok(Value::String(
            zb.sandbox(arg(args, "id"))
                .files()
                .read_text(arg(args, "path"))
                .await?,
        )),
        "files.write" => zb
            .sandbox(arg(args, "id"))
            .files()
            .write(arg(args, "path"), arg(args, "content").as_bytes().to_vec())
            .await
            .map(|_| Value::Null),
        "files.list" => Ok(to(serde_json::to_value(
            zb.sandbox(arg(args, "id"))
                .files()
                .list(arg(args, "path"))
                .await?,
        ))),
        "files.stat" => Ok(to(serde_json::to_value(
            zb.sandbox(arg(args, "id"))
                .files()
                .stat(arg(args, "path"))
                .await?,
        ))),
        "files.mkdir" => zb
            .sandbox(arg(args, "id"))
            .files()
            .mkdir(arg(args, "path"))
            .await
            .map(|_| Value::Null),
        "files.move" => zb
            .sandbox(arg(args, "id"))
            .files()
            .move_to(arg(args, "path"), arg(args, "destination"))
            .await
            .map(|_| Value::Null),
        "files.copy" => zb
            .sandbox(arg(args, "id"))
            .files()
            .copy(arg(args, "path"), arg(args, "destination"))
            .await
            .map(|_| Value::Null),
        "files.delete" => zb
            .sandbox(arg(args, "id"))
            .files()
            .delete(arg(args, "path"))
            .await
            .map(|_| Value::Null),
        "preview.create" => {
            let port = args["port"].as_u64().expect("port") as u16;
            let preview = zb
                .sandbox(arg(args, "id"))
                .preview()
                .create(port, args["label"].as_str())
                .await?;
            Ok(to(serde_json::to_value(preview)))
        }
        "sandbox.setPrivate" => zb
            .sandbox(arg(args, "id"))
            .set_private(args["enabled"].as_bool().expect("enabled"))
            .await
            .map(|_| Value::Null),
        "tailscale.status" => Ok(to(serde_json::to_value(
            zb.sandbox(arg(args, "id")).tailscale().status().await?,
        ))),
        "tailscale.connect" => zb
            .sandbox(arg(args, "id"))
            .tailscale()
            .connect(arg(args, "authKey"))
            .await
            .map(|_| Value::Null),
        "tailscale.disconnect" => zb
            .sandbox(arg(args, "id"))
            .tailscale()
            .disconnect()
            .await
            .map(|_| Value::Null),
        "account.balance" => zb.account().balance().await,
        "account.whoami" => zb.account().whoami().await,
        other => panic!("unknown op {other}"),
    }
}

fn error_json(err: &Error) -> Value {
    json!({
        "code": err.code.as_str(),
        "status": err.status,
        "requestId": err.request_id,
        "docsUrl": err.docs_url,
        "message": err.message,
        "retryable": err.retryable,
    })
}

async fn run_fixture(path: PathBuf) -> Result<(), String> {
    let (server, listener) = MockServer::bind().await;
    let raw = std::fs::read_to_string(&path).unwrap();
    let raw = raw
        .replace("{{BASE}}", &server.base)
        .replace("{{HOST}}", &server.host)
        .replace("{{DEAD}}", &dead_origin());
    let fixture: Value = serde_json::from_str(&raw).unwrap();
    server.state.lock().unwrap().exchanges = fixture["exchanges"].as_array().unwrap().clone();
    server.serve(listener);

    let zb = Zipbox::builder()
        .api_key(API_KEY)
        .base_url(&server.base)
        .retry_base_delay(Duration::from_millis(1))
        .poll_interval(Duration::from_millis(1))
        .build()
        .unwrap();
    let outcome = tokio::time::timeout(
        Duration::from_secs(10),
        run_op(
            &zb,
            fixture["call"]["op"].as_str().unwrap(),
            &fixture["call"]["args"],
        ),
    )
    .await
    .map_err(|_| "timed out".to_string())?;

    let mut problems = server.finish();
    let args = &fixture["call"]["args"];
    if let Some(auth_key) = args["authKey"]
        .as_str()
        .or(args["tailscaleAuthKey"].as_str())
    {
        let leaked = server
            .state
            .lock()
            .unwrap()
            .bodies
            .iter()
            .any(|b| String::from_utf8_lossy(b).contains(auth_key));
        if leaked {
            problems.push("the auth key left the client in a request body".into());
        }
        if let Err(err) = &outcome {
            if format!("{err} {err:?}").contains(auth_key) {
                problems.push("the auth key is in an error".into());
            }
        }
    }
    let expect = &fixture["expect"];
    match (outcome, expect.get("result"), expect.get("error")) {
        (Ok(value), Some(result), None) => {
            if let Err(why) = subset(result, &value, "result") {
                problems.push(why);
            }
        }
        (Err(err), None, Some(expected)) => {
            if let Err(why) = subset(expected, &error_json(&err), "error") {
                problems.push(why);
            }
        }
        (Ok(value), None, Some(_)) => problems.push(format!("expected an error, got {value}")),
        (Err(err), Some(_), None) => problems.push(format!("unexpected error: {err}")),
        _ => problems.push("fixture has neither result nor error".into()),
    }
    if problems.is_empty() {
        Ok(())
    } else {
        Err(problems.join("; "))
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn every_fixture_passes() {
    let mut paths: Vec<_> = std::fs::read_dir(fixtures_dir())
        .expect("fixtures dir")
        .map(|e| e.unwrap().path())
        .filter(|p| p.extension().is_some_and(|e| e == "json"))
        .collect();
    paths.sort();
    assert!(!paths.is_empty(), "no fixtures found");
    let mut failures = Vec::new();
    for path in &paths {
        let name = path.file_stem().unwrap().to_string_lossy().to_string();
        if let Err(why) = run_fixture(path.clone()).await {
            failures.push(format!("{name}: {why}"));
        }
    }
    println!("{} fixtures, {} failed", paths.len(), failures.len());
    assert!(failures.is_empty(), "\n{}", failures.join("\n"));
}

#[tokio::test(flavor = "multi_thread")]
async fn preview_pending_survives_the30second_exec_window() {
    let (server, listener) = MockServer::bind().await;
    let raw = std::fs::read_to_string(fixtures_dir().join("preview_pending.json"))
        .unwrap()
        .replace("{{BASE}}", &server.base)
        .replace("{{HOST}}", &server.host)
        .replace("{{DEAD}}", &dead_origin());
    let mut fixture: Value = serde_json::from_str(&raw).unwrap();
    let body = fixture["exchanges"][1]["response"]["bodyText"]
        .as_str()
        .unwrap()
        .replace("retryAfterMs\\\": 0", "retryAfterMs\\\": 31000");
    assert!(body.contains("31000"));
    fixture["exchanges"][1]["response"]["bodyText"] = Value::String(body);
    let exchanges = fixture["exchanges"].as_array().unwrap();
    server.state.lock().unwrap().exchanges = vec![
        exchanges[0].clone(),
        exchanges[1].clone(),
        exchanges.last().unwrap().clone(),
    ];
    server.serve(listener);
    let client = Zipbox::builder()
        .api_key(API_KEY)
        .base_url(&server.base)
        .build()
        .unwrap();
    let start = std::time::Instant::now();
    let preview = client
        .sandbox("sbx_1")
        .preview()
        .create(3000, Some("web"))
        .await
        .unwrap();
    assert!(start.elapsed() >= Duration::from_secs(31));
    assert_eq!(preview.url, format!("https://web.{}", server.host));
    assert!(server.finish().is_empty(), "{:?}", server.finish());
}
