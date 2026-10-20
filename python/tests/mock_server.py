"""The conformance mock server (sdks/conformance/README.md): serves a fixture's
exchanges in order and records every mismatch."""

from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

ANY = "{{ANY}}"


def substitute(value: Any, replacements: dict[str, str]) -> Any:
    """Replace placeholders in every string of a JSON value."""
    if isinstance(value, str):
        for key, replacement in replacements.items():
            value = value.replace(key, replacement)
        return value
    if isinstance(value, list):
        return [substitute(item, replacements) for item in value]
    if isinstance(value, dict):
        return {k: substitute(v, replacements) for k, v in value.items()}
    return value


class MockServer:
    """One fixture's server. ``start()`` returns the base URL."""

    def __init__(self) -> None:
        self.exchanges: list[dict[str, Any]] = []
        self.index = 0
        self.errors: list[str] = []
        self._lock = threading.Lock()
        server = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.0"

            def log_message(self, format: str, *args: Any) -> None:
                return

            def _handle(self) -> None:
                length = int(self.headers.get("content-length") or 0)
                body = self.rfile.read(length) if length else b""
                status, headers, payload = server.serve(
                    self.command,
                    self.path,
                    {k.lower(): v for k, v in self.headers.items()},
                    body,
                )
                self.send_response(status)
                for key, value in headers.items():
                    self.send_header(key, value)
                self.send_header("content-length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

            do_GET = _handle
            do_POST = _handle
            do_DELETE = _handle
            do_PUT = _handle
            do_PATCH = _handle

        self._server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)

    @property
    def base_url(self) -> str:
        host, port = self._server.server_address[:2]
        return f"http://{host!s}:{port}"

    def start(self) -> str:
        self._thread.start()
        return self.base_url

    def stop(self) -> None:
        self._server.shutdown()
        self._server.server_close()

    def load(self, exchanges: list[dict[str, Any]]) -> None:
        self.exchanges = exchanges
        self.index = 0

    def serve(
        self, method: str, path: str, headers: dict[str, str], body: bytes
    ) -> tuple[int, dict[str, str], bytes]:
        with self._lock:
            if self.index >= len(self.exchanges):
                self.errors.append(f"unexpected request {method} {path}")
                return 599, {}, b""
            exchange = self.exchanges[self.index]
            self.index += 1
        expected = exchange["request"]
        problems: list[str] = []
        if expected["method"] != method:
            problems.append(f"method {method} != {expected['method']}")
        if expected["path"] != path:
            problems.append(f"path {path} != {expected['path']}")
        for name, value in (expected.get("headers") or {}).items():
            actual = headers.get(name.lower())
            if value == ANY:
                if not actual:
                    problems.append(f"header {name} missing")
            elif actual != value:
                problems.append(f"header {name}: {actual!r} != {value!r}")
        if "body" in expected:
            try:
                parsed = json.loads(body) if body else None
            except ValueError:
                parsed = f"<non-json {body!r}>"
            if parsed != expected["body"]:
                problems.append(f"body {parsed!r} != {expected['body']!r}")
        if "bodyText" in expected and body.decode("utf-8", "replace") != expected["bodyText"]:
            problems.append(f"bodyText {body!r} != {expected['bodyText']!r}")
        if problems:
            self.errors.append(
                f"exchange {self.index - 1} ({method} {path}): " + "; ".join(problems)
            )
        response = exchange["response"]
        out_headers = dict(response.get("headers") or {})
        if "body" in response:
            payload = json.dumps(response["body"]).encode()
            out_headers.setdefault("content-type", "application/json")
        else:
            payload = str(response.get("bodyText", "")).encode()
        return int(response["status"]), out_headers, payload

    def unserved(self) -> int:
        return len(self.exchanges) - self.index
