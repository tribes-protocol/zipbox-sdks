import type { EngineInterface, HttpResponse } from 'claude-code'

// How this window reaches Zipbox, shared by the plugin's modules: the account key,
// the control plane, and each box's own origin and token (zipbox.ai/docs/box-api).

const API_URL = 'https://api.tribes.xyz'
export const WEB_URL = 'https://zipbox.ai'
/** Where a box keeps each command-center task's Pi conversation, one folder per task. */
const PI_SESSION_ROOT = '/root/.local/share/zipbox/command-center/pi-sessions'
const KEY_PATTERN = /^zb_[A-Za-z0-9_-]{43}$/

export type KeySource = 'env' | 'cli'
type Account = { key: string; source: KeySource; api: string }
type Box = { baseUrl: string; token: string }

let account: Account | null = null
/** Each box's origin and token, read once from the control plane; a 401 reads it again. */
const boxes = new Map<string, Box>()

const isAbsolute = (path: string) => /^(\/|[A-Za-z]:[\\/])/.test(path)

/**
 * Where the `zipbox` CLI may keep its key, most specific first: ZIPBOX_CONFIG_DIR,
 * then Go's user config folder on Windows, macOS and Linux. Only absolute paths, so
 * no key is ever read from a project.
 */
export function cliConfigPaths(env: {
  configDir?: string
  appData?: string
  xdg?: string
  home?: string
}): string[] {
  const dirs = env.configDir
    ? [env.configDir]
    : [
        env.appData && `${env.appData}/zipbox`,
        env.home && `${env.home}/Library/Application Support/zipbox`,
        env.xdg && `${env.xdg}/zipbox`,
        env.home && `${env.home}/.config/zipbox`
      ]
  return dirs
    .filter((dir): dir is string => !!dir && isAbsolute(dir))
    .map((dir) => `${dir}/config.json`)
}

/** The key in a CLI config file, or null. */
export function cliKey(config: string): string | null {
  try {
    const key = (JSON.parse(config) as { apiKey?: unknown }).apiKey
    return typeof key === 'string' && KEY_PATTERN.test(key) ? key : null
  } catch {
    return null
  }
}

/** The CLI's config files for this machine. */
async function cliConfigFiles($: EngineInterface): Promise<string[]> {
  return cliConfigPaths({
    configDir: await $.env.get('ZIPBOX_CONFIG_DIR'),
    appData: await $.env.get('APPDATA'),
    xdg: await $.env.get('XDG_CONFIG_HOME'),
    home: await $.env.get('HOME')
  })
}

/**
 * Reads the key this window uses: ZIPBOX_API_KEY always wins, else the key
 * `zipbox login` stored. Read again on every command, so a login in another
 * terminal needs no restart here.
 */
export async function connect($: EngineInterface): Promise<Account | null> {
  const api = ((await $.env.get('ZIPBOX_API_URL')) || API_URL).replace(/\/+$/, '')
  const env = (await $.env.get('ZIPBOX_API_KEY'))?.trim()
  let next: Account | null = env ? { key: env, source: 'env', api } : null
  for (const file of next ? [] : await cliConfigFiles($)) {
    const key = cliKey(await $.fs.read(file).catch(() => ''))
    if (key) {
      next = { key, source: 'cli', api }
      break
    }
  }
  if (next?.key !== account?.key) boxes.clear()
  account = next
  return account
}

export function signedIn(): Account | null {
  return account
}

export function forget() {
  account = null
  boxes.clear()
}

/** The message a refusal carries, in the control plane's envelope or a box's. */
function reason(res: HttpResponse): string {
  try {
    const body = JSON.parse(res.text) as {
      error?: string | { message?: string }
      message?: string
    }
    const error = typeof body.error === 'string' ? body.error : body.error?.message
    return body.message ?? error ?? `HTTP ${res.status}`
  } catch {
    return `HTTP ${res.status}`
  }
}

export class ZipboxError extends Error {}

/** A control-plane call with the account key. */
export async function api<T>(
  $: EngineInterface,
  path: string,
  init: { method?: string; body?: unknown } = {}
): Promise<T> {
  if (!account) throw new ZipboxError('Not signed in. /zipbox login signs in.')
  const res = await $.http.fetch(`${account.api}${path}`, {
    method: init.method ?? 'GET',
    headers: { authorization: `Bearer ${account.key}`, 'content-type': 'application/json' },
    body: init.body === undefined ? undefined : JSON.stringify(init.body)
  })
  if (res.status === 401)
    throw new ZipboxError(
      account.source === 'env'
        ? 'Zipbox refused ZIPBOX_API_KEY.'
        : 'Zipbox refused the stored key. /zipbox login signs in again.'
    )
  if (!res.ok) throw new ZipboxError(reason(res))
  return JSON.parse(res.text) as T
}

export type BoxInfo = { id: string; name?: string | null; status: string; tier?: string | null }
type BoxDetail = BoxInfo & { ready: boolean; baseUrl?: string | null; boxToken?: string | null }

/** The account's boxes. */
export function listBoxes($: EngineInterface): Promise<BoxInfo[]> {
  return api<BoxInfo[]>($, '/sandbox')
}

async function resolve($: EngineInterface, id: string): Promise<Box> {
  const detail = await api<BoxDetail>($, `/sandbox/${encodeURIComponent(id)}`)
  if (!detail.ready || !detail.baseUrl || !detail.boxToken)
    throw new ZipboxError(`${detail.name ?? id} is not running. Start it at ${WEB_URL}.`)
  const box = { baseUrl: detail.baseUrl.replace(/\/+$/, ''), token: detail.boxToken }
  boxes.set(id, box)
  return box
}

/** The box's HTTPS origin. */
export async function boxOrigin($: EngineInterface, id: string): Promise<string> {
  return (boxes.get(id) ?? (await resolve($, id))).baseUrl
}

/**
 * A call to the box itself with its box token. A 401 reads the token again and
 * retries once, as the SDKs do.
 */
async function boxFetch(
  $: EngineInterface,
  id: string,
  path: string,
  init: { method?: string; body?: unknown; accept?: string } = {}
): Promise<HttpResponse> {
  const send = (box: Box) =>
    $.http.fetch(`${box.baseUrl}${path}`, {
      method: init.method ?? 'GET',
      headers: {
        authorization: `Bearer ${box.token}`,
        'content-type': 'application/json',
        ...(init.accept ? { accept: init.accept } : {})
      },
      body: init.body === undefined ? undefined : JSON.stringify(init.body)
    })
  let res = await send(boxes.get(id) ?? (await resolve($, id)))
  if (res.status === 401) res = await send(await resolve($, id))
  if (!res.ok) throw new ZipboxError(reason(res))
  return res
}

async function boxJson<T>(
  $: EngineInterface,
  id: string,
  path: string,
  init: { method?: string; body?: unknown } = {}
): Promise<T> {
  return JSON.parse((await boxFetch($, id, path, init)).text) as T
}

// ── exec ──────────────────────────────────────────────────────────────────────

export type ExecResult = { stdout: string; stderr: string; exitCode: number | null }

/** Reads the server-sent events of a finished `POST /exec`. */
export function parseExec(stream: string): ExecResult {
  const result: ExecResult = { stdout: '', stderr: '', exitCode: null }
  for (const block of stream.split(/\r?\n\r?\n/)) {
    const event = /^event: (.*)$/m.exec(block)?.[1]
    const data = /^data: (.*)$/m.exec(block)?.[1]
    if (!event || data === undefined) continue
    const parsed = JSON.parse(data) as { data?: string; exitCode?: number | null }
    if (event === 'stdout') result.stdout += parsed.data ?? ''
    if (event === 'stderr') result.stderr += parsed.data ?? ''
    if (event === 'exit') result.exitCode = parsed.exitCode ?? null
  }
  return result
}

/**
 * Runs a command on the box and reads its output once it exits: `cmd` through
 * `bash -lc`, or `argv` with no shell.
 */
export async function exec(
  $: EngineInterface,
  id: string,
  command: { cmd: string } | { argv: string[] }
): Promise<ExecResult> {
  const res = await boxFetch($, id, '/exec', {
    method: 'POST',
    body: command,
    accept: 'text/event-stream'
  })
  return parseExec(res.text)
}

// ── files ─────────────────────────────────────────────────────────────────────

export type FileEntry = { name: string; kind: string; sizeBytes: number; modifiedAtMs: number }

/** A path on the box: absolute, or under /root/workspace. */
export function boxPath(path: string): string {
  const trimmed = path.trim()
  if (trimmed.startsWith('/')) return trimmed
  return trimmed ? `/root/workspace/${trimmed}` : '/root/workspace'
}

export async function listFiles($: EngineInterface, id: string, path: string) {
  return boxJson<{ path: string; entries: FileEntry[]; truncated?: boolean }>(
    $,
    id,
    `/files?path=${encodeURIComponent(path)}`
  )
}

export async function readFile($: EngineInterface, id: string, path: string): Promise<string> {
  return (await boxFetch($, id, `/files/download?path=${encodeURIComponent(path)}`)).text
}

// ── command center: the box's agent sessions and tasks ───────────────────────

export type AgentSession = {
  id: string
  title?: string | null
  firstPrompt?: string | null
  status: string
  taskId?: string | null
  taskState?: string | null
}

export type Task = {
  id: string
  prompt: string
  state: string
  sessionId: string
  updatedAt: number
  outcomeNote?: string | null
  failure?: { reason: string; message: string } | null
}

/** The box's agent sessions, most recent first (the first page). */
export async function listSessions($: EngineInterface, id: string): Promise<AgentSession[]> {
  return (await boxJson<{ sessions: AgentSession[] }>($, id, '/command-center/sessions')).sessions
}

export function getTask($: EngineInterface, box: string, task: string): Promise<Task> {
  return boxJson<Task>($, box, `/command-center/tasks/${encodeURIComponent(task)}`)
}

export async function createTask($: EngineInterface, box: string, prompt: string): Promise<Task> {
  const created = await boxJson<{ task: Task }>($, box, '/command-center/tasks', {
    method: 'POST',
    body: { requestId: crypto.randomUUID(), prompt, contextRefs: [], source: 'api' }
  })
  return created.task
}

export function sendMessage($: EngineInterface, box: string, task: string, text: string) {
  return boxJson<Task>($, box, `/command-center/tasks/${encodeURIComponent(task)}/messages`, {
    method: 'POST',
    body: { kind: 'message', messageId: crypto.randomUUID(), text }
  })
}

export function taskAction($: EngineInterface, box: string, task: string, action: string) {
  return boxJson<Task>($, box, `/command-center/tasks/${encodeURIComponent(task)}/${action}`, {
    method: 'POST',
    body: {}
  })
}

/** The newest Pi conversation file of a task, or null before its Pi has written one. */
export async function transcriptFile(
  $: EngineInterface,
  box: string,
  task: string
): Promise<string | null> {
  const dir = `${PI_SESSION_ROOT}/${task}`
  const listed = await listFiles($, box, dir).catch(() => null)
  const newest = (listed?.entries ?? [])
    .filter((entry) => entry.kind === 'file' && entry.name.endsWith('.jsonl'))
    .sort((a, b) => b.modifiedAtMs - a.modifiedAtMs)[0]
  return newest ? `${dir}/${newest.name}` : null
}

/**
 * How many bytes one read of a file takes. The box caps a command's output at
 * 1 MiB; this many bytes in base64 stay under it.
 */
export const READ_BYTES = 750_000

/**
 * At most READ_BYTES of a file past `offset`, read on the box. The bytes travel as
 * base64: the box decodes a command's output chunk by chunk, so a character split
 * between two chunks would arrive as U+FFFD and move every later offset.
 *
 * With `waitSeconds`, the box waits up to that long for the file to grow past
 * `offset` first, so a reader waiting for the next row waits on the box.
 */
export async function readFrom(
  $: EngineInterface,
  box: string,
  file: string,
  offset: number,
  waitSeconds = 0
): Promise<Uint8Array> {
  const ran = await exec($, box, {
    argv: [
      'bash',
      '-c',
      'set -o pipefail; for _ in $(seq "$4"); do [ "$(stat -c %s -- "$2")" -ge "$1" ] && break; sleep 1; done; tail -c +"$1" -- "$2" | head -c "$3" | base64 -w0',
      'bash',
      String(offset + 1),
      file,
      String(READ_BYTES),
      String(waitSeconds)
    ]
  })
  if (ran.exitCode !== 0) throw new ZipboxError(ran.stderr.trim() || 'could not read the task')
  return Uint8Array.from(atob(ran.stdout.trim()), (char) => char.charCodeAt(0))
}
