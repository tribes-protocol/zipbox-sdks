"""PTY reconnect policy (scripted) and the sync/async drivers (real websocket server)."""

from __future__ import annotations

import json
import threading
from collections.abc import Generator, Iterator
from typing import Any

import pytest
from websockets.sync.server import ServerConnection, serve

from zipbox import AsyncZipbox, Zipbox, ZipboxError
from zipbox._core import BoxTarget, ClientConfig, Core
from zipbox._effects import Emit, HttpRequest, HttpResponse, Sleep
from zipbox._pty import (
    PtyEnded,
    PtyOutput,
    WsClose,
    WsClosed,
    WsConnect,
    WsRecv,
    WsRefused,
    WsSend,
    new_state,
    pty_loop,
    socket_url,
)

KEY = "zb_" + "T" * 43


def _core() -> Core:
    core = Core(ClientConfig(api_key=KEY, base_url="http://api.invalid", rng=lambda: 1.0))
    core._targets["sbx"] = BoxTarget("https://demo.zbox.sh", "bt")
    return core


class Script:
    """Steps the loop, answering each effect with the next scripted reply."""

    def __init__(self, op: Generator[Any, Any, None]) -> None:
        self.op = op
        self.pending: Any = None
        self.throw: BaseException | None = None

    def next(self) -> Any:
        effect = self.op.throw(self.throw) if self.throw else self.op.send(self.pending)
        self.pending, self.throw = None, None
        return effect

    def reply(self, value: Any = None) -> None:
        self.pending = value

    def fail(self, error: BaseException) -> None:
        self.throw = error


def test_reconnect_ladder_grows_without_ceiling_and_resets_only_on_a_frame() -> None:
    state = new_state(None, 100, 30)
    script = Script(pty_loop(_core(), "sbx", state))
    delays = []
    for _ in range(12):
        effect = script.next()
        assert isinstance(effect, WsConnect)
        assert effect.subprotocols == ("tribes.session", "bt")
        assert "resume=1" not in effect.url
        script.fail(WsRefused(None))
        sleep = script.next()
        assert isinstance(sleep, Sleep)
        delays.append(sleep.seconds)
    assert delays == [0.5, 1, 2, 4, 8, 16, 30, 30, 30, 30, 30, 30]

    # A handshake that completes but delivers no frame does NOT reset the ladder.
    assert isinstance(script.next(), WsConnect)
    resize = script.next()
    assert isinstance(resize, WsSend)
    assert json.loads(resize.text) == {"t": "r", "c": 100, "r": 30}
    assert isinstance(script.next(), WsRecv)
    script.fail(WsClosed(1006))
    sleep = script.next()
    assert isinstance(sleep, Sleep) and sleep.seconds == 30

    # A received frame resets it; reconnects resume the same session and client.
    connect = script.next()
    assert isinstance(connect, WsConnect) and "resume=1" in connect.url
    assert f"session={state.session_id}" in connect.url
    assert f"clientId={state.client_id}" in connect.url
    assert isinstance(script.next(), WsSend)
    assert isinstance(script.next(), WsRecv)
    script.reply('{"t":"o","d":"hi"}')
    emitted = script.next()
    assert isinstance(emitted, Emit) and emitted.value == PtyOutput("hi")
    assert isinstance(script.next(), WsRecv)
    script.fail(WsClosed(1006))
    sleep = script.next()
    assert isinstance(sleep, Sleep) and sleep.seconds == 0.5


def test_blocked_sends_takeover_and_queued_input_flushes_on_connect() -> None:
    state = new_state("bash-00000000-0000-4000-8000-000000000000", 80, 24)
    state.pending.append('{"t":"i","d":"ls\\n"}')
    script = Script(pty_loop(_core(), "sbx", state))
    assert isinstance(script.next(), WsConnect)
    assert isinstance(script.next(), WsSend)  # resize
    flushed = script.next()
    assert isinstance(flushed, WsSend) and json.loads(flushed.text)["d"] == "ls\n"
    assert isinstance(script.next(), WsRecv)
    script.reply('{"t":"blocked","holder":{"clientId":"browser","label":"Chrome"}}')
    takeover = script.next()
    assert isinstance(takeover, WsSend) and json.loads(takeover.text) == {"t": "takeover"}


def test_exit_ends_the_loop_and_unknown_session_raises() -> None:
    state = new_state(None, 80, 24)
    script = Script(pty_loop(_core(), "sbx", state))
    script.next()
    script.next()
    assert isinstance(script.next(), WsRecv)
    script.reply('{"t":"x","code":7}')
    ended = script.next()
    assert isinstance(ended, Emit) and ended.value == PtyEnded(7)
    assert isinstance(script.next(), WsClose)
    with pytest.raises(StopIteration):
        script.next()
    assert state.exit_code == 7

    script = Script(pty_loop(_core(), "sbx", new_state(None, 80, 24)))
    script.next()
    script.next()
    script.next()
    script.fail(WsClosed(4404))
    with pytest.raises(ZipboxError) as caught:
        script.next()
    assert caught.value.code == "not_found"


def test_revoked_close_drops_the_cached_token() -> None:
    core = _core()
    script = Script(pty_loop(core, "sbx", new_state(None, 80, 24)))
    script.next()
    script.next()
    script.next()
    script.fail(WsClosed(4401))
    assert isinstance(script.next(), Sleep)
    assert "sbx" not in core._targets


def _detail_response(token: str) -> HttpResponse:
    body = {"id": "sbx", "status": "running", "ready": True, "baseUrl": "https://demo.zbox.sh"}
    return HttpResponse(200, {}, json.dumps({**body, "boxToken": token}).encode())


def test_box_401_refetches_the_token_once_then_raises_unauthorized() -> None:
    core = _core()
    script = Script(pty_loop(core, "sbx", new_state(None, 80, 24)))
    assert isinstance(script.next(), WsConnect)
    script.fail(WsRefused(401))
    refetch = script.next()
    assert isinstance(refetch, HttpRequest) and refetch.url.endswith("/sandbox/sbx")
    script.reply(_detail_response("bt_rotated"))
    connect = script.next()
    assert isinstance(connect, WsConnect) and connect.subprotocols[1] == "bt_rotated"
    script.fail(WsRefused(401))
    with pytest.raises(ZipboxError) as caught:
        script.next()
    assert caught.value.code == "unauthorized" and caught.value.status == 401


def test_a_401_after_the_box_accepted_a_handshake_refetches_again() -> None:
    core = _core()
    script = Script(pty_loop(core, "sbx", new_state(None, 80, 24)))
    script.next()
    script.fail(WsRefused(401))
    script.next()
    script.reply(_detail_response("bt_2"))
    assert isinstance(script.next(), WsConnect)  # accepted
    assert isinstance(script.next(), WsSend)
    assert isinstance(script.next(), WsRecv)
    script.fail(WsClosed(1006))
    assert isinstance(script.next(), Sleep)
    assert isinstance(script.next(), WsConnect)
    script.fail(WsRefused(401))
    refetch = script.next()
    assert isinstance(refetch, HttpRequest)


class Box:
    """A terminal websocket that echoes input and drops the first connection."""

    def __init__(self) -> None:
        self.urls: list[str] = []
        self.protocols: list[str] = []
        self.received: list[dict[str, Any]] = []
        self.connections = 0
        self._server = serve(self.handle, "127.0.0.1", 0, subprotocols=["tribes.session"])  # type: ignore[list-item]
        self.port = self._server.socket.getsockname()[1]
        threading.Thread(target=self._server.serve_forever, daemon=True).start()

    def handle(self, ws: ServerConnection) -> None:
        self.connections += 1
        assert ws.request is not None
        self.urls.append(ws.request.path)
        self.protocols.append(ws.request.headers.get("Sec-WebSocket-Protocol", ""))
        ws.send(json.dumps({"t": "o", "d": f"hello {self.connections}\r\n"}))
        if self.connections == 1:
            ws.close()
            return
        for raw in ws:
            message = json.loads(raw)
            self.received.append(message)
            if message.get("t") == "i":
                ws.send(json.dumps({"t": "o", "d": message["d"]}))
                if message["d"] == "exit\n":
                    ws.send(json.dumps({"t": "x", "code": 0}))

    def stop(self) -> None:
        self._server.shutdown()


@pytest.fixture
def box() -> Iterator[Box]:
    server = Box()
    yield server
    server.stop()


def test_sync_pty_reconnects_and_streams(box: Box) -> None:
    with Zipbox(
        KEY, base_url="http://api.invalid", pty_base_delay_s=0.01, pty_max_delay_s=0.05
    ) as zb:
        zb._core._targets["sbx"] = BoxTarget(f"http://127.0.0.1:{box.port}", "bt")
        pty = zb.sandbox("sbx").pty.create(cols=120, rows=40)
        chunks: list[str] = []
        for chunk in pty:
            chunks.append(chunk)
            if chunk.startswith("hello 2"):
                pty.write("exit\n")
        assert pty.wait() == 0
    assert chunks[0] == "hello 1\r\n" and chunks[1] == "hello 2\r\n" and "exit\n" in chunks
    assert box.protocols[0] == "tribes.session, bt"
    assert "resume=1" not in box.urls[0] and "resume=1" in box.urls[1]
    assert {"t": "r", "c": 120, "r": 40} in box.received


async def test_async_pty_reconnects_and_streams(box: Box) -> None:
    async with AsyncZipbox(
        KEY, base_url="http://api.invalid", pty_base_delay_s=0.01, pty_max_delay_s=0.05
    ) as zb:
        zb._core._targets["sbx"] = BoxTarget(f"http://127.0.0.1:{box.port}", "bt")
        pty = await zb.sandbox("sbx").pty.create()
        chunks: list[str] = []
        async for chunk in pty:
            chunks.append(chunk)
            if chunk.startswith("hello 2"):
                await pty.write("exit\n")
        assert await pty.wait() == 0
    assert chunks[:2] == ["hello 1\r\n", "hello 2\r\n"]
    assert "resume=1" in box.urls[1]


def test_sync_detach_stops_reconnecting(box: Box) -> None:
    with Zipbox(
        KEY, base_url="http://api.invalid", pty_base_delay_s=0.01, pty_max_delay_s=0.05
    ) as zb:
        zb._core._targets["sbx"] = BoxTarget(f"http://127.0.0.1:{box.port}", "bt")
        pty = zb.sandbox("sbx").pty.create()
        for chunk in pty:
            if chunk.startswith("hello 2"):
                break
        pty.detach()
        assert box.connections == 2


def test_socket_url_keeps_a_base_path() -> None:
    state = new_state("bash-x", 80, 24)
    url = socket_url("https://term.example.test/42001", state, resume=True)
    assert url.startswith("wss://term.example.test/42001/?session=bash-x&")
    assert url.endswith("&resume=1")
    assert socket_url("https://demo.zbox.sh", state, resume=False).startswith(
        "wss://demo.zbox.sh/?"
    )
