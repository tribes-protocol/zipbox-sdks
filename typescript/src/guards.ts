import { ZipboxError } from './errors.js'
import type {
  CommandRecord,
  FileEntry,
  JsonObject,
  SandboxDetail,
  SandboxFork,
  SandboxInfo,
  TransitionCancelOutcome
} from './types.js'

// Light runtime checks at the response boundary. They verify the fields the SDK
// relies on, so a proxy error page or a renamed field fails loudly here instead of
// as an `undefined` three calls later.

export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

export function isJsonObject(value: unknown): value is JsonObject {
  return isRecord(value)
}

export function isSandboxInfo(value: unknown): value is SandboxInfo {
  return isRecord(value) && typeof value.id === 'string' && typeof value.status === 'string'
}

export function isSandboxDetail(value: unknown): value is SandboxDetail {
  return isSandboxInfo(value) && typeof Reflect.get(value, 'ready') === 'boolean'
}

export function isSandboxFork(value: unknown): value is SandboxFork {
  return (
    isRecord(value) &&
    typeof value.operationId === 'string' &&
    typeof value.childSandboxId === 'string' &&
    typeof value.phase === 'string'
  )
}

/** `202` from `POST /sandbox/:id/fork`. */
export interface ForkAccepted {
  operationId: string
  // Null once a cancelled or failed fork removed the box (a replay of the same
  // Idempotency-Key answers its tombstone).
  sandbox: SandboxInfo | null
  fork: SandboxFork
}

export function isForkAccepted(value: unknown): value is ForkAccepted {
  return (
    isRecord(value) &&
    typeof value.operationId === 'string' &&
    (value.sandbox === null || isSandboxInfo(value.sandbox)) &&
    isSandboxFork(value.fork)
  )
}

export interface TransitionCancelResult {
  outcome: TransitionCancelOutcome
}

export function isTransitionCancelResult(value: unknown): value is TransitionCancelResult {
  return (
    isRecord(value) &&
    (value.outcome === 'cleared' || value.outcome === 'cancelling' || value.outcome === 'none')
  )
}

export function isCommandRecord(value: unknown): value is CommandRecord {
  return isRecord(value) && typeof value.id === 'string' && typeof value.state === 'string'
}

function isFileEntry(value: unknown): value is FileEntry {
  return isRecord(value) && typeof value.name === 'string' && typeof value.kind === 'string'
}

export interface FileListing {
  path: string
  entries: FileEntry[]
  truncated: boolean
}

export function isFileListing(value: unknown): value is FileListing {
  return (
    isRecord(value) &&
    typeof value.path === 'string' &&
    Array.isArray(value.entries) &&
    value.entries.every(isFileEntry)
  )
}

/** Read a JSON body and check it, or throw an `unavailable` error naming what was expected. */
export async function readJson<T>(
  response: Response,
  guard: (value: unknown) => value is T,
  what: string
): Promise<T> {
  // A connection the server closed mid-body rejects the read (undici: "terminated"):
  // that is the usual retryable network error, never a raw TypeError.
  const text = await response.text().catch((caught: unknown) => {
    throw new ZipboxError({
      code: 'unavailable',
      message: caught instanceof Error ? caught.message : 'network error',
      status: response.status,
      retryable: true
    })
  })
  let parsed: unknown
  try {
    parsed = JSON.parse(text)
  } catch {
    parsed = undefined
  }
  if (!guard(parsed)) {
    throw new ZipboxError({
      code: 'unavailable',
      message: `unexpected response for ${what}`,
      status: response.status,
      retryable: false
    })
  }
  return parsed
}

export function isArrayOf<T>(guard: (value: unknown) => value is T) {
  return (value: unknown): value is T[] => Array.isArray(value) && value.every(guard)
}
