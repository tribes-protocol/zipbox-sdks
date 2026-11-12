import { describe, expect, it } from 'vitest'

import { Zipbox } from '../src/index.js'
import type { FetchLike } from '../src/types.js'

// Pre-connect on resume (sdks/conformance/README.md, "Pre-connect on resume"): the
// box's last known origin is dialled in parallel with POST /resume, so the first
// box call after it reuses the connection. Never waited for, never retried.

const API_KEY = `zb_${'T'.repeat(43)}`

const DETAIL = {
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
  readiness: 'ready',
  baseUrl: 'https://box--demo.zbox.sh',
  boxToken: 'bt_test'
}
const PAUSED = {
  ...DETAIL,
  status: 'paused',
  ready: false,
  readiness: 'paused',
  baseUrl: null,
  boxToken: null
}
const EXEC_EVENTS =
  'event: start\ndata: {"id":"cmd_1"}\n\n' +
  'event: exit\ndata: {"state":"exited","exitCode":0,"signal":null,"timedOut":false}\n\n'

interface Seen {
  url: string
  method: string
  authorization: string | null
  signal: AbortSignal | null
}

interface Harness {
  zb: Zipbox
  seen: Seen[]
  // Resolves the pending POST /resume with this answer.
  answerResume: (body: unknown) => void
}

function harness(options: { preconnectFails?: boolean } = {}): Harness {
  const seen: Seen[] = []
  let answerResume: (body: unknown) => void = () => undefined
  const fetch: FetchLike = (input, init) => {
    const url = new URL(input)
    const headers = new Headers(init?.headers)
    seen.push({
      url: url.toString(),
      method: init?.method ?? 'GET',
      authorization: headers.get('authorization'),
      signal: init?.signal ?? null
    })
    if (url.pathname === '/healthz') {
      if (options.preconnectFails === true) return Promise.reject(new TypeError('fetch failed'))
      return Promise.resolve(new Response('{"ok":true}', { status: 200 }))
    }
    if (url.pathname === '/sandbox/sbx_1/resume') {
      // Held until the test answers: the pre-connect must already be on its way.
      return new Promise((resolve) => {
        answerResume = (body) => {
          resolve(new Response(JSON.stringify(body), { status: 200 }))
        }
      })
    }
    if (url.pathname === '/sandbox/sbx_1/pause') {
      return Promise.resolve(new Response(JSON.stringify(PAUSED), { status: 200 }))
    }
    if (url.pathname === '/exec') {
      return Promise.resolve(
        new Response(EXEC_EVENTS, { status: 200, headers: { 'content-type': 'text/event-stream' } })
      )
    }
    return Promise.resolve(new Response(JSON.stringify(DETAIL), { status: 200 }))
  }
  const zb = new Zipbox({ apiKey: API_KEY, fetch })
  return {
    zb,
    seen,
    answerResume: (body) => {
      answerResume(body)
    }
  }
}

async function settled(): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, 0))
}

describe('pre-connect on resume', () => {
  it('dials the last known origin while POST /resume is in flight, with no token', async () => {
    const { zb, seen, answerResume } = harness()
    const box = await zb.sandboxes.get('sbx_1')
    await box.pause()

    const resuming = box.resume()
    await settled()
    // Before the resume answered: the pre-connect is already sent, to the box's origin.
    const preconnect = seen.find((request) => request.url === 'https://box--demo.zbox.sh/healthz')
    expect(preconnect).toMatchObject({ method: 'GET', authorization: null })
    answerResume(DETAIL)
    await resuming
    await box.exec('true')

    // The first box call goes to the same origin through the same fetch (its pool).
    const exec = seen.find((request) => request.url === 'https://box--demo.zbox.sh/exec')
    expect(exec?.authorization).toBe('Bearer bt_test')
    expect(preconnect?.signal?.aborted).toBe(false)
  })

  it('the client-level resume and its start alias pre-connect the same way', async () => {
    const { zb, seen, answerResume } = harness()
    await zb.sandboxes.get('sbx_1')
    const resuming = zb.sandboxes.start('sbx_1')
    await settled()
    expect(seen.some((request) => request.url.endsWith('/healthz'))).toBe(true)
    answerResume(DETAIL)
    await resuming
  })

  it('drops the pre-connect when the resume answers on another origin', async () => {
    const { zb, seen, answerResume } = harness()
    const box = await zb.sandboxes.get('sbx_1')
    const resuming = box.resume()
    await settled()
    answerResume({ ...DETAIL, baseUrl: 'https://box.demo.zbox.sh', boxToken: 'bt_moved' })
    await resuming
    await box.exec('true')

    const preconnect = seen.find((request) => request.url.endsWith('/healthz'))
    expect(preconnect?.signal?.aborted).toBe(true)
    expect(seen.at(-1)).toMatchObject({
      url: 'https://box.demo.zbox.sh/exec',
      authorization: 'Bearer bt_moved'
    })
  })

  it('a failed pre-connect changes nothing: the resume and the first call succeed', async () => {
    const { zb, seen, answerResume } = harness({ preconnectFails: true })
    const box = await zb.sandboxes.get('sbx_1')
    const resuming = box.resume()
    await settled()
    answerResume(DETAIL)
    await expect(resuming).resolves.toMatchObject({ status: 'running' })
    await expect(box.exec('true')).resolves.toMatchObject({ exitCode: 0 })
    expect(seen.filter((request) => request.url.endsWith('/healthz'))).toHaveLength(1)
  })

  it('a client that never saw the box ready sends no pre-connect', async () => {
    const { zb, seen, answerResume } = harness()
    const resuming = zb.sandboxes.resume('sbx_1')
    await settled()
    answerResume(DETAIL)
    await resuming
    expect(seen.some((request) => request.url.endsWith('/healthz'))).toBe(false)
  })
})
