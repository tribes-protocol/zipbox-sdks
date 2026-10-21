import { describe, expect, it } from 'vitest'

import { ZipboxError } from '../src/errors.js'
import { HttpClient, isUnsentFailure } from '../src/http.js'
import { BoxTransport, ControlPlane } from '../src/transport.js'
import type { FetchLike } from '../src/types.js'

// What Node's fetch (undici) throws: a TypeError whose cause carries the code.
function fetchFailed(code: string): TypeError {
  return new TypeError('fetch failed', { cause: Object.assign(new Error(code), { code }) })
}

function client(fetch: FetchLike, maxRetries = 0): HttpClient {
  return new HttpClient({ fetch, maxRetries, retryBaseDelayMs: 1, random: () => 0 })
}

describe('isUnsentFailure', () => {
  it('recognizes connection and TLS-handshake failures, however deep the cause', () => {
    expect(isUnsentFailure(fetchFailed('ECONNREFUSED'))).toBe(true)
    expect(isUnsentFailure(fetchFailed('ENOTFOUND'))).toBe(true)
    // The TLS internal alert a fresh box name answers until its certificate lands.
    expect(isUnsentFailure(fetchFailed('ERR_SSL_TLSV1_ALERT_INTERNAL_ERROR'))).toBe(true)
    expect(isUnsentFailure(fetchFailed('ERR_TLS_CERT_ALTNAME_INVALID'))).toBe(true)
    expect(isUnsentFailure(fetchFailed('UNABLE_TO_VERIFY_LEAF_SIGNATURE'))).toBe(true)
    // Bun puts the code on the error itself.
    expect(
      isUnsentFailure(Object.assign(new Error('Unable to connect'), { code: 'ConnectionRefused' }))
    ).toBe(true)
  })

  it('never claims a failure that can follow a sent request', () => {
    expect(isUnsentFailure(fetchFailed('ECONNRESET'))).toBe(false)
    expect(isUnsentFailure(fetchFailed('UND_ERR_SOCKET'))).toBe(false)
    expect(isUnsentFailure(new Error('network error'))).toBe(false)
    expect(isUnsentFailure('nope')).toBe(false)
  })
})

describe('HttpClient.send', () => {
  // #4139: exec on a box whose HTTPS was not up yet failed at once — a POST was only
  // retried with retry:true, and at most maxRetries times. Nothing was sent.
  it('retries an unsent failure on a POST, past maxRetries, until it connects', async () => {
    let calls = 0
    const fetch: FetchLike = () => {
      calls++
      if (calls <= 5) return Promise.reject(fetchFailed('ERR_SSL_TLSV1_ALERT_INTERNAL_ERROR'))
      return Promise.resolve(new Response('{}', { status: 200 }))
    }
    const response = await client(fetch).send({ method: 'POST', url: 'https://box.test/exec' })
    expect(response.status).toBe(200)
    expect(calls).toBe(6)
  })

  it('does not repeat a POST whose failure may have followed the send', async () => {
    let calls = 0
    const fetch: FetchLike = () => {
      calls++
      return Promise.reject(fetchFailed('ECONNRESET'))
    }
    await expect(
      client(fetch, 3).send({ method: 'POST', url: 'https://box.test/exec' })
    ).rejects.toMatchObject({ code: 'unavailable' })
    expect(calls).toBe(1)
  })

  it('stops retrying the moment the caller aborts', async () => {
    const controller = new AbortController()
    let calls = 0
    const fetch: FetchLike = () => {
      calls++
      if (calls === 3) controller.abort(new Error('caller gave up'))
      return Promise.reject(fetchFailed('ECONNREFUSED'))
    }
    await expect(
      client(fetch).send({
        method: 'POST',
        url: 'https://box.test/exec',
        signal: controller.signal
      })
    ).rejects.toThrow('fetch failed')
    expect(calls).toBe(3)
  })
})

describe('BoxTransport 401', () => {
  // #4137: the old wording ("was the key deleted?") was false — the control plane had
  // just re-issued the token, which it never does for a deleted key.
  it('says the box has not received a live key yet, not that the key was deleted', async () => {
    const detail = {
      id: 'sbx_1',
      name: 'demo',
      status: 'running',
      tier: 's',
      ready: true,
      baseUrl: 'https://box.test',
      boxToken: 'bt_x'
    }
    const fetch: FetchLike = (url) =>
      Promise.resolve(
        url.endsWith('/sandbox/sbx_1')
          ? new Response(JSON.stringify(detail), { status: 200 })
          : new Response('{"error":"unauthorized"}', { status: 401 })
      )
    const http = client(fetch)
    const boxes = new BoxTransport(new ControlPlane(http, 'https://api.test', 'zb_key'), http)

    const error: unknown = await boxes
      .request('sbx_1', { method: 'POST', path: '/exec' })
      .catch((caught: unknown) => caught)

    expect(error).toBeInstanceOf(ZipboxError)
    expect(error).toMatchObject({ code: 'unauthorized', status: 401, retryable: true })
    expect(error instanceof ZipboxError ? error.message : '').toContain('has not received')
  })
})
