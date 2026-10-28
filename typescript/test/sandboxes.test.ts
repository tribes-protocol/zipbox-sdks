import { describe, expect, it } from 'vitest'

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
