"""Run every shared conformance fixture against the sync and the async client."""

from __future__ import annotations

import dataclasses
import json
from collections.abc import Iterator
from pathlib import Path
from typing import Any
from urllib.parse import urlparse

import pytest

from mock_server import MockServer, substitute
from zipbox import AsyncZipbox, Zipbox, ZipboxError
from zipbox._async_client import AsyncCommand, AsyncSandbox
from zipbox._client import Command, Sandbox

FIXTURES = Path(__file__).resolve().parents[2] / "conformance" / "fixtures"
KEY = "zb_" + "T" * 43


def _fixtures() -> list[Path]:
    return sorted(FIXTURES.glob("*.json"))


def _camel(name: str) -> str:
    head, *rest = name.split("_")
    return head + "".join(part.title() for part in rest)


def to_json(value: Any) -> Any:
    """The SDK's return value as the fixtures describe it (camelCase JSON)."""
    if isinstance(value, (Sandbox, AsyncSandbox, Command, AsyncCommand)):
        return to_json(value.info)
    if dataclasses.is_dataclass(value) and not isinstance(value, type):
        return {
            _camel(f.name): to_json(getattr(value, f.name))
            for f in dataclasses.fields(value)
            if f.name != "raw"
        }
    if isinstance(value, list):
        return [to_json(item) for item in value]
    if isinstance(value, dict):
        return {k: to_json(v) for k, v in value.items()}
    return value


def matches(expected: Any, actual: Any, where: str = "result") -> list[str]:
    """Subset match per the conformance README."""
    if expected is None:
        return [] if actual is None else [f"{where}: expected nothing, got {actual!r}"]
    if isinstance(expected, dict) and set(expected) <= {"length", "items"} and "length" in expected:
        if not isinstance(actual, list):
            return [f"{where}: expected a list, got {actual!r}"]
        problems = [] if len(actual) == expected["length"] else [f"{where}: length {len(actual)}"]
        for index, item in enumerate(expected.get("items", [])):
            problems += matches(
                item, actual[index] if index < len(actual) else None, f"{where}[{index}]"
            )
        return problems
    if (
        isinstance(expected, dict)
        and set(expected) == {"text"}
        and isinstance(actual, (bytes, str))
    ):
        text = actual.decode() if isinstance(actual, bytes) else actual
        return [] if text == expected["text"] else [f"{where}: text {text!r}"]
    if isinstance(expected, dict):
        if not isinstance(actual, dict):
            return [f"{where}: expected object, got {actual!r}"]
        found: list[str] = []
        for key, value in expected.items():
            found += matches(value, actual.get(key), f"{where}.{key}")
        return found
    if isinstance(expected, list):
        if not isinstance(actual, list) or len(actual) != len(expected):
            return [f"{where}: expected {expected!r}, got {actual!r}"]
        problems = []
        for index, item in enumerate(expected):
            problems += matches(item, actual[index], f"{where}[{index}]")
        return problems
    return [] if expected == actual else [f"{where}: {actual!r} != {expected!r}"]


def run_sync_op(zb: Zipbox, op: str, a: dict[str, Any]) -> Any:
    box = zb.sandbox(a["id"]) if "id" in a else None
    match op:
        case "sandboxes.list":
            return zb.sandboxes.list()
        case "sandboxes.get":
            return zb.sandboxes.get(a["id"])
        case "sandboxes.create":
            return zb.sandboxes.create(name=a.get("name"), tier=a.get("tier"))
        case "sandboxes.start":
            return zb.sandboxes.start(a["id"])
        case "sandboxes.stop":
            return zb.sandboxes.stop(a["id"])
        case "sandboxes.restart":
            return zb.sandboxes.restart(a["id"])
        case "sandboxes.setTier":
            return zb.sandboxes.set_tier(a["id"], a["tier"])
        case "sandboxes.delete":
            return zb.sandboxes.delete(a["id"])
    assert box is not None or op.startswith("account.")
    match op:
        case "sandbox.exec":
            return box.exec(a["cmd"])  # type: ignore[union-attr]
        case "sandbox.execBackground":
            return box.exec_background(a["cmd"])  # type: ignore[union-attr]
        case "commands.events":
            return {"events": list(box.commands.events(a["commandId"], since=a.get("since")))}  # type: ignore[union-attr]
        case "commands.signal":
            return box.commands.signal(a["commandId"], a["signal"])  # type: ignore[union-attr]
        case "files.read":
            return box.files.read(a["path"])  # type: ignore[union-attr]
        case "files.write":
            return box.files.write(a["path"], a["content"])  # type: ignore[union-attr]
        case "files.list":
            return box.files.list(a["path"])  # type: ignore[union-attr]
        case "files.stat":
            return box.files.stat(a["path"])  # type: ignore[union-attr]
        case "files.mkdir":
            return box.files.mkdir(a["path"])  # type: ignore[union-attr]
        case "files.move":
            return box.files.move(a["path"], a["destination"])  # type: ignore[union-attr]
        case "files.delete":
            return box.files.delete(a["path"])  # type: ignore[union-attr]
        case "preview.create":
            return box.preview.create(a["port"], label=a.get("label"))  # type: ignore[union-attr]
        case "account.balance":
            return zb.account.balance()
        case "account.whoami":
            return zb.account.whoami()
    raise AssertionError(f"unknown op {op}")


async def run_async_op(zb: AsyncZipbox, op: str, a: dict[str, Any]) -> Any:
    box = zb.sandbox(a.get("id", ""))
    match op:
        case "sandboxes.list":
            return await zb.sandboxes.list()
        case "sandboxes.get":
            return await zb.sandboxes.get(a["id"])
        case "sandboxes.create":
            return await zb.sandboxes.create(name=a.get("name"), tier=a.get("tier"))
        case "sandboxes.start":
            return await zb.sandboxes.start(a["id"])
        case "sandboxes.stop":
            return await zb.sandboxes.stop(a["id"])
        case "sandboxes.restart":
            return await zb.sandboxes.restart(a["id"])
        case "sandboxes.setTier":
            return await zb.sandboxes.set_tier(a["id"], a["tier"])
        case "sandboxes.delete":
            return await zb.sandboxes.delete(a["id"])
        case "sandbox.exec":
            return await box.exec(a["cmd"])
        case "sandbox.execBackground":
            return await box.exec_background(a["cmd"])
        case "commands.events":
            return {
                "events": [e async for e in box.commands.events(a["commandId"], a.get("since"))]
            }
        case "commands.signal":
            return await box.commands.signal(a["commandId"], a["signal"])
        case "files.read":
            return await box.files.read(a["path"])
        case "files.write":
            return await box.files.write(a["path"], a["content"])
        case "files.list":
            return await box.files.list(a["path"])
        case "files.stat":
            return await box.files.stat(a["path"])
        case "files.mkdir":
            return await box.files.mkdir(a["path"])
        case "files.move":
            return await box.files.move(a["path"], a["destination"])
        case "files.delete":
            return await box.files.delete(a["path"])
        case "preview.create":
            return await box.preview.create(a["port"], label=a.get("label"))
        case "account.balance":
            return await zb.account.balance()
        case "account.whoami":
            return await zb.account.whoami()
    raise AssertionError(f"unknown op {op}")


@pytest.fixture(scope="module")
def server() -> Iterator[MockServer]:
    mock = MockServer()
    mock.start()
    yield mock
    mock.stop()


def _load(server: MockServer, path: Path) -> dict[str, Any]:
    base = server.base_url
    fixture: dict[str, Any] = substitute(
        json.loads(path.read_text()), {"{{BASE}}": base, "{{HOST}}": urlparse(base).hostname or ""}
    )
    server.load(fixture["exchanges"])
    server.errors.clear()
    return fixture


def _verify(
    server: MockServer, fixture: dict[str, Any], result: Any, error: ZipboxError | None
) -> None:
    problems = list(server.errors)
    if server.unserved():
        problems.append(f"{server.unserved()} exchange(s) never requested")
    expect = fixture["expect"]
    if "error" in expect:
        if error is None:
            problems.append(f"expected error {expect['error']}, got result {to_json(result)!r}")
        else:
            actual = {
                "code": error.code,
                "status": error.status,
                "requestId": error.request_id,
                "docsUrl": error.docs_url,
            }
            problems += matches(expect["error"], actual, "error")
    elif error is not None:
        problems.append(f"unexpected error {error!r}")
    else:
        problems += matches(expect["result"], to_json(result))
    assert not problems, "\n".join(problems)


@pytest.mark.parametrize("path", _fixtures(), ids=lambda p: p.stem)
def test_sync(server: MockServer, path: Path) -> None:
    fixture = _load(server, path)
    result: Any = None
    error: ZipboxError | None = None
    with Zipbox(
        KEY,
        base_url=server.base_url,
        retry_base_delay_s=0.001,
        poll_interval_s=0.001,
        poll_max_interval_s=0.002,
    ) as zb:
        try:
            result = run_sync_op(zb, fixture["call"]["op"], fixture["call"]["args"])
        except ZipboxError as caught:
            error = caught
    _verify(server, fixture, result, error)


@pytest.mark.parametrize("path", _fixtures(), ids=lambda p: p.stem)
async def test_async(server: MockServer, path: Path) -> None:
    fixture = _load(server, path)
    result: Any = None
    error: ZipboxError | None = None
    async with AsyncZipbox(
        KEY,
        base_url=server.base_url,
        retry_base_delay_s=0.001,
        poll_interval_s=0.001,
        poll_max_interval_s=0.002,
    ) as zb:
        try:
            result = await run_async_op(zb, fixture["call"]["op"], fixture["call"]["args"])
        except ZipboxError as caught:
            error = caught
    _verify(server, fixture, result, error)


def test_every_fixture_is_loaded() -> None:
    assert len(_fixtures()) >= 27
