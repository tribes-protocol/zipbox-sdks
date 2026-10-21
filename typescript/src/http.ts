import { backoffDelay, sleep } from './backoff.js'
import { errorFromResponse, ZipboxError } from './errors.js'
import type { FetchLike } from './types.js'

const SDK_VERSION = '0.1.2'
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

// Error codes that can only come from establishing the connection: DNS, refusal,
// unreachable routes, and the TLS handshake (alerts and certificate verification).
// Node's fetch (undici) and Bun both put them on the error or its `cause`.
// ECONNRESET and "socket closed" are deliberately absent — those can follow a
// request that was already sent.
const UNSENT_CODES = new Set([
  'ECONNREFUSED',
  'ENOTFOUND',
  'EAI_AGAIN',
  'EHOSTUNREACH',
  'ENETUNREACH',
  'EHOSTDOWN',
  'EPROTO',
  'UND_ERR_CONNECT_TIMEOUT',
  'ConnectionRefused',
  'FailedToOpenSocket',
  'UNABLE_TO_GET_ISSUER_CERT',
  'UNABLE_TO_GET_ISSUER_CERT_LOCALLY',
  'UNABLE_TO_VERIFY_LEAF_SIGNATURE',
  'DEPTH_ZERO_SELF_SIGNED_CERT',
  'SELF_SIGNED_CERT_IN_CHAIN',
  'CERT_HAS_EXPIRED',
  'CERT_NOT_YET_VALID'
])
const UNSENT_CODE_PREFIXES = ['ERR_SSL_', 'ERR_TLS_', 'TLSV1_ALERT_', 'SSL_']

function errorCode(value: unknown): string | null {
  if (typeof value !== 'object' || value === null || !('code' in value)) return null
  return typeof value.code === 'string' ? value.code : null
}

/**
 * Whether a fetch failure provably happened before any request byte left: the
 * connection or its TLS handshake failed. A fresh box name can refuse TLS for a while
 * after the box is ready, until its certificate lands, and nothing was sent — so
 * every method may retry it, POSTs included.
 */
export function isUnsentFailure(error: unknown): boolean {
  let current: unknown = error
  for (let depth = 0; depth < 5 && current !== undefined && current !== null; depth++) {
    const code = errorCode(current)
    if (
      code !== null &&
      (UNSENT_CODES.has(code) || UNSENT_CODE_PREFIXES.some((prefix) => code.startsWith(prefix)))
    ) {
      return true
    }
    current = current instanceof Error ? current.cause : undefined
  }
  return false
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

  /**
   * Send a request; resolves with a 2xx response or throws {@link ZipboxError}.
   *
   * A failure to connect or to complete the TLS handshake is retried on EVERY method
   * and with no attempt ceiling (CLAUDE.md §0): nothing was sent, so nothing can
   * repeat, and a box's certificate can trail its readiness. The ladder is capped
   * and jittered; the caller's `signal` is the only bound.
   */
  async send(request: HttpRequest): Promise<Response> {
    const retryAllowed = request.retry === true || request.method === 'GET'
    let unsentAttempt = 0
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
        if (isUnsentFailure(caught)) {
          await sleep(
            backoffDelay({
              attempt: unsentAttempt++,
              baseMs: this.options.retryBaseDelayMs,
              capMs: RETRY_CAP_MS,
              random: this.options.random()
            }),
            request.signal
          )
          attempt--
          continue
        }
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
