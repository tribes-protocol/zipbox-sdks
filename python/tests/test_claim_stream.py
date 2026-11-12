"""Streamed create (sdks/conformance/README.md, "Streamed create").

create asks for its answer streamed and reads the last line's answer exactly like an
unstreamed one. The placed line is informational: its origin is handed out before its
DNS record is known to be live, so create never dials it. An unstreamed answer reads as
before.
"""

from __future__ import annotations

import json
from collections.abc import AsyncIterator, Iterator
from typing import Any

import httpx
import pytest

from zipbox import AsyncZipbox, Zipbox, ZipboxError

API = "https://api.test"
ORIGIN = "https://box--warm-slug.zbox.sh"
NDJSON = {"content-type": "application/x-ndjson; charset=utf-8"}
EVENTS = (
    'event: start\ndata: {"id":"cmd_1"}\n\n'
    'event: exit\ndata: {"state":"exited","exitCode":0,"signal":null,"timedOut":false}\n\n'
)


def claimed(**overrides: Any) -> dict[str, Any]:
    body: dict[str, Any] = {
        "sandboxId": "sbx",
        "name": "demo",
        "status": "running",
        "created": True,
        "ready": True,
        "readiness": "ready",
        "baseUrl": ORIGIN,
        "boxToken": "bt",
    }
    body.update(overrides)
    return body


def line(value: dict[str, Any]) -> bytes:
    return json.dumps(value).encode() + b"\n"


PLACED = line({"type": "placed", "sandboxId": "sbx", "baseUrl": ORIGIN})


def answer(status: int, body: dict[str, Any]) -> bytes:
    return line({"type": "answer", "status": status, "headers": {}, "body": body})


class Api:
    """A control plane and box behind one mock transport. Each streamed line records
    every request seen before it, so a dial of the placed origin while the box binds
    would show up in `before_answer`."""

    def __init__(self, claims: list[list[bytes] | dict[str, Any]]) -> None:
        self.claims = claims
        self.seen: list[tuple[str, str, str | None]] = []
        self.keys: list[str | None] = []
        self.accepts: list[str | None] = []
        self.before_answer: list[tuple[str, str, str | None]] = []

    def stream(self, lines: list[bytes]) -> Iterator[bytes]:
        for chunk in lines:
            self.before_answer = list(self.seen)
            yield chunk

    def answer(self, request: httpx.Request) -> httpx.Response:
        self.seen.append((request.method, str(request.url), request.headers.get("authorization")))
        if request.url.path == "/sandbox/identity/claim":
            self.keys.append(request.headers.get("idempotency-key"))
            self.accepts.append(request.headers.get("accept"))
            claim = self.claims.pop(0)
            if isinstance(claim, dict):
                return httpx.Response(200, json=claim)
            return httpx.Response(200, headers=NDJSON, content=self.stream(claim))
        if request.url.path == "/exec":
            return httpx.Response(200, text=EVENTS, headers={"content-type": "text/event-stream"})
        return httpx.Response(200, json={**claimed(), "id": "sbx"})

    def dialled_origin(self) -> bool:
        return any(url.startswith(ORIGIN) and url != f"{ORIGIN}/exec" for _, url, _ in self.seen)


def client(api: Api) -> Zipbox:
    http = httpx.Client(transport=httpx.MockTransport(api.answer))
    return Zipbox("zb_k", base_url=API, http_client=http, retry_base_delay_s=0.001)


def test_create_reads_the_last_line_and_never_dials_the_placed_origin() -> None:
    api = Api([[PLACED, answer(200, claimed())]])
    with client(api) as zb:
        sandbox = zb.sandboxes.create(name="demo")
        assert sandbox.exec("true").exit_code == 0
    assert "application/x-ndjson" in (api.accepts[0] or "")
    assert not any(url.startswith(ORIGIN) for _, url, _ in api.before_answer)
    assert not api.dialled_origin()
    assert api.seen[-1] == ("POST", f"{ORIGIN}/exec", "Bearer bt")


def test_create_reads_an_unstreamed_answer_as_before() -> None:
    api = Api([claimed()])
    with client(api) as zb:
        assert zb.sandboxes.create(name="demo").id == "sbx"
    assert not api.dialled_origin()


def test_create_raises_the_error_in_the_last_line() -> None:
    conflict = {
        "error": {
            "code": "conflict",
            "message": "Sandbox name is already taken",
            "requestId": "req_1",
            "retryable": False,
            "retryAfterMs": None,
            "docsUrl": "https://zipbox.ai/docs/errors#conflict",
        }
    }
    api = Api([[PLACED, answer(409, conflict)]])
    with client(api) as zb, pytest.raises(ZipboxError) as raised:
        zb.sandboxes.create(name="demo")
    assert raised.value.code == "conflict"
    assert raised.value.status == 409
    assert raised.value.request_id == "req_1"
    assert not api.dialled_origin()


def test_create_retries_a_stream_that_ends_before_its_answer_with_the_same_key() -> None:
    api = Api([[PLACED], claimed()])
    with client(api) as zb:
        assert zb.sandboxes.create(name="demo").id == "sbx"
    assert len(api.keys) == 2
    assert api.keys[0] == api.keys[1]
    assert not api.dialled_origin()


async def test_async_create_never_dials_the_placed_origin() -> None:
    api = Api([])

    async def stream() -> AsyncIterator[bytes]:
        yield PLACED
        api.before_answer = list(api.seen)
        yield answer(200, claimed())

    async def respond(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/sandbox/identity/claim":
            api.seen.append((request.method, str(request.url), None))
            return httpx.Response(200, headers=NDJSON, content=stream())
        return api.answer(request)

    http = httpx.AsyncClient(transport=httpx.MockTransport(respond))
    async with AsyncZipbox("zb_k", base_url=API, http_client=http) as zb:
        sandbox = await zb.sandboxes.create(name="demo")
        assert (await sandbox.exec("true")).exit_code == 0
    assert not any(url.startswith(ORIGIN) for _, url, _ in api.before_answer)
    assert not api.dialled_origin()
    assert api.seen[-1] == ("POST", f"{ORIGIN}/exec", "Bearer bt")
