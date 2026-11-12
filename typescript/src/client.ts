import { CLAIM_STREAM_MEDIA_TYPE, readClaimStream } from './claimstream.js'
import {
  cancelTransition,
  type ClientContext,
  deleteBox,
  getDetail,
  lifecycle,
  pauseBox,
  readDetailInWait,
  remember,
  requestResume,
  resumeBox,
  setTier,
  waitUntilReady
} from './context.js'
import { ZipboxError } from './errors.js'
import { forkBox, getFork } from './fork.js'
import {
  isArrayOf,
  isJsonObject,
  isRecord,
  isSandboxDetail,
  isSandboxInfo,
  readJson
} from './guards.js'
import { HttpClient } from './http.js'
import { readinessOf } from './readiness.js'
import { Sandbox } from './sandbox.js'
import { BoxTransport, ControlPlane } from './transport.js'
import type {
  CallOptions,
  CreateSandboxOptions,
  DeleteSandboxOptions,
  ForkSandboxOptions,
  JsonObject,
  SandboxDetail,
  SandboxFork,
  SandboxInfo,
  SandboxTier,
  TransitionCancelOutcome,
  ZipboxOptions
} from './types.js'

const DEFAULT_API_URL = 'https://api.tribes.xyz'

function env(name: string): string | undefined {
  const value = typeof process === 'undefined' ? undefined : process.env[name]
  return value === undefined || value === '' ? undefined : value
}

/** Box lifecycle: `/sandbox/*` on the control plane. */
export class Sandboxes {
  constructor(private readonly ctx: ClientContext) {}

  /** Your boxes. */
  async list(options: CallOptions = {}): Promise<SandboxInfo[]> {
    const response = await this.ctx.api.request({
      method: 'GET',
      path: '/sandbox',
      signal: options.signal
    })
    return readJson(response, isArrayOf(isSandboxInfo), 'GET /sandbox')
  }

  /** One box, with its connection details. */
  async get(id: string, options: CallOptions = {}): Promise<Sandbox> {
    return new Sandbox(this.ctx, id, await getDetail(this.ctx, id, options.signal))
  }

  /**
   * Create a box and return it once it answers: at once when the server bound a
   * booted machine inside the request (`ready` in its answer), else after waiting
   * for it. Safe to retry: the request carries an Idempotency-Key. Waiting has no
   * deadline; pass a signal to stop waiting (the box keeps booting).
   */
  async create(options: CreateSandboxOptions = {}): Promise<Sandbox> {
    const body: Record<string, string | boolean> = {}
    if (options.name !== undefined) body.name = options.name
    if (options.tier !== undefined) body.tier = options.tier
    if (options.sshPublicKey !== undefined) body.sshPublicKey = options.sshPublicKey
    if (options.mailbox !== undefined) body.mailbox = options.mailbox
    if (options.private !== undefined) body.private = options.private
    // The answer streamed (conformance README, streamed create); its placed line is
    // never dialled. A server that does not stream answers as before.
    const response = await this.ctx.api.request({
      method: 'POST',
      path: '/sandbox/identity/claim',
      headers: {
        'idempotency-key': crypto.randomUUID(),
        accept: `${CLAIM_STREAM_MEDIA_TYPE}, application/json`
      },
      body: { json: body },
      retry: true,
      unwrap: readClaimStream,
      signal: options.signal
    })
    const claimed = await readJson(
      response,
      (value: unknown): value is { sandboxId: string } =>
        isRecord(value) && typeof value.sandboxId === 'string',
      'POST /sandbox/identity/claim'
    )
    const created = { ...claimed, id: claimed.sandboxId }
    if (
      isSandboxDetail(created) &&
      readinessOf(created) === 'ready' &&
      typeof created.baseUrl === 'string'
    ) {
      return new Sandbox(this.ctx, created.id, remember(this.ctx, created))
    }
    return this.waitReady(claimed.sandboxId, options)
  }

  /**
   * The box with this name, resumed if it was paused, or a new one created with
   * `options`. Use it to reuse an environment across runs.
   *
   * The options (tier, SSH key, mailbox, private) apply only when a box is
   * created. A box that already has this name is returned as it is, even when it
   * differs (another tier, not private): that is not an error and nothing is
   * resized. Check `box.tier` and call `setTier` if it matters.
   */
  async getOrCreate(
    name: string,
    options: Omit<CreateSandboxOptions, 'name'> = {}
  ): Promise<Sandbox> {
    const existing = (await this.list(options)).find(
      (row) => row.name === name && row.status !== 'destroyed' && row.status !== 'deleting'
    )
    if (existing === undefined) return this.create({ ...options, name })
    // `stopping`: a Pause is still landing; requestResume waits it out, then resumes.
    if (existing.status === 'paused' || existing.status === 'stopping') {
      let accepted: SandboxDetail | null = null
      try {
        accepted = await requestResume(this.ctx, existing.id, options.signal)
      } catch (error) {
        if (!(error instanceof ZipboxError)) throw error
        // The name was being deleted: it is gone now, so make it.
        if (error.code === 'not_found') return this.create({ ...options, name })
        // Another caller resumed it first: the server's idempotent answer to a
        // second resume. A box now coming up is waited on like any other.
        if (error.code !== 'conflict') throw error
        const detail = await readDetailInWait(this.ctx, existing.id, options.signal)
        if (detail.status !== 'provisioning' && detail.status !== 'running') throw error
      }
      if (accepted !== null && readinessOf(accepted) === 'ready') {
        return new Sandbox(this.ctx, existing.id, accepted)
      }
    }
    return this.waitReady(existing.id, options)
  }

  /**
   * Wait until a box is ready, on `GET /sandbox/:id/wait` (the server answers when
   * it is; an older API is polled with backoff instead). No deadline; pass a signal
   * to stop waiting. A private box that has not joined a tailnet yet is returned
   * once it is running with its key, since it is not ready until it joins. A box
   * that will not become ready by itself raises: paused `box_not_running`, a failed
   * start `start_failed`, deleting or gone `conflict`, none of them retryable.
   */
  async waitReady(id: string, options: CallOptions = {}): Promise<Sandbox> {
    return new Sandbox(this.ctx, id, await waitUntilReady(this.ctx, id, options.signal))
  }

  /**
   * Pause a box and return it paused. Nothing is billed while it is paused, and
   * `resume` brings it back with its files and installed software. Returns at once
   * when the server answers with the paused box, else waits for the Pause to take
   * effect (no deadline; pass a signal to stop waiting). A box that reads running
   * again (the Pause was cancelled) raises `conflict`.
   */
  async pause(id: string, options: CallOptions = {}): Promise<Sandbox> {
    return new Sandbox(this.ctx, id, await pauseBox(this.ctx, id, options.signal))
  }

  /**
   * Resume a paused box and return it ready. A box still `stopping` is resumed once
   * its Pause lands. Returns at once when the server answers with the ready box,
   * else waits as `waitReady` does. Credit-gated: `insufficient_credits` when the
   * balance is dry.
   */
  async resume(id: string, options: CallOptions = {}): Promise<Sandbox> {
    return new Sandbox(this.ctx, id, await resumeBox(this.ctx, id, options.signal))
  }

  /** Alias of {@link pause}: the same request and the same result. */
  stop(id: string, options: CallOptions = {}): Promise<Sandbox> {
    return this.pause(id, options)
  }

  /** Alias of {@link resume}: the same request and the same result. */
  start(id: string, options: CallOptions = {}): Promise<Sandbox> {
    return this.resume(id, options)
  }

  /** Hard-restart a running box; files survive, running processes do not. */
  async restart(id: string, options: CallOptions = {}): Promise<void> {
    await lifecycle(this.ctx, id, 'reset', options.signal)
  }

  /**
   * Cancel the box's state change in flight - a pause, a resume, a restart, a tier
   * change - and return it to where it was. Returns what happened: `cleared` (it is
   * back already), `cancelling` (the host's abort is on its way) or `none` (nothing
   * was in flight). Past the point of no return it raises `conflict` with the reason.
   */
  cancelTransition(id: string, options: CallOptions = {}): Promise<TransitionCancelOutcome> {
    return cancelTransition(this.ctx, id, options.signal)
  }

  /**
   * Fork a box: a new box with the source's files, installed software and
   * settings, its own name and a fresh identity. RAM and running processes are
   * not copied. It starts public unless `private: true`. A running source needs `sourcePolicy: 'pause_and_resume'` (it
   * pauses for the capture and resumes right after); `require_paused` answers
   * `pause_required` instead. Safe to retry: the request carries an
   * Idempotency-Key. Waits until the new box is ready unless `wait: false`; a
   * `tailscaleAuthKey` is sealed on this machine to the new box once it runs.
   * The signal stops only this wait: cancel the fork with `cancelTransition`
   * on the new box.
   */
  async fork(sourceId: string, options: ForkSandboxOptions): Promise<Sandbox> {
    const child = await forkBox(this.ctx, sourceId, options)
    return new Sandbox(this.ctx, child.id, child)
  }

  /** The fork that is making box `id` (the new box): phase, source recovery, waits, error. */
  getFork(id: string, options: CallOptions = {}): Promise<SandboxFork> {
    return getFork(this.ctx, id, options.signal)
  }

  /** Delete a box. By default a recoverable copy is kept for 30 days. */
  delete(id: string, options: DeleteSandboxOptions = {}): Promise<void> {
    return deleteBox(this.ctx, id, options.archive, options.signal)
  }

  /** Move a box to another tier. */
  setTier(id: string, tier: SandboxTier, options: CallOptions = {}): Promise<void> {
    return setTier(this.ctx, id, tier, options.signal)
  }
}

/** Account reads. Billing changes stay in the dashboard. */
export class Account {
  constructor(private readonly ctx: ClientContext) {}

  /** Who the key belongs to: `{ userId, apiKey }` (`GET /account/whoami`). */
  async whoami(options: CallOptions = {}): Promise<JsonObject> {
    const response = await this.ctx.api.request({
      method: 'GET',
      path: '/account/whoami',
      signal: options.signal
    })
    return readJson(response, isJsonObject, 'GET /account/whoami')
  }

  /** Credit balance and auto top-up state (`GET /billing/summary`). */
  async balance(options: CallOptions = {}): Promise<JsonObject> {
    const response = await this.ctx.api.request({
      method: 'GET',
      path: '/billing/summary',
      signal: options.signal
    })
    return readJson(response, isJsonObject, 'GET /billing/summary')
  }

  /** Recent metered usage (`GET /billing/usage`). */
  async usage(options: CallOptions = {}): Promise<JsonObject> {
    const response = await this.ctx.api.request({
      method: 'GET',
      path: '/billing/usage',
      signal: options.signal
    })
    return readJson(response, isJsonObject, 'GET /billing/usage')
  }
}

/**
 * The Zipbox client.
 *
 * ```ts
 * const zb = new Zipbox() // reads ZIPBOX_API_KEY
 * const box = await zb.sandboxes.getOrCreate('demo', { tier: 's' })
 * console.log((await box.exec('uname -a')).stdout)
 * ```
 */
export class Zipbox {
  /** Box lifecycle. */
  readonly sandboxes: Sandboxes
  /** Balance and usage. */
  readonly account: Account
  private readonly ctx: ClientContext

  constructor(options: ZipboxOptions = {}) {
    const apiKey = options.apiKey ?? env('ZIPBOX_API_KEY')
    if (apiKey === undefined) {
      throw new ZipboxError({
        code: 'unauthorized',
        message:
          'no API key: pass { apiKey } or set ZIPBOX_API_KEY (zipbox.ai → Settings → API keys)'
      })
    }
    const baseUrl = (options.baseUrl ?? env('ZIPBOX_API_URL') ?? DEFAULT_API_URL).replace(
      /\/+$/,
      ''
    )
    const fetchImpl = options.fetch ?? ((input: string, init?: RequestInit) => fetch(input, init))
    const random = Math.random
    const maxRetries = options.maxRetries ?? 3
    const retryBaseDelayMs = options.retryBaseDelayMs ?? 500
    const retryMaxDelayMs = options.retryMaxDelayMs ?? 30_000
    const http = new HttpClient({
      fetch: fetchImpl,
      maxRetries,
      retryBaseDelayMs,
      retryMaxDelayMs,
      random
    })
    const api = new ControlPlane(http, baseUrl, apiKey)
    this.ctx = {
      api,
      // A box request that reached nothing re-reads the box with the wait create uses.
      boxes: new BoxTransport(api, http, (id, signal) => waitUntilReady(this.ctx, id, signal)),
      retryBaseDelayMs,
      retryMaxDelayMs,
      pollIntervalMs: options.pollIntervalMs ?? 1000,
      pollMaxIntervalMs: options.pollMaxIntervalMs ?? 10_000,
      ptyBaseDelayMs: options.ptyBaseDelayMs ?? 500,
      ptyMaxDelayMs: options.ptyMaxDelayMs ?? 30_000,
      random,
      WebSocket: options.WebSocket
    }
    this.sandboxes = new Sandboxes(this.ctx)
    this.account = new Account(this.ctx)
  }

  /** A handle on a box by id, without a network call. */
  sandbox(id: string): Sandbox {
    return new Sandbox(this.ctx, id, null)
  }
}
