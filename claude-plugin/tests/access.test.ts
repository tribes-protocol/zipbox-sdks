import { expect, test } from 'claude-code/testing'

import { fakeZipbox, KEY, start, zipbox } from './fake'

const CLI_CONFIG = '/Users/ana/.config/zipbox/config.json'

test('ZIPBOX_API_KEY signs the window in', async ($, on) => {
  const zb = fakeZipbox(on)
  await start($)
  const answer = await zipbox($, 'whoami')
  expect(answer.text).toContain('user  u-1')
  expect(answer.text).toContain('from  ZIPBOX_API_KEY')
  expect(zb.asked).toContain(`GET https://api.tribes.xyz/account/whoami Bearer ${KEY}`)
})

test('without the variable, the key `zipbox login` stored is used', async ($, on) => {
  fakeZipbox(on, {
    env: { HOME: '/Users/ana' },
    files: { [CLI_CONFIG]: JSON.stringify({ apiKey: KEY }) }
  })
  await start($)
  expect((await zipbox($, 'whoami')).text).toContain('from  the zipbox CLI')
})

test('with no key and no home folder, nothing is read and the window says how to sign in', async ($, on) => {
  const zb = fakeZipbox(on, { env: {} })
  await start($)
  expect((await zipbox($, '')).text).toBe('Not signed in. /zipbox login signs in.')
  expect(zb.read).toEqual([])
  expect(zb.asked).toEqual([])
})

test('/zipbox login runs the CLI sign-in and reads the key it stores', async ($, on) => {
  const files: Record<string, string> = {}
  const zb = fakeZipbox(on, { env: { HOME: '/Users/ana' }, files })
  on('process.spawn', async function* (_, e) {
    expect(e.argv).toEqual(['zipbox', 'login'])
    yield {
      stream: 'stderr' as const,
      text: 'Approve this machine in your browser:\n  https://zipbox.ai/cli/auth?port=5000&state=s\n'
    }
    files[CLI_CONFIG] = JSON.stringify({ apiKey: KEY })
    return { value: { code: 0, signal: null } }
  })
  await start($)
  const answer = await zipbox($, 'login')
  expect(answer.text).toBe(
    'Approve this computer in the browser: https://zipbox.ai/cli/auth?port=5000&state=s'
  )
  await zb.clock.advance(10)
  expect(zb.toasts).toContain('Signed in to Zipbox. /zipbox opens the pane.')
  expect((await zipbox($, 'whoami')).text).toContain('user  u-1')
})

test('/zipbox login without the CLI says how to get it', async ($, on) => {
  fakeZipbox(on, { env: { HOME: '/Users/ana' } })
  on('process.spawn', async function* () {
    throw new Error('ENOENT')
  })
  await start($)
  expect((await zipbox($, 'login')).text).toContain('which is not installed here')
})

test('/zipbox logout has the CLI forget the stored key', async ($, on) => {
  const files = { [CLI_CONFIG]: JSON.stringify({ apiKey: KEY }) }
  const zb = fakeZipbox(on, { env: { HOME: '/Users/ana' }, files })
  await start($)
  expect((await zipbox($, 'logout')).text).toContain('Signed out')
  expect(zb.ran).toContainEqual({ argv: ['zipbox', 'logout'], stdin: undefined })
})

test('/zipbox logout without the CLI says the key stays', async ($, on) => {
  fakeZipbox(on, {
    env: { HOME: '/Users/ana' },
    files: { [CLI_CONFIG]: JSON.stringify({ apiKey: KEY }) },
    missing: ['zipbox']
  })
  await start($)
  expect((await zipbox($, 'logout')).text).toContain('Still signed in')
  expect((await zipbox($, 'whoami')).text).toContain('user  u-1')
})
