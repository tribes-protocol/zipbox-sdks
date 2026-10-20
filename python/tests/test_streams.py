"""Stream lifetimes: early exit closes the stream; a dropped follow re-attaches."""

from __future__ import annotations

from collections.abc import Generator
from typing import Any

import pytest

from mock_server import MockServer
from zipbox import Zipbox, ZipboxError
from zipbox._effects import Emit, HttpRequest, HttpResponse, ReadChunk, iterate_sync

KEY = "zb_" + "T" * 43
AUTH = {"authorization": f"Bearer {KEY}"}
BOX = {"authorization": "Bearer bt"}


class FakeTransport:
    def __init__(self) -> None:
        self.closed: list[HttpResponse] = []

    def send(self, request: HttpRequest) -> HttpResponse:
        return HttpResponse(200, {}, handle=object())

    def read_chunk(self, response: HttpResponse) -> bytes:
        return b"x"

    def read_all(self, response: HttpResponse) -> bytes:
        return b""

    def close_stream(self, response: HttpResponse) -> None:
        self.closed.append(response)


def test_leaving_an_iteration_early_closes_its_stream() -> None:
    def op() -> Generator[Any, Any, None]:
        response = yield HttpRequest("POST", "http://box/exec", {}, None, stream=True)
        while True:
            chunk = yield ReadChunk(response)
            yield Emit(chunk)

    transport = FakeTransport()
    iterator = iterate_sync(transport, op())
    next(iterator)
    assert isinstance(iterator, Generator)
    iterator.close()
    assert len(transport.closed) == 1


def _detail(base: str) -> dict[str, Any]:
    return {"id": "sbx_1", "status": "running", "ready": True, "baseUrl": base, "boxToken": "bt"}


def _sse(*events: tuple[str, str, str]) -> str:
    return "".join(f"id: {c}\nevent: {e}\ndata: {d}\n\n" for c, e, d in events)


@pytest.fixture
def server() -> Any:
    mock = MockServer()
    mock.start()
    yield mock
    mock.stop()


def test_wait_reattaches_from_the_last_cursor_after_a_drop(server: MockServer) -> None:
    # The re-attach goes straight to ?since=: the box replays what was printed during
    # the drop and ends with exit, even if the command finished meanwhile.
    base = server.base_url
    stream = {"content-type": "text/event-stream"}
    server.load(
        [
            {
                "request": {"method": "GET", "path": "/sandbox/sbx_1", "headers": AUTH},
                "response": {"status": 200, "body": _detail(base)},
            },
            {
                "request": {"method": "GET", "path": "/exec/cmd_1/events", "headers": BOX},
                "response": {
                    "status": 200,
                    "headers": stream,
                    "bodyText": _sse(
                        ("0-0", "start", '{"id":"cmd_1"}'), ("3-0", "stdout", '{"data":"ab\\n"}')
                    ),
                },
            },
            {
                "request": {
                    "method": "GET",
                    "path": "/exec/cmd_1/events?since=3-0",
                    "headers": BOX,
                },
                "response": {
                    "status": 200,
                    "headers": stream,
                    "bodyText": _sse(
                        ("6-0", "stdout", '{"data":"cd\\n"}'),
                        ("6-0", "exit", '{"state":"exited","exitCode":0}'),
                    ),
                },
            },
        ]
    )
    with Zipbox(KEY, base_url=base, retry_base_delay_s=0.001) as zb:
        result = zb.sandbox("sbx_1").commands.handle("cmd_1").wait()
    assert server.errors == [] and server.unserved() == 0
    assert (result.id, result.exit_code, result.stdout) == ("cmd_1", 0, "ab\ncd\n")


def test_exec_stream_that_ends_without_exit_is_unavailable(server: MockServer) -> None:
    base = server.base_url
    server.load(
        [
            {
                "request": {"method": "GET", "path": "/sandbox/sbx_1"},
                "response": {"status": 200, "body": _detail(base)},
            },
            {
                "request": {"method": "POST", "path": "/exec"},
                "response": {
                    "status": 200,
                    "headers": {"content-type": "text/event-stream"},
                    "bodyText": _sse(("0-0", "start", '{"id":"c"}')),
                },
            },
        ]
    )
    with Zipbox(KEY, base_url=base) as zb, pytest.raises(ZipboxError) as caught:
        zb.sandbox("sbx_1").exec("sleep 1")
    assert caught.value.code == "unavailable"


def test_second_box_401_is_unauthorized(server: MockServer) -> None:
    base = server.base_url
    detail = {
        "request": {"method": "GET", "path": "/sandbox/sbx_1"},
        "response": {"status": 200, "body": _detail(base)},
    }
    refused = {
        "request": {"method": "GET", "path": "/files?path=%2Froot%2Fworkspace"},
        "response": {"status": 401, "body": {"error": "unauthorized"}},
    }
    server.load([detail, refused, detail, refused])
    with Zipbox(KEY, base_url=base) as zb, pytest.raises(ZipboxError) as caught:
        zb.sandbox("sbx_1").files.list()
    assert caught.value.code == "unauthorized" and server.unserved() == 0


def test_follow_outlasts_max_retries_and_retries_a_restarting_box(server: MockServer) -> None:
    base = server.base_url
    detail = {
        "request": {"method": "GET", "path": "/sandbox/sbx_1"},
        "response": {"status": 200, "body": _detail(base)},
    }
    down = {
        "request": {"method": "GET", "path": "/exec/cmd_1/events?since=3-0"},
        "response": {"status": 503, "body": {"error": "restarting"}},
    }
    gateway = {
        "request": {"method": "GET", "path": "/exec/cmd_1/events?since=3-0"},
        "response": {"status": 502, "body": {"error": "bad gateway"}},
    }
    done = {
        "request": {"method": "GET", "path": "/exec/cmd_1/events?since=3-0"},
        "response": {
            "status": 200,
            "headers": {"content-type": "text/event-stream"},
            "bodyText": _sse(("6-0", "exit", '{"state":"exited","exitCode":3}')),
        },
    }
    server.load([detail, down, down, down, gateway, down, down, done])
    with Zipbox(KEY, base_url=base, max_retries=0, retry_base_delay_s=0.001) as zb:
        events = list(zb.sandbox("sbx_1").commands.events("cmd_1", since="3-0"))
    assert server.errors == [] and server.unserved() == 0
    assert [(e.type, e.exit_code) for e in events] == [("exit", 3)]


def test_follow_stops_on_an_answered_verdict(server: MockServer) -> None:
    base = server.base_url
    server.load(
        [
            {
                "request": {"method": "GET", "path": "/sandbox/sbx_1"},
                "response": {"status": 200, "body": _detail(base)},
            },
            {
                "request": {"method": "GET", "path": "/exec/gone/events"},
                "response": {
                    "status": 404,
                    "body": {
                        "error": {
                            "code": "not_found",
                            "message": "no such command",
                            "requestId": "r",
                            "retryable": False,
                            "docsUrl": "https://zipbox.ai/docs/errors#not_found",
                        }
                    },
                },
            },
        ]
    )
    with Zipbox(KEY, base_url=base) as zb, pytest.raises(ZipboxError) as caught:
        list(zb.sandbox("sbx_1").commands.events("gone"))
    assert caught.value.code == "not_found" and server.unserved() == 0
