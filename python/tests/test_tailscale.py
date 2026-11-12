"""Tailscale on a box: the sealed auth key and private-box reachability."""

from __future__ import annotations

import base64
import json
import socket
import ssl
import time
from pathlib import Path
from typing import Any

import httpx
import pytest
from cryptography.exceptions import InvalidTag
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.hazmat.primitives.ciphers.aead import AESGCM
from cryptography.hazmat.primitives.kdf.hkdf import HKDF

from mock_server import MockServer
from zipbox import Zipbox, ZipboxError
from zipbox._core import PRIVATE_PROBE_TIMEOUT_S, BoxTarget, Core, resolve_config
from zipbox._effects import HttpRequest, HttpResponse, run_sync
from zipbox._envelope import seal, seal_with, tailscale_info
from zipbox._transport import HttpxSyncTransport, _network_error

VECTOR: dict[str, Any] = json.loads(
    (
        Path(__file__).resolve().parents[2] / "conformance" / "vectors" / "box_envelope.json"
    ).read_text()
)


def _unb64url(value: str) -> bytes:
    return base64.urlsafe_b64decode(value + "=" * (-len(value) % 4))


def open_envelope(envelope: dict[str, str], private_key_pem: str, info: str) -> str:
    """The box's opener, in this SDK's crypto stack."""
    private_key = serialization.load_pem_private_key(private_key_pem.encode(), password=None)
    assert isinstance(private_key, ec.EllipticCurvePrivateKey)
    ephemeral = serialization.load_der_public_key(_unb64url(envelope["ephemeralPublicKey"]))
    assert isinstance(ephemeral, ec.EllipticCurvePublicKey)
    shared = private_key.exchange(ec.ECDH(), ephemeral)
    key = HKDF(algorithm=hashes.SHA256(), length=32, salt=b"", info=info.encode()).derive(shared)
    plaintext = AESGCM(key).decrypt(
        _unb64url(envelope["iv"]), _unb64url(envelope["ciphertext"]), None
    )
    return plaintext.decode()


def _vector_seal() -> dict[str, str]:
    ephemeral = ec.derive_private_key(int(VECTOR["ephemeral"]["privateKeyHex"], 16), ec.SECP256R1())
    return seal_with(
        VECTOR["recipient"]["agentPublicKey"],
        VECTOR["info"],
        VECTOR["plaintext"],
        ephemeral,
        _unb64url(VECTOR["iv"]),
    )


def test_the_info_string_binds_the_box_and_the_purpose() -> None:
    assert tailscale_info(VECTOR["sandboxId"]) == VECTOR["info"]


def test_seal_reproduces_the_conformance_vector_byte_for_byte() -> None:
    assert _vector_seal() == VECTOR["envelope"]


def test_the_vector_round_trips_through_the_opener() -> None:
    pem = VECTOR["recipient"]["privateKeyPkcs8Pem"]
    assert open_envelope(VECTOR["envelope"], pem, VECTOR["info"]) == VECTOR["plaintext"]


def test_a_fresh_seal_opens_and_never_repeats() -> None:
    pem = VECTOR["recipient"]["privateKeyPkcs8Pem"]
    first = seal(VECTOR["recipient"]["agentPublicKey"], VECTOR["info"], VECTOR["plaintext"])
    second = seal(VECTOR["recipient"]["agentPublicKey"], VECTOR["info"], VECTOR["plaintext"])
    assert first != second
    assert open_envelope(first, pem, VECTOR["info"]) == VECTOR["plaintext"]
    assert VECTOR["plaintext"] not in json.dumps(first)


def test_another_purpose_cannot_open_the_envelope() -> None:
    with pytest.raises(InvalidTag):
        open_envelope(
            VECTOR["envelope"], VECTOR["recipient"]["privateKeyPkcs8Pem"], VECTOR["wrongInfo"]
        )


def test_the_box_key_is_accepted_url_safe_and_unpadded() -> None:
    standard = VECTOR["recipient"]["agentPublicKey"]
    url_safe = standard.replace("+", "-").replace("/", "_").rstrip("=")
    envelope = seal(url_safe, VECTOR["info"], VECTOR["plaintext"])
    pem = VECTOR["recipient"]["privateKeyPkcs8Pem"]
    assert open_envelope(envelope, pem, VECTOR["info"]) == VECTOR["plaintext"]


def test_a_malformed_box_key_never_echoes_the_auth_key() -> None:
    with pytest.raises(ZipboxError) as caught:
        seal("not-a-key", VECTOR["info"], VECTOR["plaintext"])
    assert VECTOR["plaintext"] not in str(caught.value) + repr(caught.value)


def _private_core() -> Core:
    core = Core(resolve_config("zb_k", "https://api.test", 0))
    core._targets["sbx"] = BoxTarget("https://box--build.zbox.sh", "bt", name="build", private=True)
    return core


def test_an_unreachable_private_box_fails_fast_with_a_clear_message() -> None:
    op = _private_core().target("sbx")
    probe = op.send(None)
    assert isinstance(probe, HttpRequest)
    assert (probe.method, probe.url) == ("GET", "https://box--build.zbox.sh/challenge")
    assert probe.timeout_s == PRIVATE_PROBE_TIMEOUT_S
    assert "authorization" not in probe.headers
    with pytest.raises(ZipboxError) as caught:
        op.throw(_network_error(httpx.ConnectTimeout("timed out")))
    assert caught.value.code == "box_private_unreachable"
    assert caught.value.retryable is False
    assert caught.value.message == (
        "build is private: it can only be reached from devices on your tailnet. "
        "Turn on Tailscale on this device (https://tailscale.com/download)."
    )


def test_a_private_box_whose_tls_handshake_fails_is_reachable() -> None:
    # The address answered; only the certificate is missing, which the box call
    # itself waits out. Off the tailnet nothing answers at all.
    core = _private_core()
    op = core.target("sbx")
    assert isinstance(op.send(None), HttpRequest)
    handshake = httpx.ConnectError("[SSL: TLSV1_ALERT_INTERNAL_ERROR] tlsv1 alert internal error")
    handshake.__cause__ = ssl.SSLError(1, "tlsv1 alert internal error")
    failure = _network_error(handshake)
    failure.__cause__ = handshake  # as the transport's `raise … from error` leaves it
    with pytest.raises(StopIteration) as done:
        op.throw(failure)
    assert done.value.value.base_url == "https://box--build.zbox.sh"
    assert "sbx" in core._reachable


def test_a_reachable_private_box_is_probed_once() -> None:
    core = _private_core()
    op = core.target("sbx")
    assert isinstance(op.send(None), HttpRequest)
    with pytest.raises(StopIteration) as done:
        # Any HTTP answer, even a 404 from a box with no owner yet, is reachable.
        op.send(HttpResponse(404, {}, b""))
    assert done.value.value.base_url == "https://box--build.zbox.sh"
    again = core.target("sbx")
    with pytest.raises(StopIteration):
        again.send(None)
    core.forget("sbx")
    assert "sbx" not in core._reachable


def test_a_public_box_is_never_probed() -> None:
    core = Core(resolve_config("zb_k", "https://api.test", 0))
    core._targets["sbx"] = BoxTarget("https://box--build.zbox.sh", "bt", name="build")
    with pytest.raises(StopIteration):
        core.target("sbx").send(None)


def _closed_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as probe:
        probe.bind(("127.0.0.1", 0))
        port: int = probe.getsockname()[1]
    return port


def test_box_calls_on_a_private_box_off_the_tailnet_raise_instead_of_hanging() -> None:
    server = MockServer()
    server.start()
    detail = {
        "id": "sbx",
        "name": "build",
        "status": "running",
        "privateMode": True,
        "agentPublicKey": VECTOR["recipient"]["agentPublicKey"],
        "ready": True,
        "baseUrl": f"http://127.0.0.1:{_closed_port()}",
        "boxToken": "bt_test",
    }
    server.load(
        [
            {
                "request": {"method": "GET", "path": "/sandbox/sbx"},
                "response": {"status": 200, "body": detail},
            }
        ]
    )
    try:
        with (
            Zipbox("zb_" + "T" * 43, base_url=server.base_url) as zb,
            pytest.raises(ZipboxError) as caught,
        ):
            zb.sandbox("sbx").exec("true")
        assert caught.value.code == "box_private_unreachable"
        assert not server.errors and not server.unserved()
    finally:
        server.stop()


def test_the_tailscale_handle_carries_no_secret() -> None:
    with Zipbox("zb_" + "T" * 43, base_url="https://api.test") as zb:
        assert repr(zb.sandbox("sbx").tailscale) == "Tailscale(sandbox_id='sbx')"


def _private_row(ready: bool, ipv4: str | None) -> dict[str, Any]:
    return {
        "id": "sbx",
        "name": "build",
        "status": "running",
        "privateMode": True,
        "agentPublicKey": VECTOR["recipient"]["agentPublicKey"],
        "tailscale": None if ipv4 is None else {"state": "connected", "ipv4": ipv4},
        "ready": ready,
        "baseUrl": "https://box--build.zbox.sh" if ready else None,
        "boxToken": "bt_test" if ready else None,
    }


def _get_or_create(rows: list[dict[str, Any]]) -> Any:
    server = MockServer()
    server.start()
    exchanges = [
        {
            "request": {"method": "GET", "path": "/sandbox"},
            "response": {"status": 200, "body": [rows[0]]},
        },
    ] + [
        {
            "request": {"method": "GET", "path": "/sandbox/sbx/wait?until=ready"},
            "response": {"status": 200, "body": row},
        }
        for row in rows
    ]
    server.load(exchanges)
    try:
        with Zipbox(
            "zb_" + "T" * 43,
            base_url=server.base_url,
            poll_interval_s=0.001,
            poll_max_interval_s=0.002,
        ) as zb:
            box = zb.sandboxes.get_or_create("build")
        assert not server.errors and not server.unserved()
        return box.info
    finally:
        server.stop()


def test_get_or_create_returns_an_unjoined_private_box_once_running_with_its_key() -> None:
    # Not joined = no tailnet IPv4: it cannot become ready, so waiting would never end.
    info = _get_or_create([_private_row(False, None)])
    assert info.private_mode and not info.ready


def test_get_or_create_waits_for_ready_on_a_joined_private_box() -> None:
    info = _get_or_create([_private_row(False, "100.101.32.7"), _private_row(True, "100.101.32.7")])
    assert info.ready


def test_the_deadline_ends_a_probe_that_never_answers() -> None:
    # A tailnet address off the tailnet swallows the request: the deadline, not the
    # network, ends the probe. Shortened here; production uses PRIVATE_PROBE_TIMEOUT_S.
    assert PRIVATE_PROBE_TIMEOUT_S == 5.0
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as silent:
        silent.bind(("127.0.0.1", 0))
        silent.listen(8)  # accepts the connection, never answers
        port = silent.getsockname()[1]
        core = Core(resolve_config("zb_k", "https://api.test", 0))
        target = BoxTarget(f"http://127.0.0.1:{port}", "bt", name="build", private=True)
        transport = HttpxSyncTransport()
        started = time.monotonic()
        with pytest.raises(ZipboxError) as caught:
            run_sync(transport, core.probe_private("sbx", target, timeout_s=0.3))
        elapsed = time.monotonic() - started
        transport.close()
    assert caught.value.code == "box_private_unreachable"
    assert 0.3 <= elapsed < 3.0
