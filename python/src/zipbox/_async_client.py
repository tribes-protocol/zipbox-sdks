"""The asyncio client: ``AsyncZipbox``. Each method drives the same core operation as
the blocking client, with async I/O."""

from __future__ import annotations

import asyncio
from collections.abc import AsyncIterator, Mapping
from pathlib import Path
from types import TracebackType
from typing import Any, TypeVar

import httpx

from zipbox._core import CommandLine, Core, exec_body, resolve_config
from zipbox._effects import Op, StreamOp, run_async, stream_async
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
    Tier,
    TransitionCancelOutcome,
)
from zipbox._pty import AsyncPty, new_state
from zipbox._transport import HttpxAsyncTransport

T = TypeVar("T")


class AsyncZipbox:
    """Asyncio AsyncZipbox client.

    >>> zb = AsyncZipbox()  # reads ZIPBOX_API_KEY
    >>> box = await zb.sandboxes.get_or_create("demo")
    >>> print((await box.exec("uname -a")).stdout)
    """

    def __init__(
        self,
        api_key: str | None = None,
        *,
        base_url: str | None = None,
        max_retries: int = 3,
        http_client: httpx.AsyncClient | None = None,
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
        self._transport = HttpxAsyncTransport(http_client)
        self.sandboxes = AsyncSandboxes(self)
        self.account = AsyncAccount(self)

    async def _run(self, op: Op[T]) -> T:
        return await run_async(self._transport, op)

    def _stream(self, op: StreamOp) -> AsyncIterator[Any]:
        return stream_async(self._transport, op)

    def sandbox(self, sandbox_id: str) -> AsyncSandbox:
        """A handle on a box by id. Makes no request."""
        return AsyncSandbox(self, sandbox_id, None)

    async def aclose(self) -> None:
        """Release HTTP connections."""
        await self._transport.aclose()

    async def __aenter__(self) -> AsyncZipbox:
        return self

    async def __aexit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        tb: TracebackType | None,
    ) -> None:
        await self.aclose()


class AsyncSandboxes:
    """Box lifecycle: ``zb.sandboxes``."""

    def __init__(self, client: AsyncZipbox) -> None:
        self._client = client

    async def create(
        self,
        *,
        name: str | None = None,
        tier: Tier | None = None,
        ssh_public_key: str | None = None,
        mailbox: bool | None = None,
        private: bool | None = None,
    ) -> AsyncSandbox:
        """Create a box and wait until it is ready (no deadline).

        ``mailbox=True`` creates the box's mailbox now; by default it is created
        the first time the box uses mail. ``private=True`` creates it private
        ("tailnet only"): it is never public, and a private box is not ready until
        it joins your tailnet, so this returns once it is running with its key. Call
        ``box.tailscale.connect(auth_key)`` next.
        """
        detail = await self._client._run(
            self._client._core.create(
                name=name,
                tier=tier,
                ssh_public_key=ssh_public_key,
                mailbox=mailbox,
                private=private,
            )
        )
        return AsyncSandbox(self._client, detail.id, detail)

    async def get_or_create(
        self,
        name: str,
        *,
        tier: Tier | None = None,
        ssh_public_key: str | None = None,
        mailbox: bool | None = None,
        private: bool | None = None,
    ) -> AsyncSandbox:
        """Return the live box named ``name`` (starting it if stopped), or create it with
        these options.

        The options apply only when a box is created. A box that already has this name
        is returned as it is, even when it differs (another tier, not private): that is
        not an error and nothing is resized. Check ``box.info.tier`` and call
        ``set_tier`` if it matters.
        """
        detail = await self._client._run(
            self._client._core.get_or_create(
                name,
                tier=tier,
                ssh_public_key=ssh_public_key,
                mailbox=mailbox,
                private=private,
            )
        )
        return AsyncSandbox(self._client, detail.id, detail)

    async def list(self) -> list[SandboxInfo]:
        """Every box on the account."""
        return await self._client._run(self._client._core.list_sandboxes())

    async def get(self, sandbox_id: str) -> AsyncSandbox:
        """One box, with its current detail."""
        detail = await self._client._run(self._client._core.get(sandbox_id))
        return AsyncSandbox(self._client, sandbox_id, detail)

    async def start(self, sandbox_id: str) -> None:
        """Start a stopped box, or a ``stopping`` one once its stop lands (402 when dry)."""
        await self._client._run(self._client._core.start(sandbox_id))

    async def stop(self, sandbox_id: str) -> None:
        """Stop a box: disk archived, nothing billed while stopped."""
        await self._client._run(self._client._core.lifecycle(sandbox_id, "pause"))

    async def restart(self, sandbox_id: str) -> None:
        """Hard-restart a running box; files survive."""
        await self._client._run(self._client._core.lifecycle(sandbox_id, "reset"))

    async def cancel_transition(self, sandbox_id: str) -> TransitionCancelOutcome:
        """Cancel the box's state change in flight and return it to where it was.

        Returns ``cleared`` (it is back already), ``cancelling`` (the host's abort is on
        its way) or ``none`` (nothing was in flight). Raises ``conflict`` with the
        reason past the point of no return.
        """
        return await self._client._run(self._client._core.cancel_transition(sandbox_id))

    async def fork(
        self,
        source_id: str,
        *,
        name: str,
        source_policy: ForkSourcePolicy,
        private: bool | None = None,
        tailscale_auth_key: str | None = None,
        wait: bool = True,
    ) -> AsyncSandbox:
        """Fork a box: a new box with the source's files and settings and a new identity.

        ``name`` is the new box's permanent name (kept even if the fork is cancelled).
        ``source_policy`` is ``"require_paused"`` (a running source raises
        ``pause_required``) or ``"pause_and_resume"`` (a running source pauses for the
        capture, its sessions disconnect, and it resumes right after). The new box
        inherits the source's size; it starts public unless ``private=True`` (never
        public, reachable only from your tailnet once it joins).

        Waits (no deadline) until the new box is ready, or for a private one until it
        is running with its key; a fork that fails raises its own code
        (``copy_failed``, ``source_gone``, …) and a cancelled one ``conflict``.
        ``tailscale_auth_key`` (needs the wait) is sealed here to the new box's own
        key once it runs and never sent in the clear. ``wait=False`` returns the
        accepted box at once. Stopping the wait never stops the fork: cancel it with
        ``cancel_transition(child_id)``. Safe to retry: the request carries an
        Idempotency-Key.
        """
        detail = await self._client._run(
            self._client._core.fork(
                source_id,
                name=name,
                source_policy=source_policy,
                private=private,
                tailscale_auth_key=tailscale_auth_key,
                wait=wait,
            )
        )
        return AsyncSandbox(self._client, detail.id, detail)

    async def get_fork(self, child_id: str) -> SandboxFork:
        """The fork that is making (or made) this box: its phase, source recovery, waits
        and error. ``not_found`` when the box was not made by a fork."""
        return await self._client._run(self._client._core.get_fork(child_id))

    async def delete(self, sandbox_id: str, *, archive: bool | None = None) -> None:
        """Delete a box. A recoverable copy is kept for 30 days unless ``archive=False``."""
        await self._client._run(self._client._core.delete(sandbox_id, archive))

    async def set_tier(self, sandbox_id: str, tier: Tier) -> None:
        """Move a box to another tier."""
        await self._client._run(self._client._core.lifecycle(sandbox_id, "tier", {"tier": tier}))

    async def wait_ready(self, sandbox_id: str) -> AsyncSandbox:
        """Block until the box is ready (no deadline). A private box: until it is
        running with its key, since it is not ready until it joins your tailnet."""
        detail = await self._client._run(self._client._core.wait_ready(sandbox_id))
        return AsyncSandbox(self._client, sandbox_id, detail)


class AsyncSandbox:
    """A box. Returned by ``create``, ``get``, ``get_or_create`` and ``zb.sandbox(id)``."""

    def __init__(self, client: AsyncZipbox, sandbox_id: str, info: SandboxDetail | None) -> None:
        self._client = client
        self.id = sandbox_id
        self.info = info
        self.commands = AsyncCommands(client, sandbox_id)
        self.files = AsyncFiles(client, sandbox_id)
        self.pty = AsyncPtyApi(client, sandbox_id)
        self.preview = AsyncPreviews(client, sandbox_id)
        self.tailscale = AsyncTailscale(client, sandbox_id)

    def __repr__(self) -> str:
        status = self.info.status if self.info else "?"
        return f"AsyncSandbox(id={self.id!r}, status={status!r})"

    async def refresh(self) -> SandboxDetail:
        """Re-read the box's detail."""
        self.info = await self._client._run(self._client._core.get(self.id))
        return self.info

    async def exec(
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
        return await self._client._run(self._client._core.exec(self.id, body))

    def exec_stream(
        self,
        cmd: CommandLine,
        *,
        cwd: str | None = None,
        env: Mapping[str, str] | None = None,
        timeout_ms: int | None = None,
        user: str | None = None,
    ) -> AsyncIterator[ExecEvent]:
        """Run a command and yield its events as they happen. Leaving the loop early
        kills the command (use ``exec_background`` for one that should outlive you)."""
        body = exec_body(cmd, cwd=cwd, env=env, timeout_ms=timeout_ms, user=user)
        return self._client._stream(self._client._core.exec_events(self.id, body))

    async def exec_background(
        self,
        cmd: CommandLine,
        *,
        cwd: str | None = None,
        env: Mapping[str, str] | None = None,
        timeout_ms: int | None = None,
        user: str | None = None,
        stdin: bool = False,
    ) -> AsyncCommand:
        """Start a command that keeps running with nobody attached; returns its handle."""
        body = exec_body(
            cmd, cwd=cwd, env=env, timeout_ms=timeout_ms, user=user, stdin=stdin or None
        )
        info = await self._client._run(self._client._core.exec_background(self.id, body))
        return AsyncCommand(self._client, self.id, info.id, info)

    async def run_code(
        self,
        language: Language,
        code: str,
        *,
        cwd: str | None = None,
        env: Mapping[str, str] | None = None,
        timeout_ms: int | None = None,
        user: str | None = None,
    ) -> ExecResult:
        """Run a snippet with ``python3``, ``node`` (as an ES module) or ``bash``: written
        to a temp file, run through the login shell with these options, then removed."""
        options = {"cwd": cwd, "env": env, "timeout_ms": timeout_ms, "user": user}
        return await self._client._run(
            self._client._core.run_code(self.id, language, code, options)
        )

    async def logs(
        self, since: str | None = None, lines: int | None = None, unit: str | None = None
    ) -> str:
        """The box's system journal (``journalctl``): since a time, the last ``lines``,
        one systemd ``unit``. A failed journalctl raises ``conflict``."""
        return await self._client._run(self._client._core.logs(self.id, since, lines, unit))

    async def ssh_info(self) -> SshInfo:
        """The direct IPv6 SSH command for this box."""
        return await self._client._run(self._client._core.ssh_info(self.id))

    async def screenshot(self) -> bytes:
        """A PNG of the box's desktop."""
        return await self._client._run(self._client._core.screenshot(self.id))

    async def start(self) -> None:
        """Start this box."""
        await self._client.sandboxes.start(self.id)

    async def stop(self) -> None:
        """Stop this box."""
        await self._client.sandboxes.stop(self.id)

    async def restart(self) -> None:
        """Hard-restart this box."""
        await self._client.sandboxes.restart(self.id)

    async def cancel_transition(self) -> TransitionCancelOutcome:
        """Cancel this box's state change in flight (``conflict`` past the point of no return)."""
        return await self._client.sandboxes.cancel_transition(self.id)

    async def fork(
        self,
        *,
        name: str,
        source_policy: ForkSourcePolicy,
        private: bool | None = None,
        tailscale_auth_key: str | None = None,
        wait: bool = True,
    ) -> AsyncSandbox:
        """Fork this box into a new one named ``name``; see ``zb.sandboxes.fork``."""
        return await self._client.sandboxes.fork(
            self.id,
            name=name,
            source_policy=source_policy,
            private=private,
            tailscale_auth_key=tailscale_auth_key,
            wait=wait,
        )

    async def fork_status(self) -> SandboxFork:
        """The fork that is making (or made) this box (``not_found`` if none did)."""
        return await self._client.sandboxes.get_fork(self.id)

    async def delete(self, *, archive: bool | None = None) -> None:
        """Delete this box (``archive=False`` skips the recoverable copy)."""
        await self._client.sandboxes.delete(self.id, archive=archive)

    async def set_tier(self, tier: Tier) -> None:
        """Move this box to another tier."""
        await self._client.sandboxes.set_tier(self.id, tier)

    async def set_private(self, enabled: bool) -> None:
        """Make the box private ("tailnet only") or public again.

        Turning it on needs the box connected to your tailnet (``tailnet_not_connected``
        otherwise). A private box answers only devices on your tailnet; box calls from
        anywhere else raise ``box_private_unreachable``.
        """
        await self._client._run(self._client._core.set_private(self.id, enabled))

    async def wait_ready(self) -> SandboxDetail:
        """Block until the box is ready (no deadline). A private box: until it is
        running with its key, since it is not ready until it joins your tailnet."""
        self.info = await self._client._run(self._client._core.wait_ready(self.id))
        return self.info


class AsyncCommand:
    """A command on a box, usually started with ``exec_background``."""

    def __init__(
        self, client: AsyncZipbox, sandbox_id: str, command_id: str, info: CommandInfo | None
    ) -> None:
        self._client = client
        self._sandbox_id = sandbox_id
        self.id = command_id
        self.info = info

    def __repr__(self) -> str:
        state = self.info.state if self.info else "?"
        return f"AsyncCommand(id={self.id!r}, state={state!r})"

    async def get(self) -> CommandInfo:
        """Re-read the command's record."""
        self.info = await self._client._run(self._client._core.command(self._sandbox_id, self.id))
        return self.info

    def events(self, since: str | None = None) -> AsyncIterator[ExecEvent]:
        """Replay from ``since`` (an event cursor) and follow until exit."""
        return self._client._stream(
            self._client._core.command_events(self._sandbox_id, self.id, since)
        )

    async def wait(self) -> ExecResult:
        """Block until the command exits; return its full output."""
        return await self._client._run(self._client._core.wait(self._sandbox_id, self.id))

    async def stdin(self, data: str | bytes) -> None:
        """Write to the command's stdin (start it with ``stdin=True``)."""
        payload = data.encode() if isinstance(data, str) else data
        await self._client._run(self._client._core.stdin(self._sandbox_id, self.id, payload, False))

    async def close_stdin(self) -> None:
        """Close the command's stdin."""
        await self._client._run(self._client._core.stdin(self._sandbox_id, self.id, b"", True))

    async def signal(self, signal: Signal = "TERM") -> None:
        """Send a signal to the command."""
        await self._client._run(self._client._core.signal(self._sandbox_id, self.id, signal))


class AsyncCommands:
    """AsyncCommands on one box: ``box.commands``."""

    def __init__(self, client: AsyncZipbox, sandbox_id: str) -> None:
        self._client = client
        self._sandbox_id = sandbox_id

    async def list(self) -> list[CommandInfo]:
        """Running and recently finished commands."""
        return await self._client._run(self._client._core.commands(self._sandbox_id))

    async def get(self, command_id: str) -> CommandInfo:
        """One command's record."""
        return await self._client._run(self._client._core.command(self._sandbox_id, command_id))

    def handle(self, command_id: str) -> AsyncCommand:
        """A handle on a command by id. Makes no request."""
        return AsyncCommand(self._client, self._sandbox_id, command_id, None)

    def events(self, command_id: str, since: str | None = None) -> AsyncIterator[ExecEvent]:
        """Replay from ``since`` and follow until exit."""
        return self.handle(command_id).events(since)

    async def stdin(self, command_id: str, data: str | bytes) -> None:
        """Write to a command's stdin."""
        await self.handle(command_id).stdin(data)

    async def close_stdin(self, command_id: str) -> None:
        """Close a command's stdin."""
        await self.handle(command_id).close_stdin()

    async def signal(self, command_id: str, signal: Signal = "TERM") -> None:
        """Signal a command."""
        await self.handle(command_id).signal(signal)


class AsyncFiles:
    """AsyncFiles on one box: ``box.files``. Relative paths resolve against /root/workspace."""

    def __init__(self, client: AsyncZipbox, sandbox_id: str) -> None:
        self._client = client
        self._sandbox_id = sandbox_id

    async def read(self, path: str) -> bytes:
        """A file's bytes (up to 250 MiB)."""
        return await self._client._run(self._client._core.read(self._sandbox_id, path))

    async def read_text(self, path: str, encoding: str = "utf-8") -> str:
        """A file decoded as text."""
        return (await self.read(path)).decode(encoding)

    async def write(self, path: str, content: str | bytes) -> None:
        """Write a file atomically (parent directory must exist)."""
        payload = content.encode() if isinstance(content, str) else content
        await self._client._run(self._client._core.write(self._sandbox_id, path, payload))

    async def list(self, path: str = ".") -> list[FileEntry]:
        """A directory's entries."""
        return await self._client._run(self._client._core.list_files(self._sandbox_id, path))

    async def stat(self, path: str) -> FileEntry:
        """One entry (``not_found`` when absent)."""
        return await self._client._run(self._client._core.stat(self._sandbox_id, path))

    async def mkdir(self, path: str) -> None:
        """Create a directory."""
        await self._client._run(self._client._core.mutate(self._sandbox_id, "mkdir", path))

    async def move(self, src: str, dst: str) -> None:
        """Move or rename."""
        await self._client._run(self._client._core.mutate(self._sandbox_id, "move", src, dst))

    async def copy(self, src: str, dst: str) -> None:
        """Copy a file or directory."""
        await self._client._run(self._client._core.mutate(self._sandbox_id, "copy", src, dst))

    async def delete(self, path: str) -> None:
        """Delete a file or directory."""
        await self._client._run(self._client._core.mutate(self._sandbox_id, "delete", path))

    async def upload(self, local_path: str | Path, remote_path: str) -> None:
        """Copy a local file to the box."""
        await self.write(remote_path, await asyncio.to_thread(Path(local_path).read_bytes))

    async def download(self, remote_path: str, local_path: str | Path) -> None:
        """Copy a file from the box to a local path."""
        data = await self.read(remote_path)
        await asyncio.to_thread(Path(local_path).write_bytes, data)


class AsyncPtyApi:
    """Terminals on one box: ``box.pty``."""

    def __init__(self, client: AsyncZipbox, sandbox_id: str) -> None:
        self._client = client
        self._sandbox_id = sandbox_id

    async def create(
        self,
        *,
        cols: int | None = None,
        rows: int | None = None,
        client_label: str | None = None,
    ) -> AsyncPty:
        """Open a new bash session; returns once the socket is up. No size = the box's
        default; ``client_label`` names this client in the box's session list."""
        return await self.attach(None, cols=cols, rows=rows, client_label=client_label)

    async def attach(
        self,
        session_id: str | None,
        *,
        cols: int | None = None,
        rows: int | None = None,
        client_label: str | None = None,
    ) -> AsyncPty:
        """Re-attach a session by id (its screen is replayed)."""
        pty = AsyncPty(
            self._client._core,
            self._client._transport,
            self._sandbox_id,
            new_state(session_id, cols, rows, client_label),
        )
        return await pty._start()


class AsyncPreviews:
    """Public HTTPS URLs for ports on one box: ``box.preview``."""

    def __init__(self, client: AsyncZipbox, sandbox_id: str) -> None:
        self._client = client
        self._sandbox_id = sandbox_id

    async def create(self, port: int, label: str | None = None) -> Preview:
        """Serve ``127.0.0.1:<port>`` at ``https://<label>.<box host>``. Public, no auth."""
        return await self._client._run(self._client._core.preview(self._sandbox_id, port, label))

    async def remove(self, label: str) -> None:
        """Stop serving a preview, by its label (or full host)."""
        await self._client._run(self._client._core.unpreview(self._sandbox_id, label))


class AsyncTailscale:
    """The box's tailnet: ``box.tailscale``.

    The auth key is sealed on this machine to the box's own key and opened only
    inside the box; Zipbox relays ciphertext and never sees the key.
    """

    def __init__(self, client: AsyncZipbox, sandbox_id: str) -> None:
        self._client = client
        self._sandbox_id = sandbox_id

    def __repr__(self) -> str:
        return f"AsyncTailscale(sandbox_id={self._sandbox_id!r})"

    async def status(self) -> TailscaleStatus | None:
        """The tailnet status, or None when the box never joined one."""
        return await self._client._run(self._client._core.tailscale_status(self._sandbox_id))

    async def connect(self, auth_key: str) -> None:
        """Join your tailnet with a one-off auth key (tailscale.com admin → Keys).

        Returns once the box has accepted the key; follow ``status()`` for the join.
        """
        await self._client._run(self._client._core.tailscale_connect(self._sandbox_id, auth_key))

    async def disconnect(self) -> None:
        """Leave the tailnet: the box logs out and forgets its node."""
        await self._client._run(self._client._core.tailscale_disconnect(self._sandbox_id))


class AsyncAccount:
    """The account: ``zb.account``."""

    def __init__(self, client: AsyncZipbox) -> None:
        self._client = client

    async def whoami(self) -> dict[str, Any]:
        """Who the key belongs to: ``{"userId": ..., "apiKey": {...}}``."""
        result: dict[str, Any] = await self._client._run(
            self._client._core.control("GET", "/account/whoami")
        )
        return result

    async def balance(self) -> dict[str, Any]:
        """Credit balance and auto top-up state."""
        result: dict[str, Any] = await self._client._run(
            self._client._core.control("GET", "/billing/summary")
        )
        return result

    async def usage(self) -> dict[str, Any]:
        """Recent metered usage."""
        result: dict[str, Any] = await self._client._run(
            self._client._core.control("GET", "/billing/usage")
        )
        return result
