import { readFileSync } from 'node:fs'
import { createServer, type Server } from 'node:http'
import { join } from 'node:path'

import { afterEach, describe, expect, it, vi } from 'vitest'

import { Zipbox, ZipboxError } from '../src/index.js'
import type { FetchLike } from '../src/types.js'

const parsed: unknown = JSON.parse(
  readFileSync(
    join(import.meta.dirname, '..', '..', 'conformance', 'vectors', 'box_envelope.json'),
    'utf8'
  )
)
function agentKeyOf(value: unknown): string {
  const recipient: unknown =
    typeof value === 'object' && value !== null ? Reflect.get(value, 'recipient') : null
  const key: unknown =
    typeof recipient === 'object' && recipient !== null
      ? Reflect.get(recipient, 'agentPublicKey')
      : null
  if (typeof key !== 'string') throw new Error('bad vector')
  return key
}
const AGENT_KEY = agentKeyOf(parsed)
const AUTH_KEY = 'tskey-auth-kSecret1CNTRL-DoNotLeakThisValueAnywhere0123456789'

function json(body: unknown, status = 200): Promise<Response> {
  return Promise.resolve(
    new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } })
  )
}

function detail(extra: Record<string, unknown>): Record<string, unknown> {
  return {
    id: 'sbx_1',
    name: 'build',
    status: 'running',
    ready: true,
    baseUrl: 'https://box--build.zbox.sh',
    boxToken: 'bt_test',
    agentPublicKey: AGENT_KEY,
    ...extra
  }
}

describe('box.tailscale.connect', () => {
  it('sends only the sealed envelope, never the auth key, and keeps it out of errors', async () => {
    const bodies: string[] = []
    const fetch: FetchLike = (url, init) => {
      if (url.endsWith('/sandbox/sbx_1')) return json(detail({}))
      bodies.push(typeof init?.body === 'string' ? init.body : '')
      return json({ error: 'Box not running', code: 'box_not_running' }, 409)
    }
    const zb = new Zipbox({ apiKey: 'zb_test', baseUrl: 'https://api.test', fetch })
    const error: unknown = await zb
      .sandbox('sbx_1')
      .tailscale.connect(AUTH_KEY)
      .catch((caught: unknown) => caught)
    expect(error).toBeInstanceOf(ZipboxError)
    expect(bodies).toHaveLength(1)
    expect(bodies[0]).not.toContain(AUTH_KEY)
    expect(bodies[0]).not.toContain('tskey')
    expect(JSON.parse(bodies[0] ?? '')).toMatchObject({ agentPublicKey: AGENT_KEY })
    const seen = error instanceof Error ? `${error.message} ${JSON.stringify(error)}` : ''
    expect(seen).not.toContain(AUTH_KEY)
  })

  it('refuses an empty key without a request', async () => {
    let calls = 0
    const fetch: FetchLike = () => {
      calls++
      return json({})
    }
    const zb = new Zipbox({ apiKey: 'zb_test', baseUrl: 'https://api.test', fetch })
    await expect(zb.sandbox('sbx_1').tailscale.connect('  ')).rejects.toMatchObject({
      code: 'invalid_request'
    })
    expect(calls).toBe(0)
  })
})

describe('private boxes', () => {
  let server: Server | null = null

  afterEach(async () => {
    await new Promise<void>((resolve) => {
      if (!server?.listening) {
        resolve()
        return
      }
      server.close(() => {
        resolve()
      })
    })
    server = null
  })

  async function listen(handler: Parameters<typeof createServer>[1]): Promise<string> {
    const started = createServer(handler)
    server = started
    await new Promise<void>((resolve) => started.listen(0, '127.0.0.1', resolve))
    const address = started.address()
    if (address === null || typeof address === 'string') throw new Error('no address')
    return `http://127.0.0.1:${address.port}`
  }

  function client(boxUrl: string): { zb: Zipbox; boxCalls: string[] } {
    const boxCalls: string[] = []
    const fetch: FetchLike = (url, init) => {
      if (url === 'https://api.test/sandbox/sbx_1') {
        return json(detail({ baseUrl: boxUrl, privateMode: true }))
      }
      boxCalls.push(url)
      return globalThis.fetch(url, init)
    }
    return { zb: new Zipbox({ apiKey: 'zb_test', baseUrl: 'https://api.test', fetch }), boxCalls }
  }

  it('fails fast with box_private_unreachable when this device is off the tailnet', async () => {
    // A port that was open a moment ago and is now closed: nothing answers there.
    const closed = await listen(() => undefined)
    await new Promise<void>((resolve) => {
      server?.close(() => {
        resolve()
      })
    })
    const { zb, boxCalls } = client(closed)
    const error: unknown = await zb
      .sandbox('sbx_1')
      .exec('true')
      .catch((caught: unknown) => caught)
    expect(error).toBeInstanceOf(ZipboxError)
    expect(error).toMatchObject({ code: 'box_private_unreachable', retryable: false })
    expect(error instanceof Error ? error.message : '').toContain('build is private')
    // Only the probe went out; exec itself was never attempted.
    expect(boxCalls).toEqual([`${closed}/challenge`])
  })

  it('probes once per box and then talks to it', async () => {
    const paths: string[] = []
    const base = await listen((req, res) => {
      paths.push(req.url ?? '')
      if (req.url === '/challenge') {
        // The probe needs an answer, not a success.
        res.writeHead(404).end()
        return
      }
      res
        .writeHead(200, { 'content-type': 'text/event-stream' })
        .end(
          'id: 0-0\nevent: start\ndata: {"id":"cmd_1"}\n\n' +
            'id: 0-0\nevent: exit\ndata: {"state":"exited","exitCode":0}\n\n'
        )
    })
    const { zb } = client(base)
    const box = zb.sandbox('sbx_1')
    expect((await box.exec('true')).exitCode).toBe(0)
    expect((await box.exec('true')).exitCode).toBe(0)
    expect(paths).toEqual(['/challenge', '/exec', '/exec'])
  })
})

describe('private probe deadline', () => {
  afterEach(() => {
    vi.useRealTimers()
  })

  it('gives up after 5 s on a box that never answers, and not before', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    const probes: string[] = []
    const fetch: FetchLike = (url, init) => {
      if (url === 'https://api.test/sandbox/sbx_1') return json(detail({ privateMode: true }))
      probes.push(url)
      // A 100.x address off the tailnet: the connect never completes; only the
      // deadline ends it.
      return new Promise<Response>((_resolve, reject) => {
        init?.signal?.addEventListener('abort', () => {
          reject(new Error('aborted'))
        })
      })
    }
    const zb = new Zipbox({ apiKey: 'zb_test', baseUrl: 'https://api.test', fetch })
    let settled: unknown = null
    const pending = zb
      .sandbox('sbx_1')
      .exec('true')
      .then(
        () => {
          settled = 'resolved'
        },
        (caught: unknown) => {
          settled = caught
        }
      )
    await vi.advanceTimersByTimeAsync(4_999)
    expect(settled).toBeNull()
    await vi.advanceTimersByTimeAsync(1)
    await pending
    expect(settled).toBeInstanceOf(ZipboxError)
    expect(settled).toMatchObject({ code: 'box_private_unreachable', retryable: false })
    expect(probes).toEqual(['https://box--build.zbox.sh/challenge'])
  })
})

describe('sandboxes.getOrCreate on a private box', () => {
  const ROW = {
    id: 'sbx_1',
    name: 'build',
    status: 'running',
    privateMode: true,
    createdAt: '2026-10-19T00:00:00.000Z',
    updatedAt: '2026-10-19T00:00:00.000Z'
  }

  function client(
    row: Record<string, unknown>,
    details: Record<string, unknown>[]
  ): { zb: Zipbox; detailReads: () => number } {
    let reads = 0
    const fetch: FetchLike = (url) => {
      if (url === 'https://api.test/sandbox') return json([row])
      if (url === 'https://api.test/sandbox/sbx_1') {
        const next = details[Math.min(reads, details.length - 1)] ?? {}
        reads++
        return json(next)
      }
      throw new Error(`unexpected ${url}`)
    }
    return {
      zb: new Zipbox({ apiKey: 'zb_test', baseUrl: 'https://api.test', fetch, pollIntervalMs: 1 }),
      detailReads: () => reads
    }
  }

  const UNJOINED = { ready: false, baseUrl: null, boxToken: null, privateMode: true }

  it('returns once a box that has not joined yet is running with its key', async () => {
    const { zb, detailReads } = client({ ...ROW, tailscale: null }, [
      detail({ ...UNJOINED, agentPublicKey: null }),
      detail(UNJOINED)
    ])
    const box = await zb.sandboxes.getOrCreate('build')
    expect(box.ready).toBe(false)
    expect(box.info).toMatchObject({ privateMode: true, agentPublicKey: AGENT_KEY })
    expect(detailReads()).toBe(2)
  })

  it('still waits for ready on a private box that has joined', async () => {
    // The detail carries the tailnet join, as GET /sandbox/:id does.
    const tailscale = { ipv4: '100.101.32.7', pending: false }
    const { zb, detailReads } = client({ ...ROW, tailscale }, [
      detail({ ...UNJOINED, tailscale }),
      detail({ privateMode: true, tailscale })
    ])
    const box = await zb.sandboxes.getOrCreate('build')
    expect(box.ready).toBe(true)
    expect(detailReads()).toBe(2)
  })
})
