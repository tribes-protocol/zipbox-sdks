"""Every Zipbox operation, written once as an effect generator (see ``_effects``)."""

from __future__ import annotations

import base64
import json
import math
import os
import posixpath
import random
import shlex
import ssl
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
from zipbox._envelope import seal, tailscale_info
from zipbox._errors import ErrorCode, ZipboxError, error_from_response, is_error_code
from zipbox._models import (
    CommandInfo,
    ExecEvent,
    ExecResult,
    FileEntry,
    ForkSourcePolicy,
    Language,
    Preview,
    SandboxDetail,
    SandboxFork,
    SandboxInfo,
    Signal,
    SshInfo,
    TailscaleStatus,
    TransitionCancelOutcome,
)
from zipbox._sse import SseMessage, SseParser

VERSION = "0.1.7"
DEFAULT_API_URL = "https://api.tribes.xyz"
WORKSPACE = "/root/workspace"
_USER_AGENT = f"zipbox-python/{VERSION}"
# get_or_create's list filter: GET /sandbox rows carry no readiness, so the list reads status.
_DEAD_STATUSES = frozenset({"destroyed", "stopped", "deleting"})
_READINESS = frozenset(
    {"ready", "settling", "awaits_tailnet", "paused", "start_failed", "deleting", "gone"}
)
# A box accepts a sealed credential while it is booting or running.
_SEALABLE_STATUSES = frozenset({"provisioning", "running"})
# How long a private box's reachability probe may take before the box counts as off
# this device's tailnet. A probe is a one-off check, not a live surface.
PRIVATE_PROBE_TIMEOUT_S = 5.0
_RUNNERS: dict[str, tuple[str, str]] = {
    "python": ("python3", "py"),
    "node": ("node", "mjs"),
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


def fork_ended_error(fork: SandboxFork) -> ZipboxError | None:
    """The error a fork that ended without its box answers: its own code (an answered
    verdict, not retried), or ``conflict`` for a cancel. None while it has not ended so."""
    error = fork.error
    if error is not None:
        code: ErrorCode = error.code if is_error_code(error.code) else "conflict"
        return ZipboxError(code, error.message, retryable=False)
    if fork.phase == "cancelled":
        return ZipboxError(
            "conflict",
            f"the fork making box {fork.child_sandbox_id} was cancelled",
            retryable=False,
        )
    return None


# A box turns ready about a second after it serves, and nothing tells the client when,
# so each poll rung is time spent asleep after it did. Doubling overshot by up to
# several seconds; 1.25 keeps the same base, cap and jitter and reaches the cap gently.
POLL_FACTOR = 1.25


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


def _fallback_readiness(detail: SandboxDetail) -> str:
    """The rule before ``readiness``, for a control plane that does not send it: ready,
    private and waiting for a tailnet, ended (destroyed, stopped, deleting), or still
    settling. Remove once every control plane sends ``readiness``."""
    if detail.ready:
        return "ready"
    joined = detail.tailscale is not None and bool(detail.tailscale.ipv4)
    if (
        detail.private_mode
        and not joined
        and detail.status == "running"
        and bool(detail.agent_public_key)
    ):
        return "awaits_tailnet"
    if detail.status in ("destroyed", "stopped"):
        return "gone"
    if detail.status == "deleting":
        return "deleting"
    return "settling"


def readiness_of(detail: SandboxDetail) -> str:
    """Whether the box will answer, as the server decided it (``readiness``)."""
    if detail.readiness in _READINESS:
        return detail.readiness
    return _fallback_readiness(detail)


def _paused_remedy(detail: SandboxDetail) -> str:
    """Start it, unless the server says it cannot start now."""
    actions = detail.allowed_actions
    if actions is None or "start" in actions:
        return "start it first"
    return "it cannot be started now; see its allowed_actions"


def ended_error(sandbox_id: str, detail: SandboxDetail) -> ZipboxError | None:
    """The error a wait raises for a box that will not become ready by itself, or None
    while it may (ready, settling, awaits_tailnet). Never retryable: an answered verdict."""
    readiness = readiness_of(detail)
    if readiness == "paused":
        return ZipboxError(
            "box_not_running",
            f"Box {sandbox_id} is paused and nothing will start it; {_paused_remedy(detail)}.",
            retryable=False,
        )
    if readiness == "start_failed":
        remedy = (
            "its name was released; create it again"
            if detail.status == "stopped"
            else "start it again"
        )
        return ZipboxError(
            "start_failed", f"Box {sandbox_id} failed to start; {remedy}.", retryable=False
        )
    if readiness in ("deleting", "gone"):
        return ZipboxError(
            "conflict",
            f"Box {sandbox_id} is {detail.status} and will not become ready.",
            retryable=False,
        )
    return None


def box_not_ready_error(sandbox_id: str, detail: SandboxDetail) -> ZipboxError:
    """The error a box call raises on a detail it cannot call: ``tailnet_not_connected``
    for a private box awaiting its tailnet, else ``box_not_running``, retryable only
    while the box is settling."""
    stated = detail.readiness if detail.readiness in _READINESS else None
    readiness = stated or _fallback_readiness(detail)
    if readiness == "awaits_tailnet":
        return ZipboxError(
            "tailnet_not_connected",
            f"Box {sandbox_id} is private and has not joined a tailnet yet; connect it "
            "with box.tailscale.connect.",
            retryable=False,
        )
    return ZipboxError(
        "box_not_running",
        f"Box {sandbox_id} is {detail.status} and not ready; start it first.",
        # Without readiness (an older control plane), the rule before it: booting or
        # restarting comes back by itself, and nothing else does.
        retryable=detail.status in ("provisioning", "running")
        if stated is None
        else stated == "settling",
    )


def _tls_failure(error: BaseException) -> bool:
    """Whether a network error was the TLS handshake (an ``ssl.SSLError`` in its chain)."""
    seen: BaseException | None = error
    for _ in range(6):
        if seen is None:
            return False
        if isinstance(seen, ssl.SSLError):
            return True
        seen = seen.__cause__ or seen.__context__
    return False


def private_unreachable_message(name: str) -> str:
    """What a box-direct call on a private box says when this device is off its tailnet."""
    return (
        f"{name} is private: it can only be reached from devices on your tailnet. "
        "Turn on Tailscale on this device (https://tailscale.com/download)."
    )


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
    name: str = ""
    private: bool = False

    @property
    def host(self) -> str:
        """The custom preview apex, without the transport prefix or port.

        The prefix is `box--` on a public box (`box--demo.zbox.sh`) and `box.` on a
        private one (`box.demo.zbox.sh`); only one is ever dropped.
        """
        host = urlparse(self.base_url).hostname or ""
        for prefix in ("box--", "box."):
            if host.startswith(prefix):
                return host[len(prefix) :]
        return host


class Core:
    """State shared by every resource: config and the per-box token cache."""

    def __init__(self, config: ClientConfig) -> None:
        self.config = config
        self._targets: dict[str, BoxTarget] = {}
        # Private boxes this client has reached at least once.
        self._reachable: set[str] = set()

    # ------------------------------------------------------------------ plumbing

    def _backoff(self) -> Backoff:
        return Backoff(
            self.config.retry_base_delay_s, self.config.retry_max_delay_s, rng=self.config.rng
        )

    def _poll_backoff(self) -> Backoff:
        return Backoff(
            self.config.poll_interval_s,
            self.config.poll_max_interval_s,
            factor=POLL_FACTOR,
            rng=self.config.rng,
        )

    def send(
        self,
        request: HttpRequest,
        *,
        retry: bool,
        passthrough: frozenset[int] = frozenset(),
        box_ladder: Backoff | None = None,
    ) -> Op[HttpResponse]:
        """Send with the SDK retry rule; raise ``ZipboxError`` on a non-2xx status.

        A failure to connect or to complete the TLS handshake is retried for EVERY
        method with no attempt ceiling (CLAUDE.md §0): nothing was sent, and a box's
        certificate can trail its readiness. The ladder is capped and jittered.

        With ``box_ladder`` (a box call's unsent ladder) that failure is raised instead,
        for the caller to re-read the box, and the ladder resets whenever a request is
        answered.
        """
        attempt = 0
        backoff = self._backoff()
        unsent_backoff = self._backoff()
        while True:
            try:
                response: HttpResponse = yield request
            except ZipboxError as network_error:
                if network_error.unsent:
                    if box_ladder is not None:
                        raise
                    yield Sleep(unsent_backoff.next())
                    continue
                error = network_error
            else:
                if box_ladder is not None:
                    box_ladder.reset()
                if 200 <= response.status < 300 or response.status in passthrough:
                    return response
                body = response.body
                if response.handle is not None:
                    body = yield ReadAll(response)
                error = error_from_response(response.status, response.headers, body)
            if not retry or not error.retryable or attempt >= self.config.max_retries:
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
        if cached is None:
            detail = yield from self.get(sandbox_id)
            cached = self._targets.get(sandbox_id)
            if cached is None:
                raise box_not_ready_error(sandbox_id, detail)
        if cached.private and sandbox_id not in self._reachable:
            yield from self.probe_private(sandbox_id, cached)
        return cached

    def probe_private(
        self, sandbox_id: str, target: BoxTarget, timeout_s: float = PRIVATE_PROBE_TIMEOUT_S
    ) -> Op[None]:
        """Check a private box answers from here before calling it.

        A private box's name resolves to its tailnet address, which off the tailnet
        does not refuse a connection, it swallows it. Any HTTP answer from
        ``/challenge`` means this device reaches the box; a network failure within
        the deadline means it does not, and the call fails now instead of hanging.
        """
        probe = HttpRequest(
            "GET",
            target.base_url + "/challenge",
            {"user-agent": _USER_AGENT},
            timeout_s=timeout_s,
        )
        try:
            yield probe
        except ZipboxError as error:
            # A failed TLS handshake is an answer: the box is reachable and only lacks
            # its certificate yet, which the box call itself waits out.
            if not _tls_failure(error):
                raise ZipboxError(
                    "box_private_unreachable",
                    private_unreachable_message(target.name or sandbox_id),
                    retryable=False,
                ) from error
        self._reachable.add(sandbox_id)
        return None

    def forget(self, sandbox_id: str) -> None:
        """Drop a cached box token (lifecycle changes may move the box)."""
        self._targets.pop(sandbox_id, None)
        self._reachable.discard(sandbox_id)

    def reresolve(self, sandbox_id: str) -> Op[None]:
        """Drop the cached box and wait for it the way ``create`` does, caching the
        target that wait read. A created box's ``baseUrl`` moves once, from its boot
        name to ``box--<name>``. Raises the wait's verdict on a paused, failed,
        deleting or gone box, and ``tailnet_not_connected`` on a private box that
        has not joined a tailnet."""
        self.forget(sandbox_id)
        detail = yield from self.wait_ready(sandbox_id)
        if sandbox_id not in self._targets:
            raise box_not_ready_error(sandbox_id, detail)
        return None

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
        """Call a box route with its token; a 401 re-fetches the token once.

        A request that never reached the box (connect, DNS, TLS handshake) waits a
        rung of one unsent ladder for the whole call, re-reads the box with
        ``reresolve`` and retries there, with no attempt ceiling.
        """
        refetched = False
        unsent_backoff = self._backoff()
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
            try:
                response = yield from self.send(
                    request,
                    retry=method == "GET",
                    passthrough=frozenset({401}),
                    box_ladder=unsent_backoff,
                )
            except ZipboxError as error:
                if not error.unsent:
                    raise
                yield Sleep(unsent_backoff.next())
                yield from self.reresolve(sandbox_id)
                continue
            if response.status != 401:
                return response
            if response.handle is not None:
                yield CloseStream(response)
            if refetched:
                # Not a deleted key: that fails the re-read above, at the control
                # plane. The control plane just vouched for this key, so the box has
                # not received its token yet.
                raise ZipboxError(
                    "unauthorized",
                    f"Box {sandbox_id} has not received this API key's token yet: the "
                    "control plane accepts the key, the box does not. A key created "
                    "moments ago reaches its running boxes within seconds; retry then.",
                    status=401,
                    retryable=True,
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
            previous = self._targets.get(sandbox_id)
            target = BoxTarget(
                detail.base_url.rstrip("/"),
                token,
                name=detail.name or sandbox_id,
                private=detail.private_mode,
            )
            if previous is None or previous.base_url != target.base_url or not target.private:
                self._reachable.discard(sandbox_id)
            self._targets[sandbox_id] = target
        else:
            self.forget(sandbox_id)
        return detail

    def get_in_wait(self, sandbox_id: str) -> Op[SandboxDetail]:
        """One read of the box inside a wait. A read that fails with a retryable error
        (a control-plane deploy) is read again on the poll ladder, never sooner than
        its ``retry_after_ms``, with no ceiling; any other error is raised."""
        backoff = self._poll_backoff()
        while True:
            try:
                return (yield from self.get(sandbox_id))
            except ZipboxError as error:
                if not error.retryable:
                    raise
                yield Sleep(max((error.retry_after_ms or 0) / 1000, backoff.next()))

    def wait_ready(self, sandbox_id: str) -> Op[SandboxDetail]:
        """Re-read the box with backoff until it is ready. No deadline.

        The server's ``readiness`` decides: ``settling`` keeps reading; a paused box
        raises ``box_not_running``, a failed start ``start_failed``, a deleting or gone
        box ``conflict``, none of them retryable. A private box that has not joined a
        tailnet (``awaits_tailnet``) is returned as soon as it is running with its
        ``agent_public_key``: the point at which a Tailscale auth key can be sealed to
        it. A read that fails with a retryable error goes on the same ladder, never
        sooner than its ``retry_after_ms``.
        """
        backoff = self._poll_backoff()
        while True:
            retry_after_s = 0.0
            try:
                detail = yield from self.get(sandbox_id)
            except ZipboxError as error:
                if not error.retryable:
                    raise
                retry_after_s = (error.retry_after_ms or 0) / 1000
            else:
                ended = ended_error(sandbox_id, detail)
                if ended is not None:
                    raise ended
                if readiness_of(detail) != "settling":
                    return detail
            yield Sleep(max(retry_after_s, backoff.next()))

    def create(
        self,
        *,
        name: str | None = None,
        tier: str | None = None,
        ssh_public_key: str | None = None,
        mailbox: bool | None = None,
        private: bool | None = None,
    ) -> Op[SandboxDetail]:
        """Claim a box (idempotent) and wait until it is ready."""
        body: dict[str, Any] = {}
        if name is not None:
            body["name"] = name
        if tier is not None:
            body["tier"] = tier
        if ssh_public_key is not None:
            body["sshPublicKey"] = ssh_public_key
        if mailbox is not None:
            body["mailbox"] = mailbox
        if private is not None:
            body["private"] = private
        created = yield from self.control(
            "POST",
            "/sandbox/identity/claim",
            body=body,
            headers={"idempotency-key": str(uuid.uuid4())},
            retry=True,
        )
        return (yield from self.wait_ready(str(created["sandboxId"])))

    def get_or_create(
        self,
        name: str,
        *,
        tier: str | None = None,
        ssh_public_key: str | None = None,
        mailbox: bool | None = None,
        private: bool | None = None,
    ) -> Op[SandboxDetail]:
        """The live box with this name, started if stopped; created with the options if
        there is none. The options apply only on create: a box that already has this
        name is returned as it is, even when it differs (another tier, not private)."""
        boxes = yield from self.list_sandboxes()
        for box in boxes:
            if box.name == name and box.status not in _DEAD_STATUSES:
                # "stopping": a stop is still landing; start() waits it out, then starts.
                # "pausing": paused while its backup uploads; the start boots it at once.
                if box.status in ("archived", "stopping", "pausing"):
                    try:
                        yield from self.start(box.id)
                    except ZipboxError as error:
                        # The name was being deleted: it is gone now, so make it.
                        if error.code == "not_found":
                            break
                        # Another caller started it first: the server's idempotent
                        # answer to a second restore. A box coming up is waited on.
                        if error.code != "conflict":
                            raise
                        detail = yield from self.get_in_wait(box.id)
                        if detail.status not in ("provisioning", "running"):
                            raise
                return (yield from self.wait_ready(box.id))
        return (
            yield from self.create(
                name=name,
                tier=tier,
                ssh_public_key=ssh_public_key,
                mailbox=mailbox,
                private=private,
            )
        )

    def fork(
        self,
        source_id: str,
        *,
        name: str,
        source_policy: ForkSourcePolicy,
        private: bool | None = None,
        tailscale_auth_key: str | None = None,
        wait: bool = True,
    ) -> Op[SandboxDetail]:
        """POST /sandbox/:id/fork (idempotent), then wait on the new box.

        One Idempotency-Key per call, reused by every retry, so a retried fork never
        makes two boxes. The body is exactly ``{name, sourcePolicy}``, plus
        ``private: true`` when ``private=True`` (absent = public): the Tailscale
        auth key never leaves this machine except sealed to the new box's own key,
        after the wait. ``wait=False`` returns the accepted box as the 202 described it.
        """
        auth_key: str | None = None
        if tailscale_auth_key is not None:
            auth_key = tailscale_auth_key.strip()
            if not auth_key:
                raise ZipboxError("invalid_request", "the Tailscale auth key is empty")
            if not wait:
                raise ZipboxError(
                    "invalid_request",
                    "tailscale_auth_key needs wait=True: the key is sealed to the new box "
                    "once it is running",
                )
        body: dict[str, Any] = {"name": name, "sourcePolicy": source_policy}
        if private:
            body["private"] = True
        # A running source pauses for its capture: its cached address goes with it.
        self.forget(source_id)
        accepted = yield from self.control(
            "POST",
            f"/sandbox/{quote(source_id, safe='')}/fork",
            body=body,
            headers={"idempotency-key": str(uuid.uuid4())},
            retry=True,
        )
        child = accepted.get("sandbox") if isinstance(accepted, dict) else None
        if child is None:
            # The key found a fork that already ended and removed its box: that fork's
            # verdict, never an "unexpected response".
            fork = SandboxFork.parse(accepted.get("fork")) if isinstance(accepted, dict) else None
            if fork is not None:
                raise fork_ended_error(fork) or ZipboxError(
                    "conflict",
                    f"the fork making box {fork.child_sandbox_id} ended",
                    retryable=False,
                )
        if not isinstance(child, dict) or not isinstance(child.get("id"), str):
            raise ZipboxError(
                "unavailable", "unexpected response for POST /sandbox/:id/fork", retryable=False
            )
        child_id: str = child["id"]
        if not wait:
            # The 202's own fork is the one making this box; no token exists yet.
            return SandboxDetail.from_json({**child, "fork": accepted.get("fork"), "ready": False})
        try:
            detail = yield from self.wait_ready(child_id)
        except ZipboxError as error:
            if error.code != "not_found":
                raise
            # A cancelled or failed fork removes its box: the box reads not_found
            # while the fork still says why.
            yield from self.raise_fork_error(child_id)
            raise
        if auth_key is not None:
            yield from self.tailscale_connect(child_id, auth_key)
        return detail

    def raise_fork_error(self, child_id: str) -> Op[None]:
        """A fork's box that went away: raise the fork's verdict (its own error, or
        ``conflict`` for a cancel), if it has one. A fork that could not be read (a
        retryable error) is no verdict: that error is raised."""
        try:
            fork = yield from self.get_fork(child_id)
        except ZipboxError as failure:
            if failure.retryable:
                raise
            return None
        error = fork_ended_error(fork)
        if error is not None:
            raise error
        return None

    def get_fork(self, child_id: str) -> Op[SandboxFork]:
        """GET /sandbox/:id/fork: the fork that is making (or made) this box."""
        data = yield from self.control("GET", f"/sandbox/{quote(child_id, safe='')}/fork")
        fork = SandboxFork.parse(data)
        if fork is None:
            raise ZipboxError(
                "unavailable", "unexpected response for GET /sandbox/:id/fork", retryable=False
            )
        return fork

    def lifecycle(self, sandbox_id: str, action: str, body: Any = None) -> Op[None]:
        """POST /sandbox/:id/<action>."""
        self.forget(sandbox_id)
        yield from self.control(
            "POST", f"/sandbox/{quote(sandbox_id, safe='')}/{action}", body=body
        )
        return None

    def cancel_transition(self, sandbox_id: str) -> Op[TransitionCancelOutcome]:
        """POST /sandbox/:id/transition/cancel; the server's outcome, not retried."""
        self.forget(sandbox_id)
        body = yield from self.control(
            "POST", f"/sandbox/{quote(sandbox_id, safe='')}/transition/cancel"
        )
        outcome = body.get("outcome") if isinstance(body, dict) else None
        if outcome == "cleared":
            return "cleared"
        if outcome == "cancelling":
            return "cancelling"
        if outcome == "none":
            return "none"
        raise ZipboxError(
            "unavailable",
            "unexpected response for POST /sandbox/:id/transition/cancel",
            retryable=False,
        )

    def set_private(self, sandbox_id: str, enabled: bool) -> Op[None]:
        """PUT /sandbox/:id/private."""
        self.forget(sandbox_id)
        yield from self.control(
            "PUT", f"/sandbox/{quote(sandbox_id, safe='')}/private", body={"enabled": enabled}
        )
        return None

    # ----------------------------------------------------------------- tailscale

    def tailscale_status(self, sandbox_id: str) -> Op[TailscaleStatus | None]:
        """GET /sandbox/:id/tailscale; None when the box never joined a tailnet."""
        data = yield from self.control("GET", f"/sandbox/{quote(sandbox_id, safe='')}/tailscale")
        return TailscaleStatus.parse((data or {}).get("tailscale"))

    def tailscale_connect(self, sandbox_id: str, auth_key: str) -> Op[None]:
        """Seal ``auth_key`` to the box's own key here, then hand only the envelope over.

        The key is read fresh for every seal: a claim re-mints it, and an envelope
        sealed to the old key cannot be opened. ``box_key_changed`` re-reads and
        re-seals once.
        """
        path = f"/sandbox/{quote(sandbox_id, safe='')}/tailscale"
        resealed = False
        while True:
            detail = yield from self.get(sandbox_id)
            box_key = detail.agent_public_key
            if not box_key or detail.status not in _SEALABLE_STATUSES:
                raise ZipboxError(
                    "box_not_running",
                    f"Box {sandbox_id} is {detail.status}; start it before connecting Tailscale.",
                    # A box that is settling grows its key by itself; one that is paused
                    # or gone does not.
                    retryable=readiness_of(detail) == "settling",
                )
            envelope = seal(box_key, tailscale_info(sandbox_id), auth_key)
            try:
                yield from self.control(
                    "PUT", path, body={"envelope": envelope, "agentPublicKey": box_key}
                )
            except ZipboxError as error:
                if error.code != "box_key_changed" or resealed:
                    raise
                resealed = True
                continue
            return None

    def tailscale_disconnect(self, sandbox_id: str) -> Op[None]:
        """DELETE /sandbox/:id/tailscale: leave the tailnet."""
        yield from self.control("DELETE", f"/sandbox/{quote(sandbox_id, safe='')}/tailscale")
        return None

    def start(self, sandbox_id: str) -> Op[None]:
        """Start (restore) a stopped box.

        A stop is accepted at once but its disk takes seconds to minutes to land;
        meanwhile the box reads ``stopping`` and the restore answers ``conflict``.
        Wait that out with backoff and start the box once it reads ``archived``. A
        box that comes out of the wait already starting or running (a resize restores
        itself, a failed archive leaves it running) is started. Any other
        ``conflict`` is raised as it came.
        """
        waited = False
        while True:
            try:
                return (yield from self.lifecycle(sandbox_id, "restore"))
            except ZipboxError as error:
                if error.code != "conflict":
                    raise
                detail = yield from self.get_in_wait(sandbox_id)
                if detail.status != "stopping":
                    # A second conflict after a stop was waited out: the box restored
                    # itself, or it is archived and will not restore (that is final).
                    if waited and detail.status in ("provisioning", "running"):
                        return None
                    raise error
                backoff = self._poll_backoff()
                while detail.status == "stopping":
                    yield Sleep(backoff.next())
                    detail = yield from self.get_in_wait(sandbox_id)
                waited = True
                if detail.status in ("provisioning", "running"):
                    return None
                if detail.status != "archived":
                    raise error

    def delete(self, sandbox_id: str, archive: bool | None = None) -> Op[None]:
        """DELETE /sandbox/:id. ``archive=False`` skips the recoverable copy."""
        self.forget(sandbox_id)
        body = None if archive is None else {"archiveBeforeDestroy": archive}
        yield from self.control("DELETE", f"/sandbox/{quote(sandbox_id, safe='')}", body=body)
        return None

    # ---------------------------------------------------------------------- exec

    def exec_events(self, sandbox_id: str, body: dict[str, Any]) -> StreamOp:
        """POST /exec and emit its events until exit. A stream that drops after the
        command started re-attaches from its last cursor (``command_events``)."""
        response = yield from self.box(
            sandbox_id, "POST", "/exec", body=body, stream=True, accept="text/event-stream"
        )
        parser = SseParser()
        command_id: str | None = None
        cursor = "0-0"
        try:
            while True:
                chunk: bytes = yield ReadChunk(response)
                if not chunk:
                    yield CloseStream(response)
                    break
                for message in parser.feed(chunk):
                    event = event_from_sse(message, message.id or parser.last_id or "0-0")
                    if event is None:
                        continue
                    cursor = event.cursor
                    if event.type == "start":
                        command_id = event.command_id
                    yield Emit(event)
                    if event.type == "exit":
                        yield CloseStream(response)
                        return
        except ZipboxError as error:
            if command_id is None or not error.retryable:
                raise
        if command_id is None:
            raise ZipboxError("unavailable", "The exec stream ended before the command exited.")
        yield from self.command_events(sandbox_id, command_id, cursor)

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
                # Never sooner than the server asked.
                retry_after_s = (error.retry_after_ms or 0) / 1000
                yield Sleep(max(retry_after_s, backoff.next()))
                continue
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

    def run_code(
        self, sandbox_id: str, language: Language, code: str, options: dict[str, Any]
    ) -> Op[ExecResult]:
        """Write ``code`` to a temp file, run it with the language's interpreter through
        the login shell (``options`` are ``exec_body`` keywords), then remove it."""
        if language not in _RUNNERS:
            raise ZipboxError("invalid_request", f"Unsupported language {language!r}.")
        interpreter, extension = _RUNNERS[language]
        path = f"/tmp/zipbox-run-{uuid.uuid4().hex}.{extension}"
        yield from self.write(sandbox_id, path, code.encode())
        quoted = shlex.quote(path)
        script = f"{interpreter} {quoted}; status=$?; rm -f {quoted}; exit $status"
        return (yield from self.exec(sandbox_id, exec_body(script, **options)))

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
        backoff = self._backoff()
        while True:
            retry_after = 0.0
            try:
                result = yield from self.exec(
                    sandbox_id,
                    exec_body(["tribes-caddy", "add", host, f"127.0.0.1:{port}", "--no-wait"]),
                )
                if result.exit_code == 0:
                    break
                if result.exit_code != 75:
                    raise ZipboxError(
                        "conflict",
                        f"tribes-caddy add failed: {(result.stderr or result.stdout).strip()}",
                    )
                try:
                    pending = json.loads(result.stdout.strip().splitlines()[-1])
                    delay = pending["retryAfterMs"]
                    if (
                        pending.get("status") != "pending"
                        or not isinstance(delay, (int, float))
                        or not math.isfinite(delay)
                        or delay < 0
                    ):
                        raise ValueError("invalid pending status")
                    retry_after = delay / 1000
                except (ValueError, KeyError, IndexError, TypeError) as error:
                    raise ZipboxError(
                        "conflict", "preview returned invalid pending status"
                    ) from error
            except ZipboxError as error:
                if not error.retryable:
                    raise
                retry_after = (error.retry_after_ms or 0) / 1000
            yield Sleep(max(backoff.next(), retry_after))
        return Preview(url=f"https://{host}", label=chosen)

    def unpreview(self, sandbox_id: str, label: str) -> Op[None]:
        """Remove a preview by label (or full host) via tribes-caddy rm."""
        target = yield from self.target(sandbox_id)
        host = label if label.endswith(f".{target.host}") else f"{label}.{target.host}"
        result = yield from self.exec(sandbox_id, exec_body(["tribes-caddy", "rm", host]))
        if result.exit_code != 0:
            raise ZipboxError(
                "conflict", f"tribes-caddy rm failed: {(result.stderr or result.stdout).strip()}"
            )
        return None

    def logs(
        self, sandbox_id: str, since: str | None, lines: int | None, unit: str | None
    ) -> Op[str]:
        """The box's system journal. A failed journalctl raises ``conflict``."""
        argv = ["journalctl", "-o", "short-iso", "--no-pager"]
        if since is not None:
            argv += ["--since", since]
        if lines is not None:
            argv += ["-n", str(lines)]
        if unit is not None:
            argv += ["-u", unit]
        result = yield from self.exec(sandbox_id, exec_body(argv))
        if result.exit_code != 0:
            raise ZipboxError(
                "conflict", f"journalctl failed: {(result.stderr or result.stdout).strip()}"
            )
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
