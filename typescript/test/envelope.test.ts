import {
  createDecipheriv,
  createPrivateKey,
  createPublicKey,
  diffieHellman,
  hkdfSync
} from 'node:crypto'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'

import { describe, expect, it } from 'vitest'

import {
  type BoxEnvelope,
  sealBoxEnvelope,
  type SealRandomness,
  tailscaleEnvelopeInfo
} from '../src/envelope.js'

interface Vector {
  sandboxId: string
  info: string
  wrongInfo: string
  plaintext: string
  recipient: { agentPublicKey: string; privateKeyPkcs8Pem: string }
  ephemeral: { privateKeyHex: string; publicKeyUncompressedHex: string }
  iv: string
  envelope: BoxEnvelope
}

function isVector(value: unknown): value is Vector {
  return typeof value === 'object' && value !== null && 'envelope' in value && 'ephemeral' in value
}

const parsed: unknown = JSON.parse(
  readFileSync(
    join(import.meta.dirname, '..', '..', 'conformance', 'vectors', 'box_envelope.json'),
    'utf8'
  )
)
if (!isVector(parsed)) throw new Error('bad vector')
const vector = parsed

function hexToBase64Url(hex: string): string {
  return Buffer.from(hex, 'hex').toString('base64url')
}

async function vectorRandomness(): Promise<SealRandomness> {
  const point = vector.ephemeral.publicKeyUncompressedHex
  const x = hexToBase64Url(point.slice(2, 66))
  const y = hexToBase64Url(point.slice(66))
  const d = hexToBase64Url(vector.ephemeral.privateKeyHex)
  const curve = { name: 'ECDH', namedCurve: 'P-256' }
  const privateKey = await crypto.subtle.importKey(
    'jwk',
    { kty: 'EC', crv: 'P-256', x, y, d },
    curve,
    false,
    ['deriveBits']
  )
  const publicKey = await crypto.subtle.importKey(
    'jwk',
    { kty: 'EC', crv: 'P-256', x, y },
    curve,
    true,
    []
  )
  return {
    ephemeral: { privateKey, publicKey },
    iv: new Uint8Array(Buffer.from(vector.iv, 'base64url'))
  }
}

// The box's opener (apps/microvmd/src/helpers/BoxEnvelope.ts), in node:crypto.
function open(envelope: BoxEnvelope, info: string): string {
  const sharedSecret = diffieHellman({
    privateKey: createPrivateKey(vector.recipient.privateKeyPkcs8Pem),
    publicKey: createPublicKey({
      key: Buffer.from(envelope.ephemeralPublicKey, 'base64url'),
      format: 'der',
      type: 'spki'
    })
  })
  const key = Buffer.from(hkdfSync('sha256', sharedSecret, Buffer.alloc(0), info, 32))
  const sealed = Buffer.from(envelope.ciphertext, 'base64url')
  const decipher = createDecipheriv('aes-256-gcm', key, Buffer.from(envelope.iv, 'base64url'))
  decipher.setAuthTag(sealed.subarray(sealed.length - 16))
  return `${decipher.update(sealed.subarray(0, sealed.length - 16), undefined, 'utf8')}${decipher.final('utf8')}`
}

describe('box envelope', () => {
  it('binds the Tailscale purpose and the box id', () => {
    expect(tailscaleEnvelopeInfo(vector.sandboxId)).toBe(vector.info)
  })

  it('reproduces the conformance vector byte for byte', async () => {
    const envelope = await sealBoxEnvelope(
      {
        agentPublicKey: vector.recipient.agentPublicKey,
        info: vector.info,
        plaintext: vector.plaintext
      },
      await vectorRandomness()
    )
    expect(envelope).toEqual(vector.envelope)
  })

  it('opens the vector with the box opener, and not under another purpose', () => {
    expect(open(vector.envelope, vector.info)).toBe(vector.plaintext)
    expect(() => open(vector.envelope, vector.wrongInfo)).toThrow()
  })

  it('seals with fresh randomness that the box opener opens', async () => {
    const params = {
      agentPublicKey: vector.recipient.agentPublicKey,
      info: vector.info,
      plaintext: vector.plaintext
    }
    const first = await sealBoxEnvelope(params)
    const second = await sealBoxEnvelope(params)
    expect(first.ephemeralPublicKey).not.toBe(second.ephemeralPublicKey)
    expect(first.iv).not.toBe(second.iv)
    expect(open(first, vector.info)).toBe(vector.plaintext)
    expect(open(second, vector.info)).toBe(vector.plaintext)
    for (const field of Object.values(first)) expect(field).toMatch(/^[A-Za-z0-9_-]+$/)
  })

  it('accepts the box key in url-safe base64 too', async () => {
    const urlSafe = Buffer.from(vector.recipient.agentPublicKey, 'base64').toString('base64url')
    const envelope = await sealBoxEnvelope({
      agentPublicKey: urlSafe,
      info: vector.info,
      plaintext: vector.plaintext
    })
    expect(open(envelope, vector.info)).toBe(vector.plaintext)
  })
})
