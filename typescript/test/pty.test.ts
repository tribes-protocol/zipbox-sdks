import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { Zipbox } from '../src/index.js'
import type { WebSocketLike } from '../src/types.js'

const sockets: FakeSocket[] = []

class FakeSocket implements WebSocketLike {
  readyState = 0
  onopen: WebSocketLike['onopen'] = null
  onmessage: WebSocketLike['onmessage'] = null
  onclose: WebSocketLike['onclose'] = null
  onerror: WebSocketLike['onerror'] = null
  sent: unknown[] = []

  constructor(
    readonly url: string,
    readonly protocols?: string | string[]
  ) {
    sockets.push(this)
  }

  send(data: string): void {
    this.sent.push(JSON.parse(data))
  }

  close(): void {
    this.readyState = 3
  }

  serverOpen(): void {
    this.readyState = 1
    this.onopen?.({})
  }

  serverSend(frame: unknown): void {
    this.onmessage?.({ data: JSON.stringify(frame) })
  }

  serverClose(code = 1006): void {
    this.readyState = 3
    this.onclose?.({ code, reason: '' })
  }
}

const detail = {
  id: 'sbx_1',
  status: 'running',
  createdAt: '2026-10-19T00:00:00.000Z',
  updatedAt: '2026-10-19T00:00:00.000Z',
  ready: true,
  baseUrl: 'https://demo.zbox.sh',
  boxToken: 'bt_1'
}

let detailFetches = 0
const fetchMock = (): Promise<Response> => {
  detailFetches++
  return Promise.resolve(
    new Response(JSON.stringify(detail), { headers: { 'content-type': 'application/json' } })
  )
}

function client(): Zipbox {
  return new Zipbox({
    apiKey: 'zb_test',
    baseUrl: 'https://api.test',
    fetch: fetchMock,
    WebSocket: FakeSocket
  })
}

function last(): FakeSocket {
  const socket = sockets.at(-1)
  if (socket === undefined) throw new Error('no socket')
  return socket
}

describe('pty', () => {
  beforeEach(() => {
    sockets.length = 0
    detailFetches = 0
    vi.useFakeTimers()
    vi.spyOn(Math, 'random').mockReturnValue(0.999)
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  it('dials the session with the box token subprotocol, resizes on open, takes over when blocked', async () => {
    const pty = await client().sandbox('sbx_1').pty.create({ cols: 100, rows: 30 })
    await vi.advanceTimersByTimeAsync(0)
    const socket = last()
    const url = new URL(socket.url)
    expect(url.origin).toBe('wss://demo.zbox.sh')
    expect(url.searchParams.get('session')).toBe(pty.sessionId)
    expect(pty.sessionId).toMatch(/^bash-[0-9a-f-]{36}$/)
    expect(url.searchParams.get('clientId')).toBe(pty.clientId)
    expect(url.searchParams.get('resume')).toBeNull()
    expect(socket.protocols).toEqual(['tribes.session', 'bt_1'])
    pty.write('ls\n')
    socket.serverOpen()
    expect(socket.sent).toEqual([
      { t: 'r', c: 100, r: 30 },
      { t: 'i', d: 'ls\n' }
    ])
    socket.serverSend({ t: 'blocked', holder: { clientId: 'browser', label: 'Chrome' } })
    expect(socket.sent.at(-1)).toEqual({ t: 'takeover' })
    pty.detach()
  })

  it('dials the root of a base URL that carries a path', async () => {
    const origin = detail.baseUrl
    detail.baseUrl = 'https://term.example.test/42001/'
    try {
      const pty = await client().sandbox('sbx_1').pty.create()
      await vi.advanceTimersByTimeAsync(0)
      const url = new URL(last().url)
      expect(`${url.origin}${url.pathname}`).toBe('wss://term.example.test/42001/')
      pty.detach()
    } finally {
      detail.baseUrl = origin
    }
  })

  it('reconnects forever on a doubling, capped ladder that resets only on a received frame', async () => {
    const pty = await client().sandbox('sbx_1').pty.create()
    await vi.advanceTimersByTimeAsync(0)
    const output: string[] = []
    pty.onData((data) => output.push(data))
    last().serverOpen()

    const delays: number[] = []
    for (let drop = 0; drop < 9; drop++) {
      const before = sockets.length
      last().serverClose()
      let waited = 0
      while (sockets.length === before) {
        await vi.advanceTimersByTimeAsync(10)
        waited += 10
      }
      delays.push(waited)
    }
    // ~500, 1000, 2000 … capped at 30 s, never giving up.
    expect(delays[0]).toBeLessThanOrEqual(510)
    expect(delays[1]).toBeGreaterThan(delays[0] ?? 0)
    expect(delays[2]).toBeGreaterThan(delays[1] ?? 0)
    expect(Math.max(...delays)).toBeLessThanOrEqual(30_010)
    expect(delays.at(-1)).toBeGreaterThanOrEqual(29_000)
    expect(new URL(last().url).searchParams.get('resume')).toBe('1')
    expect(new URL(last().url).searchParams.get('clientId')).toBe(pty.clientId)
    // Repeated failed dials re-read the box token in case it rotated.
    expect(detailFetches).toBeGreaterThan(1)

    // A received frame resets the ladder.
    last().serverOpen()
    last().serverSend({ t: 'o', d: 'hello' })
    expect(output).toEqual(['hello'])
    const before = sockets.length
    last().serverClose()
    await vi.advanceTimersByTimeAsync(510)
    expect(sockets.length).toBe(before + 1)
    pty.detach()
  })

  it('stops on exit and reports the code', async () => {
    const pty = await client().sandbox('sbx_1').pty.create()
    await vi.advanceTimersByTimeAsync(0)
    const exits: (number | null)[] = []
    const ends: string[] = []
    pty.onExit((code) => exits.push(code))
    pty.onEnd((reason) => ends.push(reason))
    last().serverOpen()
    last().serverSend({ t: 'x', code: 0 })
    last().serverClose(1000)
    const count = sockets.length
    await vi.advanceTimersByTimeAsync(60_000)
    expect(sockets.length).toBe(count)
    expect(exits).toEqual([0])
    expect(ends).toEqual(['exit'])
  })

  it('gives up on an unknown session instead of retrying it', async () => {
    const pty = await client()
      .sandbox('sbx_1')
      .pty.attach('bash-00000000-0000-4000-8000-000000000000')
    await vi.advanceTimersByTimeAsync(0)
    const ends: string[] = []
    pty.onEnd((reason) => ends.push(reason))
    last().serverClose(4404)
    await vi.advanceTimersByTimeAsync(60_000)
    expect(sockets).toHaveLength(1)
    expect(ends).toEqual(['unknown_session'])
  })

  it('ends with an error when the control plane refuses the key on a reconnect', async () => {
    let revoked = false
    const zb = new Zipbox({
      apiKey: 'zb_test',
      baseUrl: 'https://api.test',
      fetch: () =>
        Promise.resolve(
          revoked
            ? new Response(
                JSON.stringify({
                  error: {
                    code: 'unauthorized',
                    message: 'key deleted',
                    requestId: 'r',
                    retryable: false,
                    docsUrl: 'https://zipbox.ai/docs/errors#unauthorized'
                  }
                }),
                { status: 401 }
              )
            : new Response(JSON.stringify(detail))
        ),
      WebSocket: FakeSocket
    })
    const pty = await zb.sandbox('sbx_1').pty.create()
    await vi.advanceTimersByTimeAsync(0)
    const ends: string[] = []
    pty.onEnd((reason) => ends.push(reason))
    last().serverOpen()
    revoked = true
    last().serverClose(4401)
    await vi.advanceTimersByTimeAsync(120_000)
    expect(ends).toEqual(['error'])
    expect(sockets).toHaveLength(1)
  })
})
