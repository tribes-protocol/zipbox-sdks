import { inspect } from 'node:util'

import { describe, expect, it } from 'vitest'

import { Zipbox, ZipboxError } from '../src/index.js'

const detail = {
  id: 'sbx_1',
  status: 'running',
  createdAt: '2026-10-19T00:00:00.000Z',
  updatedAt: '2026-10-19T00:00:00.000Z',
  ready: true,
  baseUrl: 'https://demo.zbox.sh',
  boxToken: 'bt_1'
}

function sse(text: string): Response {
  return new Response(text, { headers: { 'content-type': 'text/event-stream' } })
}

describe('exec', () => {
  it('resumes from the last cursor when the stream drops before exit', async () => {
    const calls: { url: string; init: RequestInit | undefined }[] = []
    const responses = [
      new Response(JSON.stringify(detail)),
      sse(
        'id: 0-0\nevent: start\ndata: {"id":"cmd_1"}\n\nid: 3-0\nevent: stdout\ndata: {"data":"ab\\n"}\n\n'
      ),
      sse(
        'id: 6-0\nevent: stdout\ndata: {"data":"cd\\n"}\n\nid: 6-0\nevent: exit\ndata: {"state":"exited","exitCode":0,"signal":null,"timedOut":false}\n\n'
      )
    ]
    const zb = new Zipbox({
      apiKey: 'zb_test',
      baseUrl: 'https://api.test',
      retryBaseDelayMs: 1,
      fetch: (url, init) => {
        calls.push({ url, init })
        const next = responses.shift()
        if (next === undefined) throw new Error('unexpected call')
        return Promise.resolve(next)
      }
    })
    const result = await zb
      .sandbox('sbx_1')
      .exec(['make', 'test'], { cwd: 'app', env: { CI: '1' } })
    expect(result).toEqual({ id: 'cmd_1', exitCode: 0, stdout: 'ab\ncd\n', stderr: '' })
    expect(calls[1]?.init?.body).toBe(
      '{"argv":["make","test"],"cwd":"/root/workspace/app","env":{"CI":"1"}}'
    )
    expect(calls[2]?.url).toBe('https://demo.zbox.sh/exec/cmd_1/events?since=3-0')
  })

  it('never sends a deadline unless asked', async () => {
    let body: unknown
    const zb = new Zipbox({
      apiKey: 'zb_test',
      baseUrl: 'https://api.test',
      fetch: (url, init) => {
        if (url.endsWith('/sandbox/sbx_1'))
          return Promise.resolve(new Response(JSON.stringify(detail)))
        body = init?.body
        return Promise.resolve(
          sse(
            'id: 0-0\nevent: start\ndata: {"id":"c"}\n\nid: 0-0\nevent: exit\ndata: {"state":"exited","exitCode":1,"signal":null,"timedOut":false}\n\n'
          )
        )
      }
    })
    const result = await zb.sandbox('sbx_1').exec('false')
    expect(result.exitCode).toBe(1)
    expect(body).toBe('{"cmd":"false"}')
  })

  it('logs() asks journalctl for everything unless lines is set (as the other SDKs)', async () => {
    const bodies: unknown[] = []
    const zb = new Zipbox({
      apiKey: 'zb_test',
      baseUrl: 'https://api.test',
      fetch: (url, init) => {
        if (url.endsWith('/sandbox/sbx_1'))
          return Promise.resolve(new Response(JSON.stringify(detail)))
        bodies.push(init?.body)
        return Promise.resolve(
          sse(
            'id: 0-0\nevent: start\ndata: {"id":"c"}\n\nid: 0-0\nevent: exit\ndata: {"state":"exited","exitCode":0,"signal":null,"timedOut":false}\n\n'
          )
        )
      }
    })
    await zb.sandbox('sbx_1').logs()
    await zb.sandbox('sbx_1').logs({ lines: 50, since: '1 hour ago' })
    expect(bodies[0]).toBe('{"argv":["journalctl","-o","short-iso","--no-pager"]}')
    expect(bodies[1]).toBe(
      '{"argv":["journalctl","-o","short-iso","--no-pager","-n","50","--since","1 hour ago"]}'
    )
  })

  it('follows a command through more consecutive failures than maxRetries (no ceiling)', async () => {
    const urls: string[] = []
    let failuresLeft = 8
    const zb = new Zipbox({
      apiKey: 'zb_test',
      baseUrl: 'https://api.test',
      maxRetries: 0,
      retryBaseDelayMs: 1,
      fetch: (url) => {
        urls.push(url)
        if (url.endsWith('/sandbox/sbx_1')) {
          return Promise.resolve(new Response(JSON.stringify(detail)))
        }
        if (failuresLeft > 0) {
          failuresLeft--
          return Promise.resolve(new Response('{"error":"bad gateway"}', { status: 503 }))
        }
        return Promise.resolve(
          sse(
            'id: 2-0\nevent: stdout\ndata: {"data":"ok"}\n\nid: 2-0\nevent: exit\ndata: {"state":"exited","exitCode":0,"signal":null,"timedOut":false}\n\n'
          )
        )
      }
    })
    const events = []
    for await (const event of zb.sandbox('sbx_1').commands.follow('cmd_1', { since: '0-0' })) {
      events.push(event.type)
    }
    expect(events).toEqual(['stdout', 'exit'])
    expect(urls.filter((url) => url.includes('/events'))).toHaveLength(9)
  })

  it('maps a connection dropped mid-stream to unavailable and resumes from the cursor', async () => {
    const encoder = new TextEncoder()
    let pulls = 0
    const dropping = new ReadableStream<Uint8Array>({
      pull(controller) {
        if (pulls++ > 0) {
          controller.error(new TypeError('terminated'))
          return
        }
        controller.enqueue(
          encoder.encode(
            'id: 0-0\nevent: start\ndata: {"id":"cmd_1"}\n\nid: 3-0\nevent: stdout\ndata: {"data":"ab\\n"}\n\n'
          )
        )
      }
    })
    const urls: string[] = []
    const responses = [
      new Response(JSON.stringify(detail)),
      new Response(dropping, { headers: { 'content-type': 'text/event-stream' } }),
      sse(
        'id: 6-0\nevent: stdout\ndata: {"data":"cd\\n"}\n\nid: 6-0\nevent: exit\ndata: {"state":"exited","exitCode":0,"signal":null,"timedOut":false}\n\n'
      )
    ]
    const zb = new Zipbox({
      apiKey: 'zb_test',
      baseUrl: 'https://api.test',
      retryBaseDelayMs: 1,
      fetch: (url) => {
        urls.push(url)
        const next = responses.shift()
        if (next === undefined) throw new Error('unexpected call')
        return Promise.resolve(next)
      }
    })
    const result = await zb.sandbox('sbx_1').exec('make')
    expect(result.stdout).toBe('ab\ncd\n')
    expect(urls.at(-1)).toBe('https://demo.zbox.sh/exec/cmd_1/events?since=3-0')
  })

  it('a stream drop before start surfaces as a ZipboxError, not a raw fetch error', async () => {
    const dropping = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.error(new TypeError('terminated'))
      }
    })
    const responses = [
      new Response(JSON.stringify(detail)),
      new Response(dropping, { headers: { 'content-type': 'text/event-stream' } })
    ]
    const zb = new Zipbox({
      apiKey: 'zb_test',
      baseUrl: 'https://api.test',
      fetch: () => {
        const next = responses.shift()
        if (next === undefined) throw new Error('unexpected call')
        return Promise.resolve(next)
      }
    })
    const error: unknown = await zb
      .sandbox('sbx_1')
      .exec('true')
      .catch((caught: unknown) => caught)
    expect(error).toBeInstanceOf(ZipboxError)
    expect(error).toMatchObject({ code: 'unavailable', retryable: true })
  })

  it('a box that is running but not ready yet is a retryable box_not_running; a stopped one is not', async () => {
    const run = async (status: string): Promise<unknown> => {
      const zb = new Zipbox({
        apiKey: 'zb_test',
        baseUrl: 'https://api.test',
        fetch: () =>
          Promise.resolve(
            new Response(JSON.stringify({ ...detail, status, ready: false, boxToken: null }))
          )
      })
      return zb
        .sandbox('sbx_1')
        .exec('true')
        .catch((caught: unknown) => caught)
    }
    expect(await run('running')).toMatchObject({ code: 'box_not_running', retryable: true })
    expect(await run('archived')).toMatchObject({ code: 'box_not_running', retryable: false })
  })

  it('never exposes the API key or a box token on a logged or serialized object', async () => {
    const zb = new Zipbox({
      apiKey: 'zb_secretkey',
      baseUrl: 'https://api.test',
      fetch: (url) =>
        Promise.resolve(
          url.endsWith('/sandbox/sbx_1')
            ? new Response(JSON.stringify({ ...detail, boxToken: 'bt_secret' }))
            : sse(
                'id: 0-0\nevent: start\ndata: {"id":"c"}\n\nid: 0-0\nevent: exit\ndata: {"state":"exited","exitCode":0,"signal":null,"timedOut":false}\n\n'
              )
        )
    })
    const box = await zb.sandboxes.get('sbx_1')
    await box.exec('true')
    const dumped = `${inspect(zb, { depth: 20 })}${inspect(box, { depth: 20 })}${JSON.stringify(box)}`
    expect(dumped).not.toContain('zb_secretkey')
    expect(dumped).not.toContain('bt_secret')
    expect(box.info?.boxToken).toBeUndefined()
  })

  it('captures the desktop with the image ffmpeg under pipefail', async () => {
    const bodies: string[] = []
    const zb = new Zipbox({
      apiKey: 'zb_test',
      baseUrl: 'https://api.test',
      fetch: (url, init) => {
        if (url.endsWith('/sandbox/sbx_1'))
          return Promise.resolve(new Response(JSON.stringify(detail)))
        bodies.push(typeof init?.body === 'string' ? init.body : '')
        return Promise.resolve(
          sse(
            'id: 0-0\nevent: start\ndata: {"id":"cmd_1"}\n\nid: 8-0\nevent: stdout\ndata: {"data":"iVBORw=="}\n\nid: 8-0\nevent: exit\ndata: {"state":"exited","exitCode":0,"signal":null,"timedOut":false}\n\n'
          )
        )
      }
    })
    const png = await zb.sandbox('sbx_1').screenshot()
    expect(Array.from(png)).toEqual([0x89, 0x50, 0x4e, 0x47])
    // The platform image has no ImageMagick; pipefail turns a failed capture into a
    // non-zero exit instead of an empty PNG.
    expect(bodies[0]).toMatch(/^\{"cmd":"set -o pipefail;/)
    expect(bodies[0]).toContain('ffmpeg -loglevel error -f x11grab')
    expect(bodies[0]).not.toContain('import ')
  })
})
