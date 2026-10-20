"""httpx-backed transports for the effect drivers."""

from __future__ import annotations

from collections.abc import AsyncIterator, Iterator
from typing import Any

import httpx

from zipbox._effects import HttpRequest, HttpResponse
from zipbox._errors import ZipboxError

# A streamed exec can stay silent for hours; nothing here may impose a deadline.
_NO_TIMEOUT = httpx.Timeout(None, connect=30.0)


def _network_error(error: Exception) -> ZipboxError:
    return ZipboxError("unavailable", f"network error: {error}", retryable=True)


class _StreamState:
    def __init__(self, response: Any, chunks: Any) -> None:
        self.response = response
        self.chunks = chunks


class HttpxSyncTransport:
    """Blocking transport over ``httpx.Client``."""

    def __init__(self, client: httpx.Client | None = None) -> None:
        self._client = client or httpx.Client(timeout=_NO_TIMEOUT)

    def close(self) -> None:
        """Release the connection pool."""
        self._client.close()

    def send(self, request: HttpRequest) -> HttpResponse:
        """Perform an ``HttpRequest`` effect."""
        try:
            built = self._client.build_request(
                request.method, request.url, headers=request.headers, content=request.body
            )
            response = self._client.send(built, stream=request.stream)
        except httpx.TransportError as error:
            raise _network_error(error) from error
        headers = {k.lower(): v for k, v in response.headers.items()}
        if request.stream:
            chunks: Iterator[bytes] = response.iter_bytes()
            return HttpResponse(
                response.status_code, headers, handle=_StreamState(response, chunks)
            )
        return HttpResponse(response.status_code, headers, response.content)

    def read_chunk(self, response: HttpResponse) -> bytes:
        """Perform a ``ReadChunk`` effect."""
        state: _StreamState = response.handle
        try:
            return bytes(next(state.chunks))
        except StopIteration:
            return b""
        except httpx.TransportError as error:
            raise _network_error(error) from error

    def read_all(self, response: HttpResponse) -> bytes:
        """Perform a ``ReadAll`` effect."""
        state: _StreamState = response.handle
        try:
            return b"".join(state.chunks)
        except httpx.TransportError as error:
            raise _network_error(error) from error
        finally:
            state.response.close()

    def close_stream(self, response: HttpResponse) -> None:
        """Perform a ``CloseStream`` effect."""
        state: _StreamState = response.handle
        state.response.close()


class HttpxAsyncTransport:
    """Async transport over ``httpx.AsyncClient``."""

    def __init__(self, client: httpx.AsyncClient | None = None) -> None:
        self._client = client or httpx.AsyncClient(timeout=_NO_TIMEOUT)

    async def aclose(self) -> None:
        """Release the connection pool."""
        await self._client.aclose()

    async def send(self, request: HttpRequest) -> HttpResponse:
        """Perform an ``HttpRequest`` effect."""
        try:
            built = self._client.build_request(
                request.method, request.url, headers=request.headers, content=request.body
            )
            response = await self._client.send(built, stream=request.stream)
        except httpx.TransportError as error:
            raise _network_error(error) from error
        headers = {k.lower(): v for k, v in response.headers.items()}
        if request.stream:
            chunks: AsyncIterator[bytes] = response.aiter_bytes()
            return HttpResponse(
                response.status_code, headers, handle=_StreamState(response, chunks)
            )
        return HttpResponse(response.status_code, headers, await response.aread())

    async def read_chunk(self, response: HttpResponse) -> bytes:
        """Perform a ``ReadChunk`` effect."""
        state: _StreamState = response.handle
        try:
            return bytes(await state.chunks.__anext__())
        except StopAsyncIteration:
            return b""
        except httpx.TransportError as error:
            raise _network_error(error) from error

    async def read_all(self, response: HttpResponse) -> bytes:
        """Perform a ``ReadAll`` effect."""
        state: _StreamState = response.handle
        parts: list[bytes] = []
        try:
            async for chunk in state.chunks:
                parts.append(bytes(chunk))
            return b"".join(parts)
        except httpx.TransportError as error:
            raise _network_error(error) from error
        finally:
            await state.response.aclose()

    async def close_stream(self, response: HttpResponse) -> None:
        """Perform a ``CloseStream`` effect."""
        state: _StreamState = response.handle
        await state.response.aclose()
