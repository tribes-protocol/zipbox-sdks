import { backoffDelay, sleep } from './backoff.js'
import { errorFromResponse, ZipboxError } from './errors.js'
import type { FetchLike } from './types.js'

const SDK_VERSION = '0.2.0'
const USER_AGENT = `zipbox-sdk-typescript/${SDK_VERSION}`

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
  /**
   * Throw an unsent failure as it came instead of retrying it in place. A box request
   * sets it: its origin can move, so the box is re-read before the retry.
   */
  raiseUnsent?: boolean
  /**
   * Turn the response into the one the request answers, before its status is read: a
   * streamed create's last line stands for its answer. A failure here is a sent
   * failure, retried like a dropped connection.
   */
  unwrap?: (response: Response) => Promise<Response>
  signal?: AbortSignal | undefined
}

export interface HttpClientOptions {
  fetch: FetchLike
  maxRetries: number
  retryBaseDelayMs: number
  retryMaxDelayMs: number
  random: () => number
}

function isAbort(error: unknown, signal: AbortSignal | undefined): boolean {
  if (signal?.aborted === true) return true
  return error instanceof Error && error.name === 'AbortError'
}

// Error codes that can only come from establishing the connection. Node's fetch
// (undici) and Bun both put them on the error or its `cause`. ECONNRESET and
// "socket closed" are deliberately absent from both sets — those can follow a
// request that was already sent. The one ECONNRESET that cannot is matched by
// its message below.
//
// Connecting itself failed: DNS, a refused or unroutable address, a connect
// timeout. Nothing at the address answered.
const CONNECT_FAILURE_CODES = new Set([
  'ECONNREFUSED',
  'ENOTFOUND',
  'EAI_AGAIN',
  'EHOSTUNREACH',
  'ENETUNREACH',
  'EHOSTDOWN',
  'UND_ERR_CONNECT_TIMEOUT',
  'ConnectionRefused',
  'FailedToOpenSocket'
])
// The connection was made and the TLS handshake failed: alerts and certificate
// verification. The peer answered, so the address IS reachable; what is missing
// is its certificate, which a fresh or newly private box grows into.
const TLS_FAILURE_CODES = new Set([
  'EPROTO',
  'UNABLE_TO_GET_ISSUER_CERT',
  'UNABLE_TO_GET_ISSUER_CERT_LOCALLY',
  'UNABLE_TO_VERIFY_LEAF_SIGNATURE',
  'DEPTH_ZERO_SELF_SIGNED_CERT',
  'SELF_SIGNED_CERT_IN_CHAIN',
  'CERT_HAS_EXPIRED',
  'CERT_NOT_YET_VALID'
])
const TLS_FAILURE_CODE_PREFIXES = ['ERR_SSL_', 'ERR_TLS_', 'TLSV1_ALERT_', 'SSL_']
// Node's TLS socket, when the peer closes before the handshake completes, raises
// ECONNRESET "Client network socket disconnected before secure TLS connection was
// established". No request byte can leave before the handshake, so it is unsent. A
// reset after the send reads "read ECONNRESET" and stays sent.
const HANDSHAKE_EOF_MESSAGE = 'before secure TLS connection was established'

function errorCode(value: unknown): string | null {
  if (typeof value !== 'object' || value === null || !('code' in value)) return null
  return typeof value.code === 'string' ? value.code : null
}

// The first error code on the error or its `cause` chain that `matches`.
function failureCode(error: unknown, matches: (code: string) => boolean): string | null {
  let current: unknown = error
  for (let depth = 0; depth < 5 && current !== undefined && current !== null; depth++) {
    const code = errorCode(current)
    if (code !== null && matches(code)) return code
    current = current instanceof Error ? current.cause : undefined
  }
  return null
}

/** Whether a fetch failure was the TLS handshake: the peer answered, its certificate did not check out. */
export function isTlsFailure(error: unknown): boolean {
  return (
    failureCode(
      error,
      (code) =>
        TLS_FAILURE_CODES.has(code) ||
        TLS_FAILURE_CODE_PREFIXES.some((prefix) => code.startsWith(prefix))
    ) !== null
  )
}

// Whether the error or its `cause` chain is the peer ending the TLS handshake.
function endedBeforeHandshake(error: unknown): boolean {
  let current: unknown = error
  for (let depth = 0; depth < 5 && current instanceof Error; depth++) {
    if (errorCode(current) === 'ECONNRESET' && current.message.includes(HANDSHAKE_EOF_MESSAGE)) {
      return true
    }
    current = current.cause
  }
  return false
}

/**
 * Whether a fetch failure provably happened before any request byte left: the
 * connection or its TLS handshake failed. A fresh box name can refuse TLS for a while
 * after the box is ready, until its certificate lands, and nothing was sent — so
 * every method may retry it, POSTs included.
 */
export function isUnsentFailure(error: unknown): boolean {
  return (
    failureCode(error, (code) => CONNECT_FAILURE_CODES.has(code)) !== null ||
    isTlsFailure(error) ||
    endedBeforeHandshake(error)
  )
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
   * and jittered; the caller's `signal` is the only bound. With `raiseUnsent` it is
   * thrown instead, for the caller to retry on its own ladder ({@link unsentDelay}).
   */
  async send(request: HttpRequest): Promise<Response> {
    const retryAllowed = request.retry === true || request.method === 'GET'
    let unsentAttempt = 0
    for (let attempt = 0; ; attempt++) {
      let error: ZipboxError
      try {
        const fetched = await this.options.fetch(request.url, this.init(request))
        const response = request.unwrap === undefined ? fetched : await request.unwrap(fetched)
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
          if (request.raiseUnsent === true) throw caught
          await sleep(this.unsentDelay(unsentAttempt++), request.signal)
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
          capMs: this.options.retryMaxDelayMs,
          random: this.options.random()
        })
      await sleep(delay, request.signal)
    }
  }

  /** The delay before the n-th retry (0 = the first) of a request nothing was sent for. */
  unsentDelay(attempt: number): number {
    return backoffDelay({
      attempt,
      baseMs: this.options.retryBaseDelayMs,
      capMs: this.options.retryMaxDelayMs,
      random: this.options.random()
    })
  }

  /**
   * One GET with a deadline and no retry: does anything answer at `url`? Any HTTP
   * response, whatever its status, is an answer, and so is a failed TLS handshake:
   * the box answered, it only lacks its certificate yet, which {@link send} waits
   * out. Used only to tell a private box this device cannot reach from a box that
   * is merely slow, so a call fails fast instead of hanging on an address that is
   * not routed here.
   */
  async answers(url: string, timeoutMs: number, signal?: AbortSignal): Promise<boolean> {
    const deadline = new AbortController()
    const timer = setTimeout(() => {
      deadline.abort()
    }, timeoutMs)
    try {
      const response = await this.options.fetch(url, {
        method: 'GET',
        headers: { 'user-agent': USER_AGENT },
        signal: signal === undefined ? deadline.signal : AbortSignal.any([signal, deadline.signal])
      })
      await response.body?.cancel()
      return true
    } catch (caught) {
      if (signal?.aborted === true) throw caught
      return isTlsFailure(caught)
    } finally {
      clearTimeout(timer)
    }
  }

  /**
   * Open a connection to `url`'s origin on the shared fetch pool, ahead of the first
   * call that needs it: one unauthenticated GET whose body is read so the connection
   * returns to the pool. Never awaited by the caller, never retried; any failure is
   * dropped (the next call connects as it always does). Aborting `signal` drops it.
   */
  preconnect(url: string, signal: AbortSignal): void {
    void this.options
      .fetch(url, { method: 'GET', headers: { 'user-agent': USER_AGENT }, signal })
      .then(async (response) => {
        await response.arrayBuffer()
      })
      .catch(() => undefined)
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
