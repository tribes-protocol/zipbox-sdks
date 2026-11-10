"""Unit tests for the pieces the conformance fixtures do not isolate."""

from __future__ import annotations

import contextlib
import json
import socket
import threading
import time
from collections.abc import Iterator
from typing import Any

import httpx
import pytest

from zipbox import ZipboxError
from zipbox._backoff import Backoff
from zipbox._core import _SCREENSHOT, BoxTarget, Core, exec_body, resolve_config, resolve_path
from zipbox._effects import HttpRequest, HttpResponse, Sleep
from zipbox._errors import code_for_status, error_from_response, parse_retry_after_ms
from zipbox._models import ExecResult
from zipbox._sse import SseParser
from zipbox._transport import HttpxAsyncTransport, HttpxSyncTransport, _network_error

EXIT = b'id: 6-0\nevent: exit\ndata: {"state":"exited","exitCode":0}\n\n'


def test_sse_survives_any_chunking_including_split_multibyte() -> None:
    payload = 'id: 4-0\nevent: stdout\ndata: {"data":"hé€\U0001f600"}\n\n'.encode() + EXIT
    for size in range(1, len(payload)):
        parser = SseParser()
        messages = []
        for start in range(0, len(payload), size):
            messages += parser.feed(payload[start : start + size])
        assert [m.event for m in messages] == ["stdout", "exit"]
        assert messages[0].data == '{"data":"hé€\U0001f600"}'
        assert messages[0].id == "4-0"
        assert parser.last_id == "6-0"


def test_sse_handles_crlf_comments_and_multiline_data() -> None:
    parser = SseParser()
    out = parser.feed(b": keepalive\r\n\r\nevent: gap\r\ndata: a\r\ndata: b\r\n\r\n")
    assert len(out) == 1
    assert out[0].event == "gap"
    assert out[0].data == "a\nb"


def test_backoff_doubles_to_the_cap_with_jitter_and_resets() -> None:
    backoff = Backoff(0.5, 30.0, rng=lambda: 1.0)
    delays = [backoff.next() for _ in range(10)]
    assert delays[:7] == [0.5, 1.0, 2.0, 4.0, 8.0, 16.0, 30.0]
    assert delays[7:] == [30.0, 30.0, 30.0]
    low = Backoff(1.0, 30.0, rng=lambda: 0.0)
    assert low.next() == 0.5
    backoff.reset()
    assert backoff.next() == 0.5


def test_backoff_has_no_attempt_ceiling() -> None:
    backoff = Backoff(0.5, 30.0, rng=lambda: 0.5)
    for _ in range(10_000):
        assert 0 < backoff.next() <= 30.0


def test_wait_ready_polls_a_settling_box_on_a_gentle_ladder() -> None:
    # A box turns ready ~1 s after it serves; doubling slept up to seconds past it.
    core = Core(resolve_config("zb_k", "https://api.test", 0, rng=lambda: 1.0))
    op = core.wait_ready("sbx")
    settling = {"id": "sbx", "status": "running", "ready": False, "readiness": "settling"}
    assert isinstance(op.send(None), HttpRequest)
    delays = []
    for _ in range(13):
        sleep = op.send(HttpResponse(200, {}, json.dumps(settling).encode()))
        assert isinstance(sleep, Sleep)
        delays.append(sleep.seconds)
        assert isinstance(op.send(None), HttpRequest)
    expected = [min(10.0, 1.25**n) for n in range(13)]
    assert delays == pytest.approx(expected)
    assert delays[:3] == pytest.approx([1.0, 1.25, 1.5625]) and delays[-2:] == [10.0, 10.0]


def test_relative_paths_resolve_against_the_workspace() -> None:
    assert resolve_path("a/b.txt") == "/root/workspace/a/b.txt"
    assert resolve_path("./x") == "/root/workspace/x"
    assert resolve_path("../etc/hosts") == "/root/etc/hosts"
    assert resolve_path("/etc//hostname") == "/etc/hostname"
    assert resolve_path("/") == "/"


def test_exec_body_omits_absent_options() -> None:
    assert exec_body("ls") == {"cmd": "ls"}
    assert exec_body(["ls", "-la"]) == {"argv": ["ls", "-la"]}
    assert exec_body("x", cwd="/tmp", env={"A": "1"}, timeout_ms=5, user="dev", stdin=True) == {
        "cmd": "x",
        "cwd": "/tmp",
        "env": {"A": "1"},
        "timeoutMs": 5,
        "user": "dev",
        "stdin": True,
    }


def test_status_mapping() -> None:
    assert code_for_status(413) == "invalid_request"
    assert code_for_status(502) == "box_not_running"
    assert code_for_status(503) == "unavailable"
    assert code_for_status(418) == "invalid_request"


def test_envelope_and_legacy_bodies() -> None:
    envelope = error_from_response(
        429,
        {"retry-after": "2"},
        b'{"error":{"code":"rate_limited","message":"slow","requestId":"r1","retryable":true}}',
    )
    assert (envelope.code, envelope.request_id, envelope.retry_after_ms) == (
        "rate_limited",
        "r1",
        2000,
    )
    assert envelope.retryable
    assert envelope.docs_url == "https://zipbox.ai/docs/errors#rate_limited"
    legacy = error_from_response(409, {}, b'{"error":"ide_not_ready"}')
    assert (legacy.code, legacy.message, legacy.retryable) == ("conflict", "ide_not_ready", False)
    html = error_from_response(503, {}, b"<html>bad gateway</html>")
    assert html.code == "unavailable" and html.retryable


def test_machine_limit_code_is_kept_and_not_retried() -> None:
    error = error_from_response(
        403,
        {},
        b'{"error":{"code":"machine_limit_reached","message":"limit of 250 machines",'
        b'"requestId":"r1","retryable":false,'
        b'"docsUrl":"https://zipbox.ai/docs/errors#machine_limit_reached"}}',
    )
    assert (error.code, error.status, error.message) == (
        "machine_limit_reached",
        403,
        "limit of 250 machines",
    )
    assert not error.retryable
    assert error.docs_url == "https://zipbox.ai/docs/errors#machine_limit_reached"


def test_missing_key_is_an_error(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("ZIPBOX_API_KEY", raising=False)
    with pytest.raises(ZipboxError) as caught:
        resolve_config(None, None, 3)
    assert caught.value.code == "unauthorized"


def test_env_supplies_key_and_url(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("ZIPBOX_API_KEY", "zb_env")
    monkeypatch.setenv("ZIPBOX_API_URL", "http://localhost:8787/")
    config = resolve_config(None, None, 3)
    assert (config.api_key, config.base_url) == ("zb_env", "http://localhost:8787")
    monkeypatch.delenv("ZIPBOX_API_URL")
    assert resolve_config(None, None, 3).base_url == "https://api.tribes.xyz"


def test_retry_after_accepts_seconds_and_an_http_date() -> None:
    from email.utils import formatdate

    assert parse_retry_after_ms({"retry-after": "2"}) == 2000
    later = parse_retry_after_ms({"retry-after": formatdate(time.time() + 60, usegmt=True)})
    assert later is not None and 55_000 <= later <= 60_000
    assert parse_retry_after_ms({"retry-after": "soon"}) is None


def test_secrets_never_appear_in_reprs() -> None:
    config = resolve_config("zb_secretkey", "https://api.test", 3)
    request = HttpRequest("GET", "https://box/exec", {"authorization": "Bearer bt_secret"})
    target = BoxTarget("https://demo.zbox.sh", "bt_secret")
    dumped = repr(config) + repr(request) + repr(target)
    assert "zb_secretkey" not in dumped and "bt_secret" not in dumped


def test_box_detail_keeps_the_token_in_the_cache_only() -> None:
    core = Core(resolve_config("zb_k", "https://api.test", 0))
    body = {"id": "sbx", "status": "running", "ready": True, "baseUrl": "https://b.zbox.sh"}
    op = core.get("sbx")
    op.send(None)
    with pytest.raises(StopIteration) as done:
        op.send(HttpResponse(200, {}, json.dumps({**body, "boxToken": "bt_secret"}).encode()))
    detail = done.value.value
    assert "boxToken" not in detail.raw and core._targets["sbx"].token == "bt_secret"


@pytest.mark.parametrize(("status", "retryable"), [("running", True), ("archived", False)])
def test_box_not_ready_is_retryable_only_while_it_can_come_back(
    status: str, retryable: bool
) -> None:
    core = Core(resolve_config("zb_k", "https://api.test", 0))
    op = core.target("sbx")
    op.send(None)
    body = {"id": "sbx", "status": status, "ready": False}
    with pytest.raises(ZipboxError) as caught:
        op.send(HttpResponse(200, {}, json.dumps(body).encode()))
    assert caught.value.code == "box_not_running" and caught.value.retryable is retryable


# audit-ax AX-03: retryable follows the server's readiness, and a private box awaiting
# its tailnet names the remedy instead of "start it".
@pytest.mark.parametrize(
    ("readiness", "status", "code", "retryable"),
    [
        ("settling", "archived", "box_not_running", True),
        ("paused", "archived", "box_not_running", False),
        ("start_failed", "archived", "box_not_running", False),
        ("awaits_tailnet", "running", "tailnet_not_connected", False),
    ],
)
def test_box_not_ready_follows_the_servers_readiness(
    readiness: str, status: str, code: str, retryable: bool
) -> None:
    core = Core(resolve_config("zb_k", "https://api.test", 0))
    op = core.target("sbx")
    op.send(None)
    body = {"id": "sbx", "status": status, "ready": False, "readiness": readiness}
    with pytest.raises(ZipboxError) as caught:
        op.send(HttpResponse(200, {}, json.dumps(body).encode()))
    assert caught.value.code == code and caught.value.retryable is retryable


# audit-ax AX-23: a follower's ladder never retries sooner than the server asked.
def test_a_followed_command_waits_out_retry_after() -> None:
    core = Core(resolve_config("zb_k", "https://api.test", 0, retry_base_delay_s=0.001))
    op = core.command_events("sbx", "cmd_1", None)
    detail = {
        "id": "sbx",
        "status": "running",
        "ready": True,
        "baseUrl": "https://b.zbox.sh",
        "boxToken": "bt",
    }
    assert isinstance(op.send(None), HttpRequest)
    events = op.send(HttpResponse(200, {}, json.dumps(detail).encode()))
    assert isinstance(events, HttpRequest) and "/events" in events.url
    effect = op.send(HttpResponse(503, {"retry-after": "30"}, b'{"error":"busy"}'))
    assert isinstance(effect, Sleep) and effect.seconds >= 30


# audit-ax AX-17: one rule. An envelope that says it is not retryable is not retried,
# whatever its code, and Retry-After wins over the envelope's own hint.
def test_retry_follows_retryable_and_the_header_wins() -> None:
    body = b'{"error":{"code":"unavailable","message":"no","retryable":false,"retryAfterMs":5}}'
    error = error_from_response(503, {"retry-after": "2"}, body)
    assert error.retryable is False and error.retry_after_ms == 2000
    core = Core(resolve_config("zb_k", "https://api.test", 3))
    op = core.control("GET", "/sandbox")
    op.send(None)
    with pytest.raises(ZipboxError) as caught:
        op.send(HttpResponse(503, {}, body))
    assert caught.value.code == "unavailable"


def test_screenshot_uses_the_images_ffmpeg_and_fails_loudly() -> None:
    # The platform image has no ImageMagick; pipefail turns a failed capture into a
    # non-zero exit instead of an empty PNG.
    assert _SCREENSHOT.startswith("set -o pipefail;")
    assert 'ffmpeg -loglevel error -f x11grab -i "${DISPLAY:-:10}"' in _SCREENSHOT
    assert "import " not in _SCREENSHOT


def test_only_connect_phase_failures_count_as_unsent() -> None:
    # ConnectError covers DNS, refusal and the TLS handshake a fresh box name fails
    # until its certificate lands (#4139); a read error may follow a sent request.
    assert _network_error(httpx.ConnectError("tlsv1 alert internal error")).unsent is True
    assert _network_error(httpx.ConnectTimeout("connect")).unsent is True
    assert _network_error(httpx.ReadError("reset")).unsent is False


def test_an_unsent_post_retries_past_max_retries_until_it_connects() -> None:
    # #4139: exec on a box whose HTTPS was not up yet failed at once — only GETs and
    # retry=True requests were retried, at most max_retries times. Nothing was sent.
    core = Core(resolve_config("zb_k", "https://api.test", 0))
    request = HttpRequest("POST", "https://box.test/exec", {})
    op = core.send(request, retry=False)
    assert op.send(None) == request
    for _ in range(5):
        effect = op.throw(_network_error(httpx.ConnectError("tlsv1 alert internal error")))
        assert isinstance(effect, Sleep)
        assert op.send(None) == request
    with pytest.raises(StopIteration) as done:
        op.send(HttpResponse(200, {}, b"{}"))
    assert done.value.value.status == 200


def test_a_post_whose_failure_may_follow_the_send_is_not_repeated() -> None:
    core = Core(resolve_config("zb_k", "https://api.test", 3))
    op = core.send(HttpRequest("POST", "https://box.test/exec", {}), retry=False)
    op.send(None)
    with pytest.raises(ZipboxError) as caught:
        op.throw(_network_error(httpx.ReadError("reset")))
    assert caught.value.code == "unavailable"


@contextlib.contextmanager
def _ends_every_handshake() -> Iterator[int]:
    """A TCP listener that accepts and closes at once: a TLS handshake that ends in EOF,
    the failure a box's retired boot route produced (plan 202611-zipbox-lifecycle-speed)."""
    listener = socket.create_server(("127.0.0.1", 0))

    def serve() -> None:
        while True:
            try:
                conn, _ = listener.accept()
            except OSError:
                return
            conn.close()

    threading.Thread(target=serve, daemon=True).start()
    try:
        yield listener.getsockname()[1]
    finally:
        listener.close()


def test_a_tls_handshake_that_ends_in_eof_is_unsent_on_the_box_path() -> None:
    with _ends_every_handshake() as port:
        core = Core(resolve_config("zb_k", "https://api.test", 0))
        core._targets["sbx"] = BoxTarget(f"https://127.0.0.1:{port}", "bt")
        op = core.box("sbx", "POST", "/exec", body={"cmd": "true"})
        request = op.send(None)
        assert isinstance(request, HttpRequest) and request.method == "POST"
        transport = HttpxSyncTransport()
        try:
            with pytest.raises(ZipboxError) as caught:
                transport.send(request)
        finally:
            transport.close()
    assert caught.value.unsent is True
    # Nothing was sent, so even a POST waits a rung and re-reads the box.
    assert isinstance(op.throw(caught.value), Sleep)


async def test_an_async_tls_handshake_that_ends_in_eof_is_unsent_on_the_box_path() -> None:
    with _ends_every_handshake() as port:
        core = Core(resolve_config("zb_k", "https://api.test", 0))
        core._targets["sbx"] = BoxTarget(f"https://127.0.0.1:{port}", "bt")
        op = core.box("sbx", "POST", "/exec", body={"cmd": "true"})
        request = op.send(None)
        assert isinstance(request, HttpRequest)
        transport = HttpxAsyncTransport()
        try:
            with pytest.raises(ZipboxError) as caught:
                await transport.send(request)
        finally:
            await transport.aclose()
    assert caught.value.unsent is True
    assert isinstance(op.throw(caught.value), Sleep)


def _ready(base_url: str, token: str) -> HttpResponse:
    body = {"id": "sbx", "status": "running", "ready": True, "baseUrl": base_url}
    return HttpResponse(200, {}, json.dumps({**body, "boxToken": token}).encode())


def _refused() -> ZipboxError:
    return _network_error(httpx.ConnectError("[Errno 111] Connection refused"))


def test_an_unsent_box_failure_rereads_the_box_and_follows_its_new_base_url() -> None:
    # After a claim a box's baseUrl moves once, from its boot name to box--<name>. A
    # client that redialled the cached origin hung forever once the boot route retired.
    core = Core(resolve_config("zb_k", "https://api.test", 0, rng=lambda: 1.0))
    core._targets["sbx"] = BoxTarget("https://box--boot.zbox.sh", "bt_boot")
    op = core.box("sbx", "POST", "/exec", body={"cmd": "true"})
    first = op.send(None)
    assert isinstance(first, HttpRequest) and first.url == "https://box--boot.zbox.sh/exec"
    delays = []
    for _ in range(4):
        sleep = op.throw(_refused())
        assert isinstance(sleep, Sleep)
        delays.append(sleep.seconds)
        reread = op.send(None)
        assert isinstance(reread, HttpRequest) and reread.method == "GET"
        assert reread.url == "https://api.test/sandbox/sbx"
        retry = op.send(_ready("https://box--boot.zbox.sh", "bt_boot"))
        assert isinstance(retry, HttpRequest) and retry.method == "POST"
    # A successful detail read never resets the ladder; only an answered box request does.
    assert delays == [0.5, 1.0, 2.0, 4.0]
    sleep = op.throw(_refused())
    assert isinstance(sleep, Sleep) and sleep.seconds == 8.0
    op.send(None)
    moved = op.send(_ready("https://box--demo.zbox.sh", "bt_live"))
    assert isinstance(moved, HttpRequest) and moved.url == "https://box--demo.zbox.sh/exec"
    assert moved.headers["authorization"] == "Bearer bt_live"
    with pytest.raises(StopIteration) as done:
        op.send(HttpResponse(200, {}, b"{}"))
    assert done.value.value.status == 200


def test_the_box_unsent_ladder_resets_when_the_box_answers() -> None:
    core = Core(resolve_config("zb_k", "https://api.test", 0, rng=lambda: 1.0))
    core._targets["sbx"] = BoxTarget("https://box--demo.zbox.sh", "bt_old")
    op = core.box("sbx", "POST", "/exec")
    op.send(None)
    for expected in (0.5, 1.0):
        sleep = op.throw(_refused())
        assert isinstance(sleep, Sleep) and sleep.seconds == expected
        op.send(None)
        op.send(_ready("https://box--demo.zbox.sh", "bt_old"))
    # The box answers (a 401, which re-reads the token), then refuses again.
    refetch = op.send(HttpResponse(401, {}, b""))
    assert isinstance(refetch, HttpRequest) and refetch.method == "GET"
    op.send(_ready("https://box--demo.zbox.sh", "bt_new"))
    sleep = op.throw(_refused())
    assert isinstance(sleep, Sleep) and sleep.seconds == 0.5


def test_a_second_401_says_the_box_has_not_received_a_live_key() -> None:
    # #4137: the old message ("was the key deleted?") was false — the control plane had
    # just re-issued the token, which it never does for a deleted key.
    core = Core(resolve_config("zb_k", "https://api.test", 0))
    core._targets["sbx"] = BoxTarget("https://b.zbox.sh", "bt_old")
    op = core.box("sbx", "POST", "/exec")
    op.send(None)
    refetch = op.send(HttpResponse(401, {}, b""))
    assert isinstance(refetch, HttpRequest) and refetch.method == "GET"
    body = {"id": "sbx", "status": "running", "ready": True, "baseUrl": "https://b.zbox.sh"}
    op.send(HttpResponse(200, {}, json.dumps({**body, "boxToken": "bt_new"}).encode()))
    with pytest.raises(ZipboxError) as caught:
        op.send(HttpResponse(401, {}, b""))
    assert caught.value.code == "unauthorized" and caught.value.retryable is True
    assert "has not received" in caught.value.message


@pytest.mark.parametrize(
    ("origin", "apex"),
    [
        ("https://box--demo.zbox.sh", "demo.zbox.sh"),
        ("https://box--box--demo.zbox.sh", "box--demo.zbox.sh"),
        # A private box serves itself on box.<apex>; only one prefix is ever dropped.
        ("https://box.demo.zbox.sh", "demo.zbox.sh"),
        ("https://box--box.zbox.sh", "box.zbox.sh"),
        ("https://demo.zbox.sh", "demo.zbox.sh"),
        ("https://mailbox--demo.zbox.sh", "mailbox--demo.zbox.sh"),
        ("http://127.0.0.1:9100", "127.0.0.1"),
    ],
)
def test_preview_apex_preserves_the_transport_origin(origin: str, apex: str) -> None:
    target = BoxTarget(origin, "bt_test")
    assert target.host == apex
    assert target.base_url == origin


def test_preview_pending_beyond30_seconds_uses_same_operation(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    core = Core(resolve_config("key", "https://api.test", 0))
    core._targets["box"] = BoxTarget("https://box--demo.zbox.sh", "bt")
    calls: list[dict[str, Any]] = []

    def execute(_box: str, body: dict[str, Any]) -> Any:
        calls.append(body)
        if False:
            yield None
        pending = len(calls) <= 16
        return ExecResult(
            id="p",
            exit_code=75 if pending else 0,
            stdout=json.dumps({"status": "pending", "retryAfterMs": 31_000}) if pending else "",
            stderr="",
        )

    monkeypatch.setattr(core, "exec", execute)
    op = core.preview("box", 4000, "web")
    elapsed = 0.0
    for _ in range(16):
        effect = op.send(None)
        assert isinstance(effect, Sleep)
        assert effect.seconds >= 31
        elapsed += effect.seconds
    assert elapsed > 30
    with pytest.raises(StopIteration) as completed:
        op.send(None)
    assert completed.value.value.url == "https://web.demo.zbox.sh"
    assert (
        calls
        == [{"argv": ["tribes-caddy", "add", "web.demo.zbox.sh", "127.0.0.1:4000", "--no-wait"]}]
        * 17
    )
