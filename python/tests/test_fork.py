"""Forking a box: one Idempotency-Key per call, and the Tailscale key stays local."""

from __future__ import annotations

import asyncio
import json
import time
from typing import Any

import pytest

from mock_server import MockServer
from zipbox import AsyncZipbox, Zipbox, ZipboxError
from zipbox._core import Core, resolve_config
from zipbox._effects import HttpRequest, HttpResponse, Sleep

AUTH_KEY = "tskey-auth-kForkUnitTest-NotARealKey0123456789"
BOX_KEY = (
    "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAENj+qimupfFq/bb5ok8+RD0Bh7Opby1hPK28tSUvvLn58j9QL"
    "IPm1e1mfo7z5H/kbmtn7lKcNZp7cT+1Lf5Enog=="
)
FORK = {
    "operationId": "fork_1",
    "sourceSandboxId": "src",
    "childSandboxId": "kid",
    "sourcePolicy": "pause_and_resume",
    "phase": "queued",
    "sourceRecovery": "pending",
    "attempt": 0,
    "createdAt": "2026-11-05T00:00:00.000Z",
    "updatedAt": "2026-11-05T00:00:00.000Z",
}
CHILD = {"id": "kid", "name": "demo-2", "status": "provisioning", "privateMode": True}
RUNNING = {**CHILD, "status": "running", "agentPublicKey": BOX_KEY, "ready": False}


def _respond(status: int, body: Any) -> HttpResponse:
    return HttpResponse(status, {}, json.dumps(body).encode())


def test_fork_reuses_one_key_across_retries_and_never_sends_the_auth_key() -> None:
    core = Core(resolve_config("zb_k", "https://api.test", 3, retry_base_delay_s=0.0))
    op = core.fork(
        "src", name="demo-2", source_policy="pause_and_resume", tailscale_auth_key=AUTH_KEY
    )
    first = op.send(None)
    assert isinstance(first, HttpRequest)
    assert first.url == "https://api.test/sandbox/src/fork"
    assert json.loads(first.body or b"") == {"name": "demo-2", "sourcePolicy": "pause_and_resume"}
    key = first.headers["idempotency-key"]
    assert len(key) == 36
    unavailable = {"error": {"code": "unavailable", "message": "busy", "retryable": True}}
    assert isinstance(op.send(_respond(503, unavailable)), Sleep)
    retry = op.send(None)
    assert isinstance(retry, HttpRequest)
    assert retry.headers["idempotency-key"] == key and retry.body == first.body
    sent: list[HttpRequest] = [first, retry]
    accepted = {"operationId": "fork_1", "sandbox": CHILD, "fork": FORK}
    request = op.send(_respond(202, accepted))
    for response in (RUNNING, RUNNING):  # the wait, then connect's fresh key read
        assert isinstance(request, HttpRequest) and request.method == "GET"
        sent.append(request)
        request = op.send(_respond(200, response))
    assert isinstance(request, HttpRequest) and request.method == "PUT"
    assert request.url == "https://api.test/sandbox/kid/tailscale"
    sent.append(request)
    with pytest.raises(StopIteration) as done:
        op.send(_respond(202, {"pending": True}))
    detail = done.value.value
    assert detail.id == "kid" and detail.status == "running"
    for each in sent:
        assert AUTH_KEY.encode() not in (each.body or b"")
        assert AUTH_KEY not in repr(each)
    assert AUTH_KEY not in repr(detail) + str(detail)


@pytest.mark.parametrize(
    ("auth_key", "wait"), [("   ", True), (AUTH_KEY, False)], ids=["blank", "no-wait"]
)
def test_a_tailscale_key_the_fork_cannot_use_is_refused_before_any_request(
    auth_key: str, wait: bool
) -> None:
    core = Core(resolve_config("zb_k", "https://api.test", 3))
    op = core.fork(
        "src",
        name="demo-2",
        source_policy="require_paused",
        tailscale_auth_key=auth_key,
        wait=wait,
    )
    with pytest.raises(ZipboxError) as caught:
        op.send(None)
    assert caught.value.code == "invalid_request"
    assert AUTH_KEY not in str(caught.value) + repr(caught.value)


def test_a_forked_handle_and_its_info_carry_no_auth_key() -> None:
    server = MockServer()
    server.start()
    accepted = {"operationId": "fork_1", "sandbox": CHILD, "fork": FORK}
    server.load(
        [
            {
                "request": {"method": "POST", "path": "/sandbox/src/fork"},
                "response": {"status": 202, "body": accepted},
            },
            {
                "request": {"method": "GET", "path": "/sandbox/kid"},
                "response": {"status": 200, "body": RUNNING},
            },
            {
                "request": {"method": "GET", "path": "/sandbox/kid"},
                "response": {"status": 200, "body": RUNNING},
            },
            {
                "request": {"method": "PUT", "path": "/sandbox/kid/tailscale"},
                "response": {"status": 202, "body": {"pending": True}},
            },
        ]
    )
    try:
        with Zipbox("zb_" + "T" * 43, base_url=server.base_url, poll_interval_s=0.001) as zb:
            child = zb.sandbox("src").fork(
                name="demo-2", source_policy="pause_and_resume", tailscale_auth_key=AUTH_KEY
            )
        assert not server.errors and not server.unserved()
        assert child.id == "kid"
        dumped = repr(child) + str(child) + repr(child.info) + str(child.info)
        assert AUTH_KEY not in dumped
        assert all(AUTH_KEY.encode() not in body for body in server.bodies)
    finally:
        server.stop()


def _a_fork_that_keeps_provisioning() -> MockServer:
    """Accepts the fork, then answers every read of the new box with provisioning."""
    server = MockServer()
    server.start()
    accepted = {"operationId": "fork_1", "sandbox": CHILD, "fork": FORK}
    provisioning = {
        "request": {"method": "GET", "path": "/sandbox/kid"},
        "response": {"status": 200, "body": {**CHILD, "ready": False}},
    }
    server.load(
        [
            {
                "request": {"method": "POST", "path": "/sandbox/src/fork"},
                "response": {"status": 202, "body": accepted},
            },
            *[provisioning] * 10_000,
        ]
    )
    return server


def _only_its_own_wait_ended(server: MockServer, served: int) -> None:
    """``served`` is the request count when the wait ended, read 0.3 s before now."""
    # Every request past the POST matched a read of the new box: no cancel, no delete.
    assert not server.errors, server.errors
    assert served >= 2, f"the wait read the new box: {served}"
    # At most the one read already in flight when the wait ended: no poll goes on.
    assert server.index <= served + 1, f"sent after the wait ended: {server.index} > {served}"


async def test_a_deadline_on_an_async_fork_ends_only_its_own_wait() -> None:
    server = _a_fork_that_keeps_provisioning()
    try:
        async with AsyncZipbox(
            "zb_" + "T" * 43,
            base_url=server.base_url,
            poll_interval_s=0.001,
            poll_max_interval_s=0.002,
        ) as zb:
            with pytest.raises(asyncio.TimeoutError):
                await asyncio.wait_for(
                    zb.sandbox("src").fork(name="demo-2", source_policy="require_paused"), 0.2
                )
            served = server.index
            await asyncio.sleep(0.3)
            _only_its_own_wait_ended(server, served)
    finally:
        server.stop()


def test_interrupting_a_sync_fork_ends_only_its_own_wait(monkeypatch: pytest.MonkeyPatch) -> None:
    """The sync client's own way to stop a wait is the caller's interrupt (Ctrl-C), or
    ``wait=False`` and its own reads."""
    server = _a_fork_that_keeps_provisioning()
    sleeps = 0
    real_sleep = time.sleep

    def interrupt_on_the_third_pause(seconds: float) -> None:
        nonlocal sleeps
        sleeps += 1
        if sleeps == 3:
            raise KeyboardInterrupt
        real_sleep(seconds)

    try:
        with Zipbox(
            "zb_" + "T" * 43,
            base_url=server.base_url,
            poll_interval_s=0.001,
            poll_max_interval_s=0.002,
        ) as zb:
            monkeypatch.setattr("zipbox._effects.time.sleep", interrupt_on_the_third_pause)
            with pytest.raises(KeyboardInterrupt):
                zb.sandbox("src").fork(name="demo-2", source_policy="require_paused")
            monkeypatch.undo()
            served = server.index
            time.sleep(0.3)
            _only_its_own_wait_ended(server, served)
    finally:
        server.stop()


def test_the_mock_server_holds_same_to_the_previous_requests_header() -> None:
    server = MockServer()
    server.start()
    exchange = {
        "request": {
            "method": "POST",
            "path": "/sandbox/src/fork",
            "headers": {"idempotency-key": "{{SAME}}"},
        },
        "response": {"status": 202, "body": {}},
    }
    server.load(
        [{**exchange, "request": {"method": "POST", "path": "/sandbox/src/fork"}}, exchange]
    )
    try:
        server.serve("POST", "/sandbox/src/fork", {"idempotency-key": "one"}, b"")
        server.serve("POST", "/sandbox/src/fork", {"idempotency-key": "two"}, b"")
        assert len(server.errors) == 1 and "previous 'one'" in server.errors[0]
    finally:
        server.stop()
