import { afterEach, describe, expect, it, vi } from 'vitest'

import { Zipbox } from '../src/index.js'
import type { FetchLike } from '../src/types.js'

const API_KEY = `zb_${'T'.repeat(43)}`

const READY_DETAIL = {
  id: 'sbx_1',
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
  createdAt: '2026-10-27T00:00:00.000Z',
  updatedAt: '2026-10-27T00:00:00.000Z',
  ready: true,
  baseUrl: 'https://demo.zbox.sh',
  boxToken: 'bt_test'
}

// Records every claim body the client sends and answers a box that is ready at once.
function recordingFetch(claimBodies: unknown[]): FetchLike {
  return (input, init) => {
    const path = new URL(input).pathname
    if (init?.method === 'POST' && path === '/sandbox/identity/claim') {
      claimBodies.push(JSON.parse(typeof init.body === 'string' ? init.body : 'null'))
      return Promise.resolve(new Response(JSON.stringify({ sandboxId: 'sbx_1' }), { status: 200 }))
    }
    return Promise.resolve(new Response(JSON.stringify(READY_DETAIL), { status: 200 }))
  }
}

describe('Sandboxes.create mailbox opt-in', () => {
  it('sends mailbox when the caller opts in', async () => {
    const claimBodies: unknown[] = []
    const zb = new Zipbox({ apiKey: API_KEY, fetch: recordingFetch(claimBodies) })

    await zb.sandboxes.create({ name: 'demo', mailbox: true })

    expect(claimBodies).toEqual([{ name: 'demo', mailbox: true }])
  })

  it('omits mailbox by default so the box gets one on first use', async () => {
    const claimBodies: unknown[] = []
    const zb = new Zipbox({ apiKey: API_KEY, fetch: recordingFetch(claimBodies) })

    await zb.sandboxes.create({})

    expect(claimBodies).toEqual([{}])
  })
})

// Review follow-ups (audit-ax AX-01): a wait names a remedy the server takes, and a
// status this SDK does not know keeps waiting instead of throwing a bare value.
describe('waitReady verdicts', () => {
  function client(details: unknown[]): Zipbox {
    return new Zipbox({
      apiKey: API_KEY,
      baseUrl: 'https://api.test',
      pollIntervalMs: 1,
      fetch: () => {
        const next = details.shift()
        if (next === undefined) throw new Error('unexpected call')
        return Promise.resolve(new Response(JSON.stringify(next)))
      }
    })
  }

  it('a paused box that cannot resume now says so instead of "resume it"', async () => {
    const paused = {
      ...READY_DETAIL,
      status: 'paused',
      ready: false,
      readiness: 'paused',
      allowedActions: ['delete']
    }
    const error = await client([paused])
      .sandboxes.waitReady('sbx_1')
      .catch((caught: unknown) => caught)
    expect(error).toMatchObject({ code: 'box_not_running', retryable: false })
    expect(String(error)).toMatch(/cannot be resumed now/)
  })

  it('a status from a later server, with no readiness, keeps waiting', async () => {
    const later = { ...READY_DETAIL, status: 'hibernating', ready: false }
    const box = await client([later, READY_DETAIL]).sandboxes.waitReady('sbx_1')
    expect(box.ready).toBe(true)
  })
})

// A box turns ready ~1 s after it serves; doubling slept up to seconds past it. An
// API without GET /sandbox/:id/wait answers it 404, and the wait polls instead.
describe('waitReady poll ladder', () => {
  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  it('re-reads a settling box 1.25x further apart each time, capped at 10 s', async () => {
    vi.useFakeTimers()
    // Jitter pinned at its top, so each delay is the ladder's own step.
    vi.spyOn(Math, 'random').mockReturnValue(1)
    const settling = { ...READY_DETAIL, ready: false, readiness: 'settling' }
    const reads: number[] = []
    const zb = new Zipbox({
      apiKey: API_KEY,
      baseUrl: 'https://api.test',
      fetch: (input) => {
        if (new URL(input).pathname.endsWith('/wait')) {
          return Promise.resolve(new Response('{"error":"Route not found"}', { status: 404 }))
        }
        reads.push(Date.now())
        const body = reads.length <= 14 ? settling : READY_DETAIL
        return Promise.resolve(new Response(JSON.stringify(body)))
      }
    })

    const ready = zb.sandboxes.waitReady('sbx_1')
    await vi.advanceTimersByTimeAsync(120_000)
    await ready

    expect(reads).toHaveLength(15)

    const gaps = reads.slice(1).map((at, index) => at - (reads[index] ?? 0))
    expect(gaps.slice(0, 3)).toEqual([1000, 1250, 1563])
    expect(gaps).toEqual(
      Array.from({ length: 14 }, (_, n) => Math.round(Math.min(10_000, 1000 * 1.25 ** n)))
    )
    expect(gaps.slice(-3)).toEqual([10_000, 10_000, 10_000])
  })
})

interface SentRequest {
  method: string
  path: string
  authorization: string | null
  body: string
}

// Answers in order and records every request; a box answers one exec.
function scripted(answers: { status?: number; body: unknown }[], sent: SentRequest[]): FetchLike {
  return (input, init) => {
    const url = new URL(input)
    sent.push({
      method: init?.method ?? 'GET',
      path: `${url.pathname}${url.search}`,
      authorization: new Headers(init?.headers).get('authorization'),
      body: typeof init?.body === 'string' ? init.body : ''
    })
    if (url.pathname === '/exec') {
      return Promise.resolve(
        new Response(
          'id: 0-0\nevent: start\ndata: {"id":"c"}\n\nid: 0-0\nevent: exit\ndata: {"state":"exited","exitCode":0}\n\n',
          { headers: { 'content-type': 'text/event-stream' } }
        )
      )
    }
    const next = answers.shift()
    if (next === undefined) throw new Error(`unexpected ${url.pathname}`)
    return Promise.resolve(new Response(JSON.stringify(next.body), { status: next.status ?? 200 }))
  }
}

const SETTLING = { ...READY_DETAIL, status: 'stopping', ready: false, readiness: 'settling' }
const PAUSED = {
  ...READY_DETAIL,
  status: 'paused',
  ready: false,
  readiness: 'paused',
  baseUrl: null,
  boxToken: null
}

describe('GET /sandbox/:id/wait', () => {
  it('asks again at once after a held answer that is still settling', async () => {
    const sent: SentRequest[] = []
    // A poll ladder of a minute: the test times out if the wait slept on it.
    const zb = new Zipbox({
      apiKey: API_KEY,
      baseUrl: 'https://api.test',
      pollIntervalMs: 60_000,
      fetch: scripted([{ body: SETTLING }, { body: SETTLING }, { body: READY_DETAIL }], sent)
    })
    const box = await zb.sandboxes.waitReady('sbx_1')
    expect(box.ready).toBe(true)
    expect(sent.map((request) => request.path)).toEqual([
      '/sandbox/sbx_1/wait?until=ready',
      '/sandbox/sbx_1/wait?until=ready',
      '/sandbox/sbx_1/wait?until=ready'
    ])
  })

  it('falls back to polling GET /sandbox/:id when the API predates /wait', async () => {
    const sent: SentRequest[] = []
    const zb = new Zipbox({
      apiKey: API_KEY,
      baseUrl: 'https://api.test',
      pollIntervalMs: 1,
      fetch: scripted(
        [
          { status: 404, body: { error: 'Route not found' } },
          { body: SETTLING },
          { body: READY_DETAIL }
        ],
        sent
      )
    })
    const box = await zb.sandboxes.waitReady('sbx_1')
    expect(box.ready).toBe(true)
    expect(sent.map((request) => request.path)).toEqual([
      '/sandbox/sbx_1/wait?until=ready',
      '/sandbox/sbx_1',
      '/sandbox/sbx_1'
    ])
  })
})

describe('Sandboxes.create bound inside the request', () => {
  it('returns at once and calls the box with the token from the claim', async () => {
    const sent: SentRequest[] = []
    const claim = {
      sandboxId: 'sbx_1',
      name: 'demo',
      status: 'running',
      created: true,
      ready: true,
      readiness: 'ready',
      httpsReady: true,
      baseUrl: 'https://demo.zbox.sh',
      boxToken: 'bt_claim'
    }
    const zb = new Zipbox({ apiKey: API_KEY, fetch: scripted([{ body: claim }], sent) })
    const box = await zb.sandboxes.create({ name: 'demo' })
    expect(box.id).toBe('sbx_1')
    expect(box.ready).toBe(true)
    expect(box.info?.boxToken).toBeUndefined()
    await box.exec('true')
    expect(sent.map((request) => `${request.method} ${request.path}`)).toEqual([
      'POST /sandbox/identity/claim',
      'POST /exec'
    ])
    expect(sent.at(-1)?.authorization).toBe('Bearer bt_claim')
  })

  it('waits when the claim answer is not ready', async () => {
    const sent: SentRequest[] = []
    const claim = { sandboxId: 'sbx_1', status: 'provisioning', created: true, ready: false }
    const zb = new Zipbox({
      apiKey: API_KEY,
      fetch: scripted([{ body: claim }, { body: READY_DETAIL }], sent)
    })
    await zb.sandboxes.create({ name: 'demo' })
    expect(sent.map((request) => request.path)).toEqual([
      '/sandbox/identity/claim',
      '/sandbox/sbx_1/wait?until=ready'
    ])
  })
})

describe('pause and resume', () => {
  async function requests(
    run: (zb: Zipbox) => Promise<unknown>,
    answers: { status?: number; body: unknown }[]
  ): Promise<SentRequest[]> {
    const sent: SentRequest[] = []
    await run(new Zipbox({ apiKey: API_KEY, fetch: scripted(answers, sent) }))
    return sent
  }

  it('stop sends exactly what pause sends, and returns the same box', async () => {
    const answers = (): { body: unknown }[] => [{ body: PAUSED }]
    const paused = await requests((zb) => zb.sandboxes.pause('sbx_1'), answers())
    const stopped = await requests((zb) => zb.sandboxes.stop('sbx_1'), answers())
    expect(stopped).toEqual(paused)
    expect(paused.map((request) => `${request.method} ${request.path}`)).toEqual([
      'POST /sandbox/sbx_1/pause'
    ])
    const onBox = await requests((zb) => zb.sandbox('sbx_1').stop(), answers())
    expect(onBox).toEqual(paused)
  })

  it('start sends exactly what resume sends', async () => {
    const answers = (): { body: unknown }[] => [{ body: READY_DETAIL }]
    const resumed = await requests((zb) => zb.sandboxes.resume('sbx_1'), answers())
    const started = await requests((zb) => zb.sandboxes.start('sbx_1'), answers())
    expect(started).toEqual(resumed)
    expect(resumed.map((request) => `${request.method} ${request.path}`)).toEqual([
      'POST /sandbox/sbx_1/resume'
    ])
    const onBox = await requests((zb) => zb.sandbox('sbx_1').start(), answers())
    expect(onBox).toEqual(resumed)
  })

  it('a pause answered 202 waits for paused and updates the box', async () => {
    const sent: SentRequest[] = []
    const zb = new Zipbox({
      apiKey: API_KEY,
      fetch: scripted([{ status: 202, body: SETTLING }, { body: PAUSED }], sent)
    })
    const box = zb.sandbox('sbx_1')
    const detail = await box.pause()
    expect(detail.status).toBe('paused')
    expect(box.status).toBe('paused')
    expect(sent.map((request) => request.path)).toEqual([
      '/sandbox/sbx_1/pause',
      '/sandbox/sbx_1/wait?until=paused'
    ])
  })

  it('a resume answered with the ready box makes no further request and keeps its token', async () => {
    const sent: SentRequest[] = []
    const zb = new Zipbox({
      apiKey: API_KEY,
      fetch: scripted([{ body: { ...READY_DETAIL, boxToken: 'bt_resumed' } }], sent)
    })
    const box = await zb.sandboxes.resume('sbx_1')
    await box.exec('true')
    expect(sent.map((request) => `${request.method} ${request.path}`)).toEqual([
      'POST /sandbox/sbx_1/resume',
      'POST /exec'
    ])
    expect(sent.at(-1)?.authorization).toBe('Bearer bt_resumed')
  })
})
