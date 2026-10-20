"""PTY sessions over the box's terminal websocket.

The reconnect policy is written once (``pty_loop``) as an effect generator, like the
rest of the core: a dropped socket re-attaches the same session with exponential
backoff, a cap and jitter, and no attempt ceiling; the ladder resets only when a
frame is actually received. ``Pty`` (thread) and ``AsyncPty`` (task) just perform the
effects.
"""

from __future__ import annotations

import asyncio
import contextlib
import json
import queue
import threading
import uuid
from collections.abc import AsyncIterator, Callable, Generator, Iterator
from dataclasses import dataclass, field
from typing import Any
from urllib.parse import quote, urlparse

from zipbox._backoff import Backoff
from zipbox._core import Core
from zipbox._effects import (
    AsyncTransport,
    Emit,
    Sleep,
    Streams,
    SyncTransport,
    perform_async,
    perform_sync,
)
from zipbox._errors import ZipboxError

SUBPROTOCOL = "tribes.session"
CLIENT_LABEL = "zipbox-sdk"
CLOSE_UNKNOWN_SESSION = 4404
CLOSE_REVOKED = 4401


@dataclass(frozen=True)
class WsConnect:
    """Open the terminal socket."""

    url: str
    subprotocols: tuple[str, ...]


@dataclass(frozen=True)
class WsRecv:
    """Receive one text frame from the open socket."""


@dataclass(frozen=True)
class WsSend:
    """Send one text frame on the open socket."""

    text: str


@dataclass(frozen=True)
class WsClose:
    """Close the open socket."""


class WsClosed(Exception):
    """The socket closed (``code`` from the close frame, when there was one)."""

    def __init__(self, code: int | None) -> None:
        super().__init__(f"websocket closed ({code})")
        self.code = code


class WsRefused(Exception):
    """The handshake failed (``status`` when the server answered HTTP)."""

    def __init__(self, status: int | None) -> None:
        super().__init__(f"websocket refused ({status})")
        self.status = status


@dataclass(frozen=True)
class PtyOutput:
    """Bytes the PTY printed (UTF-8 text)."""

    data: str


@dataclass(frozen=True)
class PtyEnded:
    """The session's shell exited (``exit_code``) or another client took it (``taken``)."""

    exit_code: int | None
    taken: bool = False


@dataclass
class PtyState:
    """What survives reconnects: the session, our client id, size and queued input."""

    session_id: str
    client_id: str
    cols: int
    rows: int
    stopping: bool = False
    connected: bool = False
    opened_once: bool = False
    pending: list[str] = field(default_factory=list)
    exit_code: int | None = None


def new_session_id() -> str:
    """A fresh API session id (``bash-<uuid4>``)."""
    return f"bash-{uuid.uuid4()}"


def socket_url(base_url: str, state: PtyState, resume: bool) -> str:
    """The terminal websocket URL for this session."""
    parsed = urlparse(base_url)
    scheme = "wss" if parsed.scheme == "https" else "ws"
    query = (
        f"session={quote(state.session_id, safe='')}&clientId={quote(state.client_id, safe='')}"
        f"&clientLabel={CLIENT_LABEL}"
    )
    if resume:
        query += "&resume=1"
    # The terminal is the root of the box's base URL, which may carry a path (a
    # local box answers under one).
    return f"{scheme}://{parsed.netloc}{parsed.path.rstrip('/')}/?{query}"


def frame(data: dict[str, Any]) -> str:
    """Serialize a client frame."""
    return json.dumps(data, separators=(",", ":"))


def pty_loop(core: Core, sandbox_id: str, state: PtyState) -> Generator[Any, Any, None]:
    """Hold one PTY session open until it exits, is taken over, or is closed."""
    config = core.config
    backoff = Backoff(config.pty_base_delay_s, config.pty_max_delay_s, rng=config.rng)
    resume = False
    # Set when a 401 made us re-read the token; a second 401 before the box accepts a
    # handshake again is an answered verdict (key deleted), not a transient.
    refetched = False
    while not state.stopping:
        try:
            target = yield from core.target(sandbox_id)
        except ZipboxError as error:
            if not error.retryable:
                raise
            yield Sleep(backoff.next())
            continue
        try:
            yield WsConnect(socket_url(target.base_url, state, resume), (SUBPROTOCOL, target.token))
        except WsRefused as refused:
            if refused.status == 401:
                if refetched:
                    raise ZipboxError(
                        "unauthorized",
                        "The box refused this key's token (was the key deleted?).",
                        status=401,
                    ) from refused
                refetched = True
                core.forget(sandbox_id)
                continue
            if refused.status in (400, 403, 404):
                raise ZipboxError(
                    "not_found" if refused.status == 404 else "forbidden",
                    f"The box refused the terminal socket ({refused.status}).",
                    status=refused.status,
                ) from refused
            yield Sleep(backoff.next())
            continue
        state.connected = True
        state.opened_once = True
        resume = True
        refetched = False  # the handshake accepted the token
        try:
            try:
                yield WsSend(frame({"t": "r", "c": state.cols, "r": state.rows}))
                while state.pending:
                    yield WsSend(state.pending[0])
                    state.pending.pop(0)
            except WsClosed:
                # The socket closed under us; frames it delivered before closing are
                # still readable, and the receive below drains them first.
                pass
            while True:
                text: str = yield WsRecv()
                backoff.reset()
                try:
                    message: Any = json.loads(text)
                except ValueError:
                    continue
                if not isinstance(message, dict):
                    continue
                kind = message.get("t")
                if kind == "o":
                    yield Emit(PtyOutput(str(message.get("d", ""))))
                elif kind == "x":
                    code = message.get("code")
                    state.exit_code = code if isinstance(code, int) else None
                    state.stopping = True
                    yield Emit(PtyEnded(state.exit_code))
                    yield WsClose()
                    return
                elif kind == "blocked":
                    yield WsSend(frame({"t": "takeover"}))
                elif kind == "taken":
                    state.stopping = True
                    yield Emit(PtyEnded(None, taken=True))
                    yield WsClose()
                    return
        except WsClosed as closed:
            state.connected = False
            if closed.code == CLOSE_UNKNOWN_SESSION:
                raise ZipboxError("not_found", f"No PTY session {state.session_id}.") from closed
            if closed.code == CLOSE_REVOKED:
                core.forget(sandbox_id)
        if state.stopping:
            return
        yield Sleep(backoff.next())


def _sync_ws_connect(effect: WsConnect) -> Any:
    from websockets.exceptions import InvalidStatus
    from websockets.sync.client import connect

    try:
        # Entering the connection is how websockets >= 17.1 marks a directly held
        # connection as intended (no deprecation warning); on 13 to 17.0 it returns self.
        return connect(
            effect.url,
            subprotocols=[*effect.subprotocols],  # type: ignore[list-item]
            max_size=None,
            open_timeout=30,
            close_timeout=2,
        ).__enter__()
    except InvalidStatus as error:
        raise WsRefused(error.response.status_code) from error
    except Exception as error:
        raise WsRefused(None) from error


class Pty:
    """A PTY session on a box (blocking API; the socket is held by a thread).

    Output arrives through ``on_data`` callbacks and by iterating the object.
    """

    def __init__(
        self, core: Core, transport: SyncTransport, sandbox_id: str, state: PtyState
    ) -> None:
        self._core = core
        self._transport = transport
        self._sandbox_id = sandbox_id
        self._state = state
        self._socket: Any = None
        self._lock = threading.Lock()
        self._stop = threading.Event()
        self._settled = threading.Event()
        self._done = threading.Event()
        self._error: BaseException | None = None
        self._callbacks: list[Callable[[str], None]] = []
        self._queue: queue.Queue[str | None] = queue.Queue()
        self._thread = threading.Thread(target=self._run, name="zipbox-pty", daemon=True)

    @property
    def session_id(self) -> str:
        """The session id; pass it to ``pty.attach`` to re-attach later."""
        return self._state.session_id

    @property
    def exit_code(self) -> int | None:
        """The shell's exit status once it has exited."""
        return self._state.exit_code

    def _start(self) -> Pty:
        self._thread.start()
        self._settled.wait()
        if self._error is not None and not self._state.opened_once:
            raise self._error
        return self

    def _run(self) -> None:
        streams = Streams()
        op = pty_loop(self._core, self._sandbox_id, self._state)
        send: Any = None
        throw: BaseException | None = None
        try:
            while True:
                try:
                    effect = op.throw(throw) if throw is not None else op.send(send)
                except StopIteration:
                    return
                throw, send = None, None
                try:
                    send = self._perform(effect, streams)
                except Exception as error:
                    throw = error
        except BaseException as error:
            self._error = error
        finally:
            op.close()
            self._drop_socket()
            self._done.set()
            self._settled.set()
            self._queue.put(None)

    def _perform(self, effect: Any, streams: Streams) -> Any:
        if isinstance(effect, Emit):
            value = effect.value
            if isinstance(value, PtyOutput):
                for callback in list(self._callbacks):
                    callback(value.data)
                self._queue.put(value.data)
            return None
        if isinstance(effect, Sleep):
            self._stop.wait(effect.seconds)
            return None
        if isinstance(effect, WsConnect):
            if self._stop.is_set():
                raise WsRefused(None)
            socket = _sync_ws_connect(effect)
            with self._lock:
                self._socket = socket
            self._settled.set()
            return None
        if isinstance(effect, WsRecv):
            return self._recv()
        if isinstance(effect, WsSend):
            self._send(effect.text)
            return None
        if isinstance(effect, WsClose):
            self._drop_socket()
            return None
        return perform_sync(self._transport, effect, streams)

    def _recv(self) -> str:
        from websockets.exceptions import ConnectionClosed

        with self._lock:
            socket = self._socket
        if socket is None:
            raise WsClosed(None)
        try:
            message = socket.recv()
        except ConnectionClosed as closed:
            # Already closed by the protocol: drop the reference without a second
            # close handshake (which would wait out close_timeout on some versions).
            with self._lock:
                if self._socket is socket:
                    self._socket = None
                self._state.connected = False
            raise WsClosed(closed.rcvd.code if closed.rcvd else None) from closed
        return message if isinstance(message, str) else bytes(message).decode("utf-8", "replace")

    def _send(self, text: str) -> None:
        from websockets.exceptions import ConnectionClosed

        with self._lock:
            socket = self._socket
        if socket is None:
            raise WsClosed(None)
        try:
            socket.send(text)
        except ConnectionClosed as closed:
            raise WsClosed(closed.rcvd.code if closed.rcvd else None) from closed

    def _drop_socket(self) -> None:
        with self._lock:
            socket, self._socket = self._socket, None
            self._state.connected = False
        if socket is not None:
            with contextlib.suppress(Exception):
                socket.close()

    def on_data(self, callback: Callable[[str], None]) -> None:
        """Call ``callback`` with every chunk of output (from the socket thread)."""
        self._callbacks.append(callback)

    def __iter__(self) -> Iterator[str]:
        """Iterate output until the session ends or is detached."""
        while True:
            item = self._queue.get()
            if item is None:
                self._queue.put(None)
                return
            yield item

    def _queue_or_send(self, text: str) -> None:
        with self._lock:
            connected = self._socket is not None
        if connected:
            try:
                self._send(text)
                return
            except WsClosed:
                pass
        self._state.pending.append(text)

    def write(self, data: str) -> None:
        """Type ``data`` into the PTY (queued while reconnecting)."""
        self._queue_or_send(frame({"t": "i", "d": data}))

    def resize(self, cols: int, rows: int) -> None:
        """Resize the PTY; the size is re-applied on every reconnect."""
        self._state.cols, self._state.rows = cols, rows
        self._queue_or_send(frame({"t": "r", "c": cols, "r": rows}))

    def detach(self) -> None:
        """Drop the socket and stop reconnecting; the session keeps running on the box."""
        self._state.stopping = True
        self._stop.set()
        self._drop_socket()
        self._thread.join()

    def close(self) -> None:
        """Kill the session on the box, then detach."""
        self.detach()
        from zipbox._effects import run_sync

        run_sync(
            self._transport,
            self._core.close_pty(self._sandbox_id, self._state.session_id, self._state.client_id),
        )

    def wait(self) -> int | None:
        """Block until the shell exits (or the session is taken/detached)."""
        self._done.wait()
        if self._error is not None:
            raise self._error
        return self._state.exit_code


async def _async_ws_connect(effect: WsConnect) -> Any:
    from websockets.asyncio.client import connect
    from websockets.exceptions import InvalidStatus

    try:
        return await connect(
            effect.url,
            subprotocols=[*effect.subprotocols],  # type: ignore[list-item]
            max_size=None,
            open_timeout=30,
            close_timeout=2,
        )
    except InvalidStatus as error:
        raise WsRefused(error.response.status_code) from error
    except Exception as error:
        raise WsRefused(None) from error


class AsyncPty:
    """A PTY session on a box (asyncio API; the socket is held by a task)."""

    def __init__(
        self, core: Core, transport: AsyncTransport, sandbox_id: str, state: PtyState
    ) -> None:
        self._core = core
        self._transport = transport
        self._sandbox_id = sandbox_id
        self._state = state
        self._socket: Any = None
        self._stop = asyncio.Event()
        self._opened = asyncio.Event()
        self._error: BaseException | None = None
        self._callbacks: list[Callable[[str], None]] = []
        self._queue: asyncio.Queue[str | None] = asyncio.Queue()
        self._task: asyncio.Task[None] | None = None

    @property
    def session_id(self) -> str:
        """The session id; pass it to ``pty.attach`` to re-attach later."""
        return self._state.session_id

    @property
    def exit_code(self) -> int | None:
        """The shell's exit status once it has exited."""
        return self._state.exit_code

    async def _start(self) -> AsyncPty:
        self._task = asyncio.create_task(self._run())
        opened = asyncio.create_task(self._opened.wait())
        await asyncio.wait({opened, self._task}, return_when=asyncio.FIRST_COMPLETED)
        opened.cancel()
        if self._error is not None and not self._state.opened_once:
            raise self._error
        return self

    async def _run(self) -> None:
        streams = Streams()
        op = pty_loop(self._core, self._sandbox_id, self._state)
        send: Any = None
        throw: BaseException | None = None
        try:
            while True:
                try:
                    effect = op.throw(throw) if throw is not None else op.send(send)
                except StopIteration:
                    return
                throw, send = None, None
                try:
                    send = await self._perform(effect, streams)
                except Exception as error:
                    throw = error
        except BaseException as error:
            self._error = error
        finally:
            op.close()
            await self._drop_socket()
            self._queue.put_nowait(None)

    async def _perform(self, effect: Any, streams: Streams) -> Any:
        if isinstance(effect, Emit):
            value = effect.value
            if isinstance(value, PtyOutput):
                for callback in list(self._callbacks):
                    callback(value.data)
                self._queue.put_nowait(value.data)
            return None
        if isinstance(effect, Sleep):
            with contextlib.suppress(asyncio.TimeoutError):
                await asyncio.wait_for(self._stop.wait(), timeout=max(0.0, effect.seconds))
            return None
        if isinstance(effect, WsConnect):
            if self._stop.is_set():
                raise WsRefused(None)
            self._socket = await _async_ws_connect(effect)
            self._opened.set()
            return None
        if isinstance(effect, WsRecv):
            return await self._recv()
        if isinstance(effect, WsSend):
            await self._send(effect.text)
            return None
        if isinstance(effect, WsClose):
            await self._drop_socket()
            return None
        return await perform_async(self._transport, effect, streams)

    async def _recv(self) -> str:
        from websockets.exceptions import ConnectionClosed

        socket = self._socket
        if socket is None:
            raise WsClosed(None)
        try:
            message = await socket.recv()
        except ConnectionClosed as closed:
            if self._socket is socket:
                self._socket = None
            self._state.connected = False
            raise WsClosed(closed.rcvd.code if closed.rcvd else None) from closed
        return message if isinstance(message, str) else bytes(message).decode("utf-8", "replace")

    async def _send(self, text: str) -> None:
        from websockets.exceptions import ConnectionClosed

        if self._socket is None:
            raise WsClosed(None)
        try:
            await self._socket.send(text)
        except ConnectionClosed as closed:
            raise WsClosed(closed.rcvd.code if closed.rcvd else None) from closed

    async def _drop_socket(self) -> None:
        socket, self._socket = self._socket, None
        self._state.connected = False
        if socket is not None:
            with contextlib.suppress(Exception):
                await socket.close()

    def on_data(self, callback: Callable[[str], None]) -> None:
        """Call ``callback`` with every chunk of output."""
        self._callbacks.append(callback)

    async def __aiter__(self) -> AsyncIterator[str]:
        """Iterate output until the session ends or is detached."""
        while True:
            item = await self._queue.get()
            if item is None:
                self._queue.put_nowait(None)
                return
            yield item

    async def _queue_or_send(self, text: str) -> None:
        if self._socket is not None:
            try:
                await self._send(text)
                return
            except WsClosed:
                pass
        self._state.pending.append(text)

    async def write(self, data: str) -> None:
        """Type ``data`` into the PTY (queued while reconnecting)."""
        await self._queue_or_send(frame({"t": "i", "d": data}))

    async def resize(self, cols: int, rows: int) -> None:
        """Resize the PTY; the size is re-applied on every reconnect."""
        self._state.cols, self._state.rows = cols, rows
        await self._queue_or_send(frame({"t": "r", "c": cols, "r": rows}))

    async def detach(self) -> None:
        """Drop the socket and stop reconnecting; the session keeps running on the box."""
        self._state.stopping = True
        self._stop.set()
        await self._drop_socket()
        if self._task is not None:
            await self._task

    async def close(self) -> None:
        """Kill the session on the box, then detach."""
        await self.detach()
        from zipbox._effects import run_async

        await run_async(
            self._transport,
            self._core.close_pty(self._sandbox_id, self._state.session_id, self._state.client_id),
        )

    async def wait(self) -> int | None:
        """Wait until the shell exits (or the session is taken/detached)."""
        if self._task is not None:
            await self._task
        if self._error is not None:
            raise self._error
        return self._state.exit_code


def new_state(session_id: str | None, cols: int, rows: int) -> PtyState:
    """State for a new or re-attached session."""
    return PtyState(
        session_id=session_id or new_session_id(),
        client_id=uuid.uuid4().hex,
        cols=cols,
        rows=rows,
    )
