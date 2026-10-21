"""The one error type every SDK call raises, and the status → code mapping."""

from __future__ import annotations

import json
import time
from email.utils import parsedate_to_datetime
from typing import Any, Literal

ErrorCode = Literal[
    "invalid_request",
    "unauthorized",
    "forbidden",
    "not_found",
    "insufficient_credits",
    "conflict",
    "rate_limited",
    "box_not_running",
    "unavailable",
]

DOCS_ERRORS_URL = "https://zipbox.ai/docs/errors"

_RETRYABLE: frozenset[str] = frozenset({"rate_limited", "unavailable", "box_not_running"})


class ZipboxError(Exception):
    """A failed Zipbox call.

    ``code`` is the stable contract (see https://zipbox.ai/docs/errors); ``message`` is
    for humans. A command that exits non-zero is a result, never a ``ZipboxError``.
    """

    def __init__(
        self,
        code: ErrorCode,
        message: str,
        *,
        status: int | None = None,
        request_id: str | None = None,
        retryable: bool | None = None,
        retry_after_ms: int | None = None,
        docs_url: str | None = None,
        unsent: bool = False,
    ) -> None:
        super().__init__(message)
        self.code: ErrorCode = code
        self.message = message
        self.status = status
        self.request_id = request_id
        self.retryable = code in _RETRYABLE if retryable is None else retryable
        self.retry_after_ms = retry_after_ms
        self.docs_url = docs_url or f"{DOCS_ERRORS_URL}#{code}"
        # True when the request provably never left: the connection or its TLS
        # handshake failed. Any method may be retried then, since nothing was sent.
        self.unsent = unsent

    def __str__(self) -> str:
        status = f" ({self.status})" if self.status is not None else ""
        return f"{self.code}{status}: {self.message} — {self.docs_url}"

    def __repr__(self) -> str:
        return f"ZipboxError(code={self.code!r}, status={self.status!r}, message={self.message!r})"


def code_for_status(status: int) -> ErrorCode:
    """Classify an HTTP status that carried no error envelope."""
    if status in (400, 413, 422):
        return "invalid_request"
    mapping: dict[int, ErrorCode] = {
        401: "unauthorized",
        402: "insufficient_credits",
        403: "forbidden",
        404: "not_found",
        409: "conflict",
        429: "rate_limited",
        502: "box_not_running",
    }
    if status in mapping:
        return mapping[status]
    return "unavailable" if status >= 500 else "invalid_request"


_CODES: frozenset[str] = frozenset(
    {
        "invalid_request",
        "unauthorized",
        "forbidden",
        "not_found",
        "insufficient_credits",
        "conflict",
        "rate_limited",
        "box_not_running",
        "unavailable",
    }
)


def parse_retry_after_ms(headers: dict[str, str]) -> int | None:
    """Read ``Retry-After`` (seconds or an HTTP date) as milliseconds."""
    value = headers.get("retry-after")
    if value is None or not value.strip():
        return None
    try:
        return max(0, int(float(value) * 1000))
    except ValueError:
        pass
    try:
        at = parsedate_to_datetime(value)
    except (TypeError, ValueError):
        return None
    return max(0, int((at.timestamp() - time.time()) * 1000))


def error_from_response(status: int, headers: dict[str, str], body: bytes) -> ZipboxError:
    """Build the error for a non-2xx response, envelope or not."""
    parsed: Any = None
    if body:
        try:
            parsed = json.loads(body)
        except (ValueError, UnicodeDecodeError):
            parsed = None
    header_retry = parse_retry_after_ms(headers)
    if isinstance(parsed, dict) and isinstance(parsed.get("error"), dict):
        env = parsed["error"]
        raw_code = env.get("code")
        code: ErrorCode = raw_code if raw_code in _CODES else code_for_status(status)
        retry_after = env.get("retryAfterMs")
        return ZipboxError(
            code,
            str(env.get("message") or code),
            status=status,
            request_id=env.get("requestId"),
            retryable=env.get("retryable") if isinstance(env.get("retryable"), bool) else None,
            retry_after_ms=retry_after if isinstance(retry_after, int) else header_retry,
            docs_url=env.get("docsUrl"),
        )
    message = f"HTTP {status}"
    if isinstance(parsed, dict) and isinstance(parsed.get("error"), str):
        message = parsed["error"]
    elif body and parsed is None:
        message = body[:200].decode("utf-8", "replace")
    return ZipboxError(
        code_for_status(status),
        message,
        status=status,
        request_id=headers.get("x-request-id"),
        retry_after_ms=header_retry,
    )
