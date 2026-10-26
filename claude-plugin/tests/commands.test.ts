import { expect, test } from 'claude-code/testing'

import { fakeZipbox, start, zipbox } from './fake'

test('with several boxes, a command asks which one, and /zipbox use picks it', async ($, on) => {
  fakeZipbox(on)
  await start($)
  expect((await zipbox($, 'run ls')).text).toContain(
    'Pick a box: /zipbox use <box>. Yours: dev, old.'
  )
  expect((await zipbox($, 'use dev')).text).toBe('Using dev.')
  expect((await zipbox($, 'run ls')).text).toBe('ran ls\nexit 0')
})

test('/zipbox files lists a folder under /root/workspace', async ($, on) => {
  const zb = fakeZipbox(on, {
    box: (_method, path) =>
      path === '/files?path=%2Froot%2Fworkspace%2Fapp'
        ? {
            status: 200,
            body: {
              path: '/root/workspace/app',
              entries: [
                { name: 'src', kind: 'directory', sizeBytes: 0, modifiedAtMs: 1 },
                { name: 'package.json', kind: 'file', sizeBytes: 9, modifiedAtMs: 1 }
              ]
            }
          }
        : undefined
  })
  await start($)
  await zipbox($, 'use dev')
  expect((await zipbox($, 'files app')).text).toBe('/root/workspace/app:\nsrc/\npackage.json')
  expect(zb.asked).toContain(
    'GET https://dev.zbox.sh/files?path=%2Froot%2Fworkspace%2Fapp Bearer bt-1'
  )
})

test('/zipbox preview publishes a port at the box host', async ($, on) => {
  const execs: unknown[] = []
  fakeZipbox(on, {
    box: (_method, path, body) => {
      if (path === '/exec') execs.push(body.argv)
      return undefined
    }
  })
  await start($)
  await zipbox($, 'use dev')
  expect((await zipbox($, 'preview 3000')).text).toContain('https://p3000.dev.zbox.sh')
  expect(execs).toContainEqual(['tribes-caddy', 'add', 'p3000.dev.zbox.sh', '127.0.0.1:3000'])
})

test('/zipbox save copies a box file into this folder', async ($, on) => {
  const zb = fakeZipbox(on)
  await start($)
  await zipbox($, 'use dev')
  expect((await zipbox($, 'save out/report.pdf')).text).toBe('Saved report.pdf.')
  const write = zb.ran.find((one) => one.argv[0] === '/bin/sh')
  expect(write?.argv.at(-1)).toBe('report.pdf')
})

test('/zipbox help lists the actions', async ($, on) => {
  fakeZipbox(on)
  await start($)
  const text = (await zipbox($, 'help')).text
  for (const action of ['login', 'new', 'cancel', 'run', 'files', 'preview', 'screenshot'])
    expect(text).toContain(`/zipbox ${action}`)
})
