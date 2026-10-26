import type { EngineInterface, Timer, TurnStepChunk, TurnStepResult } from 'claude-code'
import { atom, update } from 'claude-code'

import type { Here } from '../types'
import {
  getTask,
  listSessions,
  READ_BYTES,
  readFrom,
  sendMessage,
  type Task,
  transcriptFile
} from './client'

// One window follows one agent task of a box. Each prompt of the task's Pi
// conversation is a turn of this window, and its reply is drawn as this window's
// own: the task runs on the box, and this process draws it and never calls a
// model. What the person types goes to the task.

const here = atom(
  { plugin: 'zipbox', key: 'here' } as const,
  {
    box: null,
    task: null
  } as Here
)

/** One row of the Pi conversation: a prompt, or a reply and the tools it called. */
type Row = { role: 'user' | 'assistant'; text: string; isLast: boolean }

type Following = {
  box: string
  boxName: string
  task: string
  title: string
  state: string | null
  file: string | null
  /** The bytes of the conversation file read so far. */
  offset: number
  /** Rows read and not yet drawn, oldest first. */
  rows: Row[]
  /** The prompts of turns queued here that have not started, oldest first. */
  waiting: string[]
  /** The turn drawing a reply now. */
  turn: string | null
  /** What the task's state changes said, shown once the rows before them are drawn. */
  said: string[]
  /** The read in flight, so two never read the same bytes. */
  reading: Promise<unknown>
  failures: number
  timer: Timer | null
}

let following: Following | null = null
/** Prompts queued as turns of a task no longer followed: each is answered with nothing. */
const dropped: string[] = []
const stale = new Set<string>()

const ACTIVE = new Set(['queued', 'running', 'retrying', 'cancelling'])
/** A Pi that is answering, or one whose status the box does not know. */
const WORKING = new Set(['working', 'running'])
const FAST_MS = 2_000
const IDLE_MS = 6_000
const CAP_MS = 30_000
/** How long one read of a turn waits on the box for the next row. */
const WAIT_SECONDS = 20

/** A line in the transcript that the model never reads. */
export async function note($: EngineInterface, text: string) {
  await $.session
    .append({ message: { type: 'system', content: [{ type: 'text', text }] } })
    .catch(() => {})
}

type PiBlock = { type?: string; text?: string; name?: string; arguments?: Record<string, unknown> }
type PiEntry = {
  type?: string
  message?: { role?: string; content?: string | PiBlock[]; stopReason?: string }
}

function brief(args: Record<string, unknown> | undefined): string {
  const value = args?.command ?? args?.path ?? args?.query ?? args?.url
  const text = typeof value === 'string' ? value : JSON.stringify(args ?? {})
  const line = text.split('\n')[0] ?? ''
  return line.length > 120 ? `${line.slice(0, 120)}…` : line
}

/** One row of a Pi conversation: a prompt, or a reply and the tools it called. */
function piRow(entry: PiEntry): Row | null {
  if (entry.type !== 'message' || !entry.message) return null
  const { role, content, stopReason } = entry.message
  const blocks = typeof content === 'string' ? [{ type: 'text', text: content }] : (content ?? [])
  const text = blocks
    .filter((block) => block.type === 'text' && block.text)
    .map((block) => block.text)
    .join('\n')
    .trim()
  if (role === 'user') return text ? { role, text, isLast: false } : null
  if (role !== 'assistant') return null
  const calls = blocks
    .filter((block) => block.type === 'toolCall')
    .map((block) => `⏺ ${block.name ?? 'tool'}(${brief(block.arguments)})`)
  // Pi's turn goes on only after a reply that called tools.
  const isLast = stopReason !== undefined && stopReason !== 'toolUse'
  return { role, text: [text, ...calls].filter(Boolean).join('\n'), isLast }
}

/**
 * The rows in a piece of a Pi conversation file (JSON lines), and how many of its
 * bytes they used: a last line still being written waits for the next read.
 */
export function piRows(chunk: Uint8Array): { rows: Row[]; consumed: number } {
  const end = chunk.lastIndexOf(0x0a)
  if (end < 0) return { rows: [], consumed: 0 }
  const consumed = end + 1
  const rows: Row[] = []
  for (const line of new TextDecoder().decode(chunk.subarray(0, consumed)).split('\n')) {
    if (!line.trim()) continue
    try {
      const row = piRow(JSON.parse(line) as PiEntry)
      if (row) rows.push(row)
    } catch {
      // A line Pi did not write as JSON is not a row of the conversation.
    }
  }
  return { rows, consumed }
}

/**
 * Reads what the box wrote past the bytes read so far into `f.rows`. A read that
 * came back full may have left more behind, so it reads on until one does not.
 */
function read($: EngineInterface, f: Following, waitSeconds: number): Promise<void> {
  const run = f.reading.then(async () => {
    f.file ??= await transcriptFile($, f.box, f.task)
    let wait = waitSeconds
    let more = !!f.file
    while (more && f.file && following === f) {
      const bytes = await readFrom($, f.box, f.file, f.offset, wait)
      const { rows, consumed } = piRows(bytes)
      f.offset += consumed
      f.rows.push(...rows)
      more = bytes.length === READ_BYTES && consumed > 0
      wait = 0
    }
  })
  f.reading = run.catch(() => {})
  return run
}

/** What a task's new state says in the transcript. */
function stateNote(task: Task): string | null {
  switch (task.state) {
    case 'completed':
      return `Task completed.${task.outcomeNote ? ` ${task.outcomeNote}` : ''}`
    case 'failed':
      return `Task failed: ${task.failure?.message ?? 'no reason given'}`
    case 'cancelled':
      return `Task cancelled.${task.outcomeNote ? ` ${task.outcomeNote}` : ''}`
    case 'interrupted':
      return 'Task interrupted. /zipbox resume continues it.'
    default:
      return null
  }
}

function status($: EngineInterface, f: Following, extra = '') {
  $.ui.status([f.boxName, f.title, f.state ?? '…', extra].filter(Boolean).join(' · '))
}

// The box's event socket needs the machine's browser ownership, which a box token
// never holds, and $.http.fetch reads whole bodies, so this window reads the task
// on a timer while it follows one: every 2 s while the agent works, 6 s otherwise.
// A failure backs off exponentially with jitter, capped at 30 s, and never gives
// up (AGENTS.md §0.0): only /zipbox leave or another task ends it.
function schedule($: EngineInterface, f: Following, ms: number) {
  if (following !== f) return
  f.timer = $.clock.after(ms, () => void tick($, f))
}

/** No turn of the task is queued or drawing here. */
const isIdle = (f: Following) => f.turn === null && f.waiting.length === 0

async function tick($: EngineInterface, f: Following) {
  if (following !== f) return
  try {
    const task = await getTask($, f.box, f.task)
    if (following !== f) return
    if (task.state !== f.state) {
      const said = f.state === null ? null : stateNote(task)
      f.state = task.state
      if (said) f.said.push(said)
    }
    // While a turn draws or waits to, its rows are its own to read.
    if (isIdle(f)) await read($, f, 0)
    if (following !== f) return
    if (isIdle(f)) {
      // Rows of a turn no longer drawn here (it was stopped) show as notes.
      while (f.rows[0] && f.rows[0].role !== 'user') await note($, f.rows.shift()?.text ?? '')
      // Each prompt the box took from elsewhere is a turn here, which draws its reply.
      for (const row of f.rows) {
        if (row.role !== 'user') continue
        f.waiting.push(row.text)
        void $.prompt.submit({ text: row.text, asUser: true }).catch(() => {
          f.waiting.splice(f.waiting.indexOf(row.text), 1)
        })
      }
    }
    // The state changes show after the rows before them.
    if (isIdle(f)) for (const said of f.said.splice(0)) await note($, said)
    f.failures = 0
    status($, f)
    schedule($, f, ACTIVE.has(f.state ?? '') ? FAST_MS : IDLE_MS)
  } catch {
    f.failures++
    status($, f, 'reconnecting')
    const backoff = Math.min(CAP_MS, FAST_MS * 2 ** f.failures)
    schedule($, f, backoff / 2 + Math.random() * (backoff / 2))
  }
}

/** Follows a task here from its first row. */
export async function follow(
  $: EngineInterface,
  target: { box: string; boxName: string; task: string; title: string }
) {
  await leave($)
  const f: Following = {
    ...target,
    state: null,
    file: null,
    offset: 0,
    rows: [],
    waiting: [],
    turn: null,
    said: [],
    reading: Promise.resolve(),
    failures: 0,
    timer: null
  }
  following = f
  await update($, here, () => ({ box: target.box, task: target.task }))
  await note(
    $,
    `Following “${target.title}” on ${target.boxName}. What you type now goes to that task; /zipbox leave ends this.`
  )
  status($, f)
  // From a timer: a command's hook holds the turn the first prompt would queue behind.
  schedule($, f, 0)
}

/** Stops following: prompts go to Claude Code's own model again. */
export async function leave($: EngineInterface): Promise<boolean> {
  if (!following) return false
  following.timer?.cancel()
  dropped.push(...following.waiting)
  following = null
  $.ui.status(undefined)
  await update($, here, (last) => ({ box: last?.box ?? null, task: null }))
  return true
}

export function followed(): { box: string; task: string } | null {
  return following && { box: following.box, task: following.task }
}

/**
 * Sends a prompt typed here to the followed task. Its turn here then draws the
 * task's reply.
 */
export async function send($: EngineInterface, text: string) {
  const f = following
  if (!f) throw new Error('not following a task')
  if (!f.file) throw new Error('the task has not started yet; send it again in a moment')
  f.waiting.push(text)
  try {
    await sendMessage($, f.box, f.task, text)
  } catch (error) {
    f.waiting.splice(f.waiting.indexOf(text), 1)
    throw error
  }
}

/** Whether a turn starting here draws a prompt of the followed task. */
export function startTurn(turnId: string, text: string): boolean {
  const left = dropped.indexOf(text)
  if (left >= 0) {
    dropped.splice(left, 1)
    stale.add(turnId)
    return true
  }
  const at = following?.waiting.indexOf(text) ?? -1
  if (!following || at < 0) return false
  following.waiting.splice(at, 1)
  following.turn = turnId
  return true
}

export function endTurn(turnId: string) {
  stale.delete(turnId)
  if (following?.turn === turnId) following.turn = null
}

export const isDrawing = (turnId: string) => stale.has(turnId) || following?.turn === turnId

/** Whether the task's Pi is still answering: no reply row comes from one that is not. */
async function answering($: EngineInterface, f: Following): Promise<boolean> {
  const task = await getTask($, f.box, f.task)
  if (ACTIVE.has(task.state)) return true
  const session = (await listSessions($, f.box)).find((one) => one.id === task.sessionId)
  return !!session && WORKING.has(session.status)
}

/**
 * Draws the reply to the turn's prompt as this window's model would: each row as
 * the box writes it, until Pi's turn ends, the task stops, or another prompt comes.
 */
export async function* drawReply(
  $: EngineInterface,
  e: { turnId: string; index: number },
  signal: AbortSignal
): AsyncGenerator<TurnStepChunk, TurnStepResult> {
  const f = following
  let answer = ''
  let index = 0
  let prompted = false
  try {
    while (f && following === f && f.turn === e.turnId && !signal.aborted) {
      const row = f.rows[0]
      if (row) {
        // The next prompt is a turn of its own.
        if (row.role === 'user' && prompted) break
        f.rows.shift()
        if (row.role === 'user') {
          prompted = true
          continue
        }
        if (row.text) {
          answer += `${answer ? '\n\n' : ''}${row.text}`
          yield { kind: 'text', index: index++, text: row.text }
        }
        if (row.isLast) break
        continue
      }
      await read($, f, WAIT_SECONDS)
      if (f.rows.length > 0) continue
      // A Pi that stopped writes no more rows; a completed task's Pi answers a
      // follow-up while the task stays completed.
      if (prompted && !(await answering($, f))) break
    }
  } catch (error) {
    // The person's interrupt cancels the read in flight: nothing was lost.
    if (!signal.aborted) {
      const text = `Lost the task on ${f?.boxName ?? 'the box'}: ${(error as Error).message}`
      answer += `${answer ? '\n\n' : ''}${text}`
      yield { kind: 'text', index: index++, text }
    }
  }
  yield { kind: 'stop', stopReason: 'end_turn', usage: null }
  return {
    turnId: e.turnId,
    index: e.index,
    answer,
    toolUses: [],
    stopReason: 'end_turn',
    usage: null
  }
}
