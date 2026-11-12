"""Pre-connect on resume (sdks/conformance/README.md, "Pre-connect on resume").

The box's last known origin is dialled in parallel with POST /resume on the shared
pool, so the first box call after it reuses the connection. Never waited for, never
retried; a different origin in the answer or a failure drops it.
"""

from __future__ import annotations

import asyncio
import threading
from typing import Any

import httpx

from zipbox import AsyncZipbox, Zipbox

API = "https://api.test"
ORIGIN = "https://box--demo.zbox.sh"
MOVED = "https://box.demo.zbox.sh"
EVENTS = (
    'event: start\ndata: {"id":"cmd_1"}\n\n'
    'event: exit\ndata: {"state":"exited","exitCode":0,"signal":null,"timedOut":false}\n\n'
)


def detail(**overrides: Any) -> dict[str, Any]:
    body: dict[str, Any] = {
        "id": "sbx",
        "name": "demo",
        "status": "running",
        "ready": True,
        "readiness": "ready",
        "baseUrl": ORIGIN,
        "boxToken": "bt",
    }
    body.update(overrides)
    return body


PAUSED = detail(status="paused", ready=False, readiness="paused", baseUrl=None, boxToken=None)


class Box:
    """A control plane and box behind one mock transport. The resume answer waits until
    the pre-connect has arrived, so a pre-connect sent after the resume would deadlock
    the test instead of passing it."""

    def __init__(self, resumed: dict[str, Any], fail_preconnect: bool = False) -> None:
        self.resumed = resumed
        self.fail_preconnect = fail_preconnect
        self.seen: list[tuple[str, str, str | None]] = []
        self.preconnected = threading.Event()

    def answer(self, request: httpx.Request) -> httpx.Response:
        url = str(request.url)
        self.seen.append((request.method, url, request.headers.get("authorization")))
        if request.url.path == "/healthz":
            self.preconnected.set()
            if self.fail_preconnect:
                raise httpx.ConnectError("refused", request=request)
            return httpx.Response(200, json={"ok": True})
        if request.url.path == "/sandbox/sbx/resume":
            assert self.preconnected.wait(5), "no pre-connect while the resume was in flight"
            return httpx.Response(200, json=self.resumed)
        if request.url.path == "/sandbox/sbx/pause":
            return httpx.Response(200, json=PAUSED)
        if request.url.path == "/exec":
            return httpx.Response(200, text=EVENTS, headers={"content-type": "text/event-stream"})
        return httpx.Response(200, json=detail())

    async def answer_async(self, request: httpx.Request) -> httpx.Response:
        if request.url.path == "/sandbox/sbx/resume":
            # The async pre-connect is a task on the same loop: let it run first.
            for _ in range(50):
                if self.preconnected.is_set():
                    break
                await asyncio.sleep(0.01)
        return self.answer(request)


def test_resume_preconnects_the_last_origin_in_parallel_and_the_first_call_follows() -> None:
    box = Box(detail())
    client = httpx.Client(transport=httpx.MockTransport(box.answer))
    with Zipbox("zb_k", base_url=API, http_client=client) as zb:
        sandbox = zb.sandboxes.get("sbx")
        sandbox.pause()
        sandbox.resume()
        assert sandbox.exec("true").exit_code == 0
    assert ("GET", f"{ORIGIN}/healthz", None) in box.seen
    assert box.seen[-1] == ("POST", f"{ORIGIN}/exec", "Bearer bt")


def test_client_level_start_preconnects_like_resume() -> None:
    box = Box(detail())
    client = httpx.Client(transport=httpx.MockTransport(box.answer))
    with Zipbox("zb_k", base_url=API, http_client=client) as zb:
        zb.sandboxes.get("sbx")
        zb.sandboxes.start("sbx")
    assert box.preconnected.is_set()


def test_a_moved_origin_is_reached_the_normal_way() -> None:
    box = Box(detail(baseUrl=MOVED, boxToken="bt_moved"))
    client = httpx.Client(transport=httpx.MockTransport(box.answer))
    with Zipbox("zb_k", base_url=API, http_client=client) as zb:
        sandbox = zb.sandboxes.get("sbx")
        sandbox.resume()
        sandbox.exec("true")
    assert box.seen[-1] == ("POST", f"{MOVED}/exec", "Bearer bt_moved")


def test_a_failed_preconnect_changes_nothing() -> None:
    box = Box(detail(), fail_preconnect=True)
    client = httpx.Client(transport=httpx.MockTransport(box.answer))
    with Zipbox("zb_k", base_url=API, http_client=client) as zb:
        sandbox = zb.sandboxes.get("sbx")
        assert sandbox.resume().status == "running"
        assert sandbox.exec("true").exit_code == 0


def test_a_client_that_never_saw_the_box_ready_sends_none() -> None:
    requests: list[str] = []

    def answer(request: httpx.Request) -> httpx.Response:
        requests.append(str(request.url))
        return httpx.Response(200, json=detail())

    client = httpx.Client(transport=httpx.MockTransport(answer))
    with Zipbox("zb_k", base_url=API, http_client=client) as zb:
        zb.sandboxes.resume("sbx")
    assert requests == [f"{API}/sandbox/sbx/resume"]


async def test_async_resume_preconnects_and_drops_on_a_moved_origin() -> None:
    box = Box(detail())
    client = httpx.AsyncClient(transport=httpx.MockTransport(box.answer_async))
    async with AsyncZipbox("zb_k", base_url=API, http_client=client) as zb:
        sandbox = await zb.sandboxes.get("sbx")
        await sandbox.resume()
        assert (await sandbox.exec("true")).exit_code == 0
    assert ("GET", f"{ORIGIN}/healthz", None) in box.seen

    moved = Box(detail(baseUrl=MOVED, boxToken="bt_moved"))
    client = httpx.AsyncClient(transport=httpx.MockTransport(moved.answer_async))
    async with AsyncZipbox("zb_k", base_url=API, http_client=client) as zb:
        sandbox = await zb.sandboxes.get("sbx")
        await sandbox.resume()
        await sandbox.exec("true")
    assert moved.seen[-1] == ("POST", f"{MOVED}/exec", "Bearer bt_moved")
