import { isRecord } from './guards.js'
import { resolveBoxPath } from './paths.js'
import type { RawSseEvent } from './sse.js'
import type { ExecBackgroundOptions, ExecEvent, ExecSignal, ExecState } from './types.js'

/** A command: a shell string (run with `bash -lc`) or an argv spawned directly. */
export type Cmd = string | readonly string[]

export interface ExecBody {
  cmd?: string
  argv?: string[]
  cwd?: string
  env?: Record<string, string>
  user?: string
  timeoutMs?: number
  background?: boolean
  stdin?: boolean
}

/** The `POST /exec` body. Only keys the caller set are sent. */
export function execBody(cmd: Cmd, options: ExecBackgroundOptions, background: boolean): ExecBody {
  const body: ExecBody = typeof cmd === 'string' ? { cmd } : { argv: [...cmd] }
  if (options.cwd !== undefined) body.cwd = resolveBoxPath(options.cwd)
  if (options.env !== undefined) body.env = options.env
  if (options.user !== undefined) body.user = options.user
  if (options.timeoutMs !== undefined) body.timeoutMs = options.timeoutMs
  if (background) body.background = true
  if (options.stdin === true) body.stdin = true
  return body
}

const STATES: readonly ExecState[] = ['running', 'exited', 'unknown']
const SIGNALS: readonly ExecSignal[] = ['TERM', 'KILL', 'INT', 'HUP', 'QUIT']

function asState(value: unknown): ExecState {
  return STATES.find((state) => state === value) ?? 'unknown'
}

function asSignal(value: unknown): ExecSignal | null {
  return SIGNALS.find((signal) => signal === value) ?? null
}

function asString(value: unknown): string {
  return typeof value === 'string' ? value : ''
}

/** Convert one wire event into an {@link ExecEvent}; unknown event types are dropped. */
export function toExecEvent(raw: RawSseEvent): ExecEvent | null {
  let data: unknown
  try {
    data = JSON.parse(raw.data)
  } catch {
    return null
  }
  if (!isRecord(data)) return null
  const cursor = raw.id ?? '0-0'
  switch (raw.event) {
    case 'start':
      return { type: 'start', cursor, id: asString(data.id) }
    case 'stdout':
      return { type: 'stdout', cursor, data: asString(data.data) }
    case 'stderr':
      return { type: 'stderr', cursor, data: asString(data.data) }
    case 'gap':
      return {
        type: 'gap',
        cursor,
        stream: data.stream === 'stderr' ? 'stderr' : 'stdout',
        message: asString(data.message)
      }
    case 'exit':
      return {
        type: 'exit',
        cursor,
        state: asState(data.state),
        exitCode: typeof data.exitCode === 'number' ? data.exitCode : null,
        signal: asSignal(data.signal),
        timedOut: data.timedOut === true
      }
    default:
      return null
  }
}
