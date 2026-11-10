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

  it('a paused box that cannot start now says so instead of "start it"', async () => {
    const paused = {
      ...READY_DETAIL,
      status: 'archived',
      ready: false,
      readiness: 'paused',
      allowedActions: ['delete']
    }
    const error = await client([paused])
      .sandboxes.waitReady('sbx_1')
      .catch((caught: unknown) => caught)
    expect(error).toMatchObject({ code: 'box_not_running', retryable: false })
    expect(String(error)).toMatch(/cannot be started now/)
  })

  it('a status from a later server, with no readiness, keeps waiting', async () => {
    const later = { ...READY_DETAIL, status: 'hibernating', ready: false }
    const box = await client([later, READY_DETAIL]).sandboxes.waitReady('sbx_1')
    expect(box.ready).toBe(true)
  })
})

// A box turns ready ~1 s after it serves; doubling slept up to seconds past it.
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
      fetch: () => {
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
