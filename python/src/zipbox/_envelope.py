"""Seal a secret to a box: ECIES to the box's own P-256 ``agentPublicKey``.

Ephemeral P-256 → ECDH (the 32-byte x coordinate) → HKDF-SHA256 (empty salt,
``info`` binds the sandbox id and the purpose) → AES-256-GCM (12-byte iv, the
16-byte tag appended to the ciphertext). Byte-compatible with ``sealBoxEnvelope``
in the dashboard and with the box's opener; pinned by
``sdks/conformance/vectors/box_envelope.json``.

The plaintext is only ever encrypted here. It is never logged, stored, or put in
an error message.
"""

from __future__ import annotations

import base64
import binascii
import os

from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.hazmat.primitives.ciphers.aead import AESGCM
from cryptography.hazmat.primitives.kdf.hkdf import HKDF

from zipbox._errors import ZipboxError

_IV_BYTES = 12


def tailscale_info(sandbox_id: str) -> str:
    """The HKDF info a Tailscale auth key is sealed under, for this one box."""
    return f"zipbox-tailscale-v1|{sandbox_id}"


def _b64url(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode()


def _box_key(agent_public_key: str) -> ec.EllipticCurvePublicKey:
    # The API serves standard base64; accept the URL-safe alphabet and missing padding too.
    normalized = agent_public_key.strip().replace("-", "+").replace("_", "/")
    normalized += "=" * (-len(normalized) % 4)
    try:
        key = serialization.load_der_public_key(base64.b64decode(normalized, validate=True))
    except (ValueError, binascii.Error, TypeError) as error:
        raise ZipboxError("conflict", "The box's agentPublicKey is not a valid key.") from error
    if not isinstance(key, ec.EllipticCurvePublicKey) or not isinstance(key.curve, ec.SECP256R1):
        raise ZipboxError("conflict", "The box's agentPublicKey is not a P-256 key.")
    return key


def seal_with(
    agent_public_key: str,
    info: str,
    plaintext: str,
    ephemeral: ec.EllipticCurvePrivateKey,
    iv: bytes,
) -> dict[str, str]:
    """Seal with a given ephemeral key and iv. Only the conformance vector pins these."""
    shared = ephemeral.exchange(ec.ECDH(), _box_key(agent_public_key))
    # salt=None is HMAC keyed with zeros, which is the same PRK as an empty salt: HMAC
    # pads a short key with zeros.
    key = HKDF(algorithm=hashes.SHA256(), length=32, salt=None, info=info.encode()).derive(shared)
    ciphertext = AESGCM(key).encrypt(iv, plaintext.encode(), None)
    spki = ephemeral.public_key().public_bytes(
        serialization.Encoding.DER, serialization.PublicFormat.SubjectPublicKeyInfo
    )
    return {
        "ephemeralPublicKey": _b64url(spki),
        "iv": _b64url(iv),
        "ciphertext": _b64url(ciphertext),
    }


def seal(agent_public_key: str, info: str, plaintext: str) -> dict[str, str]:
    """Seal ``plaintext`` so only the box holding ``agent_public_key`` can open it."""
    return seal_with(
        agent_public_key,
        info,
        plaintext,
        ec.generate_private_key(ec.SECP256R1()),
        os.urandom(_IV_BYTES),
    )
