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
