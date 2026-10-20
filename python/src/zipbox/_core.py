"""Every Zipbox operation, written once as an effect generator (see ``_effects``)."""

from __future__ import annotations

import base64
import json
import os
import posixpath
import random
import uuid
from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from typing import Any
from urllib.parse import quote, urlparse

from zipbox._backoff import Backoff
from zipbox._effects import (
    CloseStream,
    Emit,
    HttpRequest,
    HttpResponse,
    Op,
    ReadAll,
    ReadChunk,
    Sleep,
    StreamOp,
)
from zipbox._errors import ZipboxError, error_from_response
from zipbox._models import (
    CommandInfo,
    ExecEvent,
    ExecResult,
    FileEntry,
    Language,
    Preview,
    SandboxDetail,
    SandboxInfo,
    Signal,
    SshInfo,
)
from zipbox._sse import SseMessage, SseParser

VERSION = "0.1.0"
DEFAULT_API_URL = "https://api.tribes.xyz"
WORKSPACE = "/root/workspace"
_USER_AGENT = f"zipbox-python/{VERSION}"
_DEAD_STATUSES = frozenset({"destroyed", "stopped"})
_RETRY_CODES = frozenset({"rate_limited", "unavailable"})
_RUNNERS: dict[str, tuple[str, str]] = {
    "python": ("python3", "py"),
    "node": ("node", "js"),
    "bash": ("bash", "sh"),
}
# ffmpeg's x11grab, which the platform image ships (it has no ImageMagick). Under
# pipefail a capture that fails is a non-zero exit, not an empty PNG.
_SCREENSHOT = (
    'set -o pipefail; XAUTHORITY="${XAUTHORITY:-/root/.Xauthority}" '
    'ffmpeg -loglevel error -f x11grab -i "${DISPLAY:-:10}" -frames:v 1 -f image2pipe '
    "-vcodec png - | base64 -w0"
)

CommandLine = str | Sequence[str]


@dataclass
class ClientConfig:
    """Resolved client settings. Delays are seconds."""

    api_key: str = field(repr=False)
    base_url: str
    max_retries: int = 3
    retry_base_delay_s: float = 0.5
    retry_max_delay_s: float = 30.0
    poll_interval_s: float = 1.0
    poll_max_interval_s: float = 10.0
    pty_base_delay_s: float = 0.5
    pty_max_delay_s: float = 30.0
    rng: Any = field(default=random.random, repr=False)


def resolve_config(
    api_key: str | None,
    base_url: str | None,
    max_retries: int,
    **overrides: Any,
) -> ClientConfig:
    """Fill the key and URL from the environment when not passed."""
    key = api_key or os.environ.get("ZIPBOX_API_KEY")
    if not key:
        raise ZipboxError(
            "unauthorized",
            "No API key. Pass api_key= or set ZIPBOX_API_KEY "
            "(create one at zipbox.ai → Settings → API keys).",
        )
    url = base_url or os.environ.get("ZIPBOX_API_URL") or DEFAULT_API_URL
    return ClientConfig(api_key=key, base_url=url.rstrip("/"), max_retries=max_retries, **overrides)


def resolve_path(path: str) -> str:
    """Absolute paths pass through; anything else is relative to /root/workspace."""
    joined = path if path.startswith("/") else posixpath.join(WORKSPACE, path)
    normalized = posixpath.normpath(joined)
    return "/" if normalized in ("", ".") else normalized


def _query(params: Mapping[str, str | None]) -> str:
    parts = [f"{k}={quote(v, safe='')}" for k, v in params.items() if v is not None]
    return ("?" + "&".join(parts)) if parts else ""


def _json(response: HttpResponse) -> Any:
    if not response.body:
        return None
    return json.loads(response.body)


def _encode(body: Any) -> bytes:
    return json.dumps(body, separators=(",", ":")).encode()


def exec_body(
    cmd: CommandLine,
    *,
    cwd: str | None = None,
    env: Mapping[str, str] | None = None,
    timeout_ms: int | None = None,
    user: str | None = None,
    background: bool | None = None,
    stdin: bool | None = None,
) -> dict[str, Any]:
    """The POST /exec body; absent options are omitted, never sent as null."""
    body: dict[str, Any] = {"cmd": cmd} if isinstance(cmd, str) else {"argv": list(cmd)}
    if cwd is not None:
        body["cwd"] = cwd
    if env is not None:
        body["env"] = dict(env)
    if user is not None:
        body["user"] = user
    if timeout_ms is not None:
        body["timeoutMs"] = timeout_ms
    if background is not None:
        body["background"] = background
    if stdin is not None:
        body["stdin"] = stdin
    return body


def event_from_sse(message: SseMessage, cursor: str) -> ExecEvent | None:
    """Turn one SSE message into an ``ExecEvent`` (None for unknown event types)."""
    try:
        data: Any = json.loads(message.data) if message.data else {}
    except ValueError:
        data = {}
    if not isinstance(data, dict):
        data = {}
    if message.event == "start":
        return ExecEvent(type="start", cursor=cursor, command_id=data.get("id"))
    if message.event == "stdout":
        return ExecEvent(type="stdout", cursor=cursor, data=str(data.get("data", "")))
    if message.event == "stderr":
        return ExecEvent(type="stderr", cursor=cursor, data=str(data.get("data", "")))
    if message.event == "gap":
        return ExecEvent(
            type="gap", cursor=cursor, stream=data.get("stream"), message=data.get("message")
        )
    if message.event == "exit":
        code = data.get("exitCode")
        return ExecEvent(
            type="exit",
            cursor=cursor,
            state=data.get("state"),
            exit_code=code if isinstance(code, int) else None,
            signal=data.get("signal"),
            timed_out=data.get("timedOut") is True,
        )
    return None


def result_from_events(events: Sequence[ExecEvent]) -> ExecResult:
    """Fold a command's events into its result."""
    command_id = ""
    stdout: list[str] = []
    stderr: list[str] = []
    final: ExecEvent | None = None
    for event in events:
        if event.type == "start" and event.command_id:
            command_id = event.command_id
        elif event.type == "stdout":
            stdout.append(event.data or "")
        elif event.type == "stderr":
            stderr.append(event.data or "")
        elif event.type == "exit":
            final = event
    return ExecResult(
        id=command_id,
        exit_code=final.exit_code if final else None,
        stdout="".join(stdout),
        stderr="".join(stderr),
        timed_out=bool(final and final.timed_out),
        signal=final.signal if final else None,
    )


def collect(op: StreamOp) -> Op[list[Any]]:
    """Run an emitting operation inside another one, gathering what it emits."""
    out: list[Any] = []
    send: Any = None
    throw: BaseException | None = None
    while True:
        try:
            item = op.throw(throw) if throw is not None else op.send(send)
        except StopIteration:
            return out
        throw, send = None, None
        if isinstance(item, Emit):
            out.append(item.value)
            continue
        try:
            send = yield item
        except Exception as error:
            throw = error


@dataclass(frozen=True)
class BoxTarget:
    """Where a box answers and the token it accepts from this key."""

    base_url: str
    token: str = field(repr=False)

    @property
    def host(self) -> str:
        """The box's hostname (no port)."""
        return urlparse(self.base_url).hostname or ""


class Core:
    """State shared by every resource: config and the per-box token cache."""

    def __init__(self, config: ClientConfig) -> None:
        self.config = config
        self._targets: dict[str, BoxTarget] = {}

    # ------------------------------------------------------------------ plumbing

    def _backoff(self) -> Backoff:
        return Backoff(
            self.config.retry_base_delay_s, self.config.retry_max_delay_s, rng=self.config.rng
        )

    def send(
        self, request: HttpRequest, *, retry: bool, passthrough: frozenset[int] = frozenset()
    ) -> Op[HttpResponse]:
        """Send with the SDK retry rule; raise ``ZipboxError`` on a non-2xx status."""
        attempt = 0
        backoff = self._backoff()
        while True:
            try:
                response: HttpResponse = yield request
            except ZipboxError as network_error:
                error = network_error
            else:
                if 200 <= response.status < 300 or response.status in passthrough:
                    return response
                body = response.body
                if response.handle is not None:
                    body = yield ReadAll(response)
                error = error_from_response(response.status, response.headers, body)
            if not retry or error.code not in _RETRY_CODES or attempt >= self.config.max_retries:
                raise error
            delay = (
                error.retry_after_ms / 1000 if error.retry_after_ms is not None else backoff.next()
            )
            yield Sleep(delay)
            attempt += 1

    def control(
        self,
        method: str,
        path: str,
        *,
        body: Any = None,
        headers: Mapping[str, str] | None = None,
        retry: bool | None = None,
    ) -> Op[Any]:
        """Call the control plane with the API key; return the parsed JSON body."""
        hdrs = {
            "authorization": f"Bearer {self.config.api_key}",
            "user-agent": _USER_AGENT,
            "accept": "application/json",
        }
        payload = None
        if body is not None:
            payload = _encode(body)
            hdrs["content-type"] = "application/json"
        hdrs.update(headers or {})
        request = HttpRequest(method, self.config.base_url + path, hdrs, payload)
        response = yield from self.send(request, retry=method == "GET" if retry is None else retry)
        return _json(response)

    def target(self, sandbox_id: str) -> Op[BoxTarget]:
        """The box's origin and token for this key, fetched once and cached."""
        cached = self._targets.get(sandbox_id)
        if cached is not None:
            return cached
        detail = yield from self.get(sandbox_id)
        cached = self._targets.get(sandbox_id)
        if cached is None:
            raise ZipboxError(
                "box_not_running",
                f"Box {sandbox_id} is {detail.status} and not ready; start it first.",
                # Booting or restarting comes back by itself; stopped or deleted does not.
                retryable=detail.status in ("provisioning", "running"),
            )
        return cached

    def forget(self, sandbox_id: str) -> None:
        """Drop a cached box token (lifecycle changes may move the box)."""
        self._targets.pop(sandbox_id, None)

    def box(
        self,
        sandbox_id: str,
        method: str,
        path: str,
        *,
        query: Mapping[str, str | None] | None = None,
        body: Any = None,
        raw: bytes | None = None,
        stream: bool = False,
        accept: str = "application/json",
    ) -> Op[HttpResponse]:
        """Call a box route with its token; a 401 re-fetches the token once."""
        refetched = False
        while True:
            target = yield from self.target(sandbox_id)
            hdrs = {
                "authorization": f"Bearer {target.token}",
                "user-agent": _USER_AGENT,
                "accept": accept,
            }
            payload = raw
            if body is not None:
                payload = _encode(body)
                hdrs["content-type"] = "application/json"
            elif raw is not None:
                hdrs["content-type"] = "application/octet-stream"
            url = target.base_url + path + _query(query or {})
            request = HttpRequest(method, url, hdrs, payload, stream=stream)
            response = yield from self.send(
                request, retry=method == "GET", passthrough=frozenset({401})
            )
            if response.status != 401:
                return response
            if response.handle is not None:
                yield CloseStream(response)
            if refetched:
                raise ZipboxError(
                    "unauthorized",
                    "The box refused this key's token (was the key deleted?).",
                    status=401,
                )
            refetched = True
            self.forget(sandbox_id)

    def box_json(self, sandbox_id: str, method: str, path: str, **kwargs: Any) -> Op[Any]:
        """``box`` returning the parsed JSON body."""
        response = yield from self.box(sandbox_id, method, path, **kwargs)
        return _json(response)

    # ----------------------------------------------------------------- sandboxes

    def list_sandboxes(self) -> Op[list[SandboxInfo]]:
        """GET /sandbox."""
        rows = yield from self.control("GET", "/sandbox")
        return [SandboxInfo.from_json(row) for row in rows or []]

    def get(self, sandbox_id: str) -> Op[SandboxDetail]:
        """GET /sandbox/:id, refreshing the cached box token as a side effect."""
        data = yield from self.control("GET", f"/sandbox/{quote(sandbox_id, safe='')}")
        # The token lives in the cache only, never on a returned (loggable) model.
        token = data.pop("boxToken", None)
        detail = SandboxDetail.from_json(data)
        if detail.ready and detail.base_url and isinstance(token, str) and token:
            self._targets[sandbox_id] = BoxTarget(detail.base_url.rstrip("/"), token)
        else:
            self.forget(sandbox_id)
        return detail

    def wait_ready(self, sandbox_id: str) -> Op[SandboxDetail]:
        """Re-read the box with backoff until it is ready. No deadline."""
        backoff = Backoff(
            self.config.poll_interval_s, self.config.poll_max_interval_s, rng=self.config.rng
        )
        while True:
            detail = yield from self.get(sandbox_id)
            if detail.ready:
                return detail
            if detail.status in _DEAD_STATUSES:
                raise ZipboxError("conflict", f"Box {sandbox_id} is {detail.status}.")
            yield Sleep(backoff.next())

    def create(
        self,
        *,
        name: str | None = None,
        tier: str | None = None,
        ssh_public_key: str | None = None,
    ) -> Op[SandboxDetail]:
        """Claim a box (idempotent) and wait until it is ready."""
        body: dict[str, Any] = {}
        if name is not None:
            body["name"] = name
        if tier is not None:
            body["tier"] = tier
        if ssh_public_key is not None:
            body["sshPublicKey"] = ssh_public_key
        created = yield from self.control(
            "POST",
            "/sandbox/identity/claim",
            body=body,
            headers={"idempotency-key": str(uuid.uuid4())},
            retry=True,
        )
        return (yield from self.wait_ready(str(created["sandboxId"])))

    def get_or_create(
        self, name: str, *, tier: str | None = None, ssh_public_key: str | None = None
    ) -> Op[SandboxDetail]:
        """The live box with this name, started if stopped; created if there is none."""
        boxes = yield from self.list_sandboxes()
        for box in boxes:
            if box.name == name and box.status not in _DEAD_STATUSES:
                if box.status == "archived" or box.is_archived:
                    yield from self.start(box.id)
                return (yield from self.wait_ready(box.id))
        return (yield from self.create(name=name, tier=tier, ssh_public_key=ssh_public_key))

    def lifecycle(self, sandbox_id: str, action: str, body: Any = None) -> Op[None]:
        """POST /sandbox/:id/<action>."""
        self.forget(sandbox_id)
        yield from self.control(
            "POST", f"/sandbox/{quote(sandbox_id, safe='')}/{action}", body=body
        )
        return None

    def start(self, sandbox_id: str) -> Op[None]:
        """Start (restore) a stopped box."""
        return (yield from self.lifecycle(sandbox_id, "restore"))

    def delete(self, sandbox_id: str) -> Op[None]:
        """DELETE /sandbox/:id."""
        self.forget(sandbox_id)
        yield from self.control("DELETE", f"/sandbox/{quote(sandbox_id, safe='')}")
        return None

    # ---------------------------------------------------------------------- exec

    def exec_events(self, sandbox_id: str, body: dict[str, Any]) -> StreamOp:
        """POST /exec and emit its events until exit."""
        response = yield from self.box(
            sandbox_id, "POST", "/exec", body=body, stream=True, accept="text/event-stream"
        )
        parser = SseParser()
        while True:
            chunk: bytes = yield ReadChunk(response)
            if not chunk:
                yield CloseStream(response)
                raise ZipboxError("unavailable", "The exec stream ended before the command exited.")
            for message in parser.feed(chunk):
                event = event_from_sse(message, message.id or parser.last_id or "0-0")
                if event is None:
                    continue
                yield Emit(event)
                if event.type == "exit":
                    yield CloseStream(response)
                    return

    def exec(self, sandbox_id: str, body: dict[str, Any]) -> Op[ExecResult]:
        """Run a command to completion and collect its output."""
        events = yield from collect(self.exec_events(sandbox_id, body))
        return result_from_events(events)

    def exec_background(self, sandbox_id: str, body: dict[str, Any]) -> Op[CommandInfo]:
        """Start a command that keeps running without a client attached."""
        data = yield from self.box_json(
            sandbox_id, "POST", "/exec", body={**body, "background": True}
        )
        return CommandInfo.from_json(data)

    def command_path(self, command_id: str, suffix: str = "") -> str:
        """``/exec/<id><suffix>``."""
        return f"/exec/{quote(command_id, safe='')}{suffix}"

    def command(self, sandbox_id: str, command_id: str) -> Op[CommandInfo]:
        """GET /exec/:id."""
        data = yield from self.box_json(sandbox_id, "GET", self.command_path(command_id))
        return CommandInfo.from_json(data)

    def commands(self, sandbox_id: str) -> Op[list[CommandInfo]]:
        """GET /exec."""
        data = yield from self.box_json(sandbox_id, "GET", "/exec")
        return [CommandInfo.from_json(row) for row in (data or {}).get("commands", [])]

    def command_events(self, sandbox_id: str, command_id: str, since: str | None) -> StreamOp:
        """Replay and follow a command from a cursor until it exits.

        A dropped stream re-attaches from the last cursor with backoff and no attempt
        ceiling — a command is a live surface. The box replays everything after the
        cursor and always ends with ``exit``, so nothing printed during a drop is lost.
        It ends on ``exit`` or on a non-retryable error (e.g. ``not_found``).
        """
        cursor = since
        backoff = self._backoff()
        while True:
            try:
                response = yield from self.box(
                    sandbox_id,
                    "GET",
                    self.command_path(command_id, "/events"),
                    query={"since": cursor},
                    stream=True,
                    accept="text/event-stream",
                )
                parser = SseParser()
                while True:
                    chunk: bytes = yield ReadChunk(response)
                    if not chunk:
                        yield CloseStream(response)
                        break
                    for message in parser.feed(chunk):
                        position = message.id or parser.last_id or cursor or "0-0"
                        event = event_from_sse(message, position)
                        if event is None:
                            continue
                        cursor = position
                        backoff.reset()
                        yield Emit(event)
                        if event.type == "exit":
                            yield CloseStream(response)
                            return
            except ZipboxError as error:
                if not error.retryable:
                    raise
            yield Sleep(backoff.next())

    def wait(self, sandbox_id: str, command_id: str) -> Op[ExecResult]:
        """Follow a command from the start to its exit and fold its output."""
        events: list[ExecEvent] = yield from collect(
            self.command_events(sandbox_id, command_id, None)
        )
        result = result_from_events(events)
        return ExecResult(
            id=command_id,
            exit_code=result.exit_code,
            stdout=result.stdout,
            stderr=result.stderr,
            timed_out=result.timed_out,
            signal=result.signal,
        )

    def stdin(self, sandbox_id: str, command_id: str, data: bytes, close: bool) -> Op[None]:
        """POST /exec/:id/stdin."""
        yield from self.box(
            sandbox_id,
            "POST",
            self.command_path(command_id, "/stdin"),
            query={"close": "1" if close else None},
            raw=data,
        )
        return None

    def signal(self, sandbox_id: str, command_id: str, signal: Signal) -> Op[None]:
        """POST /exec/:id/signal."""
        yield from self.box(
            sandbox_id, "POST", self.command_path(command_id, "/signal"), body={"signal": signal}
        )
        return None

    def run_code(self, sandbox_id: str, language: Language, code: str) -> Op[ExecResult]:
        """Write ``code`` to a temp file and run it with the language's interpreter."""
        if language not in _RUNNERS:
            raise ZipboxError("invalid_request", f"Unsupported language {language!r}.")
        interpreter, extension = _RUNNERS[language]
        path = f"/tmp/zipbox-run-{uuid.uuid4().hex}.{extension}"
        yield from self.write(sandbox_id, path, code.encode())
        script = '"$0" "$1"; rc=$?; rm -f "$1"; exit $rc'
        return (
            yield from self.exec(sandbox_id, exec_body(["bash", "-c", script, interpreter, path]))
        )

    # --------------------------------------------------------------------- files

    def read(self, sandbox_id: str, path: str) -> Op[bytes]:
        """GET /files/download."""
        response = yield from self.box(
            sandbox_id,
            "GET",
            "/files/download",
            query={"path": resolve_path(path)},
            accept="application/octet-stream",
        )
        return response.body

    def write(self, sandbox_id: str, path: str, content: bytes) -> Op[None]:
        """POST /files/write (atomic on the box)."""
        directory, name = posixpath.split(resolve_path(path))
        if not name:
            raise ZipboxError("invalid_request", f"{path!r} names a directory, not a file.")
        yield from self.box(
            sandbox_id,
            "POST",
            "/files/write",
            query={"dir": directory or "/", "name": name},
            raw=content,
        )
        return None

    def list_files(self, sandbox_id: str, path: str) -> Op[list[FileEntry]]:
        """GET /files."""
        resolved = resolve_path(path)
        data = yield from self.box_json(sandbox_id, "GET", "/files", query={"path": resolved})
        parent = str(data.get("path") or resolved)
        return [FileEntry.from_json(entry, parent) for entry in data.get("entries", [])]

    def stat(self, sandbox_id: str, path: str) -> Op[FileEntry]:
        """List the parent directory and return the entry for ``path``."""
        resolved = resolve_path(path)
        parent, name = posixpath.split(resolved)
        entries = yield from self.list_files(sandbox_id, parent or "/")
        for entry in entries:
            if entry.name == name:
                return entry
        raise ZipboxError("not_found", f"No such file: {resolved}")

    def mutate(
        self, sandbox_id: str, action: str, path: str, destination: str | None = None
    ) -> Op[None]:
        """POST /files/mutate."""
        body: dict[str, Any] = {"action": action, "path": resolve_path(path)}
        if destination is not None:
            body["destination"] = resolve_path(destination)
        yield from self.box(sandbox_id, "POST", "/files/mutate", body=body)
        return None

    # ------------------------------------------------------------------- extras

    def preview(self, sandbox_id: str, port: int, label: str | None) -> Op[Preview]:
        """Publish ``port`` at ``https://<label>.<box host>`` via tribes-caddy."""
        target = yield from self.target(sandbox_id)
        chosen = label or f"p{port}"
        host = f"{chosen}.{target.host}"
        result = yield from self.exec(
            sandbox_id, exec_body(["tribes-caddy", "add", host, f"127.0.0.1:{port}"])
        )
        if result.exit_code != 0:
            raise ZipboxError(
                "conflict", f"tribes-caddy add failed: {(result.stderr or result.stdout).strip()}"
            )
        return Preview(url=f"https://{host}", label=chosen)

    def unpreview(self, sandbox_id: str, label: str) -> Op[None]:
        """Remove a preview via tribes-caddy rm."""
        target = yield from self.target(sandbox_id)
        result = yield from self.exec(
            sandbox_id, exec_body(["tribes-caddy", "rm", f"{label}.{target.host}"])
        )
        if result.exit_code != 0:
            raise ZipboxError(
                "conflict", f"tribes-caddy rm failed: {(result.stderr or result.stdout).strip()}"
            )
        return None

    def logs(self, sandbox_id: str, since: str | None, lines: int | None) -> Op[str]:
        """The box's system journal."""
        argv = ["journalctl", "-o", "short-iso", "--no-pager"]
        if since is not None:
            argv += ["--since", since]
        if lines is not None:
            argv += ["-n", str(lines)]
        result = yield from self.exec(sandbox_id, exec_body(argv))
        return result.stdout

    def ssh_info(self, sandbox_id: str) -> Op[SshInfo]:
        """The direct IPv6 SSH line for the box."""
        detail = yield from self.get(sandbox_id)
        if not detail.ipv6:
            raise ZipboxError("box_not_running", f"Box {sandbox_id} has no address yet.")
        return SshInfo(host=detail.ipv6, user="root", command=f"ssh root@{detail.ipv6}")

    def screenshot(self, sandbox_id: str) -> Op[bytes]:
        """A PNG of the box's desktop."""
        result = yield from self.exec(sandbox_id, exec_body(_SCREENSHOT))
        if result.exit_code != 0:
            raise ZipboxError(
                "conflict", f"No desktop to capture: {result.stderr.strip() or 'capture failed'}"
            )
        return base64.b64decode(result.stdout.strip())

    def close_pty(self, sandbox_id: str, session_id: str, client_id: str) -> Op[None]:
        """POST /session/close: kill a PTY session."""
        yield from self.box(
            sandbox_id,
            "POST",
            "/session/close",
            query={"session": session_id, "clientId": client_id},
        )
        return None
