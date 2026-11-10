"""Zipbox SDK: drive Zipbox boxes from Python.

>>> from zipbox import Zipbox
>>> zb = Zipbox()  # reads ZIPBOX_API_KEY
>>> box = zb.sandboxes.get_or_create("demo")
>>> print(box.exec("echo hello").stdout)
"""

from zipbox._async_client import (
    AsyncAccount,
    AsyncCommand,
    AsyncCommands,
    AsyncFiles,
    AsyncPreviews,
    AsyncPtyApi,
    AsyncSandbox,
    AsyncSandboxes,
    AsyncTailscale,
    AsyncZipbox,
)
from zipbox._client import (
    Account,
    Command,
    Commands,
    Files,
    Previews,
    PtyApi,
    Sandbox,
    Sandboxes,
    Tailscale,
    Zipbox,
)
from zipbox._core import DEFAULT_API_URL, VERSION, WORKSPACE
from zipbox._errors import ErrorCode, ZipboxError
from zipbox._models import (
    CommandInfo,
    CommandState,
    ExecEvent,
    ExecResult,
    FileEntry,
    FileKind,
    ForkSourcePolicy,
    Language,
    Preview,
    SandboxDetail,
    SandboxFork,
    SandboxForkError,
    SandboxInfo,
    Signal,
    SshInfo,
    TailscalePeer,
    TailscaleStatus,
    Tier,
    TransitionCancelOutcome,
)
from zipbox._pty import AsyncPty, Pty

__version__ = VERSION

__all__ = [
    "DEFAULT_API_URL",
    "WORKSPACE",
    "Account",
    "AsyncAccount",
    "AsyncCommand",
    "AsyncCommands",
    "AsyncFiles",
    "AsyncPreviews",
    "AsyncPty",
    "AsyncPtyApi",
    "AsyncSandbox",
    "AsyncSandboxes",
    "AsyncTailscale",
    "AsyncZipbox",
    "Command",
    "CommandInfo",
    "CommandState",
    "Commands",
    "ErrorCode",
    "ExecEvent",
    "ExecResult",
    "FileEntry",
    "FileKind",
    "Files",
    "ForkSourcePolicy",
    "Language",
    "Preview",
    "Previews",
    "Pty",
    "PtyApi",
    "Sandbox",
    "SandboxDetail",
    "SandboxFork",
    "SandboxForkError",
    "SandboxInfo",
    "Sandboxes",
    "Signal",
    "SshInfo",
    "Tailscale",
    "TailscalePeer",
    "TailscaleStatus",
    "Tier",
    "TransitionCancelOutcome",
    "Zipbox",
    "ZipboxError",
    "__version__",
]
