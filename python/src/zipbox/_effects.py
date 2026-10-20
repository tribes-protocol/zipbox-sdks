"""The I/O vocabulary the client core speaks, and the drivers that perform it.

Every operation is written ONCE as a generator that yields effects (send this HTTP
request, read a chunk, sleep) and receives their results. ``Zipbox`` runs those
generators with a blocking transport and ``AsyncZipbox`` with an async one, so the
business logic — token caching, 401 recovery, retries, polling, SSE collection — has
exactly one implementation.
"""

from __future__ import annotations

import asyncio
import contextlib
import time
from collections.abc import AsyncIterator, Generator, Iterator
from dataclasses import dataclass, field
from typing import Any, Protocol, TypeVar

T = TypeVar("T")


@dataclass(frozen=True)
class HttpRequest:
    """Send one HTTP request. ``stream=True`` returns before the body is read."""

    method: str
    url: str
    headers: dict[str, str] = field(repr=False)  # carries the API key or box token
    body: bytes | None = None
    stream: bool = False


@dataclass
class HttpResponse:
    """A response. For a streamed request ``body`` is empty and ``handle`` is set."""

    status: int
    headers: dict[str, str]
    body: bytes = b""
    handle: Any = field(default=None, repr=False)


@dataclass(frozen=True)
class ReadChunk:
    """Read the next chunk of a streamed response; ``b""`` at end of stream."""

    response: HttpResponse


@dataclass(frozen=True)
class ReadAll:
    """Read the rest of a streamed response and close it."""

    response: HttpResponse


@dataclass(frozen=True)
class CloseStream:
    """Close a streamed response early (cancels a foreground exec)."""

    response: HttpResponse


@dataclass(frozen=True)
class Sleep:
    """Wait."""

    seconds: float


@dataclass(frozen=True)
class Emit:
    """Hand a value to the consumer of an iterating operation."""

    value: Any


Effect = HttpRequest | ReadChunk | ReadAll | CloseStream | Sleep
Op = Generator[Effect, Any, T]
StreamOp = Generator[Effect | Emit, Any, None]


class SyncTransport(Protocol):
    """Blocking I/O for the effects."""

    def send(self, request: HttpRequest) -> HttpResponse: ...
    def read_chunk(self, response: HttpResponse) -> bytes: ...
    def read_all(self, response: HttpResponse) -> bytes: ...
    def close_stream(self, response: HttpResponse) -> None: ...


class AsyncTransport(Protocol):
    """Async I/O for the effects."""

    async def send(self, request: HttpRequest) -> HttpResponse: ...
    async def read_chunk(self, response: HttpResponse) -> bytes: ...
    async def read_all(self, response: HttpResponse) -> bytes: ...
    async def close_stream(self, response: HttpResponse) -> None: ...


class Streams:
    """Streams a driver opened, so an abandoned generator never leaks one."""

    def __init__(self) -> None:
        self.open: list[HttpResponse] = []

    def track(self, result: Any) -> None:
        if isinstance(result, HttpResponse) and result.handle is not None:
            self.open.append(result)

    def forget(self, response: HttpResponse) -> None:
        if response in self.open:
            self.open.remove(response)


def perform_sync(transport: SyncTransport, effect: Effect, streams: Streams) -> Any:
    if isinstance(effect, HttpRequest):
        result = transport.send(effect)
        streams.track(result)
        return result
    if isinstance(effect, ReadChunk):
        return transport.read_chunk(effect.response)
    if isinstance(effect, ReadAll):
        streams.forget(effect.response)
        return transport.read_all(effect.response)
    if isinstance(effect, CloseStream):
        streams.forget(effect.response)
        transport.close_stream(effect.response)
        return None
    if effect.seconds > 0:
        time.sleep(effect.seconds)
    return None


async def perform_async(transport: AsyncTransport, effect: Effect, streams: Streams) -> Any:
    if isinstance(effect, HttpRequest):
        result = await transport.send(effect)
        streams.track(result)
        return result
    if isinstance(effect, ReadChunk):
        return await transport.read_chunk(effect.response)
    if isinstance(effect, ReadAll):
        streams.forget(effect.response)
        return await transport.read_all(effect.response)
    if isinstance(effect, CloseStream):
        streams.forget(effect.response)
        await transport.close_stream(effect.response)
        return None
    await asyncio.sleep(max(0.0, effect.seconds))
    return None


class _Done:
    """Sentinel wrapper for an operation's return value."""

    __slots__ = ("value",)

    def __init__(self, value: Any) -> None:
        self.value = value


def _step(op: Generator[Any, Any, Any], send_value: Any, throw: BaseException | None) -> Any:
    try:
        return op.throw(throw) if throw is not None else op.send(send_value)
    except StopIteration as stop:
        return _Done(stop.value)


def iterate_sync(transport: SyncTransport, op: Generator[Any, Any, Any]) -> Iterator[Any]:
    """Drive an operation, yielding every ``Emit`` value to the caller."""
    streams = Streams()
    send_value: Any = None
    throw: BaseException | None = None
    try:
        while True:
            item = _step(op, send_value, throw)
            throw, send_value = None, None
            if isinstance(item, _Done):
                if item.value is not None:
                    yield _Done(item.value)
                return
            if isinstance(item, Emit):
                yield item.value
                continue
            try:
                send_value = perform_sync(transport, item, streams)
            except Exception as error:
                throw = error
    finally:
        op.close()
        for response in streams.open:
            with contextlib.suppress(Exception):
                transport.close_stream(response)


def run_sync(transport: SyncTransport, op: Op[T]) -> T:
    """Drive an operation to its return value with blocking I/O."""
    result: Any = None
    for item in iterate_sync(transport, op):
        if not isinstance(item, _Done):
            raise RuntimeError(f"operation emitted unexpectedly: {item!r}")
        result = item.value
    return result  # type: ignore[no-any-return]


def stream_sync(transport: SyncTransport, op: StreamOp) -> Iterator[Any]:
    """Iterate an emitting operation's values."""
    for item in iterate_sync(transport, op):
        if not isinstance(item, _Done):
            yield item


async def iterate_async(
    transport: AsyncTransport, op: Generator[Any, Any, Any]
) -> AsyncIterator[Any]:
    """Drive an operation, yielding every ``Emit`` value to the caller."""
    streams = Streams()
    send_value: Any = None
    throw: BaseException | None = None
    try:
        while True:
            item = _step(op, send_value, throw)
            throw, send_value = None, None
            if isinstance(item, _Done):
                if item.value is not None:
                    yield _Done(item.value)
                return
            if isinstance(item, Emit):
                yield item.value
                continue
            try:
                send_value = await perform_async(transport, item, streams)
            except Exception as error:
                throw = error
    finally:
        op.close()
        for response in streams.open:
            with contextlib.suppress(Exception):
                await transport.close_stream(response)


async def run_async(transport: AsyncTransport, op: Op[T]) -> T:
    """Drive an operation to its return value with async I/O."""
    result: Any = None
    async for item in iterate_async(transport, op):
        if not isinstance(item, _Done):
            raise RuntimeError(f"operation emitted unexpectedly: {item!r}")
        result = item.value
    return result  # type: ignore[no-any-return]


async def stream_async(transport: AsyncTransport, op: StreamOp) -> AsyncIterator[Any]:
    """Iterate an emitting operation's values."""
    async for item in iterate_async(transport, op):
        if not isinstance(item, _Done):
            yield item
