import { backoffDelay } from './backoff.js'
import type { ClientContext } from './context.js'
import { ZipboxError } from './errors.js'
import { isRecord } from './guards.js'
import type { PtyCreateOptions, WebSocketConstructor, WebSocketLike } from './types.js'

const SUBPROTOCOL = 'tribes.session'
const CLIENT_LABEL = 'zipbox-sdk'
const PING_INTERVAL_MS = 30_000
const OPEN = 1
// microvmd's terminal close codes (apps/microvmd/src/types/TerminalProtocol.ts).
const CLOSE_UNKNOWN_SESSION = 4404
const CLOSE_REVOKED = 4401

/** Why a PTY stopped for good. */
export type PtyEndReason = 'exit' | 'closed' | 'detached' | 'taken' | 'unknown_session' | 'error'

type DataListener = (data: string) => void
type ExitListener = (code: number | null) => void
type EndListener = (reason: PtyEndReason, detail: string) => void

function randomId(): string {
  return crypto.randomUUID().replace(/-/g, '').slice(0, 16)
}

function websocketCtor(ctx: ClientContext): WebSocketConstructor {
  const ctor = ctx.WebSocket ?? globalThis.WebSocket
  if (typeof ctor !== 'function') {
    throw new ZipboxError({
      code: 'invalid_request',
      message:
        'no WebSocket available: pass `WebSocket` (e.g. from the `ws` package) to new Zipbox()'
    })
  }
  return ctor
}

/** Terminals on one box. A session is a PTY that outlives every socket attached to it. */
export class PtyApi {
  constructor(
    private readonly ctx: ClientContext,
    private readonly sandboxId: string
  ) {}

  /** Open a new bash session. */
  async create(options: PtyCreateOptions = {}): Promise<Pty> {
    return this.open(`bash-${crypto.randomUUID()}`, true, options)
  }

  /** Re-attach to an existing session; its screen is replayed. */
  async attach(sessionId: string, options: PtyCreateOptions = {}): Promise<Pty> {
    return this.open(sessionId, false, options)
  }

  private async open(sessionId: string, owned: boolean, options: PtyCreateOptions): Promise<Pty> {
    const ctor = websocketCtor(this.ctx)
    // Surface box_not_running/unauthorized/box_private_unreachable now rather than
    // as a silent reconnect loop.
    await this.ctx.boxes.reach(this.sandboxId)
    const pty = new Pty({
      ctx: this.ctx,
      sandboxId: this.sandboxId,
      sessionId,
      owned,
      ctor,
      options
    })
    pty.connect()
    return pty
  }
}

interface PtyParams {
  ctx: ClientContext
  sandboxId: string
  sessionId: string
  owned: boolean
  ctor: WebSocketConstructor
  options: PtyCreateOptions
}

/**
 * A live terminal. Drops reconnect by themselves with exponential backoff (cap
 * `ptyMaxDelayMs`, default 30 s, jitter) and no attempt ceiling; the ladder resets only when a frame
 * arrives. Input written while disconnected is sent on reconnect. A dial that fails
 * before the upgrade, a failed reconnect or a revoked token re-reads the box with the
 * readiness wait before the next dial, so a moved baseUrl is followed and a paused,
 * failed or deleted box ends the PTY.
 */
export class Pty {
  /** The session id; pass it to `pty.attach` to come back to this terminal. */
  readonly sessionId: string
  /** This client's identity on the session; reconnects reuse it. */
  readonly clientId = randomId()
  private readonly ctx: ClientContext
  private readonly sandboxId: string
  private readonly owned: boolean
  private readonly ctor: WebSocketConstructor
  private readonly clientLabel: string
  private ws: WebSocketLike | null = null
  private cols: number | null
  private rows: number | null
  private attempt = 0
  private connectedOnce = false
  // The cached box may be stale: the next dial re-reads it with the readiness wait.
  private stale = false
  private ended = false
  // Aborted when the PTY ends, so a readiness wait in flight stops with it.
  private readonly lifetime = new AbortController()
  private pending: string[] = []
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null
  private pingTimer: ReturnType<typeof setInterval> | null = null
  private pingSeq = 0
  private readonly dataListeners = new Set<DataListener>()
  private readonly exitListeners = new Set<ExitListener>()
  private readonly endListeners = new Set<EndListener>()

  constructor(params: PtyParams) {
    this.ctx = params.ctx
    this.sandboxId = params.sandboxId
    this.sessionId = params.sessionId
    this.owned = params.owned
    this.ctor = params.ctor
    this.clientLabel = params.options.clientLabel ?? CLIENT_LABEL
    this.cols = params.options.cols ?? null
    this.rows = params.options.rows ?? null
  }

  /** Terminal output. Returns an unsubscribe function. */
  onData(listener: DataListener): () => void {
    this.dataListeners.add(listener)
    return () => this.dataListeners.delete(listener)
  }

  /** The shell exited. Returns an unsubscribe function. */
  onExit(listener: ExitListener): () => void {
    this.exitListeners.add(listener)
    return () => this.exitListeners.delete(listener)
  }

  /** The PTY stopped for good (exit, close, detach, taken over, unknown session). */
  onEnd(listener: EndListener): () => void {
    this.endListeners.add(listener)
    return () => this.endListeners.delete(listener)
  }

  /** Send keystrokes / bytes. */
  write(data: string): void {
    if (this.ended) return
    this.sendOrQueue({ t: 'i', d: data })
  }

  /** Resize the terminal. */
  resize(cols: number, rows: number): void {
    this.cols = cols
    this.rows = rows
    if (this.ws?.readyState === OPEN) this.send({ t: 'r', c: cols, r: rows })
  }

  /** Take the session over from another client attached to it. */
  takeover(): void {
    this.sendOrQueue({ t: 'takeover' })
  }

  /** Kill the session (the shell and everything in it) and disconnect. */
  async close(): Promise<void> {
    this.finish('closed', 'closed by client')
    const query = `session=${encodeURIComponent(this.sessionId)}&clientId=${encodeURIComponent(this.clientId)}`
    await this.ctx.boxes.request(this.sandboxId, {
      method: 'POST',
      path: `/session/close?${query}`
    })
  }

  /** Disconnect and leave the session running; `pty.attach(sessionId)` comes back to it. */
  detach(): void {
    this.finish('detached', 'detached by client')
  }

  /** @internal */
  connect(): void {
    if (this.ended) return
    void this.dial()
  }

  private async dial(): Promise<void> {
    let box
    try {
      const signal = this.lifetime.signal
      box = this.stale
        ? await this.ctx.boxes.reresolve(this.sandboxId, signal)
        : await this.ctx.boxes.resolve(this.sandboxId, signal)
      this.stale = false
    } catch (error) {
      // An answered verdict (key deleted, box stopped or deleted) ends the PTY; a
      // transient (network, box still booting) is retried forever. A session that was
      // open is a live surface: a private box this device lost its tailnet path to
      // comes back when Tailscale does, so it keeps trying. A first attach fails fast.
      const reattaching =
        this.connectedOnce &&
        error instanceof ZipboxError &&
        error.code === 'box_private_unreachable'
      if (error instanceof ZipboxError && !error.retryable && !reattaching) {
        this.finish('error', error.message)
        return
      }
      this.scheduleReconnect()
      return
    }
    if (this.ended) return
    // The box's terminal websocket is the root of its base URL, which may carry a
    // path (a local box answers under one); `new URL('/', base)` would drop it.
    const url = new URL(`${box.baseUrl}/`.replace(/^http/, 'ws'))
    url.searchParams.set('session', this.sessionId)
    url.searchParams.set('clientId', this.clientId)
    url.searchParams.set('clientLabel', this.clientLabel)
    if (this.connectedOnce) url.searchParams.set('resume', '1')
    let ws: WebSocketLike
    try {
      ws = new this.ctor(url.toString(), [SUBPROTOCOL, box.token])
    } catch {
      this.scheduleReconnect()
      return
    }
    this.ws = ws
    let opened = false
    ws.onopen = () => {
      opened = true
      this.connectedOnce = true
      if (this.cols !== null && this.rows !== null)
        this.send({ t: 'r', c: this.cols, r: this.rows })
      const queued = this.pending
      this.pending = []
      for (const frame of queued) ws.send(frame)
      this.startPing()
    }
    ws.onmessage = (event) => {
      this.attempt = 0
      if (typeof event.data === 'string') this.handleFrame(event.data)
    }
    ws.onclose = (event) => {
      if (this.ws === ws) this.ws = null
      this.stopPing()
      if (this.ended) return
      if (event.code === CLOSE_UNKNOWN_SESSION) {
        this.finish('unknown_session', `no session ${this.sessionId} on this box`)
        return
      }
      if (event.code === CLOSE_REVOKED || this.attempt > 0 || !opened) {
        // A revoked or rotated token, or an origin that refused the dial (a claimed
        // box's baseUrl moves once): re-read the box before the next dial.
        this.stale = true
      }
      this.scheduleReconnect()
    }
    ws.onerror = () => undefined
  }

  private handleFrame(raw: string): void {
    let frame: unknown
    try {
      frame = JSON.parse(raw)
    } catch {
      return
    }
    if (!isRecord(frame)) return
    switch (frame.t) {
      case 'o':
        if (typeof frame.d === 'string') {
          const data = frame.d
          for (const listener of this.dataListeners) listener(data)
        }
        return
      case 'x': {
        const code = typeof frame.code === 'number' ? frame.code : null
        for (const listener of this.exitListeners) listener(code)
        this.finish('exit', `exited with ${code ?? 'unknown status'}`)
        return
      }
      case 'blocked':
        if (this.owned) this.send({ t: 'takeover' })
        return
      case 'taken':
        this.finish('taken', 'another client took this session over')
        return
      default:
        return
    }
  }

  private scheduleReconnect(): void {
    if (this.ended || this.reconnectTimer !== null) return
    const delay = backoffDelay({
      attempt: this.attempt,
      baseMs: this.ctx.ptyBaseDelayMs,
      capMs: this.ctx.ptyMaxDelayMs,
      random: this.ctx.random()
    })
    this.attempt++
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null
      this.connect()
    }, delay)
  }

  private startPing(): void {
    this.stopPing()
    this.pingTimer = setInterval(() => {
      this.send({ t: 'ping', n: this.pingSeq++ })
    }, PING_INTERVAL_MS)
    if (typeof this.pingTimer === 'object' && 'unref' in this.pingTimer) this.pingTimer.unref()
  }

  private stopPing(): void {
    if (this.pingTimer !== null) clearInterval(this.pingTimer)
    this.pingTimer = null
  }

  private sendOrQueue(frame: Record<string, string | number>): void {
    if (this.ws?.readyState === OPEN) {
      this.send(frame)
    } else {
      this.pending.push(JSON.stringify(frame))
    }
  }

  private send(frame: Record<string, string | number>): void {
    if (this.ws?.readyState === OPEN) this.ws.send(JSON.stringify(frame))
  }

  private finish(reason: PtyEndReason, detail: string): void {
    if (this.ended) return
    this.ended = true
    this.lifetime.abort()
    if (this.reconnectTimer !== null) clearTimeout(this.reconnectTimer)
    this.reconnectTimer = null
    this.stopPing()
    const ws = this.ws
    this.ws = null
    ws?.close(1000, reason)
    for (const listener of this.endListeners) listener(reason, detail)
  }
}
