/** Parameters of {@link backoffDelay}. */
export interface BackoffParams {
  /** 0 for the first retry. */
  attempt: number
  baseMs: number
  capMs: number
  /** A value in [0, 1). */
  random: number
}

/**
 * Exponential backoff with jitter: `min(cap, base * 2^attempt)`, scaled into
 * [50%, 100%) of that so a fleet of clients never retries in lockstep.
 */
export function backoffDelay(params: BackoffParams): number {
  const exponential = params.baseMs * 2 ** Math.min(params.attempt, 30)
  const ceiling = Math.min(params.capMs, exponential)
  return Math.round(ceiling * (0.5 + params.random * 0.5))
}

/** Resolve after `ms`, or reject with the signal's reason when it aborts first. */
export function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted === true) {
      reject(abortReason(signal))
      return
    }
    const onAbort = (): void => {
      clearTimeout(timer)
      reject(abortReason(signal))
    }
    const timer = setTimeout(() => {
      signal?.removeEventListener('abort', onAbort)
      resolve()
    }, ms)
    signal?.addEventListener('abort', onAbort, { once: true })
  })
}

function abortReason(signal: AbortSignal | undefined): Error {
  const reason: unknown = signal?.reason
  return reason instanceof Error
    ? reason
    : new DOMException('The operation was aborted.', 'AbortError')
}
