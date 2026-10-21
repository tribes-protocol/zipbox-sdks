import { ZipboxError } from './errors.js'
import { isSandboxDetail, readJson } from './guards.js'
import type { HttpBody, HttpClient, HttpMethod } from './http.js'
import type { SandboxDetail } from './types.js'

export interface ApiRequest {
  method: HttpMethod
  path: string
  headers?: Record<string, string>
  body?: HttpBody
  retry?: boolean
  signal?: AbortSignal | undefined
}

/** The control plane: `Authorization: Bearer <apiKey>` on every request. */
export class ControlPlane {
  // ES-private so `console.log`/`util.inspect` of a client never prints the key.
  readonly #apiKey: string

  constructor(
    private readonly http: HttpClient,
    private readonly baseUrl: string,
    apiKey: string
  ) {
    this.#apiKey = apiKey
  }

  request(request: ApiRequest): Promise<Response> {
    return this.http.send({
      method: request.method,
      url: `${this.baseUrl}${request.path}`,
      headers: { authorization: `Bearer ${this.#apiKey}`, ...request.headers },
      ...(request.body === undefined ? {} : { body: request.body }),
      ...(request.retry === undefined ? {} : { retry: request.retry }),
      signal: request.signal
    })
  }

  async getDetail(id: string, signal?: AbortSignal): Promise<SandboxDetail> {
    const response = await this.request({
      method: 'GET',
      path: `/sandbox/${encodeURIComponent(id)}`,
      signal
    })
    return readJson(response, isSandboxDetail, 'GET /sandbox/:id')
  }
}

/** Where a box answers and the token it accepts. */
export interface ResolvedBox {
  baseUrl: string
  token: string
  detail: SandboxDetail
}

function resolvedFromDetail(detail: SandboxDetail): ResolvedBox | null {
  if (!detail.ready || typeof detail.baseUrl !== 'string' || typeof detail.boxToken !== 'string') {
    return null
  }
  return { baseUrl: detail.baseUrl.replace(/\/+$/, ''), token: detail.boxToken, detail }
}

/**
 * Requests to a box. The box token comes from `GET /sandbox/:id` on first use and
 * is cached for the life of the client. A 401 from the box re-reads the detail
 * once and retries once; a second 401 is the caller's `unauthorized`.
 *
 * That second 401 is NOT a deleted key: a deleted key fails the re-read itself,
 * at the control plane. The control plane just vouched for the key, so the box has
 * not received its token yet — say so, and leave the retry to the caller.
 */
export class BoxTransport {
  // ES-private: these hold box tokens, which must never show up in a logged object.
  readonly #boxes = new Map<string, ResolvedBox>()
  readonly #inFlight = new Map<string, Promise<ResolvedBox>>()

  constructor(
    private readonly api: ControlPlane,
    private readonly http: HttpClient
  ) {}

  /** Remember a detail the caller already fetched (create/get), so the box costs no extra GET. */
  seed(detail: SandboxDetail): void {
    const resolved = resolvedFromDetail(detail)
    if (resolved === null) {
      this.#boxes.delete(detail.id)
    } else {
      this.#boxes.set(detail.id, resolved)
    }
  }

  invalidate(id: string): void {
    this.#boxes.delete(id)
  }

  /** The box's origin and token, fetching the detail when none is cached. */
  async resolve(id: string, signal?: AbortSignal): Promise<ResolvedBox> {
    const cached = this.#boxes.get(id)
    if (cached !== undefined) return cached
    const pending = this.#inFlight.get(id)
    if (pending !== undefined) return pending
    const promise = this.fetch(id, signal).finally(() => this.#inFlight.delete(id))
    this.#inFlight.set(id, promise)
    return promise
  }

  async request(id: string, request: ApiRequest): Promise<Response> {
    const box = await this.resolve(id, request.signal)
    try {
      return await this.send(box, request)
    } catch (error) {
      if (!(error instanceof ZipboxError) || error.status !== 401) throw error
      this.invalidate(id)
      const fresh = await this.resolve(id, request.signal)
      try {
        return await this.send(fresh, request)
      } catch (second) {
        if (!(second instanceof ZipboxError) || second.status !== 401) throw second
        throw new ZipboxError({
          code: 'unauthorized',
          message:
            `box ${id} has not received this API key's token yet: the control plane ` +
            'accepts the key, the box does not. A key created moments ago reaches its ' +
            'running boxes within seconds; retry then.',
          status: 401,
          retryable: true,
          requestId: second.requestId
        })
      }
    }
  }

  private send(box: ResolvedBox, request: ApiRequest): Promise<Response> {
    return this.http.send({
      method: request.method,
      url: `${box.baseUrl}${request.path}`,
      headers: { authorization: `Bearer ${box.token}`, ...request.headers },
      ...(request.body === undefined ? {} : { body: request.body }),
      ...(request.retry === undefined ? {} : { retry: request.retry }),
      signal: request.signal
    })
  }

  private async fetch(id: string, signal?: AbortSignal): Promise<ResolvedBox> {
    const detail = await this.api.getDetail(id, signal)
    const resolved = resolvedFromDetail(detail)
    if (resolved === null) {
      throw new ZipboxError({
        code: 'box_not_running',
        message: `box ${id} is not ready (status: ${detail.status}); start it with sandboxes.start`,
        // Booting or restarting comes back by itself; stopped or deleted does not.
        retryable: detail.status === 'provisioning' || detail.status === 'running'
      })
    }
    this.#boxes.set(id, resolved)
    return resolved
  }
}
