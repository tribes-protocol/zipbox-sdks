import { describe, expect, it } from 'vitest'

import { Zipbox, ZipboxError } from '../src/index.js'
import type { FetchLike } from '../src/types.js'

// Streamed create (sdks/conformance/README.md, "Streamed create"): create asks for its
// answer streamed and reads the last line exactly like an unstreamed answer. The placed
// line is informational: its origin is handed out before its DNS record is known to be
// live, so create never dials it.

const API_KEY = `zb_${'T'.repeat(43)}`
const ORIGIN = 'https://box--warm-slug.zbox.sh'

const CLAIMED = {
  sandboxId: 'sbx_1',
  name: 'demo',
  domain: 'zbox.sh',
  ipv6: '2a01::1',
  status: 'running',
  tier: 's',
  pendingTier: null,
  isArchived: false,
  cpuPct: 0,
  memPct: 0,
  diskPct: 0,
  createdAt: '2026-11-11T00:00:00.000Z',
  updatedAt: '2026-11-11T00:00:00.000Z',
  created: true,
  ready: true,
  readiness: 'ready',
  httpsReady: true,
  baseUrl: ORIGIN,
  boxToken: 'bt_test'
}
const EXEC_EVENTS =
  'event: start\ndata: {"id":"cmd_1"}\n\n' +
  'event: exit\ndata: {"state":"exited","exitCode":0,"signal":null,"timedOut":false}\n\n'
const NDJSON = { 'content-type': 'application/x-ndjson; charset=utf-8' }

interface Seen {
  url: string
  method: string
  accept: string | null
  authorization: string | null
  idempotencyKey: string | null
}

interface Harness {
  zb: Zipbox
  seen: Seen[]
  // The answer for each POST /sandbox/identity/claim, in order; a stream by default.
  claims: (() => Response)[]
  streams: ReadableStreamDefaultController<Uint8Array>[]
}

function streamedClaim(streams: ReadableStreamDefaultController<Uint8Array>[]): Response {
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      streams.push(controller)
    }
  })
  return new Response(body, { status: 200, headers: NDJSON })
}

function harness(): Harness {
  const seen: Seen[] = []
  const claims: (() => Response)[] = []
  const streams: ReadableStreamDefaultController<Uint8Array>[] = []
  const fetch: FetchLike = (input, init) => {
    const url = new URL(input)
    const headers = new Headers(init?.headers)
    seen.push({
      url: url.toString(),
      method: init?.method ?? 'GET',
      accept: headers.get('accept'),
      authorization: headers.get('authorization'),
      idempotencyKey: headers.get('idempotency-key')
    })
    if (url.pathname === '/healthz') {
      return Promise.resolve(new Response('{"ok":true}', { status: 200 }))
    }
    if (url.pathname === '/sandbox/identity/claim') {
      const next = claims.shift()
      return Promise.resolve(next === undefined ? streamedClaim(streams) : next())
    }
    if (url.pathname === '/exec') {
      return Promise.resolve(
        new Response(EXEC_EVENTS, { status: 200, headers: { 'content-type': 'text/event-stream' } })
      )
    }
    return Promise.resolve(new Response(JSON.stringify(CLAIMED), { status: 200 }))
  }
  const zb = new Zipbox({ apiKey: API_KEY, fetch, retryBaseDelayMs: 1, retryMaxDelayMs: 1 })
  return { zb, seen, claims, streams }
}

function line(value: unknown): Uint8Array {
  return new TextEncoder().encode(`${JSON.stringify(value)}\n`)
}

function answer(status: number, body: unknown, headers: Record<string, string> = {}): Uint8Array {
  return line({ type: 'answer', status, headers, body })
}

async function settled(): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, 0))
}

describe('streamed create', () => {
  it('reads the last line as the answer and never dials the placed origin', async () => {
    const { zb, seen, streams } = harness()
    const creating = zb.sandboxes.create({ name: 'demo' })
    await settled()
    expect(seen[0]?.accept).toContain('application/x-ndjson')
    streams[0]?.enqueue(line({ type: 'placed', sandboxId: 'sbx_1', baseUrl: ORIGIN }))
    await settled()
    expect(seen.some((request) => request.url.startsWith(ORIGIN))).toBe(false)

    streams[0]?.enqueue(answer(200, CLAIMED, { 'server-timing': 'total;dur=480' }))
    streams[0]?.close()
    const box = await creating
    await box.exec('true')

    expect(box.id).toBe('sbx_1')
    expect(seen.some((request) => request.url === `${ORIGIN}/healthz`)).toBe(false)
    const exec = seen.find((request) => request.url === `${ORIGIN}/exec`)
    expect(exec?.authorization).toBe('Bearer bt_test')
  })

  it('reads an unstreamed answer as before', async () => {
    const { zb, seen, claims } = harness()
    claims.push(() => new Response(JSON.stringify(CLAIMED), { status: 200 }))

    const box = await zb.sandboxes.create({ name: 'demo' })

    expect(box.id).toBe('sbx_1')
    expect(seen.some((request) => request.url.endsWith('/healthz'))).toBe(false)
  })

  it('raises the error the last line carries, as an unstreamed answer would', async () => {
    const { zb, seen, streams } = harness()
    const creating = zb.sandboxes.create({ name: 'demo' })
    await settled()
    streams[0]?.enqueue(line({ type: 'placed', sandboxId: 'sbx_1', baseUrl: ORIGIN }))
    streams[0]?.enqueue(
      answer(409, {
        error: {
          code: 'conflict',
          message: 'Sandbox name is already taken',
          requestId: 'req_1',
          retryable: false,
          retryAfterMs: null,
          docsUrl: 'https://zipbox.ai/docs/errors#conflict'
        }
      })
    )
    streams[0]?.close()

    const error: unknown = await creating.catch((caught: unknown) => caught)
    expect(error).toBeInstanceOf(ZipboxError)
    expect(error).toMatchObject({ code: 'conflict', status: 409, requestId: 'req_1' })
    expect(seen.some((request) => request.url.startsWith(ORIGIN))).toBe(false)
  })

  it('retries a stream that ends before its answer with the same Idempotency-Key', async () => {
    const { zb, seen, claims, streams } = harness()
    claims.push(() => streamedClaim(streams))
    claims.push(() => new Response(JSON.stringify(CLAIMED), { status: 200 }))
    const creating = zb.sandboxes.create({ name: 'demo' })
    await settled()
    streams[0]?.enqueue(line({ type: 'placed', sandboxId: 'sbx_1', baseUrl: ORIGIN }))
    streams[0]?.close()

    const box = await creating

    expect(box.id).toBe('sbx_1')
    const claimCalls = seen.filter((request) => request.url.endsWith('/sandbox/identity/claim'))
    expect(claimCalls).toHaveLength(2)
    expect(claimCalls[1]?.idempotencyKey).toBe(claimCalls[0]?.idempotencyKey)
    expect(seen.some((request) => request.url.startsWith(ORIGIN))).toBe(false)
  })
})
