import {
  type ClientContext,
  deleteBox,
  getDetail,
  lifecycle,
  setTier,
  startBox,
  waitUntilReady
} from './context.js'
import { ZipboxError } from './errors.js'
import { isArrayOf, isJsonObject, isRecord, isSandboxInfo, readJson } from './guards.js'
import { HttpClient } from './http.js'
import { Sandbox } from './sandbox.js'
import { BoxTransport, ControlPlane } from './transport.js'
import type {
  CallOptions,
  CreateSandboxOptions,
  DeleteSandboxOptions,
  JsonObject,
  SandboxInfo,
  SandboxTier,
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
   * Create a box and wait until it answers. Safe to retry: the request carries an
   * Idempotency-Key. Waiting has no deadline; pass a signal to stop waiting (the
   * box keeps booting).
   */
  async create(options: CreateSandboxOptions = {}): Promise<Sandbox> {
    const body: Record<string, string | boolean> = {}
    if (options.name !== undefined) body.name = options.name
    if (options.tier !== undefined) body.tier = options.tier
    if (options.sshPublicKey !== undefined) body.sshPublicKey = options.sshPublicKey
    if (options.mailbox !== undefined) body.mailbox = options.mailbox
    if (options.private !== undefined) body.private = options.private
    const response = await this.ctx.api.request({
      method: 'POST',
      path: '/sandbox/identity/claim',
      headers: { 'idempotency-key': crypto.randomUUID() },
      body: { json: body },
      retry: true,
      signal: options.signal
    })
    const claimed = await readJson(
      response,
      (value: unknown): value is { sandboxId: string } =>
        isRecord(value) && typeof value.sandboxId === 'string',
      'POST /sandbox/identity/claim'
    )
    return this.waitReady(claimed.sandboxId, options)
  }

  /**
   * The box with this name, started if it was stopped, or a new one created with
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
      (row) =>
        row.name === name &&
        row.status !== 'destroyed' &&
        row.status !== 'stopped' &&
        row.status !== 'deleting'
    )
    if (existing === undefined) return this.create({ ...options, name })
    // `stopping`: a stop is still landing; startBox waits it out, then starts.
    if (existing.status === 'archived' || existing.status === 'stopping') {
      await startBox(this.ctx, existing.id, options.signal)
    }
    return this.waitReady(existing.id, options)
  }

  /**
   * Re-read a box with backoff until it is ready. No deadline; pass a signal to
   * stop waiting. A private box that has not joined a tailnet yet is returned once
   * it is running with its key, since it is not ready until it joins.
   */
  async waitReady(id: string, options: CallOptions = {}): Promise<Sandbox> {
    return new Sandbox(this.ctx, id, await waitUntilReady(this.ctx, id, options.signal))
  }

  /**
   * Start a stopped box. A box still `stopping` is started once its stop lands.
   * Credit-gated: `insufficient_credits` when the balance is dry.
   */
  start(id: string, options: CallOptions = {}): Promise<void> {
    return startBox(this.ctx, id, options.signal)
  }

  /** Stop a box. Its disk is archived and nothing is billed while it is stopped. */
  stop(id: string, options: CallOptions = {}): Promise<void> {
    return lifecycle(this.ctx, id, 'pause', options.signal)
  }

  /** Hard-restart a running box; files survive, running processes do not. */
  restart(id: string, options: CallOptions = {}): Promise<void> {
    return lifecycle(this.ctx, id, 'reset', options.signal)
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
      boxes: new BoxTransport(api, http),
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
