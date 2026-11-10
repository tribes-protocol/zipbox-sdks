import { createServer } from 'node:net'

import { afterEach, describe, expect, it, vi } from 'vitest'

import { ZipboxError } from '../src/errors.js'
import { HttpClient, isTlsFailure, isUnsentFailure } from '../src/http.js'
import { Zipbox } from '../src/index.js'
import { BoxTransport, ControlPlane } from '../src/transport.js'
import type { FetchLike } from '../src/types.js'

// What Node's fetch (undici) throws: a TypeError whose cause carries the code.
function fetchFailed(code: string): TypeError {
  return new TypeError('fetch failed', { cause: Object.assign(new Error(code), { code }) })
}

function client(fetch: FetchLike, maxRetries = 0): HttpClient {
  return new HttpClient({
    fetch,
    maxRetries,
    retryBaseDelayMs: 1,
    retryMaxDelayMs: 30_000,
    random: () => 0
  })
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

  it('counts a TLS handshake the peer ended as unsent, and only that reset', () => {
    const reset = (message: string): TypeError =>
      new TypeError('fetch failed', {
        cause: Object.assign(new Error(message), { code: 'ECONNRESET' })
      })
    const handshake = reset(
      'Client network socket disconnected before secure TLS connection was established'
    )
    expect(isUnsentFailure(handshake)).toBe(true)
    // What Node raises for a reset after the request was written.
    expect(isUnsentFailure(reset('read ECONNRESET'))).toBe(false)
  })

  it('never claims a failure that can follow a sent request', () => {
    expect(isUnsentFailure(fetchFailed('ECONNRESET'))).toBe(false)
    expect(isUnsentFailure(fetchFailed('UND_ERR_SOCKET'))).toBe(false)
    expect(isUnsentFailure(new Error('network error'))).toBe(false)
    expect(isUnsentFailure('nope')).toBe(false)
  })
})

describe('HttpClient.answers', () => {
  it('counts any HTTP response as an answer', async () => {
    const http = client(() => Promise.resolve(new Response('no owner yet', { status: 404 })))
    expect(await http.answers('https://box--build.zbox.sh/challenge', 5_000)).toBe(true)
  })

  it('counts a failed TLS handshake as an answer: the box is reachable, its certificate is not there yet', async () => {
    for (const code of ['ERR_SSL_TLSV1_ALERT_INTERNAL_ERROR', 'CERT_HAS_EXPIRED', 'EPROTO']) {
      const http = client(() => Promise.reject(fetchFailed(code)))
      expect(await http.answers('https://box--build.zbox.sh/challenge', 5_000)).toBe(true)
    }
    expect(isTlsFailure(fetchFailed('ERR_TLS_CERT_ALTNAME_INVALID'))).toBe(true)
    expect(isTlsFailure(fetchFailed('ECONNREFUSED'))).toBe(false)
  })

  it('counts a connection that was never made as no answer', async () => {
    for (const code of ['ECONNREFUSED', 'ENETUNREACH', 'EHOSTUNREACH', 'UND_ERR_CONNECT_TIMEOUT']) {
      const http = client(() => Promise.reject(fetchFailed(code)))
      expect(await http.answers('https://box--build.zbox.sh/challenge', 5_000)).toBe(false)
    }
  })

  it('counts the deadline as no answer', async () => {
    const http = client(
      (_url, init) =>
        new Promise<Response>((_resolve, reject) => {
          init?.signal?.addEventListener('abort', () => {
            reject(new Error('aborted'))
          })
        })
    )
    expect(await http.answers('https://box--build.zbox.sh/challenge', 5)).toBe(false)
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

  // §2.1 of the lifecycle-speed plan: a retired boot route accepts TCP and closes
  // before the TLS handshake. A real socket, so the error is the one Node's fetch raises.
  it('treats a TLS handshake the server closed as unsent, so a box POST is re-resolved', async () => {
    const server = createServer((socket) => {
      socket.destroy()
    })
    await new Promise<void>((resolve) => {
      server.listen(0, '127.0.0.1', resolve)
    })
    const address = server.address()
    if (address === null || typeof address === 'string') throw new Error('no address')
    try {
      const error: unknown = await client((url, init) => fetch(url, init))
        .send({
          method: 'POST',
          url: `https://127.0.0.1:${address.port}/exec`,
          body: { json: { cmd: 'true' } },
          raiseUnsent: true
        })
        .catch((caught: unknown) => caught)
      expect(error).not.toBeInstanceOf(ZipboxError)
      expect(isUnsentFailure(error)).toBe(true)
    } finally {
      await new Promise<void>((resolve) => {
        server.close(() => {
          resolve()
        })
      })
    }
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
    const api = new ControlPlane(http, 'https://api.test', 'zb_key')
    const boxes = new BoxTransport(api, http, () => Promise.reject(new Error('never waits')))

    const error: unknown = await boxes
      .request('sbx_1', { method: 'POST', path: '/exec' })
      .catch((caught: unknown) => caught)

    expect(error).toBeInstanceOf(ZipboxError)
    expect(error).toMatchObject({ code: 'unauthorized', status: 401, retryable: true })
    expect(error instanceof ZipboxError ? error.message : '').toContain('has not received')
  })
})

interface BoxSend {
  at: number
  url: string
  authorization: string | null
}

describe('BoxTransport unsent failure', () => {
  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  // A claimed box's baseUrl moves once, to its chosen name; exec redialled the old
  // origin forever. Each unsent failure re-reads the box and retries on the new target.
  it('re-reads the box after each unsent failure, on a ladder a successful read never resets', async () => {
    vi.useFakeTimers()
    vi.spyOn(Math, 'random').mockReturnValue(0)
    let reads = 0
    const sends: BoxSend[] = []
    const fetch: FetchLike = (url, init) => {
      if (url === 'https://api.test/sandbox/sbx_1') {
        reads++
        return Promise.resolve(
          new Response(
            JSON.stringify({
              id: 'sbx_1',
              status: 'running',
              ready: true,
              readiness: 'ready',
              baseUrl: `https://box-${reads}.test`,
              boxToken: `bt_${reads}`
            })
          )
        )
      }
      const authorization = new Headers(init?.headers).get('authorization')
      sends.push({ at: Date.now(), url, authorization })
      if (sends.length <= 4) return Promise.reject(fetchFailed('ECONNREFUSED'))
      return Promise.resolve(new Response('{}', { status: 200 }))
    }
    const zb = new Zipbox({
      apiKey: 'zb_test',
      baseUrl: 'https://api.test',
      fetch,
      retryBaseDelayMs: 100,
      pollIntervalMs: 1
    })

    const done = zb.sandbox('sbx_1').commands.signal('cmd_1', 'TERM')
    await vi.advanceTimersByTimeAsync(1_000)
    await done

    // One read to start, then one readiness read per unsent failure.
    expect(reads).toBe(5)
    expect(sends.map((send) => send.url)).toEqual([
      'https://box-1.test/exec/cmd_1/signal',
      'https://box-2.test/exec/cmd_1/signal',
      'https://box-3.test/exec/cmd_1/signal',
      'https://box-4.test/exec/cmd_1/signal',
      'https://box-5.test/exec/cmd_1/signal'
    ])
    expect(sends.at(-1)?.authorization).toBe('Bearer bt_5')
    // base 100 ms, doubling, jitter at its floor (50%): 50, 100, 200, 400. A ladder
    // reset by each successful read would wait 50 every time.
    const gaps = sends.slice(1).map((send, index) => send.at - (sends[index]?.at ?? 0))
    expect(gaps).toEqual([50, 100, 200, 400])
  })

  it('ends the call with the wait verdict when the re-read box is paused', async () => {
    let reads = 0
    const fetch: FetchLike = (url) => {
      if (url !== 'https://api.test/sandbox/sbx_1') {
        return Promise.reject(fetchFailed('ECONNREFUSED'))
      }
      reads++
      return Promise.resolve(
        new Response(
          JSON.stringify(
            reads === 1
              ? {
                  id: 'sbx_1',
                  status: 'running',
                  ready: true,
                  readiness: 'ready',
                  baseUrl: 'https://box-1.test',
                  boxToken: 'bt_1'
                }
              : { id: 'sbx_1', status: 'archived', ready: false, readiness: 'paused' }
          )
        )
      )
    }
    const zb = new Zipbox({
      apiKey: 'zb_test',
      baseUrl: 'https://api.test',
      fetch,
      retryBaseDelayMs: 1
    })

    await expect(zb.sandbox('sbx_1').commands.signal('cmd_1', 'TERM')).rejects.toMatchObject({
      code: 'box_not_running',
      retryable: false
    })
    expect(reads).toBe(2)
  })
})
