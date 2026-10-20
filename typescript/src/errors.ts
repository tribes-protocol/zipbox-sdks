/** Every public error code. The code is the contract; the message is for humans. */
export type ZipboxErrorCode =
  | 'invalid_request'
  | 'unauthorized'
  | 'forbidden'
  | 'not_found'
  | 'insufficient_credits'
  | 'conflict'
  | 'rate_limited'
  | 'box_not_running'
  | 'unavailable'

const ERROR_CODES: readonly ZipboxErrorCode[] = [
  'invalid_request',
  'unauthorized',
  'forbidden',
  'not_found',
  'insufficient_credits',
  'conflict',
  'rate_limited',
  'box_not_running',
  'unavailable'
]

const DOCS_ERRORS_URL = 'https://zipbox.ai/docs/errors'

/** Constructor parameters of {@link ZipboxError}. */
export interface ZipboxErrorInit {
  code: ZipboxErrorCode
  message: string
  status?: number | null
  requestId?: string | null
  retryable?: boolean
  retryAfterMs?: number | null
}

/** The one error every SDK call raises. */
export class ZipboxError extends Error {
  /** Public error code; see https://zipbox.ai/docs/errors. */
  readonly code: ZipboxErrorCode
  /** HTTP status, when the failure was an HTTP response. */
  readonly status: number | null
  readonly requestId: string | null
  /** True when the same request may succeed later. */
  readonly retryable: boolean
  readonly retryAfterMs: number | null
  /** `https://zipbox.ai/docs/errors#<code>`. */
  readonly docsUrl: string

  constructor(init: ZipboxErrorInit) {
    super(init.message)
    this.name = 'ZipboxError'
    this.code = init.code
    this.status = init.status ?? null
    this.requestId = init.requestId ?? null
    this.retryable = init.retryable ?? isRetryableCode(init.code)
    this.retryAfterMs = init.retryAfterMs ?? null
    this.docsUrl = `${DOCS_ERRORS_URL}#${init.code}`
  }

  /** A plain object view, for logging. */
  toJSON(): Record<string, string | number | boolean | null> {
    return {
      name: this.name,
      code: this.code,
      message: this.message,
      status: this.status,
      requestId: this.requestId,
      retryable: this.retryable,
      retryAfterMs: this.retryAfterMs,
      docsUrl: this.docsUrl
    }
  }
}

function isRetryableCode(code: ZipboxErrorCode): boolean {
  switch (code) {
    case 'rate_limited':
    case 'unavailable':
    case 'box_not_running':
      return true
    case 'invalid_request':
    case 'unauthorized':
    case 'forbidden':
    case 'not_found':
    case 'insufficient_credits':
    case 'conflict':
      return false
  }
}

/** The status → code mapping for a response that carried no error envelope. */
export function codeForStatus(status: number): ZipboxErrorCode {
  switch (status) {
    case 400:
    case 413:
    case 422:
      return 'invalid_request'
    case 401:
      return 'unauthorized'
    case 402:
      return 'insufficient_credits'
    case 403:
      return 'forbidden'
    case 404:
      return 'not_found'
    case 409:
      return 'conflict'
    case 429:
      return 'rate_limited'
    case 502:
      return 'box_not_running'
    default:
      return status >= 500 ? 'unavailable' : 'invalid_request'
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function isErrorCode(value: unknown): value is ZipboxErrorCode {
  return typeof value === 'string' && ERROR_CODES.some((code) => code === value)
}

/** `Retry-After` in seconds or an HTTP date, as milliseconds. */
export function parseRetryAfter(header: string | null, nowMs: number): number | null {
  if (header === null || header.trim() === '') return null
  const seconds = Number(header)
  if (Number.isFinite(seconds)) return Math.max(0, Math.round(seconds * 1000))
  const at = Date.parse(header)
  return Number.isNaN(at) ? null : Math.max(0, at - nowMs)
}

/** Build the error for a non-2xx response from its status, headers and parsed body. */
export function errorFromResponse(params: {
  status: number
  headers: Headers
  body: unknown
}): ZipboxError {
  const retryAfterHeader = parseRetryAfter(params.headers.get('retry-after'), Date.now())
  const headerRequestId = params.headers.get('x-request-id') ?? params.headers.get('cf-ray')
  const envelope = isRecord(params.body) ? params.body.error : undefined
  if (isRecord(envelope) && isErrorCode(envelope.code)) {
    const retryAfterMs =
      typeof envelope.retryAfterMs === 'number' ? envelope.retryAfterMs : retryAfterHeader
    return new ZipboxError({
      code: envelope.code,
      message: typeof envelope.message === 'string' ? envelope.message : envelope.code,
      status: params.status,
      requestId: typeof envelope.requestId === 'string' ? envelope.requestId : headerRequestId,
      ...(typeof envelope.retryable === 'boolean' ? { retryable: envelope.retryable } : {}),
      retryAfterMs: retryAfterHeader ?? retryAfterMs
    })
  }
  const code = codeForStatus(params.status)
  const message =
    typeof envelope === 'string' && envelope.length > 0 ? envelope : `HTTP ${params.status}`
  return new ZipboxError({
    code,
    message,
    status: params.status,
    requestId: headerRequestId,
    retryAfterMs: retryAfterHeader
  })
}
