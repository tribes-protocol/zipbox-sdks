import { expect, test } from 'claude-code/testing'

import { fakeZipbox, PI_ROWS, start, turn, zipbox } from './fake'

const row = (role: string, text: string, stopReason?: string) =>
  `${JSON.stringify({ type: 'message', message: { role, content: [{ type: 'text', text }], stopReason } })}\n`

const REPLY = row('assistant', 'The tests pass.', 'stop')

test('/zipbox new starts a task on the box and follows it', async ($, on) => {
  const created: Record<string, unknown>[] = []
  const zb = fakeZipbox(on, {
    box: (method, path, body) => {
      if (method !== 'POST' || path !== '/command-center/tasks') return undefined
      created.push(body)
      return {
        status: 201,
        body: { task: { id: 't-1', state: 'queued', sessionId: 's-1' }, created: true }
      }
    }
  })
  await start($)
  expect((await zipbox($, 'new dev fix the login')).text).toBe('Started a task on dev.')
  expect(created[0]).toMatchObject({ prompt: 'fix the login', source: 'api', contextRefs: [] })
  expect(zb.notes()[0]).toContain('Following “fix the login” on dev.')
})

test("the task's prompt is a turn here and its reply that turn's answer", async ($, on) => {
  const zb = fakeZipbox(on, { transcript: () => `${PI_ROWS}${REPLY}` })
  await start($)
  await zipbox($, 'new dev fix the login')
  await zb.clock.advance(0)
  expect(zb.submitted).toEqual(['fix the login'])
  expect(await turn($, 'fix the login', 'c-1')).toEqual([
    'Looking at the login handler.\n⏺ bash(npm test)',
    'The tests pass.'
  ])
  // Drawn in the turn, not as notes only the transcript view shows.
  expect(zb.notes()).toHaveLength(1)
  await zb.clock.advance(2_000)
  expect(zb.submitted).toEqual(['fix the login'])
})

test('a turn waits on the box for its next row, and a half-written row waits', async ($, on) => {
  let reads = 0
  const zb = fakeZipbox(on, {
    transcript: () => {
      reads++
      if (reads <= 2) return PI_ROWS
      if (reads === 3) return `${PI_ROWS}${REPLY.slice(0, 20)}`
      return `${PI_ROWS}${REPLY}`
    }
  })
  await start($)
  await zipbox($, 'new dev fix the login')
  await zb.clock.advance(0)
  const drawn = await turn($, 'fix the login', 'c-1')
  expect(drawn.at(-1)).toBe('The tests pass.')
  // The turn's reads wait on the box; the timer's never do.
  expect(zb.reads.map((one) => one.wait)).toEqual([0, 20, 20, 20])
})

test('a reply wider than its characters moves the next read by its bytes', async ($, on) => {
  const first = row('assistant', 'Done — the tests pass ✓ 🎉', 'stop')
  let transcript = `${PI_ROWS}${first}`
  const zb = fakeZipbox(on, { transcript: () => transcript })
  await start($)
  await zipbox($, 'new dev fix the login')
  await zb.clock.advance(0)
  expect((await turn($, 'fix the login', 'c-1')).at(-1)).toBe('Done — the tests pass ✓ 🎉')
  transcript += `${row('user', 'and the docs')}${row('assistant', 'And the docs.', 'stop')}`
  await zb.clock.advance(2_000)
  expect(zb.submitted).toEqual(['fix the login', 'and the docs'])
  expect(zb.reads.at(-1)?.offset).toBe(new TextEncoder().encode(`${PI_ROWS}${first}`).length)
  expect(await turn($, 'and the docs', 'c-2')).toEqual(['And the docs.'])
})

test('a history longer than one read is drawn whole', async ($, on) => {
  const long = 'x'.repeat(20_000)
  const rows = Array.from({ length: 60 }, (_, i) =>
    row('assistant', `${i} ${long}`, i === 59 ? 'stop' : 'toolUse')
  )
  const zb = fakeZipbox(on, { transcript: () => `${row('user', 'go')}${rows.join('')}` })
  await start($)
  await zipbox($, 'new dev go')
  await zb.clock.advance(0)
  const drawn = await turn($, 'go', 'c-1')
  expect(drawn).toHaveLength(60)
  expect(drawn[59]).toBe(`59 ${long}`)
  expect(zb.reads.length).toBeGreaterThan(1)
})

test('a prompt typed while following goes to the task and its turn draws the reply', async ($, on) => {
  const sent: Record<string, unknown>[] = []
  let transcript = `${PI_ROWS}${REPLY}`
  const zb = fakeZipbox(on, {
    transcript: () => transcript,
    box: (_method, path, body) => {
      if (path !== '/command-center/tasks/t-1/messages') return undefined
      sent.push(body)
      transcript += `${row('user', 'and the docs')}${row('assistant', 'Docs updated.', 'stop')}`
      return { status: 200, body: { id: 't-1', state: 'completed' } }
    }
  })
  await start($)
  await zipbox($, 'new dev fix the login')
  await zb.clock.advance(0)
  await turn($, 'fix the login', 'c-1')
  const answer = await $.prompt.submit({
    text: 'and the docs',
    wait: false,
    origin: { kind: 'composer' }
  })
  // Entered as a prompt, so the box clears and nothing reads as dropped.
  expect(answer).toMatchObject({ text: 'and the docs' })
  expect(answer.drop).toBeUndefined()
  expect(sent[0]).toMatchObject({ kind: 'message', text: 'and the docs' })
  expect(await turn($, 'and the docs', 'c-2')).toEqual(['Docs updated.'])

  await zipbox($, 'leave')
  const local = await $.prompt.submit({ text: 'hello', wait: false, origin: { kind: 'composer' } })
  expect(local.drop).toBeUndefined()
  expect(sent).toHaveLength(1)
  expect(await turn($, 'hello', 'c-3')).toEqual(['from the model'])
})

test('a follow-up to a completed task is drawn while its Pi answers', async ($, on) => {
  let transcript = `${PI_ROWS}${REPLY}`
  let reads = 0
  const zb = fakeZipbox(on, {
    transcript: () => {
      reads++
      // The reply comes two waits after the prompt; the task stays completed meanwhile.
      if (reads === 6) transcript += row('assistant', 'Docs updated.', 'stop')
      return transcript
    },
    state: () => 'completed',
    box: (_method, path) => {
      if (path !== '/command-center/tasks/t-1/messages') return undefined
      transcript += row('user', 'and the docs')
      return { status: 200, body: { id: 't-1', state: 'completed' } }
    }
  })
  await start($)
  await zipbox($, 'new dev fix the login')
  await zb.clock.advance(0)
  await turn($, 'fix the login', 'c-1')
  await $.prompt.submit({ text: 'and the docs', wait: false, origin: { kind: 'composer' } })
  expect(await turn($, 'and the docs', 'c-2')).toEqual(['Docs updated.'])
})

test('a completed task whose Pi is idle ends its turn instead of waiting', async ($, on) => {
  let transcript = `${PI_ROWS}${REPLY}`
  const zb = fakeZipbox(on, {
    transcript: () => transcript,
    state: () => 'completed',
    session: () => 'idle',
    box: (_method, path) => {
      if (path !== '/command-center/tasks/t-1/messages') return undefined
      transcript += row('user', 'and the docs')
      return { status: 200, body: { id: 't-1', state: 'completed' } }
    }
  })
  await start($)
  await zipbox($, 'new dev fix the login')
  await zb.clock.advance(0)
  await turn($, 'fix the login', 'c-1')
  await $.prompt.submit({ text: 'and the docs', wait: false, origin: { kind: 'composer' } })
  expect(await turn($, 'and the docs', 'c-2')).toEqual([])
})

test("a prompt of the task queued before /zipbox leave is not the model's", async ($, on) => {
  let transcript = `${PI_ROWS}${REPLY}`
  const zb = fakeZipbox(on, { transcript: () => transcript })
  await start($)
  await zipbox($, 'new dev fix the login')
  await zb.clock.advance(0)
  await turn($, 'fix the login', 'c-1')
  transcript += `${row('user', 'and the docs')}${row('assistant', 'Docs updated.', 'stop')}`
  await zb.clock.advance(2_000)
  expect(zb.submitted).toEqual(['fix the login', 'and the docs'])
  await zipbox($, 'leave')
  expect(await turn($, 'and the docs', 'c-2')).toEqual([])
  expect(await turn($, 'hello', 'c-3')).toEqual(['from the model'])
})

test('a prompt the task does not take goes back to the prompt box', async ($, on) => {
  const zb = fakeZipbox(on, {
    box: (_method, path) => {
      if (path === '/command-center/tasks/t-1/messages')
        return {
          status: 409,
          body: { error: 'task_not_attached', message: "This task's Pi is not running." }
        }
      return undefined
    }
  })
  await start($)
  await zipbox($, 'new dev fix the login')
  await zb.clock.advance(0)
  const answer = await $.prompt.submit({
    text: 'and the docs',
    wait: false,
    origin: { kind: 'composer' }
  })
  expect(answer.drop).toContain("This task's Pi is not running.")
  expect(zb.filled).toEqual(['and the docs'])
})

test('a state change shows after the rows the box wrote before it', async ($, on) => {
  let transcript = `${PI_ROWS}${REPLY}`
  let state = 'running'
  const zb = fakeZipbox(on, { transcript: () => transcript, state: () => state })
  await start($)
  await zipbox($, 'new dev fix the login')
  await zb.clock.advance(0)
  await turn($, 'fix the login', 'c-1')
  transcript += `${row('user', 'and the docs')}${row('assistant', 'Docs updated.', 'stop')}`
  state = 'completed'
  await zb.clock.advance(2_000)
  expect(zb.notes().join('\n')).not.toContain('Task completed.')
  expect(await turn($, 'and the docs', 'c-2')).toEqual(['Docs updated.'])
  await zb.clock.advance(6_000)
  expect(zb.notes().at(-1)).toBe('Task completed.')
})

test('a box that answers 401 is asked for a new token once', async ($, on) => {
  const zb = fakeZipbox(on)
  await start($)
  await zipbox($, 'use dev')
  expect((await zipbox($, 'run echo hi')).text).toBe('ran echo hi\nexit 0')
  zb.rotateToken()
  expect((await zipbox($, 'run echo hi')).text).toBe('ran echo hi\nexit 0')
  expect(
    zb.asked.filter((one) => one.startsWith('GET https://api.tribes.xyz/sandbox/b-1'))
  ).toHaveLength(2)
})

test('a box that stops answering is read again with backoff, never given up on', async ($, on) => {
  let down = false
  const zb = fakeZipbox(on, {
    box: (_method, path) =>
      down && path.startsWith('/command-center/tasks/t-1')
        ? { status: 503, body: { error: 'unavailable' } }
        : undefined
  })
  await start($)
  await zipbox($, 'new dev fix the login')
  await zb.clock.advance(0)
  down = true
  const reads = () => zb.asked.filter((one) => one.includes('/command-center/tasks/t-1 ')).length
  const before = reads()
  await zb.clock.advance(60_000)
  const during = reads() - before
  // Backed off: far fewer reads than one every 2 s, and still reading.
  expect(during).toBeGreaterThan(1)
  expect(during).toBeLessThan(15)
  down = false
  await zb.clock.advance(60_000)
  expect(reads() - before - during).toBeGreaterThan(5)
})

test('a deleted key ends following with a note instead of reconnecting', async ($, on) => {
  const zb = fakeZipbox(on)
  await start($)
  await zipbox($, 'new dev fix the login')
  await zb.clock.advance(0)
  zb.deleteKey()
  await zb.clock.advance(2_000)
  expect(zb.notes().at(-1)).toBe(
    'Stopped following “fix the login” on dev: Zipbox refused ZIPBOX_API_KEY.'
  )
  const asked = zb.asked.length
  await zb.clock.advance(60_000)
  expect(zb.asked).toHaveLength(asked)
  expect(await turn($, 'hello', 'c-1')).toEqual(['from the model'])
})

test('a task the box no longer has ends following with a note', async ($, on) => {
  let gone = false
  const zb = fakeZipbox(on, {
    box: (_method, path) =>
      gone && path === '/command-center/tasks/t-1'
        ? { status: 404, body: { error: 'not_found', message: 'No task t-1.' } }
        : undefined
  })
  await start($)
  await zipbox($, 'new dev fix the login')
  await zb.clock.advance(0)
  gone = true
  await zb.clock.advance(2_000)
  expect(zb.notes().at(-1)).toBe('Stopped following “fix the login” on dev: No task t-1.')
  const asked = zb.asked.length
  await zb.clock.advance(60_000)
  expect(zb.asked).toHaveLength(asked)
})

test('a box that has not received a live key yet is read again, not given up on', async ($, on) => {
  let pending = false
  const zb = fakeZipbox(on, {
    box: (_method, path) =>
      pending && path === '/command-center/tasks/t-1'
        ? { status: 401, body: { error: 'unauthorized' } }
        : undefined
  })
  await start($)
  await zipbox($, 'new dev fix the login')
  await zb.clock.advance(0)
  pending = true
  await zb.clock.advance(60_000)
  pending = false
  const reads = () => zb.asked.filter((one) => one.includes('/command-center/tasks/t-1 ')).length
  const before = reads()
  await zb.clock.advance(60_000)
  expect(reads() - before).toBeGreaterThan(5)
  expect(zb.notes().join('\n')).not.toContain('Stopped following')
})
