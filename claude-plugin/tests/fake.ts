import type { On } from 'claude-code'
import { mock, type TestBody } from 'claude-code/testing'

// Stands in for Zipbox (the control plane and one running box) and for the
// engine beneath the plugin, and records what reaches them.

export const KEY = `zb_${'a'.repeat(43)}`
export const PI_FILE =
  '/root/.local/share/zipbox/command-center/pi-sessions/t-1/2026-10-26T10-00-00-000Z_pi.jsonl'

/** A task's Pi conversation as the box writes it: a prompt, then a reply with a tool call. */
export const PI_ROWS = [
  { type: 'session', version: 3, id: 'pi', cwd: '/root/workspace' },
  {
    type: 'message',
    message: { role: 'user', content: [{ type: 'text', text: 'fix the login' }] }
  },
  {
    type: 'message',
    message: {
      role: 'assistant',
      content: [
        { type: 'thinking', thinking: 'The cookie is unset.' },
        { type: 'text', text: 'Looking at the login handler.' },
        { type: 'toolCall', id: 'c1', name: 'bash', arguments: { command: 'npm test' } }
      ],
      stopReason: 'toolUse'
    }
  },
  { type: 'message', message: { role: 'toolResult', content: [{ type: 'text', text: 'ok' }] } }
]
  .map((row) => `${JSON.stringify(row)}\n`)
  .join('')

type Answer = { status: number; body: unknown }

export type FakeOptions = {
  boxOrigin?: string
  env?: Record<string, string>
  files?: Record<string, string>
  /** The box's answer to a path, before the defaults. */
  box?: (method: string, path: string, body: Record<string, unknown>) => Answer | undefined
  transcript?: () => string
  /** The followed task's state. */
  state?: () => string
  /** The followed task's Pi session's status. */
  session?: () => string
  /** Programs this computer does not have. */
  missing?: string[]
  /** Box b-1 is private; how its `/challenge` probe goes from this computer. */
  privateBox?: 'answers' | 'refused' | 'silent'
}

const sse = (stdout: string, exitCode = 0) =>
  [
    'id: 0-0\nevent: start\ndata: {"id":"cmd_1"}',
    `id: 1-0\nevent: stdout\ndata: ${JSON.stringify({ data: stdout })}`,
    `id: 1-0\nevent: exit\ndata: ${JSON.stringify({ state: 'exited', exitCode })}`
  ].join('\n\n')

export function fakeZipbox(on: On, options: FakeOptions = {}) {
  const asked: string[] = []
  const toasts: string[] = []
  const ran: { argv: readonly string[]; stdin?: string }[] = []
  const read: string[] = []
  /** Each read of a task's conversation file: which file, from which byte. */
  const reads: { file: string; offset: number; wait: number }[] = []
  const filled: string[] = []
  /** The prompts this window queued as turns of its own. */
  const submitted: string[] = []
  let token = 'bt-1'
  mock.env(on, options.env ?? { ZIPBOX_API_KEY: KEY })
  const clock = mock.clock(on)
  const session = mock.session(on)
  on('fs.read', (_, e) => {
    read.push(e.path)
    const text = options.files?.[e.path]
    if (text === undefined) throw new Error('no such file')
    return { value: text }
  })
  on('http.fetch', (_, e) => {
    const url = new URL(e.url)
    const method = e.init?.method ?? 'GET'
    const headers = (e.init?.headers ?? {}) as Record<string, string>
    const body = e.init?.body ? (JSON.parse(e.init.body) as Record<string, unknown>) : {}
    const path = `${url.pathname}${url.search}`
    asked.push(`${method} ${url.origin}${path} ${headers.authorization ?? ''}`.trim())
    const answer = (status: number, value: unknown) => ({
      value: {
        status,
        ok: status < 300,
        headers: {},
        text: typeof value === 'string' ? value : JSON.stringify(value)
      }
    })
    if (url.origin === 'https://api.tribes.xyz') {
      if (headers.authorization !== `Bearer ${KEY}`)
        return answer(401, { error: { message: 'no' } })
      if (path === '/sandbox')
        return answer(200, [
          { id: 'b-1', name: 'dev', status: 'running', tier: 's' },
          { id: 'b-2', name: 'old', status: 'stopped', tier: 's' }
        ])
      if (path === '/sandbox/b-1')
        return answer(200, {
          id: 'b-1',
          name: 'dev',
          status: 'running',
          ready: true,
          baseUrl: options.boxOrigin ?? 'https://dev.zbox.sh',
          boxToken: token,
          privateMode: options.privateBox !== undefined
        })
      if (path === '/account/whoami')
        return answer(200, { userId: 'u-1', apiKey: { name: 'CLI', kind: 'cli', last4: 'aaaa' } })
      return answer(404, { error: { message: 'not found' } })
    }
    if (path === '/challenge' && options.privateBox !== undefined) {
      if (options.privateBox === 'refused') throw new Error('connect ECONNREFUSED')
      // Off the tailnet a 100.x address is not routed: the connect never ends.
      if (options.privateBox === 'silent') return new Promise<never>(() => undefined)
      return answer(404, { error: 'no owner yet' })
    }
    if (headers.authorization !== `Bearer ${token}`) return answer(401, { error: 'unauthorized' })
    const own = options.box?.(method, path, body)
    if (own) return answer(own.status, own.body)
    if (path === '/command-center/sessions')
      return answer(200, {
        sessions: [
          {
            id: 's-1',
            title: 'login bug',
            status: options.session?.() ?? 'working',
            taskId: 't-1',
            taskState: 'running'
          },
          { id: 's-2', title: null, firstPrompt: 'tidy the docs', status: 'idle', taskId: null }
        ],
        history: true
      })
    if (path === '/command-center/tasks' && method === 'POST')
      return answer(201, { task: { id: 't-1', state: 'queued', sessionId: 's-1' }, created: true })
    if (path === '/command-center/tasks/t-1')
      return answer(200, {
        id: 't-1',
        prompt: 'fix the login',
        state: options.state?.() ?? 'running',
        sessionId: 's-1',
        updatedAt: 1
      })
    if (path.startsWith('/files?path=') && path.includes('pi-sessions'))
      return answer(200, {
        path: '/x',
        entries: [{ name: PI_FILE.split('/').at(-1), kind: 'file', sizeBytes: 1, modifiedAtMs: 1 }]
      })
    if (path === '/exec') {
      const argv = (body.argv ?? []) as string[]
      if (argv[0] === 'bash' && argv[2]?.includes('tail -c')) {
        // `tail -c +<from> <file> | head -c <limit> | base64 -w0`, byte for byte.
        const from = Number(argv[4]) - 1
        const limit = Number(argv[6])
        reads.push({ file: String(argv[5]), offset: from, wait: Number(argv[7]) })
        const bytes = new TextEncoder().encode(options.transcript?.() ?? PI_ROWS)
        let binary = ''
        for (const byte of bytes.subarray(from, from + limit)) binary += String.fromCharCode(byte)
        return answer(200, sse(btoa(binary)))
      }
      return answer(200, sse(`ran ${String(body.cmd ?? argv.join(' '))}\n`))
    }
    return answer(404, { error: 'not found' })
  })
  on('process.run', (_, e) => {
    ran.push({ argv: e.argv, stdin: e.init?.stdin })
    if (options.missing?.includes(String(e.argv[0]))) throw new Error('ENOENT')
    return {
      value: {
        exitCode: 0,
        stdout: '',
        stderr: '',
        isStdoutTruncated: false,
        isStderrTruncated: false
      }
    }
  })
  on('ui.toast', (_, e) => {
    toasts.push(e.text)
    return { value: undefined }
  })
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  on('prompt.fill', (_, e) => {
    filled.push(e.text)
    return { isFilled: true }
  })
  on('prompt.submit', (_, e) => {
    if (e.origin.kind === 'plugin') submitted.push(e.text)
    return { text: e.text }
  })
  on('turn.start', (_, e) => ({ turnId: e.turnId }))
  on('turn.complete', () => ({ text: '' }))
  // The model, for a turn this window does not draw.
  on('turn.step', async function* (_, e) {
    yield { kind: 'text', index: 0, text: 'from the model' }
    yield { kind: 'stop', stopReason: 'end_turn', usage: null }
    return {
      turnId: e.turnId,
      index: e.index,
      answer: 'from the model',
      toolUses: [],
      stopReason: 'end_turn',
      usage: null
    }
  })
  for (const event of ['command.register', 'ui.open', 'ui.status'] as const)
    on(event, () => ({ value: {} }) as never)
  return {
    asked,
    read,
    reads,
    filled,
    submitted,
    toasts,
    ran,
    clock,
    /** The notes this window drew, as text. */
    notes: () =>
      session.appended().map((row) => (row.message.content as { text?: string }[])[0]?.text ?? ''),
    /** The box hands out a new token, as after a restart: the old one gets 401. */
    rotateToken: () => {
      token = 'bt-2'
    }
  }
}

export const start = ($: Parameters<TestBody>[0]) =>
  $.session.start({ cwd: '/work', surface: 'terminal', isInteractive: true })

export const zipbox = ($: Parameters<TestBody>[0], args: string) =>
  $.command.run({ command: 'zipbox', args } as never)

/** Runs one turn here as the engine does, and hands back the texts it drew. */
export async function turn($: Parameters<TestBody>[0], text: string, turnId: string) {
  await $.turn.start({ text, turnId })
  const texts: string[] = []
  for await (const chunk of $.turn.step({ turnId, index: 0, model: 'opus', messageCount: 1 }))
    if (chunk.kind === 'text') texts.push(chunk.text)
  await $.turn.complete({
    turnId,
    answer: texts.join(''),
    durationMs: 1,
    isAborted: false,
    reason: 'answer'
  } as never)
  return texts
}

export const PANE = {
  plugin: 'zipbox',
  component: 'Pane',
  requestId: 'zipbox',
  props: {
    title: 'Zipbox',
    isFocused: true,
    bodyColumns: 48,
    placement: 'dock',
    scroll: { offset: 0, bodyRows: 30 },
    view: {}
  },
  viewport: { columns: 160, rows: 40, isFullscreen: true }
} as const
