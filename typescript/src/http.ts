import { backoffDelay, sleep } from './backoff.js'
import { errorFromResponse, ZipboxError } from './errors.js'
import type { FetchLike } from './types.js'

const SDK_VERSION = '0.1.1'
const USER_AGENT = `zipbox-sdk-typescript/${SDK_VERSION}`
const RETRY_CAP_MS = 30_000

export type HttpMethod = 'GET' | 'POST' | 'PUT' | 'DELETE'

/** A request body: JSON, or raw bytes/text sent as application/octet-stream. */
export type HttpBody = { json: unknown } | { raw: string | Uint8Array }

export interface HttpRequest {
  method: HttpMethod
  url: string
  headers?: Record<string, string>
  body?: HttpBody
  /**
   * Retry retryable failures. GET requests always do; a POST does only when it is
   * safe to repeat (create carries an Idempotency-Key).
   */
  retry?: boolean
  signal?: AbortSignal | undefined
}

export interface HttpClientOptions {
  fetch: FetchLike
  maxRetries: number
  retryBaseDelayMs: number
  random: () => number
}

function isAbort(error: unknown, signal: AbortSignal | undefined): boolean {
  if (signal?.aborted === true) return true
  return error instanceof Error && error.name === 'AbortError'
}

async function readErrorBody(response: Response): Promise<unknown> {
  const text = await response.text().catch(() => '')
  if (text === '') return null
  try {
    const parsed: unknown = JSON.parse(text)
    return parsed
  } catch {
    return null
  }
}

/** fetch with the SDK's error mapping and retry policy. Never sets a deadline. */
export class HttpClient {
  constructor(private readonly options: HttpClientOptions) {}

  /** Send a request; resolves with a 2xx response or throws {@link ZipboxError}. */
  async send(request: HttpRequest): Promise<Response> {
    const retryAllowed = request.retry === true || request.method === 'GET'
    for (let attempt = 0; ; attempt++) {
      let error: ZipboxError
      try {
        const response = await this.options.fetch(request.url, this.init(request))
        if (response.ok) return response
        error = errorFromResponse({
          status: response.status,
          headers: response.headers,
          body: await readErrorBody(response)
        })
      } catch (caught) {
        if (isAbort(caught, request.signal)) throw caught
        if (caught instanceof ZipboxError) throw caught
        error = new ZipboxError({
          code: 'unavailable',
          message: caught instanceof Error ? caught.message : 'network error',
          retryable: true
        })
      }
      if (!retryAllowed || !error.retryable || attempt >= this.options.maxRetries) {
        throw error
      }
      const delay =
        error.retryAfterMs ??
        backoffDelay({
          attempt,
          baseMs: this.options.retryBaseDelayMs,
          capMs: RETRY_CAP_MS,
          random: this.options.random()
        })
      await sleep(delay, request.signal)
    }
  }

  private init(request: HttpRequest): RequestInit {
    const headers: Record<string, string> = { 'user-agent': USER_AGENT, ...request.headers }
    let body: BodyInit | undefined
    if (request.body !== undefined) {
      if ('json' in request.body) {
        headers['content-type'] = 'application/json'
        body = JSON.stringify(request.body.json)
      } else {
        headers['content-type'] = 'application/octet-stream'
        body =
          typeof request.body.raw === 'string' ? request.body.raw : new Blob([request.body.raw])
      }
    }
    return {
      method: request.method,
      headers,
      ...(body === undefined ? {} : { body }),
      ...(request.signal === undefined ? {} : { signal: request.signal })
    }
  }
}
