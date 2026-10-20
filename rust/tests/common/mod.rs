//! A tiny HTTP/1.1 mock server that serves conformance exchanges in order.

#![allow(dead_code)]

use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use serde_json::Value;
use tokio::io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader};
use tokio::net::{TcpListener, TcpStream};

pub struct ReceivedRequest {
    pub method: String,
    pub path: String,
    pub headers: HashMap<String, String>,
    pub body: Vec<u8>,
}

#[derive(Default)]
pub struct State {
    pub exchanges: Vec<Value>,
    pub next: usize,
    pub failures: Vec<String>,
}

pub struct MockServer {
    pub base: String,
    pub host: String,
    pub state: Arc<Mutex<State>>,
}

impl MockServer {
    pub async fn bind() -> (MockServer, TcpListener) {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let server = MockServer {
            base: format!("http://127.0.0.1:{}", addr.port()),
            host: "127.0.0.1".to_string(),
            state: Arc::new(Mutex::new(State::default())),
        };
        (server, listener)
    }

    pub fn serve(&self, listener: TcpListener) {
        let state = self.state.clone();
        tokio::spawn(async move {
            loop {
                let Ok((stream, _)) = listener.accept().await else {
                    return;
                };
                let state = state.clone();
                tokio::spawn(async move {
                    let _ = handle(stream, state).await;
                });
            }
        });
    }

    pub fn finish(&self) -> Vec<String> {
        let state = self.state.lock().unwrap();
        let mut failures = state.failures.clone();
        if state.next < state.exchanges.len() {
            failures.push(format!(
                "{} exchange(s) left unserved",
                state.exchanges.len() - state.next
            ));
        }
        failures
    }
}

async fn read_request(
    reader: &mut BufReader<TcpStream>,
) -> std::io::Result<Option<ReceivedRequest>> {
    let mut line = String::new();
    if reader.read_line(&mut line).await? == 0 {
        return Ok(None);
    }
    let mut parts = line.trim_end().splitn(3, ' ');
    let method = parts.next().unwrap_or("").to_string();
    let path = parts.next().unwrap_or("").to_string();
    let mut headers = HashMap::new();
    loop {
        let mut h = String::new();
        reader.read_line(&mut h).await?;
        let h = h.trim_end();
        if h.is_empty() {
            break;
        }
        if let Some((k, v)) = h.split_once(':') {
            headers.insert(k.trim().to_ascii_lowercase(), v.trim().to_string());
        }
    }
    let mut body = Vec::new();
    if let Some(len) = headers
        .get("content-length")
        .and_then(|l| l.parse::<usize>().ok())
    {
        body.resize(len, 0);
        reader.read_exact(&mut body).await?;
    } else if headers
        .get("transfer-encoding")
        .is_some_and(|t| t.eq_ignore_ascii_case("chunked"))
    {
        loop {
            let mut size = String::new();
            reader.read_line(&mut size).await?;
            let n = usize::from_str_radix(size.trim(), 16).unwrap_or(0);
            let mut chunk = vec![0; n + 2];
            reader.read_exact(&mut chunk).await?;
            if n == 0 {
                break;
            }
            body.extend_from_slice(&chunk[..n]);
        }
    }
    Ok(Some(ReceivedRequest {
        method,
        path,
        headers,
        body,
    }))
}

async fn handle(stream: TcpStream, state: Arc<Mutex<State>>) -> std::io::Result<()> {
    let mut reader = BufReader::new(stream);
    while let Some(req) = read_request(&mut reader).await? {
        let response = {
            let mut st = state.lock().unwrap();
            match st.exchanges.get(st.next).cloned() {
                None => {
                    st.failures.push(format!(
                        "unexpected extra request {} {}",
                        req.method, req.path
                    ));
                    None
                }
                Some(exchange) => match mismatch(&exchange["request"], &req) {
                    Some(why) => {
                        let message =
                            format!("exchange {}: {} {}: {why}", st.next, req.method, req.path);
                        st.failures.push(message);
                        None
                    }
                    None => {
                        st.next += 1;
                        Some(exchange["response"].clone())
                    }
                },
            }
        };
        let (status, headers, body) = match response {
            Some(resp) => render(&resp),
            None => (
                400,
                vec![("content-type".to_string(), "application/json".to_string())],
                br#"{"error":"mock mismatch"}"#.to_vec(),
            ),
        };
        let mut out = format!("HTTP/1.1 {status} X\r\ncontent-length: {}\r\n", body.len());
        for (k, v) in headers {
            out.push_str(&format!("{k}: {v}\r\n"));
        }
        out.push_str("\r\n");
        let stream = reader.get_mut();
        stream.write_all(out.as_bytes()).await?;
        stream.write_all(&body).await?;
        stream.flush().await?;
    }
    Ok(())
}

fn render(resp: &Value) -> (u16, Vec<(String, String)>, Vec<u8>) {
    let status = resp["status"].as_u64().unwrap_or(200) as u16;
    let mut headers: Vec<(String, String)> = resp["headers"]
        .as_object()
        .map(|h| {
            h.iter()
                .map(|(k, v)| (k.clone(), v.as_str().unwrap_or("").to_string()))
                .collect()
        })
        .unwrap_or_default();
    let body = if let Some(text) = resp.get("bodyText").and_then(|t| t.as_str()) {
        text.as_bytes().to_vec()
    } else if let Some(body) = resp.get("body") {
        if !headers.iter().any(|(k, _)| k == "content-type") {
            headers.push(("content-type".into(), "application/json".into()));
        }
        serde_json::to_vec(body).unwrap()
    } else {
        Vec::new()
    };
    (status, headers, body)
}

fn mismatch(expected: &Value, req: &ReceivedRequest) -> Option<String> {
    if expected["method"].as_str() != Some(req.method.as_str()) {
        return Some(format!("method: expected {}", expected["method"]));
    }
    if expected["path"].as_str() != Some(req.path.as_str()) {
        return Some(format!("path: expected {}", expected["path"]));
    }
    if let Some(headers) = expected["headers"].as_object() {
        for (name, value) in headers {
            let got = req.headers.get(&name.to_ascii_lowercase());
            let want = value.as_str().unwrap_or("");
            let ok = match got {
                None => false,
                Some(g) if want == "{{ANY}}" => !g.is_empty(),
                Some(g) => g == want || (name == "content-type" && g.starts_with(want)),
            };
            if !ok {
                return Some(format!("header {name}: expected {want}, got {got:?}"));
            }
        }
    }
    if let Some(body) = expected.get("body") {
        match serde_json::from_slice::<Value>(&req.body) {
            Ok(got) if &got == body => {}
            Ok(got) => return Some(format!("body: expected {body}, got {got}")),
            Err(_) => return Some(format!("body: expected {body}, got non-JSON")),
        }
    }
    if let Some(text) = expected.get("bodyText").and_then(|t| t.as_str()) {
        if req.body != text.as_bytes() {
            return Some(format!(
                "bodyText: expected {text:?}, got {:?}",
                String::from_utf8_lossy(&req.body)
            ));
        }
    }
    None
}

/// Subset match per the conformance README.
pub fn subset(expected: &Value, actual: &Value, at: &str) -> Result<(), String> {
    match expected {
        Value::Null => {
            if actual.is_null() {
                Ok(())
            } else {
                Err(format!("{at}: expected null, got {actual}"))
            }
        }
        Value::Object(map) if map.contains_key("length") && actual.is_array() => {
            let items = actual.as_array().unwrap();
            if map["length"].as_u64() != Some(items.len() as u64) {
                return Err(format!(
                    "{at}: expected length {}, got {}",
                    map["length"],
                    items.len()
                ));
            }
            if let Some(expected_items) = map.get("items").and_then(|i| i.as_array()) {
                for (i, e) in expected_items.iter().enumerate() {
                    subset(e, &items[i], &format!("{at}[{i}]"))?;
                }
            }
            Ok(())
        }
        Value::Object(map) if map.len() == 1 && map.contains_key("text") && actual.is_string() => {
            if map["text"] == *actual {
                Ok(())
            } else {
                Err(format!("{at}: expected text {}, got {actual}", map["text"]))
            }
        }
        Value::Object(map) => {
            for (k, e) in map {
                let a = actual.get(k).unwrap_or(&Value::Null);
                subset(e, a, &format!("{at}.{k}"))?;
            }
            Ok(())
        }
        Value::Array(items) => {
            let got = actual
                .as_array()
                .ok_or(format!("{at}: expected array, got {actual}"))?;
            if got.len() != items.len() {
                return Err(format!(
                    "{at}: expected {} items, got {}",
                    items.len(),
                    got.len()
                ));
            }
            for (i, e) in items.iter().enumerate() {
                subset(e, &got[i], &format!("{at}[{i}]"))?;
            }
            Ok(())
        }
        _ => {
            if expected == actual {
                Ok(())
            } else {
                Err(format!("{at}: expected {expected}, got {actual}"))
            }
        }
    }
}
