import { expect, it, vi } from 'vitest'

import { Zipbox } from '../src/index.js'

it.each([
  ['https://box--demo.zbox.sh', 'demo.zbox.sh'],
  ['https://box--box--demo.zbox.sh', 'box--demo.zbox.sh'],
  // A private box serves itself on box.<apex>; only one prefix is ever dropped.
  ['https://box.demo.zbox.sh', 'demo.zbox.sh'],
  ['https://box--box.zbox.sh', 'box.zbox.sh'],
  ['https://demo.zbox.sh', 'demo.zbox.sh'],
  ['https://mailbox--demo.zbox.sh', 'mailbox--demo.zbox.sh'],
  ['http://127.0.0.1:9100', '127.0.0.1']
])('publishes and removes previews beneath the custom apex of %s', async (origin, apex) => {
  const commands: unknown[] = []
  const client = new Zipbox({
    apiKey: 'zb_test',
    baseUrl: 'https://api.test',
    fetch: (url, init) => {
      if (url === 'https://api.test/sandbox/sbx_1') {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              id: 'sbx_1',
              status: 'running',
              ready: true,
              baseUrl: origin,
              boxToken: 'bt_test'
            })
          )
        )
      }
      expect(url).toBe(`${origin}/exec`)
      commands.push(init?.body)
      return Promise.resolve(
        new Response(
          'id: 0-0\nevent: start\ndata: {"id":"cmd_p"}\n\n' +
            'id: 0-0\nevent: exit\ndata: {"state":"exited","exitCode":0}\n\n',
          { headers: { 'content-type': 'text/event-stream' } }
        )
      )
    }
  })
  const box = client.sandbox('sbx_1')
  expect(await box.preview.create(3000, { label: 'web' })).toEqual({
    url: `https://web.${apex}`,
    label: 'web'
  })
  await box.preview.remove('web')
  expect(commands).toEqual([
    JSON.stringify({ argv: ['tribes-caddy', 'add', `web.${apex}`, '127.0.0.1:3000', '--no-wait'] }),
    JSON.stringify({ argv: ['tribes-caddy', 'rm', `web.${apex}`] })
  ])
})

it('keeps preview pending beyond30 seconds and honors Retry-After without returning a URL', async () => {
  vi.useFakeTimers()
  try {
    let executions = 0
    let ready = false
    const client = new Zipbox({
      apiKey: 'key',
      baseUrl: 'https://api.test',
      fetch: (url, init) => {
        if (url.endsWith('/sandbox/box'))
          return Promise.resolve(
            new Response(
              JSON.stringify({
                id: 'box',
                status: 'running',
                ready: true,
                baseUrl: 'https://box--demo.zbox.sh',
                boxToken: 'bt'
              })
            )
          )
        expect(init?.body).toBe(
          JSON.stringify({
            argv: ['tribes-caddy', 'add', 'web.demo.zbox.sh', '127.0.0.1:4000', '--no-wait']
          })
        )
        const pending = ++executions <= 16
        const output = pending
          ? `event: stdout\ndata: ${JSON.stringify({ data: JSON.stringify({ status: 'pending', retryAfterMs: 31_000 }) })}\n\n`
          : ''
        return Promise.resolve(
          new Response(
            `event: start\ndata: {"id":"p"}\n\n${output}event: exit\ndata: {"state":"exited","exitCode":${pending ? 75 : 0}}\n\n`,
            { headers: { 'content-type': 'text/event-stream' } }
          )
        )
      }
    })
    const result = client
      .sandbox('box')
      .preview.create(4000, { label: 'web' })
      .then((value) => {
        ready = true
        return value
      })
    await vi.advanceTimersByTimeAsync(30_001)
    expect(executions).toBe(1)
    expect(ready).toBe(false)
    await vi.advanceTimersByTimeAsync(31_000 * 16)
    expect((await result).url).toBe('https://web.demo.zbox.sh')
    expect(executions).toBe(17)
  } finally {
    vi.useRealTimers()
  }
})

// audit-ax AX-23: a follower's ladder never retries sooner than the server asked.
it('a followed command waits out Retry-After before it re-attaches', async () => {
  vi.useFakeTimers()
  try {
    const opened: number[] = []
    const client = new Zipbox({
      apiKey: 'key',
      baseUrl: 'https://api.test',
      maxRetries: 0,
      retryBaseDelayMs: 1,
      fetch: (url) => {
        if (url.endsWith('/sandbox/box'))
          return Promise.resolve(
            new Response(
              JSON.stringify({
                id: 'box',
                status: 'running',
                ready: true,
                baseUrl: 'https://box--demo.zbox.sh',
                boxToken: 'bt'
              })
            )
          )
        opened.push(Date.now())
        if (opened.length === 1)
          return Promise.resolve(
            new Response(JSON.stringify({ error: 'busy' }), {
              status: 503,
              headers: { 'retry-after': '30' }
            })
          )
        return Promise.resolve(
          new Response('id: 0-1\nevent: exit\ndata: {"state":"exited","exitCode":0}\n\n', {
            headers: { 'content-type': 'text/event-stream' }
          })
        )
      }
    })
    const events: string[] = []
    const done = (async () => {
      for await (const event of client.sandbox('box').commands.events('cmd_1')) {
        events.push(event.type)
      }
    })()
    await vi.advanceTimersByTimeAsync(29_000)
    expect(opened).toHaveLength(1)
    await vi.advanceTimersByTimeAsync(1_100)
    await done
    expect(opened).toHaveLength(2)
    expect(events).toEqual(['exit'])
  } finally {
    vi.useRealTimers()
  }
})

// audit-ax AX-10: a shared first read is bound to its starter's signal, so a caller
// with a signal reads alone and another caller's abort never ends this one's call.
it("a box call never inherits another caller's abort", async () => {
  let release: () => void = () => undefined
  const gate = new Promise<void>((resolve) => {
    release = resolve
  })
  const client = new Zipbox({
    apiKey: 'key',
    baseUrl: 'https://api.test',
    fetch: async (url, init) => {
      if (url.endsWith('/sandbox/box')) {
        await gate
        if (init?.signal?.aborted === true) throw new DOMException('aborted', 'AbortError')
        return new Response(
          JSON.stringify({
            id: 'box',
            status: 'running',
            ready: true,
            baseUrl: 'https://box--demo.zbox.sh',
            boxToken: 'bt'
          })
        )
      }
      return new Response(
        'id: 0-0\nevent: start\ndata: {"id":"c"}\n\nid: 0-1\nevent: exit\ndata: {"state":"exited","exitCode":0}\n\n',
        { headers: { 'content-type': 'text/event-stream' } }
      )
    }
  })
  const controller = new AbortController()
  const first = client
    .sandbox('box')
    .exec('true', { signal: controller.signal })
    .catch((caught: unknown) => caught)
  const second = client.sandbox('box').exec('true')
  controller.abort()
  release()
  expect(await first).toBeInstanceOf(Error)
  expect((await second).exitCode).toBe(0)
})
