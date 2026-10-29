// ECIES to a box: seal a credential on the caller's machine so that only the box
// can open it. Byte-compatible with the dashboard's sealBoxEnvelope and the box's
// opener (sdks/conformance/README.md rule 9, vector in
// sdks/conformance/vectors/box_envelope.json):
//
//   ephemeral P-256 → ECDH against the box's agentPublicKey (SPKI DER) →
//   HKDF-SHA256 (empty salt, purpose-bound info, 32 bytes) → AES-256-GCM
//   (12-byte iv; the 16-byte tag stays appended to the ciphertext).
//
// Every envelope field is base64url without padding. WebCrypto only, so it runs
// in Node, Deno, Bun and workers alike.

const encoder = new TextEncoder()

/** A credential sealed to one box. Only that box can open it. */
export interface BoxEnvelope {
  /** The ephemeral P-256 public key, SPKI DER, base64url. */
  ephemeralPublicKey: string
  /** The 12-byte AES-GCM iv, base64url. */
  iv: string
  /** AES-256-GCM ciphertext with its 16-byte tag appended, base64url. */
  ciphertext: string
}

/** @internal Fixed randomness, so a test can reproduce the conformance vector. */
export interface SealRandomness {
  ephemeral: CryptoKeyPair
  iv: Uint8Array
}

/** @internal */
export interface SealParams {
  /** The box's `agentPublicKey`: SPKI DER, base64 (standard or url-safe). */
  agentPublicKey: string
  /** The HKDF info, e.g. `zipbox-tailscale-v1|<sandbox id>`. */
  info: string
  /** The secret. Never logged, never returned. */
  plaintext: string
}

/** The HKDF info that binds a Tailscale auth key to one box. */
export function tailscaleEnvelopeInfo(sandboxId: string): string {
  return `zipbox-tailscale-v1|${sandboxId}`
}

function base64ToBytes(value: string): Uint8Array {
  const normalized = value.replace(/-/g, '+').replace(/_/g, '/')
  const binary = atob(normalized.padEnd(Math.ceil(normalized.length / 4) * 4, '='))
  return Uint8Array.from(binary, (char) => char.charCodeAt(0))
}

function bytesToBase64Url(bytes: Uint8Array): string {
  let binary = ''
  for (const byte of bytes) binary += String.fromCharCode(byte)
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

// A fresh Uint8Array over its own ArrayBuffer, which is what WebCrypto's
// BufferSource typing accepts.
function buffer(bytes: Uint8Array): Uint8Array<ArrayBuffer> {
  const copy = new Uint8Array(new ArrayBuffer(bytes.byteLength))
  copy.set(bytes)
  return copy
}

async function freshRandomness(): Promise<SealRandomness> {
  const ephemeral = await globalThis.crypto.subtle.generateKey(
    { name: 'ECDH', namedCurve: 'P-256' },
    true,
    ['deriveBits']
  )
  return { ephemeral, iv: globalThis.crypto.getRandomValues(new Uint8Array(12)) }
}

/** @internal Seal `plaintext` to the box key. `randomness` is for tests only. */
export async function sealBoxEnvelope(
  params: SealParams,
  randomness?: SealRandomness
): Promise<BoxEnvelope> {
  const subtle = globalThis.crypto.subtle
  const recipient = await subtle.importKey(
    'spki',
    buffer(base64ToBytes(params.agentPublicKey)),
    { name: 'ECDH', namedCurve: 'P-256' },
    false,
    []
  )
  const { ephemeral, iv } = randomness ?? (await freshRandomness())
  const shared = await subtle.deriveBits(
    { name: 'ECDH', public: recipient },
    ephemeral.privateKey,
    256
  )
  const hkdf = await subtle.importKey('raw', shared, 'HKDF', false, ['deriveKey'])
  const aesKey = await subtle.deriveKey(
    { name: 'HKDF', hash: 'SHA-256', salt: new Uint8Array(0), info: encoder.encode(params.info) },
    hkdf,
    { name: 'AES-GCM', length: 256 },
    false,
    ['encrypt']
  )
  const ciphertext = await subtle.encrypt(
    { name: 'AES-GCM', iv: buffer(iv) },
    aesKey,
    encoder.encode(params.plaintext)
  )
  const spki = await subtle.exportKey('spki', ephemeral.publicKey)
  return {
    ephemeralPublicKey: bytesToBase64Url(new Uint8Array(spki)),
    iv: bytesToBase64Url(iv),
    ciphertext: bytesToBase64Url(new Uint8Array(ciphertext))
  }
}
