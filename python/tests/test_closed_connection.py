"""A server that closes the connection ends the call with the usual network error,
never a call left waiting. No timeout makes this true: the close itself does
(AGENTS.md §0.0 forbids a request deadline on these calls). An exec that may have
reached the box is never sent again: running a command twice is worse than an error."""

from __future__ import annotations

import socket
import threading
from collections.abc import Iterator
from dataclasses import dataclass, field
from typing import Any

import httpx
import pytest

from zipbox import AsyncZipbox, Zipbox, ZipboxError

MID_BODY = (
    b'HTTP/1.1 200 OK\r\ncontent-type: application/json\r\ncontent-length: 400\r\n\r\n[{"id":'
)
MID_EVENTS = (
    b"HTTP/1.1 200 OK\r\ncontent-type: text/event-stream\r\ntransfer-encoding: chunked\r\n\r\n"
    b"40\r\nevent: sta"
)


@dataclass
class ClosingServer:
    """Accepts, reads the request head, writes `reply` (if any) and closes. Records the
    request line of each request it read."""

    url: str
    requests: list[str] = field(default_factory=list)


def _serve(listener: socket.socket, reply: bytes, server: ClosingServer) -> None:
    while True:
        try:
            conn, _ = listener.accept()
        except OSError:
            return
        with conn:
            head = b""
            while b"\r\n\r\n" not in head:
                chunk = conn.recv(4096)
                if not chunk:
                    break
                head += chunk
            server.requests.append(head.split(b"\r\n", 1)[0].decode())
            if reply:
                conn.sendall(reply)


def _closing_server(reply: bytes) -> Iterator[ClosingServer]:
    listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    listener.bind(("127.0.0.1", 0))
    listener.listen()
    server = ClosingServer(f"http://127.0.0.1:{listener.getsockname()[1]}")
    threading.Thread(target=_serve, args=(listener, reply, server), daemon=True).start()
    try:
        yield server
    finally:
        listener.close()


@pytest.fixture
def no_response() -> Iterator[ClosingServer]:
    yield from _closing_server(b"")


@pytest.fixture
def mid_body() -> Iterator[ClosingServer]:
    yield from _closing_server(MID_BODY)


@pytest.fixture
def mid_events() -> Iterator[ClosingServer]:
    yield from _closing_server(MID_EVENTS)


def _assert_retryable(error: ZipboxError) -> None:
    assert error.code == "unavailable"
    assert error.retryable


def test_a_connection_closed_with_no_response_is_a_retryable_error(
    no_response: ClosingServer,
) -> None:
    zb = Zipbox("zb_k", base_url=no_response.url, max_retries=0)
    with zb, pytest.raises(ZipboxError) as raised:
        zb.sandboxes.list()
    _assert_retryable(raised.value)


def test_a_connection_closed_mid_body_is_a_retryable_error(mid_body: ClosingServer) -> None:
    zb = Zipbox("zb_k", base_url=mid_body.url, max_retries=0)
    with zb, pytest.raises(ZipboxError) as raised:
        zb.sandboxes.list()
    _assert_retryable(raised.value)


async def test_async_a_connection_closed_mid_body_is_a_retryable_error(
    mid_body: ClosingServer,
) -> None:
    async with AsyncZipbox("zb_k", base_url=mid_body.url, max_retries=0) as zb:
        with pytest.raises(ZipboxError) as raised:
            await zb.sandboxes.list()
    _assert_retryable(raised.value)


def _box_detail(box: ClosingServer) -> dict[str, Any]:
    return {
        "id": "sbx",
        "sandboxId": "sbx",
        "name": "demo",
        "status": "running",
        "ready": True,
        "readiness": "ready",
        "httpsReady": True,
        "baseUrl": box.url,
        "boxToken": "bt",
    }


def _exec_client(box: ClosingServer) -> Zipbox:
    """The control plane is mocked; the box is the real closing socket."""
    control = httpx.MockTransport(lambda _: httpx.Response(200, json=_box_detail(box)))
    http = httpx.Client(mounts={"https://api.test": control})
    return Zipbox("zb_k", base_url="https://api.test", http_client=http, max_retries=3)


def test_an_exec_on_a_connection_closed_with_no_response_is_sent_once(
    no_response: ClosingServer,
) -> None:
    zb = _exec_client(no_response)
    with zb, pytest.raises(ZipboxError) as raised:
        zb.sandboxes.get("sbx").exec("true")
    assert raised.value.code == "unavailable"
    assert no_response.requests == ["POST /exec HTTP/1.1"]


def test_an_exec_whose_events_closed_before_its_start_is_sent_once(
    mid_events: ClosingServer,
) -> None:
    zb = _exec_client(mid_events)
    with zb, pytest.raises(ZipboxError) as raised:
        zb.sandboxes.get("sbx").exec("true")
    assert raised.value.code == "unavailable"
    assert mid_events.requests == ["POST /exec HTTP/1.1"]
