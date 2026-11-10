import { describe, expect, it } from 'vitest'

import { Zipbox, ZipboxError } from '../src/index.js'
import type { FetchLike } from '../src/types.js'

const API_KEY = `zb_${'T'.repeat(43)}`
const AUTH_KEY = 'tskey-auth-kForkUnitTest-NotARealKey0123456789'
const AGENT_KEY =
  'MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAENj+qimupfFq/bb5ok8+RD0Bh7Opby1hPK28tSUvvLn58j9QLIPm1e1mfo7z5H/kbmtn7lKcNZp7cT+1Lf5Enog=='
const T0 = '2026-11-05T00:00:00.000Z'

const FORK = {
  operationId: 'fork_1',
  sourceSandboxId: 'sbx_1',
  childSandboxId: 'sbx_2',
  sourcePolicy: 'pause_and_resume',
  phase: 'queued',
  sourceRecovery: 'pending',
  attempt: 0,
  createdAt: T0,
  updatedAt: T0
}

const CHILD = {
  id: 'sbx_2',
  name: 'demo-2',
  status: 'running',
  privateMode: true,
  agentPublicKey: AGENT_KEY,
  tailscale: null,
  createdAt: T0,
  updatedAt: T0,
  ready: false,
  baseUrl: null,
  boxToken: null
}

interface Sent {
  method: string
  path: string
  key: string | null
  body: string
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' }
  })
}

// A control plane that refuses the first fork POST with a 503, then accepts, and
// answers a private child that is running with its key.
function server(sent: Sent[], failFirst: boolean): FetchLike {
  let posts = 0
  return (input, init) => {
    const path = new URL(input).pathname
    const method = init?.method ?? 'GET'
    const headers = new Headers(init?.headers)
    sent.push({
      method,
      path,
      key: headers.get('idempotency-key'),
      body: typeof init?.body === 'string' ? init.body : ''
    })
    if (method === 'POST' && path === '/sandbox/sbx_1/fork') {
      posts += 1
      if (failFirst && posts === 1) {
        return Promise.resolve(
          json({ error: { code: 'unavailable', message: 'busy', retryable: true } }, 503)
        )
      }
      return Promise.resolve(
        json(
          { operationId: 'fork_1', sandbox: { ...CHILD, status: 'provisioning' }, fork: FORK },
          202
        )
      )
    }
    if (method === 'GET' && path === '/sandbox/sbx_2/fork') return Promise.resolve(json(FORK))
    if (method === 'PUT' && path === '/sandbox/sbx_2/tailscale') {
      return Promise.resolve(json({ pending: true }, 202))
    }
    return Promise.resolve(json(CHILD))
  }
}

function client(fetch: FetchLike): Zipbox {
  return new Zipbox({ apiKey: API_KEY, fetch, retryBaseDelayMs: 1, pollIntervalMs: 1 })
}

describe('Sandboxes.fork', () => {
  it('reuses one Idempotency-Key across retries and mints a new one per call', async () => {
    const sent: Sent[] = []
    const zb = client(server(sent, true))
    await zb.sandboxes.fork('sbx_1', {
      name: 'demo-2',
      sourcePolicy: 'pause_and_resume',
      wait: false
    })
    await zb.sandboxes.fork('sbx_1', {
      name: 'demo-2',
      sourcePolicy: 'pause_and_resume',
      wait: false
    })
    const keys = sent.filter((request) => request.method === 'POST').map((request) => request.key)
    expect(keys).toHaveLength(3)
    expect(keys[0]).toMatch(/^[0-9a-f-]{36}$/)
    expect(keys[1]).toBe(keys[0])
    expect(keys[2]).not.toBe(keys[0])
  })

  it('never sends the Tailscale auth key in the clear and never returns it', async () => {
    const sent: Sent[] = []
    const zb = client(server(sent, false))
    const child = await zb.sandboxes.fork('sbx_1', {
      name: 'demo-2',
      sourcePolicy: 'pause_and_resume',
      private: true,
      tailscaleAuthKey: AUTH_KEY
    })
    expect(sent.map((request) => `${request.method} ${request.path}`)).toEqual([
      'POST /sandbox/sbx_1/fork',
      'GET /sandbox/sbx_2',
      'GET /sandbox/sbx_2',
      'PUT /sandbox/sbx_2/tailscale'
    ])
    expect(JSON.parse(sent[0]?.body ?? '')).toStrictEqual({
      name: 'demo-2',
      sourcePolicy: 'pause_and_resume',
      private: true
    })
    for (const request of sent) expect(request.body).not.toContain(AUTH_KEY)
    expect(JSON.stringify(child)).not.toContain(AUTH_KEY)
    expect(child.id).toBe('sbx_2')
  })

  it('refuses a Tailscale key without the wait, before any request', async () => {
    const sent: Sent[] = []
    const zb = client(server(sent, false))
    const refused = zb.sandboxes.fork('sbx_1', {
      name: 'demo-2',
      sourcePolicy: 'require_paused',
      tailscaleAuthKey: AUTH_KEY,
      wait: false
    })
    await expect(refused).rejects.toBeInstanceOf(ZipboxError)
    await expect(refused).rejects.toMatchObject({ code: 'invalid_request' })
    await refused.catch((error: unknown) => {
      expect(String(error)).not.toContain(AUTH_KEY)
    })
    expect(sent).toEqual([])
  })

  it('forks from a handle and reads its fork status', async () => {
    const sent: Sent[] = []
    const zb = client(server(sent, false))
    const child = await zb
      .sandbox('sbx_1')
      .fork({ name: 'demo-2', sourcePolicy: 'pause_and_resume', wait: false })
    expect(child.status).toBe('provisioning')
    expect(child.info?.fork?.operationId).toBe('fork_1')
    expect(child.ready).toBe(false)
    sent.length = 0
    expect(await child.forkStatus()).toMatchObject({ operationId: 'fork_1', phase: 'queued' })
    expect(sent.map((request) => `${request.method} ${request.path}`)).toEqual([
      'GET /sandbox/sbx_2/fork'
    ])
  })

  // AX-19: a fork error code this SDK does not know yet is still the fork's verdict,
  // raised as `conflict` with the server's message, never the vanished box's not_found.
  it('raises an unknown fork error code as conflict with its message', async () => {
    const sent: Sent[] = []
    const answer = server(sent, false)
    const zb = client((input, init) => {
      const path = new URL(input).pathname
      if (path === '/sandbox/sbx_2') {
        return Promise.resolve(json({ error: { code: 'not_found', message: 'gone' } }, 404))
      }
      if (path === '/sandbox/sbx_2/fork') {
        return Promise.resolve(
          json({
            ...FORK,
            phase: 'failed',
            error: { code: 'copy_quota', message: 'disk full on host' }
          })
        )
      }
      return answer(input, init)
    })
    await expect(
      zb.sandboxes.fork('sbx_1', { name: 'demo-2', sourcePolicy: 'pause_and_resume' })
    ).rejects.toMatchObject({ code: 'conflict', message: 'disk full on host', retryable: false })
  })

  // A fork's verdict is never retryable, even under a code that is retryable by
  // default, as in Python, Go and Rust.
  it('raises a fork verdict with a retryable-by-default code as not retryable', async () => {
    const sent: Sent[] = []
    const answer = server(sent, false)
    const zb = client((input, init) => {
      const path = new URL(input).pathname
      if (path === '/sandbox/sbx_2') {
        return Promise.resolve(json({ error: { code: 'not_found', message: 'gone' } }, 404))
      }
      if (path === '/sandbox/sbx_2/fork') {
        return Promise.resolve(
          json({ ...FORK, phase: 'failed', error: { code: 'unavailable', message: 'host lost' } })
        )
      }
      return answer(input, init)
    })
    await expect(
      zb.sandboxes.fork('sbx_1', { name: 'demo-2', sourcePolicy: 'pause_and_resume' })
    ).rejects.toMatchObject({ code: 'unavailable', retryable: false })
  })

  it('stops only its own wait on abort: the fork is never cancelled or deleted', async () => {
    const sent: Sent[] = []
    const answer = server(sent, false)
    const controller = new AbortController()
    let reads = 0
    // The new box keeps provisioning; the caller gives up after a few reads.
    const zb = client((input, init) => {
      const method = init?.method ?? 'GET'
      if (method === 'GET' && new URL(input).pathname === '/sandbox/sbx_2') {
        sent.push({ method, path: '/sandbox/sbx_2', key: null, body: '' })
        reads += 1
        if (reads === 3) controller.abort()
        return Promise.resolve(json({ ...CHILD, status: 'provisioning' }))
      }
      return answer(input, init)
    })
    await expect(
      zb.sandboxes.fork('sbx_1', {
        name: 'demo-2',
        sourcePolicy: 'pause_and_resume',
        signal: controller.signal
      })
    ).rejects.toThrow()
    const writes = sent.filter((request) => request.method !== 'GET')
    expect(writes.map((request) => `${request.method} ${request.path}`)).toEqual([
      'POST /sandbox/sbx_1/fork'
    ])
  })
})
