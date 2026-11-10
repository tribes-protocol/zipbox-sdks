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

test('/zipbox preview uses the custom apex of a platform transport host', async ($, on) => {
  const execs: unknown[] = []
  fakeZipbox(on, {
    boxOrigin: 'https://box--dev.zbox.sh',
    box: (_method, path, body) => {
      if (path === '/exec') execs.push(body.argv)
      return undefined
    }
  })
  await start($)
  await zipbox($, 'use dev')
  expect((await zipbox($, 'preview 3000')).text).toContain('https://p3000.dev.zbox.sh')
  expect(execs).toContainEqual([
    'tribes-caddy',
    'add',
    'p3000.dev.zbox.sh',
    '127.0.0.1:3000',
    '--no-wait'
  ])
})

// A private box serves itself on its own box.<apex>; previews still live under the apex.
test('/zipbox preview uses the custom apex of a private box own host', async ($, on) => {
  const execs: unknown[] = []
  fakeZipbox(on, {
    boxOrigin: 'https://box.dev.zbox.sh',
    box: (_method, path, body) => {
      if (path === '/exec') execs.push(body.argv)
      return undefined
    }
  })
  await start($)
  await zipbox($, 'use dev')
  expect((await zipbox($, 'preview 3000')).text).toContain('https://p3000.dev.zbox.sh')
  expect(execs).toContainEqual([
    'tribes-caddy',
    'add',
    'p3000.dev.zbox.sh',
    '127.0.0.1:3000',
    '--no-wait'
  ])
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

test('a private box this computer can reach is probed once, then used', async ($, on) => {
  const zb = fakeZipbox(on, { privateBox: 'answers' })
  await start($)
  await zipbox($, 'use dev')
  expect((await zipbox($, 'run ls')).text).toBe('ran ls\nexit 0')
  expect((await zipbox($, 'run ls')).text).toBe('ran ls\nexit 0')
  expect(zb.asked.filter((line) => line.includes('/challenge'))).toEqual([
    'GET https://dev.zbox.sh/challenge'
  ])
})

// AX-20: a failed TLS handshake is an answer. The box is reachable from here and only
// lacks its certificate yet, so it is not "off your tailnet".
test('a private box whose certificate has not landed counts as reached', async ($, on) => {
  const zb = fakeZipbox(on, { privateBox: 'tls' })
  await start($)
  await zipbox($, 'use dev')
  expect((await zipbox($, 'run ls')).text).toBe('ran ls\nexit 0')
  expect(zb.asked.filter((line) => line.includes('/challenge'))).toHaveLength(1)
})

test('a private box off this tailnet says so instead of failing to connect', async ($, on) => {
  const zb = fakeZipbox(on, { privateBox: 'refused' })
  await start($)
  await zipbox($, 'use dev')
  expect((await zipbox($, 'run ls')).text).toBe(
    'Zipbox: dev is private: it can only be reached from devices on your tailnet. ' +
      'Turn on Tailscale on this computer (https://tailscale.com/download).'
  )
  expect(zb.asked.some((line) => line.includes('/exec'))).toBe(false)
})

test('a private box that never answers fails after 5 seconds, not before', async ($, on) => {
  const zb = fakeZipbox(on, { privateBox: 'silent' })
  await start($)
  await zipbox($, 'use dev')
  let text: string | null = null
  const pending = zipbox($, 'run ls').then((answer) => {
    text = answer.text
  })
  await zb.clock.advance(4_999)
  expect(text).toBeNull()
  await zb.clock.advance(1)
  await pending
  expect(text).toContain('dev is private')
  expect(zb.asked.some((line) => line.includes('/exec'))).toBe(false)
})

// A created box's address moves once, to box--<name>, minutes after create. The old
// one refuses: the window reads the box again, once, and runs the command where it moved.
test('a box whose address moved is read again, and the command runs there', async ($, on) => {
  const zb = fakeZipbox(on)
  await start($)
  await zipbox($, 'use dev')
  expect((await zipbox($, 'run ls')).text).toBe('ran ls\nexit 0')
  zb.moveBox('https://box--dev.zbox.sh')
  expect((await zipbox($, 'run ls')).text).toBe('ran ls\nexit 0')
  expect(zb.asked.filter((line) => line.startsWith('POST') && line.includes('/exec'))).toEqual([
    'POST https://dev.zbox.sh/exec Bearer bt-1',
    'POST https://dev.zbox.sh/exec Bearer bt-1',
    'POST https://box--dev.zbox.sh/exec Bearer bt-1'
  ])
})
