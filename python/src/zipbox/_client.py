"""The blocking client: ``Zipbox``. Each method drives one core operation."""

from __future__ import annotations

from collections.abc import Iterator, Mapping
from pathlib import Path
from types import TracebackType
from typing import Any, TypeVar

import httpx

from zipbox._core import CommandLine, Core, exec_body, resolve_config
from zipbox._effects import Op, StreamOp, run_sync, stream_sync
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
    Tier,
)
from zipbox._pty import Pty, new_state
from zipbox._transport import HttpxSyncTransport

T = TypeVar("T")


class Zipbox:
    """Blocking Zipbox client.

    >>> zb = Zipbox()  # reads ZIPBOX_API_KEY
    >>> box = zb.sandboxes.get_or_create("demo")
    >>> print(box.exec("uname -a").stdout)
    """

    def __init__(
        self,
        api_key: str | None = None,
        *,
        base_url: str | None = None,
        max_retries: int = 3,
        http_client: httpx.Client | None = None,
        retry_base_delay_s: float = 0.5,
        retry_max_delay_s: float = 30.0,
        poll_interval_s: float = 1.0,
        poll_max_interval_s: float = 10.0,
        pty_base_delay_s: float = 0.5,
        pty_max_delay_s: float = 30.0,
    ) -> None:
        """Create a client. ``api_key`` defaults to ``$ZIPBOX_API_KEY`` and ``base_url``
        to ``$ZIPBOX_API_URL`` or https://api.tribes.xyz. No call has a default timeout."""
        self._core = Core(
            resolve_config(
                api_key,
                base_url,
                max_retries,
                retry_base_delay_s=retry_base_delay_s,
                retry_max_delay_s=retry_max_delay_s,
                poll_interval_s=poll_interval_s,
                poll_max_interval_s=poll_max_interval_s,
                pty_base_delay_s=pty_base_delay_s,
                pty_max_delay_s=pty_max_delay_s,
            )
        )
        self._transport = HttpxSyncTransport(http_client)
        self.sandboxes = Sandboxes(self)
        self.account = Account(self)

    def _run(self, op: Op[T]) -> T:
        return run_sync(self._transport, op)

    def _stream(self, op: StreamOp) -> Iterator[Any]:
        return stream_sync(self._transport, op)

    def sandbox(self, sandbox_id: str) -> Sandbox:
        """A handle on a box by id. Makes no request."""
        return Sandbox(self, sandbox_id, None)

    def close(self) -> None:
        """Release HTTP connections."""
        self._transport.close()

    def __enter__(self) -> Zipbox:
        return self

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        tb: TracebackType | None,
    ) -> None:
        self.close()


class Sandboxes:
    """Box lifecycle: ``zb.sandboxes``."""

    def __init__(self, client: Zipbox) -> None:
        self._client = client

    def create(
        self,
        *,
        name: str | None = None,
        tier: Tier | None = None,
        ssh_public_key: str | None = None,
    ) -> Sandbox:
        """Create a box and wait until it is ready (no deadline)."""
        detail = self._client._run(
            self._client._core.create(name=name, tier=tier, ssh_public_key=ssh_public_key)
        )
        return Sandbox(self._client, detail.id, detail)

    def get_or_create(
        self, name: str, *, tier: Tier | None = None, ssh_public_key: str | None = None
    ) -> Sandbox:
        """Return the live box named ``name`` (starting it if stopped), or create it."""
        detail = self._client._run(
            self._client._core.get_or_create(name, tier=tier, ssh_public_key=ssh_public_key)
        )
        return Sandbox(self._client, detail.id, detail)

    def list(self) -> list[SandboxInfo]:
        """Every box on the account."""
        return self._client._run(self._client._core.list_sandboxes())

    def get(self, sandbox_id: str) -> Sandbox:
        """One box, with its current detail."""
        detail = self._client._run(self._client._core.get(sandbox_id))
        return Sandbox(self._client, sandbox_id, detail)

    def start(self, sandbox_id: str) -> None:
        """Start a stopped box (402 ``insufficient_credits`` when the account is dry)."""
        self._client._run(self._client._core.start(sandbox_id))

    def stop(self, sandbox_id: str) -> None:
        """Stop a box: disk archived, nothing billed while stopped."""
        self._client._run(self._client._core.lifecycle(sandbox_id, "pause"))

    def restart(self, sandbox_id: str) -> None:
        """Hard-restart a running box; files survive."""
        self._client._run(self._client._core.lifecycle(sandbox_id, "reset"))

    def delete(self, sandbox_id: str) -> None:
        """Delete a box (a recoverable copy is kept for 30 days)."""
        self._client._run(self._client._core.delete(sandbox_id))

    def set_tier(self, sandbox_id: str, tier: Tier) -> None:
        """Move a box to another tier."""
        self._client._run(self._client._core.lifecycle(sandbox_id, "tier", {"tier": tier}))


class Sandbox:
    """A box. Returned by ``create``, ``get``, ``get_or_create`` and ``zb.sandbox(id)``."""

    def __init__(self, client: Zipbox, sandbox_id: str, info: SandboxDetail | None) -> None:
        self._client = client
        self.id = sandbox_id
        self.info = info
        self.commands = Commands(client, sandbox_id)
        self.files = Files(client, sandbox_id)
        self.pty = PtyApi(client, sandbox_id)
        self.preview = Previews(client, sandbox_id)

    def __repr__(self) -> str:
        status = self.info.status if self.info else "?"
        return f"Sandbox(id={self.id!r}, status={status!r})"

    def refresh(self) -> SandboxDetail:
        """Re-read the box's detail."""
        self.info = self._client._run(self._client._core.get(self.id))
        return self.info

    def exec(
        self,
        cmd: CommandLine,
        *,
        cwd: str | None = None,
        env: Mapping[str, str] | None = None,
        timeout_ms: int | None = None,
        user: str | None = None,
    ) -> ExecResult:
        """Run a command to completion. A string runs under ``bash -lc``; a list is argv.
        A non-zero exit is returned in ``exit_code``, never raised."""
        body = exec_body(cmd, cwd=cwd, env=env, timeout_ms=timeout_ms, user=user)
        return self._client._run(self._client._core.exec(self.id, body))

    def exec_stream(
        self,
        cmd: CommandLine,
        *,
        cwd: str | None = None,
        env: Mapping[str, str] | None = None,
        timeout_ms: int | None = None,
        user: str | None = None,
    ) -> Iterator[ExecEvent]:
        """Run a command and yield its events as they happen. Leaving the loop early
        kills the command (use ``exec_background`` for one that should outlive you)."""
        body = exec_body(cmd, cwd=cwd, env=env, timeout_ms=timeout_ms, user=user)
        return self._client._stream(self._client._core.exec_events(self.id, body))

    def exec_background(
        self,
        cmd: CommandLine,
        *,
        cwd: str | None = None,
        env: Mapping[str, str] | None = None,
        timeout_ms: int | None = None,
        user: str | None = None,
        stdin: bool = False,
    ) -> Command:
        """Start a command that keeps running with nobody attached; returns its handle."""
        body = exec_body(
            cmd, cwd=cwd, env=env, timeout_ms=timeout_ms, user=user, stdin=stdin or None
        )
        info = self._client._run(self._client._core.exec_background(self.id, body))
        return Command(self._client, self.id, info.id, info)

    def run_code(self, language: Language, code: str) -> ExecResult:
        """Run a snippet with ``python3``, ``node`` or ``bash``."""
        return self._client._run(self._client._core.run_code(self.id, language, code))

    def logs(self, since: str | None = None, lines: int | None = None) -> str:
        """The box's system journal (``journalctl --since``)."""
        return self._client._run(self._client._core.logs(self.id, since, lines))

    def ssh_info(self) -> SshInfo:
        """The direct IPv6 SSH command for this box."""
        return self._client._run(self._client._core.ssh_info(self.id))

    def screenshot(self) -> bytes:
        """A PNG of the box's desktop."""
        return self._client._run(self._client._core.screenshot(self.id))

    def start(self) -> None:
        """Start this box."""
        self._client.sandboxes.start(self.id)

    def stop(self) -> None:
        """Stop this box."""
        self._client.sandboxes.stop(self.id)

    def restart(self) -> None:
        """Hard-restart this box."""
        self._client.sandboxes.restart(self.id)

    def delete(self) -> None:
        """Delete this box."""
        self._client.sandboxes.delete(self.id)

    def set_tier(self, tier: Tier) -> None:
        """Move this box to another tier."""
        self._client.sandboxes.set_tier(self.id, tier)

    def wait_ready(self) -> SandboxDetail:
        """Block until the box is ready (no deadline)."""
        self.info = self._client._run(self._client._core.wait_ready(self.id))
        return self.info


class Command:
    """A command on a box, usually started with ``exec_background``."""

    def __init__(
        self, client: Zipbox, sandbox_id: str, command_id: str, info: CommandInfo | None
    ) -> None:
        self._client = client
        self._sandbox_id = sandbox_id
        self.id = command_id
        self.info = info

    def __repr__(self) -> str:
        state = self.info.state if self.info else "?"
        return f"Command(id={self.id!r}, state={state!r})"

    def get(self) -> CommandInfo:
        """Re-read the command's record."""
        self.info = self._client._run(self._client._core.command(self._sandbox_id, self.id))
        return self.info

    def events(self, since: str | None = None) -> Iterator[ExecEvent]:
        """Replay from ``since`` (an event cursor) and follow until exit."""
        return self._client._stream(
            self._client._core.command_events(self._sandbox_id, self.id, since)
        )

    def wait(self) -> ExecResult:
        """Block until the command exits; return its full output."""
        return self._client._run(self._client._core.wait(self._sandbox_id, self.id))

    def stdin(self, data: str | bytes) -> None:
        """Write to the command's stdin (start it with ``stdin=True``)."""
        payload = data.encode() if isinstance(data, str) else data
        self._client._run(self._client._core.stdin(self._sandbox_id, self.id, payload, False))

    def close_stdin(self) -> None:
        """Close the command's stdin."""
        self._client._run(self._client._core.stdin(self._sandbox_id, self.id, b"", True))

    def signal(self, signal: Signal = "TERM") -> None:
        """Send a signal to the command."""
        self._client._run(self._client._core.signal(self._sandbox_id, self.id, signal))


class Commands:
    """Commands on one box: ``box.commands``."""

    def __init__(self, client: Zipbox, sandbox_id: str) -> None:
        self._client = client
        self._sandbox_id = sandbox_id

    def list(self) -> list[CommandInfo]:
        """Running and recently finished commands."""
        return self._client._run(self._client._core.commands(self._sandbox_id))

    def get(self, command_id: str) -> CommandInfo:
        """One command's record."""
        return self._client._run(self._client._core.command(self._sandbox_id, command_id))

    def handle(self, command_id: str) -> Command:
        """A handle on a command by id. Makes no request."""
        return Command(self._client, self._sandbox_id, command_id, None)

    def events(self, command_id: str, since: str | None = None) -> Iterator[ExecEvent]:
        """Replay from ``since`` and follow until exit."""
        return self.handle(command_id).events(since)

    def stdin(self, command_id: str, data: str | bytes) -> None:
        """Write to a command's stdin."""
        self.handle(command_id).stdin(data)

    def close_stdin(self, command_id: str) -> None:
        """Close a command's stdin."""
        self.handle(command_id).close_stdin()

    def signal(self, command_id: str, signal: Signal = "TERM") -> None:
        """Signal a command."""
        self.handle(command_id).signal(signal)


class Files:
    """Files on one box: ``box.files``. Relative paths resolve against /root/workspace."""

    def __init__(self, client: Zipbox, sandbox_id: str) -> None:
        self._client = client
        self._sandbox_id = sandbox_id

    def read(self, path: str) -> bytes:
        """A file's bytes (up to 250 MiB)."""
        return self._client._run(self._client._core.read(self._sandbox_id, path))

    def read_text(self, path: str, encoding: str = "utf-8") -> str:
        """A file decoded as text."""
        return self.read(path).decode(encoding)

    def write(self, path: str, content: str | bytes) -> None:
        """Write a file atomically (parent directory must exist)."""
        payload = content.encode() if isinstance(content, str) else content
        self._client._run(self._client._core.write(self._sandbox_id, path, payload))

    def list(self, path: str = ".") -> list[FileEntry]:
        """A directory's entries."""
        return self._client._run(self._client._core.list_files(self._sandbox_id, path))

    def stat(self, path: str) -> FileEntry:
        """One entry (``not_found`` when absent)."""
        return self._client._run(self._client._core.stat(self._sandbox_id, path))

    def mkdir(self, path: str) -> None:
        """Create a directory."""
        self._client._run(self._client._core.mutate(self._sandbox_id, "mkdir", path))

    def move(self, src: str, dst: str) -> None:
        """Move or rename."""
        self._client._run(self._client._core.mutate(self._sandbox_id, "move", src, dst))

    def delete(self, path: str) -> None:
        """Delete a file or directory."""
        self._client._run(self._client._core.mutate(self._sandbox_id, "delete", path))

    def upload(self, local_path: str | Path, remote_path: str) -> None:
        """Copy a local file to the box."""
        self.write(remote_path, Path(local_path).read_bytes())

    def download(self, remote_path: str, local_path: str | Path) -> None:
        """Copy a file from the box to a local path."""
        Path(local_path).write_bytes(self.read(remote_path))


class PtyApi:
    """Terminals on one box: ``box.pty``."""

    def __init__(self, client: Zipbox, sandbox_id: str) -> None:
        self._client = client
        self._sandbox_id = sandbox_id

    def create(self, *, cols: int = 80, rows: int = 24) -> Pty:
        """Open a new bash session; returns once the socket is up."""
        return self.attach(None, cols=cols, rows=rows)

    def attach(self, session_id: str | None, *, cols: int = 80, rows: int = 24) -> Pty:
        """Re-attach a session by id (its screen is replayed)."""
        pty = Pty(
            self._client._core,
            self._client._transport,
            self._sandbox_id,
            new_state(session_id, cols, rows),
        )
        return pty._start()


class Previews:
    """Public HTTPS URLs for ports on one box: ``box.preview``."""

    def __init__(self, client: Zipbox, sandbox_id: str) -> None:
        self._client = client
        self._sandbox_id = sandbox_id

    def create(self, port: int, label: str | None = None) -> Preview:
        """Serve ``127.0.0.1:<port>`` at ``https://<label>.<box host>``. Public, no auth."""
        return self._client._run(self._client._core.preview(self._sandbox_id, port, label))

    def remove(self, label: str) -> None:
        """Stop serving a preview."""
        self._client._run(self._client._core.unpreview(self._sandbox_id, label))


class Account:
    """The account: ``zb.account``."""

    def __init__(self, client: Zipbox) -> None:
        self._client = client

    def whoami(self) -> dict[str, Any]:
        """Who the key belongs to: ``{"userId": ..., "apiKey": {...}}``."""
        result: dict[str, Any] = self._client._run(
            self._client._core.control("GET", "/account/whoami")
        )
        return result

    def balance(self) -> dict[str, Any]:
        """Credit balance and auto top-up state."""
        result: dict[str, Any] = self._client._run(
            self._client._core.control("GET", "/billing/summary")
        )
        return result

    def usage(self) -> dict[str, Any]:
        """Recent metered usage."""
        result: dict[str, Any] = self._client._run(
            self._client._core.control("GET", "/billing/usage")
        )
        return result
