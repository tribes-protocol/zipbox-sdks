//! Seal a secret to a box: ECIES to the box's own P-256 `agentPublicKey`.
//!
//! Ephemeral P-256 → ECDH (the 32-byte x coordinate) → HKDF-SHA256 (empty salt,
//! `info` binds the sandbox id and the purpose) → AES-256-GCM (12-byte iv, the
//! 16-byte tag appended to the ciphertext). Byte-compatible with `sealBoxEnvelope`
//! in the dashboard and with the box's opener; pinned by
//! `sdks/conformance/vectors/box_envelope.json`.
//!
//! The plaintext is only ever encrypted here. It is never logged, stored, or put
//! in an error message.

use aes_gcm::aead::{Aead, KeyInit};
use aes_gcm::{Aes256Gcm, Nonce};
use base64::engine::general_purpose::{STANDARD_NO_PAD, URL_SAFE_NO_PAD};
use base64::Engine;
use hkdf::Hkdf;
use p256::pkcs8::{DecodePublicKey, EncodePublicKey};
use p256::{PublicKey, SecretKey};
use rand::rngs::OsRng;
use rand::RngCore;
use serde::Serialize;
use sha2::Sha256;

use crate::error::{Error, ErrorCode, Result};

/// HKDF-Extract with an empty salt: HMAC pads it to a zero key.
const EMPTY_SALT: &[u8] = &[];

/// The sealed form a box opens. Every field is base64url without padding.
#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub(crate) struct BoxEnvelope {
    pub ephemeral_public_key: String,
    pub iv: String,
    pub ciphertext: String,
}

/// The HKDF info a Tailscale auth key is sealed under, for this one box.
pub(crate) fn tailscale_info(sandbox_id: &str) -> String {
    format!("zipbox-tailscale-v1|{sandbox_id}")
}

fn box_key(agent_public_key: &str) -> Result<PublicKey> {
    // The API serves standard base64; accept the URL-safe alphabet and missing
    // padding too.
    let normalized: String = agent_public_key
        .trim()
        .trim_end_matches('=')
        .chars()
        .map(|c| match c {
            '-' => '+',
            '_' => '/',
            other => other,
        })
        .collect();
    let der = STANDARD_NO_PAD
        .decode(normalized)
        .map_err(|_| invalid_key())?;
    PublicKey::from_public_key_der(&der).map_err(|_| invalid_key())
}

fn invalid_key() -> Error {
    Error::new(
        ErrorCode::Conflict,
        "the box's agentPublicKey is not a P-256 key",
    )
}

/// Seals with a given ephemeral key and iv. Only the conformance vector pins these.
pub(crate) fn seal_with(
    agent_public_key: &str,
    info: &str,
    plaintext: &str,
    ephemeral: &SecretKey,
    iv: &[u8; 12],
) -> Result<BoxEnvelope> {
    let recipient = box_key(agent_public_key)?;
    let shared = p256::ecdh::diffie_hellman(ephemeral.to_nonzero_scalar(), recipient.as_affine());
    let mut key = [0u8; 32];
    Hkdf::<Sha256>::new(Some(EMPTY_SALT), shared.raw_secret_bytes())
        .expand(info.as_bytes(), &mut key)
        .expect("32 bytes is a valid HKDF-SHA256 length");
    let cipher = Aes256Gcm::new_from_slice(&key).expect("a 32-byte key");
    let ciphertext = cipher
        .encrypt(&Nonce::from(*iv), plaintext.as_bytes())
        .map_err(|_| Error::new(ErrorCode::InvalidRequest, "the secret could not be sealed"))?;
    let spki = ephemeral
        .public_key()
        .to_public_key_der()
        .map_err(|_| Error::new(ErrorCode::InvalidRequest, "the secret could not be sealed"))?;
    Ok(BoxEnvelope {
        ephemeral_public_key: URL_SAFE_NO_PAD.encode(spki.as_bytes()),
        iv: URL_SAFE_NO_PAD.encode(iv),
        ciphertext: URL_SAFE_NO_PAD.encode(ciphertext),
    })
}

/// Seals `plaintext` so only the box holding `agent_public_key` can open it.
pub(crate) fn seal(agent_public_key: &str, info: &str, plaintext: &str) -> Result<BoxEnvelope> {
    let mut iv = [0u8; 12];
    OsRng.fill_bytes(&mut iv);
    seal_with(
        agent_public_key,
        info,
        plaintext,
        &SecretKey::random(&mut OsRng),
        &iv,
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use p256::pkcs8::DecodePrivateKey;
    use serde_json::Value;

    fn vector() -> Value {
        let path = concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../conformance/vectors/box_envelope.json"
        );
        serde_json::from_str(&std::fs::read_to_string(path).expect("vector")).expect("json")
    }

    fn s<'a>(v: &'a Value, pointer: &str) -> &'a str {
        v.pointer(pointer).and_then(Value::as_str).expect(pointer)
    }

    fn hex(value: &str) -> Vec<u8> {
        (0..value.len())
            .step_by(2)
            .map(|i| u8::from_str_radix(&value[i..i + 2], 16).expect("hex"))
            .collect()
    }

    /// The box's opener, in this SDK's crypto stack.
    fn open(envelope: &BoxEnvelope, private_key_pem: &str, info: &str) -> Option<String> {
        let secret = SecretKey::from_pkcs8_pem(private_key_pem).expect("pkcs8");
        let ephemeral = PublicKey::from_public_key_der(
            &URL_SAFE_NO_PAD
                .decode(&envelope.ephemeral_public_key)
                .ok()?,
        )
        .ok()?;
        let shared = p256::ecdh::diffie_hellman(secret.to_nonzero_scalar(), ephemeral.as_affine());
        let mut key = [0u8; 32];
        Hkdf::<Sha256>::new(Some(EMPTY_SALT), shared.raw_secret_bytes())
            .expand(info.as_bytes(), &mut key)
            .ok()?;
        let iv: [u8; 12] = URL_SAFE_NO_PAD.decode(&envelope.iv).ok()?.try_into().ok()?;
        let sealed = URL_SAFE_NO_PAD.decode(&envelope.ciphertext).ok()?;
        let plain = Aes256Gcm::new_from_slice(&key)
            .ok()?
            .decrypt(&Nonce::from(iv), sealed.as_slice())
            .ok()?;
        String::from_utf8(plain).ok()
    }

    fn vector_seal(v: &Value) -> BoxEnvelope {
        let ephemeral =
            SecretKey::from_slice(&hex(s(v, "/ephemeral/privateKeyHex"))).expect("scalar");
        let iv: [u8; 12] = URL_SAFE_NO_PAD
            .decode(s(v, "/iv"))
            .expect("iv")
            .try_into()
            .expect("12 bytes");
        seal_with(
            s(v, "/recipient/agentPublicKey"),
            s(v, "/info"),
            s(v, "/plaintext"),
            &ephemeral,
            &iv,
        )
        .expect("seal")
    }

    #[test]
    fn the_info_string_binds_the_box_and_the_purpose() {
        let v = vector();
        assert_eq!(tailscale_info(s(&v, "/sandboxId")), s(&v, "/info"));
    }

    #[test]
    fn seal_reproduces_the_conformance_vector_byte_for_byte() {
        let v = vector();
        let sealed = vector_seal(&v);
        assert_eq!(serde_json::to_value(&sealed).unwrap(), v["envelope"]);
    }

    #[test]
    fn the_vector_round_trips_and_another_purpose_cannot_open_it() {
        let v = vector();
        let pem = s(&v, "/recipient/privateKeyPkcs8Pem");
        let sealed = vector_seal(&v);
        assert_eq!(
            open(&sealed, pem, s(&v, "/info")).as_deref(),
            Some(s(&v, "/plaintext"))
        );
        assert_eq!(open(&sealed, pem, s(&v, "/wrongInfo")), None);
    }

    #[test]
    fn a_fresh_seal_opens_never_repeats_and_hides_the_secret() {
        let v = vector();
        let pem = s(&v, "/recipient/privateKeyPkcs8Pem");
        let plaintext = s(&v, "/plaintext");
        let first = seal(
            s(&v, "/recipient/agentPublicKey"),
            s(&v, "/info"),
            plaintext,
        )
        .unwrap();
        let second = seal(
            s(&v, "/recipient/agentPublicKey"),
            s(&v, "/info"),
            plaintext,
        )
        .unwrap();
        assert_ne!(first, second);
        assert_eq!(
            open(&first, pem, s(&v, "/info")).as_deref(),
            Some(plaintext)
        );
        assert!(!format!("{first:?}").contains(plaintext));
    }

    #[test]
    fn the_box_key_is_accepted_url_safe_and_unpadded() {
        let v = vector();
        let url_safe: String = s(&v, "/recipient/agentPublicKey")
            .trim_end_matches('=')
            .replace('+', "-")
            .replace('/', "_");
        let sealed = seal(&url_safe, s(&v, "/info"), s(&v, "/plaintext")).unwrap();
        let pem = s(&v, "/recipient/privateKeyPkcs8Pem");
        assert_eq!(
            open(&sealed, pem, s(&v, "/info")).as_deref(),
            Some(s(&v, "/plaintext"))
        );
    }

    #[test]
    fn a_malformed_box_key_never_echoes_the_secret() {
        let err = seal("not-a-key", "info", "tskey-auth-secret").unwrap_err();
        assert!(!format!("{err} {err:?}").contains("tskey-auth-secret"));
    }
}
