import { expect, test } from 'claude-code/testing'

import { fakeZipbox, PANE, start } from './fake'

for (const surface of ['terminal', 'desktop'] as const) {
  test(`the pane lists the boxes and each running box's agent sessions on ${surface}`, async ($, on) => {
    fakeZipbox(on)
    await start($)
    const ui = await $.ui.mount({ ...PANE, surface })
    await expect(ui.find({ key: 'box-b-1' })).resolves.toBeDefined()
    await expect(ui.find({ key: 'box-b-2' })).resolves.toBeDefined()
    const task = await ui.find({ key: 's-s-1' })
    expect(task?.text).toContain('login bug')
    expect(task?.text).toContain('running')
    // A session with no title shows its first prompt.
    expect((await ui.find({ key: 's-s-2' }))?.text).toContain('tidy the docs')
    await ui.unmount()
  })
}

test('a press on a task follows it here, from its first row', async ($, on) => {
  const zb = fakeZipbox(on)
  await start($)
  const ui = await $.ui.mount({ ...PANE, surface: 'terminal' })
  await ui.press({ key: 's-s-1' })
  await zb.clock.advance(0)
  expect(zb.notes()).toEqual([
    'Following “login bug” on dev. What you type now goes to that task; /zipbox leave ends this.'
  ])
  // Its first prompt is a turn here, which draws the reply.
  expect(zb.submitted).toEqual(['fix the login'])
  // The followed task is lit; after /zipbox leave it is not.
  const lit = async () =>
    JSON.stringify(await ui.find({ key: 'row-s-1' })).includes('userMessageBackground')
  expect(await lit()).toBe(true)
  await $.command.run({ command: 'zipbox', args: 'leave' } as never)
  expect(await lit()).toBe(false)
  await ui.unmount()
})

// The other half, that `ui.close` on the pane ends the timer, has no engine
// call a test can make: the kit's `$` raises no close.
test('the pane reads the boxes again every 10 s while it is open', async ($, on) => {
  const zb = fakeZipbox(on)
  await start($)
  const ui = await $.ui.mount({ ...PANE, surface: 'terminal' })
  await ui.find({ key: 'box-b-1' })
  const lists = () =>
    zb.asked.filter((one) => one.includes(' https://api.tribes.xyz/sandbox ')).length
  const before = lists()
  await zb.clock.advance(20_000)
  expect(lists()).toBe(before + 2)
  await ui.unmount()
})

test('a session that is no task is not followed', async ($, on) => {
  const zb = fakeZipbox(on)
  await start($)
  const ui = await $.ui.mount({ ...PANE, surface: 'terminal' })
  await ui.press({ key: 's-s-2' })
  expect(zb.notes()).toEqual([])
  expect(zb.toasts[0]).toContain('/zipbox open')
  await ui.unmount()
})
