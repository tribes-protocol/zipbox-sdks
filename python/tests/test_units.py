"""Unit tests for the pieces the conformance fixtures do not isolate."""

from __future__ import annotations

import json
import time

import pytest

from zipbox import ZipboxError
from zipbox._backoff import Backoff
from zipbox._core import _SCREENSHOT, BoxTarget, Core, exec_body, resolve_config, resolve_path
from zipbox._effects import HttpRequest, HttpResponse
from zipbox._errors import code_for_status, error_from_response, parse_retry_after_ms
from zipbox._sse import SseParser

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


def test_screenshot_uses_the_images_ffmpeg_and_fails_loudly() -> None:
    # The platform image has no ImageMagick; pipefail turns a failed capture into a
    # non-zero exit instead of an empty PNG.
    assert _SCREENSHOT.startswith("set -o pipefail;")
    assert 'ffmpeg -loglevel error -f x11grab -i "${DISPLAY:-:10}"' in _SCREENSHOT
    assert "import " not in _SCREENSHOT
