"""Typed views over the wire shapes. Every model keeps the raw JSON in ``raw``."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Literal

Tier = Literal["xs", "s", "m", "l", "xl", "xxl", "enterprise"]
Language = Literal["python", "node", "bash"]
Signal = Literal["TERM", "KILL", "INT", "HUP", "QUIT"]
CommandState = Literal["running", "exited", "unknown"]
ExecEventType = Literal["start", "stdout", "stderr", "gap", "exit"]
FileKind = Literal["directory", "file", "symlink", "other"]
# What ``cancel_transition`` did: ``cleared`` (nothing was executing; the box is
# already back), ``cancelling`` (the host's abort is on its way), ``none`` (nothing
# was in flight). A change past its point of no return raises ``conflict``.
TransitionCancelOutcome = Literal["cleared", "cancelling", "none"]
# How a fork may treat its source. ``require_paused`` refuses a running source
# (``pause_required``); ``pause_and_resume`` lets a running source pause for its
# capture (its sessions disconnect) and resume right after. A paused source stays
# paused either way.
ForkSourcePolicy = Literal["require_paused", "pause_and_resume"]


def _opt_str(value: Any) -> str | None:
    return value if isinstance(value, str) else None


def _opt_num(value: Any) -> float | None:
    return float(value) if isinstance(value, (int, float)) and not isinstance(value, bool) else None


def _opt_int(value: Any) -> int | None:
    return value if isinstance(value, int) and not isinstance(value, bool) else None


@dataclass(frozen=True)
class TailscalePeer:
    """One of the owner's other devices on the tailnet, as the box sees it.

    ``connection`` is ``direct`` (peer to peer), ``relay`` (through a Tailscale
    DERP relay) or None (no recent traffic). Values this SDK does not know are kept
    as given.
    """

    name: str
    dns_name: str | None
    os: str | None
    online: bool
    ipv4: str | None
    connection: str | None
    raw: dict[str, Any] = field(repr=False, compare=False)

    @classmethod
    def parse(cls, value: Any) -> TailscalePeer | None:
        """Parse one peer; None for an entry without a name."""
        if not isinstance(value, dict) or not isinstance(value.get("name"), str):
            return None
        return cls(
            name=value["name"],
            dns_name=_opt_str(value.get("dnsName")),
            os=_opt_str(value.get("os")),
            online=value.get("online") is True,
            ipv4=_opt_str(value.get("ipv4")),
            connection=_opt_str(value.get("connection")),
            raw=value,
        )


def _peers(value: Any) -> tuple[TailscalePeer, ...] | None:
    if not isinstance(value, list):
        return None
    return tuple(peer for peer in (TailscalePeer.parse(item) for item in value) if peer is not None)


@dataclass(frozen=True)
class TailscaleStatus:
    """A box's tailnet: its live status merged with the last known identity.

    ``state`` is ``joining``, ``connected``, ``needsApproval``, ``needsLogin``,
    ``stopped`` or ``failed``, or None when only the identity is known (the box is
    paused). ``pending`` is True while an auth key is on its way to the box; it
    dropping without the state advancing means the key never arrived.
    ``key_expiry`` is None when key expiry is off, which is the recommended setting.
    ``peers`` is the owner's other devices (online first, at most 50), or None while
    the box is not reporting.
    """

    state: str | None
    tailnet: str | None
    ipv4: str | None
    ipv6: str | None
    dns_name: str | None
    key_expiry: str | None
    pending: bool
    raw: dict[str, Any] = field(repr=False, compare=False)
    peers: tuple[TailscalePeer, ...] | None = None

    @classmethod
    def from_json(cls, data: dict[str, Any]) -> TailscaleStatus:
        """Parse a box's ``tailscale`` field."""
        return cls(
            state=_opt_str(data.get("state")),
            tailnet=_opt_str(data.get("tailnet")),
            ipv4=_opt_str(data.get("ipv4")),
            ipv6=_opt_str(data.get("ipv6")),
            dns_name=_opt_str(data.get("dnsName")),
            key_expiry=_opt_str(data.get("keyExpiry")),
            pending=data.get("pending") is True,
            raw=data,
            peers=_peers(data.get("peers")),
        )

    @classmethod
    def parse(cls, value: Any) -> TailscaleStatus | None:
        """Parse a nullable ``tailscale`` field."""
        return cls.from_json(value) if isinstance(value, dict) else None


@dataclass(frozen=True)
class SandboxForkError:
    """Why a fork ended without a new box: ``source_unsupported``, ``source_gone``,
    ``identity_reused``, ``copy_failed`` or ``start_failed``. Codes this SDK does not
    know are kept as given."""

    code: str
    message: str

    @classmethod
    def parse(cls, value: Any) -> SandboxForkError | None:
        """Parse a nullable fork ``error``."""
        if not isinstance(value, dict) or not isinstance(value.get("code"), str):
            return None
        return cls(code=value["code"], message=str(value.get("message") or value["code"]))


@dataclass(frozen=True)
class SandboxFork:
    """One fork operation, as ``GET /sandbox/:child_id/fork`` and the child's row carry it.

    ``phase`` is ``preparing_identity``, ``queued``, ``capturing``, ``copying``,
    ``starting``, ``complete``, ``cancelling``, ``cancelled`` or ``failed``.
    ``source_recovery`` is ``not_needed`` (the source was paused and stays paused),
    ``pending`` (it owes its resume), ``resumed`` or ``superseded``. ``capture_mode``
    is ``reflink`` or ``copy`` once captured. ``waiting_on`` says why a step waits
    (``capacity``, ``host_offline``, ``busy``, …), with ``attempt`` and
    ``next_attempt_at`` the retry ladder's position. ``error`` is set on ``failed``
    only. Values this SDK does not know are kept as given.
    """

    operation_id: str
    source_sandbox_id: str
    child_sandbox_id: str
    source_policy: str
    phase: str
    source_recovery: str
    capture_mode: str | None
    waiting_on: str | None
    attempt: int
    next_attempt_at: str | None
    cancel_requested_at: str | None
    error: SandboxForkError | None
    created_at: str | None
    updated_at: str | None
    completed_at: str | None
    raw: dict[str, Any] = field(repr=False, compare=False)

    @classmethod
    def parse(cls, value: Any) -> SandboxFork | None:
        """Parse a fork projection; None for anything without an operation id."""
        if not isinstance(value, dict) or not isinstance(value.get("operationId"), str):
            return None
        return cls(
            operation_id=value["operationId"],
            source_sandbox_id=str(value.get("sourceSandboxId") or ""),
            child_sandbox_id=str(value.get("childSandboxId") or ""),
            source_policy=str(value.get("sourcePolicy") or ""),
            phase=str(value.get("phase") or ""),
            source_recovery=str(value.get("sourceRecovery") or ""),
            capture_mode=_opt_str(value.get("captureMode")),
            waiting_on=_opt_str(value.get("waitingOn")),
            attempt=_opt_int(value.get("attempt")) or 0,
            next_attempt_at=_opt_str(value.get("nextAttemptAt")),
            cancel_requested_at=_opt_str(value.get("cancelRequestedAt")),
            error=SandboxForkError.parse(value.get("error")),
            created_at=_opt_str(value.get("createdAt")),
            updated_at=_opt_str(value.get("updatedAt")),
            completed_at=_opt_str(value.get("completedAt")),
            raw=value,
        )


@dataclass(frozen=True)
class SandboxInfo:
    """One box, as ``GET /sandbox`` lists it."""

    id: str
    name: str | None
    domain: str | None
    ipv6: str | None
    # provisioning, running, stopping, paused, deleting or destroyed. paused: its Pause
    # took effect and nothing is billed; resume() brings it back. stopping is a Pause
    # still landing.
    status: str
    tier: str | None
    pending_tier: str | None
    is_archived: bool | None
    cpu_pct: float | None
    mem_pct: float | None
    disk_pct: float | None
    created_at: str | None
    updated_at: str | None
    # Private ("tailnet only"): reachable only from devices on the owner's tailnet.
    private_mode: bool
    # The box's own P-256 public key (base64 SPKI DER) that box credentials are sealed
    # to. None until the box has started; a claim re-mints it.
    agent_public_key: str | None
    tailscale: TailscaleStatus | None
    # False when the box's ZipOS predates Tailscale (connecting raises
    # ``tailscale_requires_update`` until the ZipOS update is accepted); None until
    # the box has reported since it booted.
    tailscale_supported: bool | None
    # The fork still making this box (phase, source recovery, waits, error); None on
    # every box that is not being forked into.
    fork: SandboxFork | None
    # What the box is doing now: starting, running, stopping, paused, resizing, forking,
    # deleting, delete_failed, start_failed or gone. None from an older control plane.
    phase: str | None
    # The lifecycle verbs the box accepts now (resume, pause, restart, delete,
    # cancel_transition, fork). A listed verb can still be refused for a reason outside
    # the box: credit, capacity, an argument. None from an older control plane.
    allowed_actions: tuple[str, ...] | None
    raw: dict[str, Any] = field(repr=False, compare=False)

    @classmethod
    def from_json(cls, data: dict[str, Any]) -> SandboxInfo:
        """Parse a box row."""
        return cls(
            id=str(data["id"]),
            name=_opt_str(data.get("name")),
            domain=_opt_str(data.get("domain")),
            ipv6=_opt_str(data.get("ipv6")),
            status=str(data.get("status", "")),
            tier=_opt_str(data.get("tier")),
            pending_tier=_opt_str(data.get("pendingTier")),
            is_archived=data.get("isArchived")
            if isinstance(data.get("isArchived"), bool)
            else None,
            cpu_pct=_opt_num(data.get("cpuPct")),
            mem_pct=_opt_num(data.get("memPct")),
            disk_pct=_opt_num(data.get("diskPct")),
            created_at=_opt_str(data.get("createdAt")),
            updated_at=_opt_str(data.get("updatedAt")),
            private_mode=data.get("privateMode") is True,
            agent_public_key=_opt_str(data.get("agentPublicKey")),
            tailscale=TailscaleStatus.parse(data.get("tailscale")),
            tailscale_supported=data.get("tailscaleSupported")
            if isinstance(data.get("tailscaleSupported"), bool)
            else None,
            fork=SandboxFork.parse(data.get("fork")),
            phase=_opt_str(data.get("phase")),
            allowed_actions=tuple(str(action) for action in data["allowedActions"])
            if isinstance(data.get("allowedActions"), list)
            else None,
            raw=data,
        )


@dataclass(frozen=True)
class SandboxDetail(SandboxInfo):
    """One box from ``GET /sandbox/:id``: the row plus whether it is ready and where."""

    ready: bool = False
    base_url: str | None = None
    # Whether base_url completes a TLS handshake with a trusted certificate yet. A
    # fresh name's certificate can trail ``ready``; the SDK's box calls retry the
    # handshake meanwhile.
    https_ready: bool = False
    # Whether the box will answer, from the same connection gate as ``ready``: what
    # every wait reads. ready, settling, awaits_tailnet, paused, start_failed, deleting
    # or gone. None from an older control plane (the SDK then reads ready and status).
    readiness: str | None = None

    @classmethod
    def from_json(cls, data: dict[str, Any]) -> SandboxDetail:
        """Parse a box detail."""
        base = SandboxInfo.from_json(data)
        return cls(
            **{k: getattr(base, k) for k in base.__dataclass_fields__},
            ready=data.get("ready") is True,
            base_url=_opt_str(data.get("baseUrl")),
            https_ready=data.get("httpsReady") is True,
            readiness=_opt_str(data.get("readiness")),
        )


@dataclass(frozen=True)
class ExecResult:
    """A finished command. ``exit_code`` is shell-style (128+N when killed by signal N)."""

    id: str
    exit_code: int | None
    stdout: str
    stderr: str
    timed_out: bool = False
    signal: str | None = None


@dataclass(frozen=True)
class ExecEvent:
    """One event of a command's output stream.

    ``cursor`` is the resume point (``<stdout bytes>-<stderr bytes>``) to pass back as
    ``since``. ``data`` is set for ``stdout``/``stderr``; the exit fields for ``exit``.
    """

    type: ExecEventType
    cursor: str
    data: str | None = None
    command_id: str | None = None
    stream: str | None = None
    message: str | None = None
    state: str | None = None
    exit_code: int | None = None
    signal: str | None = None
    timed_out: bool | None = None


@dataclass(frozen=True)
class CommandInfo:
    """A command's record on the box."""

    id: str
    state: CommandState
    argv: list[str]
    cwd: str
    background: bool
    exit_code: int | None
    signal: str | None
    timed_out: bool
    started_at: str | None
    ended_at: str | None
    stdout_bytes: int
    stderr_bytes: int
    raw: dict[str, Any] = field(repr=False, compare=False)

    @classmethod
    def from_json(cls, data: dict[str, Any]) -> CommandInfo:
        """Parse a command record."""
        state = data.get("state")
        return cls(
            id=str(data["id"]),
            state=state if state in ("running", "exited", "unknown") else "unknown",
            argv=[str(a) for a in data.get("argv") or []],
            cwd=str(data.get("cwd") or ""),
            background=data.get("background") is True,
            exit_code=_opt_int(data.get("exitCode")),
            signal=_opt_str(data.get("signal")),
            timed_out=data.get("timedOut") is True,
            started_at=_opt_str(data.get("startedAt")),
            ended_at=_opt_str(data.get("endedAt")),
            stdout_bytes=_opt_int(data.get("stdoutBytes")) or 0,
            stderr_bytes=_opt_int(data.get("stderrBytes")) or 0,
            raw=data,
        )


@dataclass(frozen=True)
class FileEntry:
    """One directory entry."""

    name: str
    kind: FileKind
    size_bytes: int
    modified_at_ms: float
    path: str

    @classmethod
    def from_json(cls, data: dict[str, Any], parent: str) -> FileEntry:
        """Parse a listing entry under ``parent``."""
        kind = data.get("kind")
        name = str(data["name"])
        return cls(
            name=name,
            kind=kind if kind in ("directory", "file", "symlink", "other") else "other",
            size_bytes=_opt_int(data.get("sizeBytes")) or 0,
            modified_at_ms=_opt_num(data.get("modifiedAtMs")) or 0.0,
            path=parent.rstrip("/") + "/" + name if parent != "/" else "/" + name,
        )


@dataclass(frozen=True)
class Preview:
    """A port published at a public HTTPS URL."""

    url: str
    label: str


@dataclass(frozen=True)
class SshInfo:
    """How to SSH into a box (IPv6, direct)."""

    host: str
    user: str
    command: str
