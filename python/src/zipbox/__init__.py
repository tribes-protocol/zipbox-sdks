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
    Language,
    Preview,
    SandboxDetail,
    SandboxInfo,
    Signal,
    SshInfo,
    Tier,
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
    "Language",
    "Preview",
    "Previews",
    "Pty",
    "PtyApi",
    "Sandbox",
    "SandboxDetail",
    "SandboxInfo",
    "Sandboxes",
    "Signal",
    "SshInfo",
    "Tier",
    "Zipbox",
    "ZipboxError",
    "__version__",
]
