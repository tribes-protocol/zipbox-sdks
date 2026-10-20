//! Runs every fixture in sdks/conformance/fixtures against a mock server.

mod common;

use std::path::PathBuf;
use std::time::Duration;

use futures::StreamExt;
use serde_json::{json, Value};
use zipbox::{CreateOptions, Error, ExecEvent, OutputStream, Signal, Zipbox};

use common::{subset, MockServer};

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

async fn run_op(zb: &Zipbox, op: &str, args: &Value) -> Result<Value, Error> {
    let to = |v: serde_json::Result<Value>| v.expect("results serialize");
    match op {
        "sandboxes.list" => Ok(to(serde_json::to_value(zb.sandboxes().list().await?))),
        "sandboxes.get" => Ok(to(serde_json::to_value(
            zb.sandboxes().get(arg(args, "id")).await?,
        ))),
        "sandboxes.create" => {
            let opts = CreateOptions {
                name: args["name"].as_str().map(str::to_string),
                tier: args["tier"].as_str().map(str::to_string),
                ssh_public_key: args["sshPublicKey"].as_str().map(str::to_string),
            };
            Ok(to(serde_json::to_value(zb.sandboxes().create(opts).await?)))
        }
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
        "sandboxes.delete" => zb
            .sandboxes()
            .delete(arg(args, "id"))
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
    })
}

async fn run_fixture(path: PathBuf) -> Result<(), String> {
    let (server, listener) = MockServer::bind().await;
    let raw = std::fs::read_to_string(&path).unwrap();
    let raw = raw
        .replace("{{BASE}}", &server.base)
        .replace("{{HOST}}", &server.host);
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
