import { sleep } from './backoff.js'
import {
  type ClientContext,
  deleteBox,
  getDetail,
  lifecycle,
  retryDelay,
  setTier
} from './context.js'
import { ZipboxError } from './errors.js'
import { type Cmd, execBody, toExecEvent } from './exec.js'
import { isCommandRecord, isFileListing, isRecord, readJson } from './guards.js'
import { resolveBoxPath, shellQuote, splitBoxPath } from './paths.js'
import { PtyApi } from './pty.js'
import { parseSse } from './sse.js'
import type {
  CallOptions,
  CommandEventsOptions,
  CommandRecord,
  DeleteSandboxOptions,
  ExecBackgroundOptions,
  ExecEvent,
  ExecOptions,
  ExecResult,
  ExecSignal,
  FileEntry,
  LogsOptions,
  PreviewLink,
  PreviewOptions,
  RunCodeLanguage,
  SandboxDetail,
  SandboxStatus,
  SandboxTier,
  SshInfo
} from './types.js'

const decoder = new TextDecoder()

function eventsQuery(since: string | undefined): string {
  return since === undefined ? '' : `?since=${encodeURIComponent(since)}`
}

async function* readEvents(
  response: Response,
  signal: AbortSignal | undefined
): AsyncGenerator<ExecEvent> {
  if (response.body === null) {
    throw new ZipboxError({ code: 'unavailable', message: 'exec response had no body' })
  }
  try {
    for await (const raw of parseSse(response.body)) {
      const event = toExecEvent(raw)
      if (event !== null) yield event
    }
  } catch (error) {
    // A connection that drops mid-stream is a transient `unavailable`, never a raw
    // fetch error; an abort the caller asked for stays an abort.
    if (signal?.aborted === true || error instanceof ZipboxError) throw error
    throw new ZipboxError({
      code: 'unavailable',
      message: `exec stream dropped: ${error instanceof Error ? error.message : String(error)}`,
      retryable: true
    })
  }
}

async function collect(events: AsyncIterable<ExecEvent>): Promise<ExecResult> {
  let id = ''
  let stdout = ''
  let stderr = ''
  for await (const event of events) {
    switch (event.type) {
      case 'start':
        id = event.id
        break
      case 'stdout':
        stdout += event.data
        break
      case 'stderr':
        stderr += event.data
        break
      case 'gap':
        break
      case 'exit':
        return { id, exitCode: event.exitCode, stdout, stderr }
    }
  }
  throw new ZipboxError({ code: 'unavailable', message: `command ${id} ended without an exit` })
}

/** Commands on one box: `GET/POST /exec/*`. */
export class Commands {
  constructor(
    private readonly ctx: ClientContext,
    private readonly sandboxId: string
  ) {}

  /** Running and recently finished commands. */
  async list(options: CallOptions = {}): Promise<CommandRecord[]> {
    const response = await this.ctx.boxes.request(this.sandboxId, {
      method: 'GET',
      path: '/exec',
      signal: options.signal
    })
    const body = await readJson(
      response,
      (value: unknown): value is { commands: CommandRecord[] } =>
        isRecord(value) && Array.isArray(value.commands) && value.commands.every(isCommandRecord),
      'GET /exec'
    )
    return body.commands
  }

  /** One command's record. */
  async get(commandId: string, options: CallOptions = {}): Promise<CommandRecord> {
    const response = await this.ctx.boxes.request(this.sandboxId, {
      method: 'GET',
      path: `/exec/${encodeURIComponent(commandId)}`,
      signal: options.signal
    })
    return readJson(response, isCommandRecord, 'GET /exec/:id')
  }

  /**
   * Replay a command's output from a cursor and follow it until it exits. The
   * stream ends after the `exit` event.
   */
  async *events(commandId: string, options: CommandEventsOptions = {}): AsyncGenerator<ExecEvent> {
    const response = await this.ctx.boxes.request(this.sandboxId, {
      method: 'GET',
      path: `/exec/${encodeURIComponent(commandId)}/events${eventsQuery(options.since)}`,
      headers: { accept: 'text/event-stream' },
      signal: options.signal
    })
    yield* readEvents(response, options.signal)
  }

  /**
   * Follow a command to its exit, re-attaching from the last cursor whenever the
   * stream drops. A followed command is a live surface: there is no attempt
   * ceiling and no idle timeout. Reconnects back off exponentially (capped, with
   * jitter) and the ladder resets only when an event arrives. It ends on `exit`,
   * on a non-retryable error (e.g. `not_found`), or when the signal aborts.
   */
  async *follow(commandId: string, options: CommandEventsOptions = {}): AsyncGenerator<ExecEvent> {
    let cursor = options.since
    let failures = 0
    for (;;) {
      try {
        for await (const event of this.events(commandId, {
          ...(cursor === undefined ? {} : { since: cursor }),
          ...(options.signal === undefined ? {} : { signal: options.signal })
        })) {
          failures = 0
          cursor = event.cursor
          yield event
          if (event.type === 'exit') return
        }
      } catch (error) {
        if (!(error instanceof ZipboxError) || !error.retryable) throw error
      }
      await sleep(retryDelay(this.ctx, failures), options.signal)
      failures++
    }
  }

  /** Append bytes to the command's stdin (started with `stdin: true`). */
  async stdin(
    commandId: string,
    data: string | Uint8Array,
    options: CallOptions = {}
  ): Promise<void> {
    await this.ctx.boxes.request(this.sandboxId, {
      method: 'POST',
      path: `/exec/${encodeURIComponent(commandId)}/stdin`,
      body: { raw: data },
      signal: options.signal
    })
  }

  /** Close the command's stdin. */
  async closeStdin(commandId: string, options: CallOptions = {}): Promise<void> {
    await this.ctx.boxes.request(this.sandboxId, {
      method: 'POST',
      path: `/exec/${encodeURIComponent(commandId)}/stdin?close=1`,
      body: { raw: '' },
      signal: options.signal
    })
  }

  /** Send a signal to the command's process group. */
  async signal(commandId: string, signal: ExecSignal, options: CallOptions = {}): Promise<void> {
    await this.ctx.boxes.request(this.sandboxId, {
      method: 'POST',
      path: `/exec/${encodeURIComponent(commandId)}/signal`,
      body: { json: { signal } },
      signal: options.signal
    })
  }
}

/** A command started with {@link Sandbox.execBackground}. */
export class Command {
  constructor(
    private readonly commands: Commands,
    /** The record as of the last read. */
    public record: CommandRecord
  ) {}

  get id(): string {
    return this.record.id
  }

  /** Re-read the record. */
  async get(options: CallOptions = {}): Promise<CommandRecord> {
    this.record = await this.commands.get(this.id, options)
    return this.record
  }

  /** Output from a cursor (default: the start), following until exit. */
  events(options: CommandEventsOptions = {}): AsyncGenerator<ExecEvent> {
    return this.commands.follow(this.id, options)
  }

  /** Wait for the command to exit and collect its whole output. */
  wait(options: CallOptions = {}): Promise<ExecResult> {
    return collect(this.commands.follow(this.id, options))
  }

  /** Append bytes to stdin. */
  stdin(data: string | Uint8Array, options: CallOptions = {}): Promise<void> {
    return this.commands.stdin(this.id, data, options)
  }

  /** Close stdin. */
  closeStdin(options: CallOptions = {}): Promise<void> {
    return this.commands.closeStdin(this.id, options)
  }

  /** Signal the command. */
  signal(signal: ExecSignal, options: CallOptions = {}): Promise<void> {
    return this.commands.signal(this.id, signal, options)
  }

  toJSON(): CommandRecord {
    return this.record
  }
}

/** Files on one box. Relative paths resolve against /root/workspace. */
export class Files {
  constructor(
    private readonly ctx: ClientContext,
    private readonly sandboxId: string
  ) {}

  /** Read a file (up to 250 MiB). */
  async read(path: string, options: CallOptions = {}): Promise<Uint8Array> {
    const response = await this.ctx.boxes.request(this.sandboxId, {
      method: 'GET',
      path: `/files/download?path=${encodeURIComponent(resolveBoxPath(path))}`,
      signal: options.signal
    })
    return new Uint8Array(await response.arrayBuffer())
  }

  /** Read a file as UTF-8 text. */
  async readText(path: string, options: CallOptions = {}): Promise<string> {
    return decoder.decode(await this.read(path, options))
  }

  /** Write a file atomically (up to 250 MiB). The directory must exist. */
  async write(
    path: string,
    content: string | Uint8Array,
    options: CallOptions = {}
  ): Promise<void> {
    const { dir, name } = splitBoxPath(path)
    await this.ctx.boxes.request(this.sandboxId, {
      method: 'POST',
      path: `/files/write?dir=${encodeURIComponent(dir)}&name=${encodeURIComponent(name)}`,
      body: { raw: content },
      signal: options.signal
    })
  }

  /** List a directory. */
  async list(path = '/root/workspace', options: CallOptions = {}): Promise<FileEntry[]> {
    const response = await this.ctx.boxes.request(this.sandboxId, {
      method: 'GET',
      path: `/files?path=${encodeURIComponent(resolveBoxPath(path))}`,
      signal: options.signal
    })
    const listing = await readJson(response, isFileListing, 'GET /files')
    return listing.entries
  }

  /** One entry's metadata, read from its parent directory. */
  async stat(path: string, options: CallOptions = {}): Promise<FileEntry> {
    const { dir, name } = splitBoxPath(path)
    const entry = (await this.list(dir, options)).find((candidate) => candidate.name === name)
    if (entry === undefined) {
      throw new ZipboxError({ code: 'not_found', message: `no such file: ${resolveBoxPath(path)}` })
    }
    return entry
  }

  /** Create a directory. */
  mkdir(path: string, options: CallOptions = {}): Promise<void> {
    return this.mutate({ action: 'mkdir', path: resolveBoxPath(path) }, options)
  }

  /** Move or rename. */
  move(from: string, to: string, options: CallOptions = {}): Promise<void> {
    return this.mutate(
      { action: 'move', path: resolveBoxPath(from), destination: resolveBoxPath(to) },
      options
    )
  }

  /** Delete a file or directory. */
  delete(path: string, options: CallOptions = {}): Promise<void> {
    return this.mutate({ action: 'delete', path: resolveBoxPath(path) }, options)
  }

  /** Copy a local file to the box (Node). */
  async upload(localPath: string, remotePath: string, options: CallOptions = {}): Promise<void> {
    const { readFile } = await import('node:fs/promises')
    await this.write(remotePath, new Uint8Array(await readFile(localPath)), options)
  }

  /** Copy a file from the box to a local path (Node). */
  async download(remotePath: string, localPath: string, options: CallOptions = {}): Promise<void> {
    const { writeFile } = await import('node:fs/promises')
    await writeFile(localPath, await this.read(remotePath, options))
  }

  private async mutate(
    body: { action: 'mkdir' | 'move' | 'delete'; path: string; destination?: string },
    options: CallOptions
  ): Promise<void> {
    await this.ctx.boxes.request(this.sandboxId, {
      method: 'POST',
      path: '/files/mutate',
      body: { json: body },
      signal: options.signal
    })
  }
}

/** Public HTTPS URLs for ports on one box, served by the box's own Caddy. */
export class Preview {
  constructor(private readonly box: Sandbox) {}

  /**
   * Publish `127.0.0.1:<port>` at `https://<label>.<box host>`. Public: anyone
   * with the URL reaches it.
   */
  async create(port: number, options: PreviewOptions = {}): Promise<PreviewLink> {
    const label = options.label ?? `p${port}`
    const host = `${label}.${await this.box.host(options)}`
    await this.box.run(['tribes-caddy', 'add', host, `127.0.0.1:${port}`], 'preview', options)
    return { url: `https://${host}`, label }
  }

  /** Remove a preview by its label (or full host). */
  async remove(label: string, options: CallOptions = {}): Promise<void> {
    const boxHost = await this.box.host(options)
    const host = label.endsWith(`.${boxHost}`) ? label : `${label}.${boxHost}`
    await this.box.run(['tribes-caddy', 'rm', host], 'preview removal', options)
  }
}

const RUN_CODE_INTERPRETERS: Record<RunCodeLanguage, { bin: string; ext: string }> = {
  python: { bin: 'python3', ext: 'py' },
  node: { bin: 'node', ext: 'mjs' },
  bash: { bin: 'bash', ext: 'sh' }
}

// Finds the box's desktop display rather than assuming :1.
// ffmpeg's x11grab, which the platform image ships (it has no ImageMagick). Under
// pipefail a capture that fails is a non-zero exit, not an empty PNG.
const SCREENSHOT_COMMAND =
  "set -o pipefail; d=$(ls /tmp/.X11-unix 2>/dev/null | sed -n 's/^X//p' | sort -n | tail -n1); " +
  'ffmpeg -loglevel error -f x11grab -i ":${d:-1}" -frames:v 1 -f image2pipe -vcodec png - | base64 -w0'

/**
 * One box. Holds the row it was read with (`info`), and every operation on the
 * box itself. Get one from `zb.sandboxes.create/get/getOrCreate` or, without a
 * network call, `zb.sandbox(id)`.
 */
export class Sandbox {
  /** Commands on this box. */
  readonly commands: Commands
  /** Files on this box. */
  readonly files: Files
  /** Terminals on this box. */
  readonly pty: PtyApi
  /** Published ports on this box. */
  readonly preview: Preview
  private detail: SandboxDetail | null

  constructor(
    private readonly ctx: ClientContext,
    /** The box id. */
    readonly id: string,
    detail: SandboxDetail | null
  ) {
    this.detail = detail
    this.commands = new Commands(ctx, id)
    this.files = new Files(ctx, id)
    this.pty = new PtyApi(ctx, id)
    this.preview = new Preview(this)
  }

  /** The row as last read, or null for a handle made by `zb.sandbox(id)`. */
  get info(): SandboxDetail | null {
    return this.detail
  }

  get name(): string | null {
    return this.detail?.name ?? null
  }

  get status(): SandboxStatus | null {
    return this.detail?.status ?? null
  }

  get tier(): SandboxTier | null {
    return this.detail?.tier ?? null
  }

  get ready(): boolean {
    return this.detail?.ready ?? false
  }

  get baseUrl(): string | null {
    return this.detail?.baseUrl ?? null
  }

  get ipv6(): string | null {
    return this.detail?.ipv6 ?? null
  }

  /** Re-read the row. */
  async refresh(options: CallOptions = {}): Promise<SandboxDetail> {
    this.detail = await getDetail(this.ctx, this.id, options.signal)
    return this.detail
  }

  /** Run a command and collect its output. A non-zero exit is a result. */
  exec(cmd: Cmd, options: ExecOptions = {}): Promise<ExecResult> {
    return collect(this.execStream(cmd, options))
  }

  /**
   * Run a command and stream its events. Breaking out of the loop (or aborting)
   * disconnects, which ends the command; use {@link execBackground} for work that
   * must outlive the caller.
   */
  async *execStream(cmd: Cmd, options: ExecOptions = {}): AsyncGenerator<ExecEvent> {
    const response = await this.ctx.boxes.request(this.id, {
      method: 'POST',
      path: '/exec',
      headers: { accept: 'text/event-stream' },
      body: { json: execBody(cmd, options, false) },
      signal: options.signal
    })
    let commandId: string | null = null
    let cursor = '0-0'
    try {
      for await (const event of readEvents(response, options.signal)) {
        cursor = event.cursor
        if (event.type === 'start') commandId = event.id
        yield event
        if (event.type === 'exit') return
      }
    } catch (error) {
      // A drop after `start` falls through to the replay below.
      if (commandId === null || !(error instanceof ZipboxError) || !error.retryable) throw error
    }
    if (commandId === null) {
      throw new ZipboxError({ code: 'unavailable', message: 'exec stream ended before start' })
    }
    yield* this.commands.follow(commandId, {
      since: cursor,
      ...(options.signal === undefined ? {} : { signal: options.signal })
    })
  }

  /** Start a command that keeps running with nobody attached. */
  async execBackground(cmd: Cmd, options: ExecBackgroundOptions = {}): Promise<Command> {
    const response = await this.ctx.boxes.request(this.id, {
      method: 'POST',
      path: '/exec',
      body: { json: execBody(cmd, options, true) },
      signal: options.signal
    })
    return new Command(this.commands, await readJson(response, isCommandRecord, 'POST /exec'))
  }

  /** Run a snippet with python3, node or bash. */
  async runCode(
    language: RunCodeLanguage,
    code: string,
    options: ExecOptions = {}
  ): Promise<ExecResult> {
    const { bin, ext } = RUN_CODE_INTERPRETERS[language]
    const path = `/tmp/zipbox-run-${crypto.randomUUID()}.${ext}`
    await this.files.write(path, code, options)
    const quoted = shellQuote(path)
    return this.exec(`${bin} ${quoted}; status=$?; rm -f ${quoted}; exit $status`, options)
  }

  /** Recent system log lines (journalctl). */
  async logs(options: LogsOptions = {}): Promise<string> {
    const argv = ['journalctl', '-o', 'short-iso', '--no-pager']
    if (options.lines !== undefined) argv.push('-n', String(options.lines))
    if (options.since !== undefined) argv.push('--since', options.since)
    return (await this.run(argv, 'logs', options)).stdout
  }

  /** How to SSH into the box (IPv6, key auth). */
  async sshInfo(options: CallOptions = {}): Promise<SshInfo> {
    const ipv6 = this.detail?.ipv6 ?? (await this.refresh(options)).ipv6
    if (typeof ipv6 !== 'string' || ipv6 === '') {
      throw new ZipboxError({ code: 'box_not_running', message: `box ${this.id} has no address` })
    }
    return { host: ipv6, user: 'root', command: `ssh root@${ipv6}` }
  }

  /** A PNG of the box's desktop. */
  async screenshot(options: CallOptions = {}): Promise<Uint8Array> {
    const { stdout } = await this.run(SCREENSHOT_COMMAND, 'screenshot', options)
    const binary = atob(stdout.trim())
    return Uint8Array.from(binary, (char) => char.charCodeAt(0))
  }

  /** Start a stopped box. */
  start(options: CallOptions = {}): Promise<void> {
    return lifecycle(this.ctx, this.id, 'restore', options.signal)
  }

  /** Stop the box (disk archived; nothing billed while stopped). */
  stop(options: CallOptions = {}): Promise<void> {
    return lifecycle(this.ctx, this.id, 'pause', options.signal)
  }

  /** Hard-restart the box; files survive. */
  restart(options: CallOptions = {}): Promise<void> {
    return lifecycle(this.ctx, this.id, 'reset', options.signal)
  }

  /** Move the box to another tier. */
  setTier(tier: SandboxTier, options: CallOptions = {}): Promise<void> {
    return setTier(this.ctx, this.id, tier, options.signal)
  }

  /** Delete the box. */
  delete(options: DeleteSandboxOptions = {}): Promise<void> {
    return deleteBox(this.ctx, this.id, options.archive, options.signal)
  }

  /** @internal The box's hostname, as it is served. */
  async host(options: CallOptions = {}): Promise<string> {
    const box = await this.ctx.boxes.resolve(this.id, options.signal)
    return new URL(box.baseUrl).hostname
  }

  /** @internal Run a command the SDK itself issues; a non-zero exit is an error. */
  async run(cmd: Cmd, what: string, options: CallOptions): Promise<ExecResult> {
    const result = await this.exec(
      cmd,
      options.signal === undefined ? {} : { signal: options.signal }
    )
    if (result.exitCode !== 0) {
      throw new ZipboxError({
        code: 'conflict',
        message: `${what} failed (exit ${result.exitCode ?? 'unknown'}): ${result.stderr.trim() || result.stdout.trim()}`
      })
    }
    return result
  }

  toJSON(): SandboxDetail | { id: string } {
    return this.detail ?? { id: this.id }
  }
}
