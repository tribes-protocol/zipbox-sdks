"""Incremental server-sent-events parser for the box's exec streams."""

from __future__ import annotations

from dataclasses import dataclass


@dataclass(frozen=True)
class SseMessage:
    """One dispatched SSE event."""

    event: str
    data: str
    id: str | None


class SseParser:
    """Feed raw bytes in any chunking; complete events come out.

    Events are split on blank lines at the byte level before decoding, so a UTF-8
    character split across two network chunks is never decoded in halves.
    """

    def __init__(self) -> None:
        self._buffer = b""
        self.last_id: str | None = None

    def feed(self, chunk: bytes) -> list[SseMessage]:
        """Add bytes; return every event they completed."""
        self._buffer += chunk.replace(b"\r\n", b"\n")
        out: list[SseMessage] = []
        while True:
            index = self._buffer.find(b"\n\n")
            if index < 0:
                break
            block, self._buffer = self._buffer[:index], self._buffer[index + 2 :]
            message = self._parse(block.decode("utf-8", "replace"))
            if message is not None:
                out.append(message)
        return out

    def _parse(self, block: str) -> SseMessage | None:
        event = "message"
        data: list[str] = []
        event_id: str | None = None
        for line in block.split("\n"):
            if not line or line.startswith(":"):
                continue
            name, _, value = line.partition(":")
            if value.startswith(" "):
                value = value[1:]
            if name == "event":
                event = value
            elif name == "data":
                data.append(value)
            elif name == "id":
                event_id = value
        if event_id is not None:
            self.last_id = event_id
        if not data and event == "message":
            return None
        return SseMessage(event=event, data="\n".join(data), id=event_id)
