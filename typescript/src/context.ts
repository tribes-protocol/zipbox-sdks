import { backoffDelay, sleep } from './backoff.js'
import { ZipboxError } from './errors.js'
import { isTransitionCancelResult, readJson } from './guards.js'
import { endedError, readinessOf } from './readiness.js'
import type { BoxTransport, ControlPlane } from './transport.js'
import type {
  SandboxDetail,
  SandboxTier,
  TransitionCancelOutcome,
  WebSocketConstructor
} from './types.js'

/** Everything a handle needs from its client. Internal. */
export interface ClientContext {
  api: ControlPlane
  boxes: BoxTransport
  retryBaseDelayMs: number
  retryMaxDelayMs: number
  pollIntervalMs: number
  pollMaxIntervalMs: number
  ptyBaseDelayMs: number
  ptyMaxDelayMs: number
  random: () => number
  WebSocket: WebSocketConstructor | undefined
}

export function boxPath(id: string, suffix = ''): string {
  return `/sandbox/${encodeURIComponent(id)}${suffix}`
}

export async function getDetail(
  ctx: ClientContext,
  id: string,
  signal?: AbortSignal
): Promise<SandboxDetail> {
  const detail = await ctx.api.getDetail(id, signal)
  ctx.boxes.seed(detail)
  // The token lives in the transport only, so logging or serializing a Sandbox never leaks it.
  const { boxToken, ...row } = detail
  void boxToken
  return row
}

// A box turns ready about a second after it serves, and nothing tells the client when,
// so each poll rung is time spent asleep after it did. Doubling overshot by up to
// several seconds; 1.25 keeps the same base, cap and jitter and reaches the cap gently.
const POLL_FACTOR = 1.25

/** The delay before the n-th re-read of a wait (0 = the first): the client's poll ladder. */
function pollDelay(ctx: ClientContext, attempt: number): number {
  return backoffDelay({
    attempt,
    baseMs: ctx.pollIntervalMs,
    capMs: ctx.pollMaxIntervalMs,
    factor: POLL_FACTOR,
    random: ctx.random()
  })
}

/**
 * Re-read the box until it is ready, or, for a private box that has not joined a
 * tailnet yet, until it is running with its own key (the caller seals an auth key to
 * it). The server's `readiness` decides: `settling` keeps reading, and `paused`,
 * `start_failed`, `deleting` and `gone` raise their answered verdict. Client-side
 * polling with backoff, never on a deadline: a box that is still booting is still
 * booting. A read that fails with a retryable error (a control-plane deploy) goes
 * on the same ladder, never sooner than its `retryAfterMs`. Abort with the signal.
 */
export async function waitUntilReady(
  ctx: ClientContext,
  id: string,
  signal?: AbortSignal
): Promise<SandboxDetail> {
  for (let attempt = 0; ; attempt++) {
    let retryAfterMs = 0
    let detail: SandboxDetail | null = null
    try {
      detail = await getDetail(ctx, id, signal)
    } catch (error) {
      if (!(error instanceof ZipboxError) || !error.retryable) throw error
      retryAfterMs = error.retryAfterMs ?? 0
    }
    if (detail !== null) {
      const ended = endedError(id, detail)
      if (ended !== null) throw ended
      if (readinessOf(detail) !== 'settling') return detail
    }
    await sleep(Math.max(retryAfterMs, pollDelay(ctx, attempt)), signal)
  }
}

/**
 * One read of the box inside a wait. A read that fails with a retryable error (a
 * control-plane deploy) is read again on the poll ladder, never sooner than its
 * `retryAfterMs`, with no ceiling; any other error is raised.
 */
export async function readDetailInWait(
  ctx: ClientContext,
  id: string,
  signal?: AbortSignal
): Promise<SandboxDetail> {
  for (let attempt = 0; ; attempt++) {
    try {
      return await getDetail(ctx, id, signal)
    } catch (error) {
      if (!(error instanceof ZipboxError) || !error.retryable) throw error
      await sleep(Math.max(error.retryAfterMs ?? 0, pollDelay(ctx, attempt)), signal)
    }
  }
}

export async function lifecycle(
  ctx: ClientContext,
  id: string,
  action: 'pause' | 'restore' | 'reset',
  signal?: AbortSignal
): Promise<void> {
  ctx.boxes.invalidate(id)
  await ctx.api.request({ method: 'POST', path: boxPath(id, `/${action}`), signal })
}

/**
 * Start a stopped box. A stop is accepted at once but its disk takes seconds to
 * minutes to land; meanwhile the box reads `stopping` and `start` answers
 * `conflict`. Wait that out, client-side with backoff, and start the box once it
 * reads `archived`. A box that comes out of the wait already starting or running
 * (a resize restores itself, a failed archive leaves it running) is started.
 * Any other `conflict` (a running box that was never stopping, a box that will
 * not restore) is raised as it came.
 */
export async function startBox(
  ctx: ClientContext,
  id: string,
  signal?: AbortSignal
): Promise<void> {
  let waited = false
  for (;;) {
    try {
      await lifecycle(ctx, id, 'restore', signal)
      return
    } catch (error) {
      if (!(error instanceof ZipboxError) || error.code !== 'conflict') throw error
      let detail = await readDetailInWait(ctx, id, signal)
      if (detail.status !== 'stopping') {
        // A second conflict after a stop was waited out: the box restored itself,
        // or it is archived and will not restore (that answer is final).
        if (waited && (detail.status === 'provisioning' || detail.status === 'running')) return
        throw error
      }
      for (let attempt = 0; detail.status === 'stopping'; attempt++) {
        await sleep(pollDelay(ctx, attempt), signal)
        detail = await readDetailInWait(ctx, id, signal)
      }
      waited = true
      if (detail.status === 'provisioning' || detail.status === 'running') return
      if (detail.status !== 'archived') throw error
    }
  }
}

/**
 * Cancel the box's state change in flight (a stop, a start, a restart, a tier
 * change, a delete that keeps a copy). Not retried: a refusal (`conflict`, past the
 * point of no return) is raised with the server's reason.
 */
export async function cancelTransition(
  ctx: ClientContext,
  id: string,
  signal?: AbortSignal
): Promise<TransitionCancelOutcome> {
  ctx.boxes.invalidate(id)
  const response = await ctx.api.request({
    method: 'POST',
    path: boxPath(id, '/transition/cancel'),
    signal
  })
  const body = await readJson(
    response,
    isTransitionCancelResult,
    'POST /sandbox/:id/transition/cancel'
  )
  return body.outcome
}

export async function setTier(
  ctx: ClientContext,
  id: string,
  tier: SandboxTier,
  signal?: AbortSignal
): Promise<void> {
  ctx.boxes.invalidate(id)
  await ctx.api.request({
    method: 'POST',
    path: boxPath(id, '/tier'),
    body: { json: { tier } },
    signal
  })
}

export async function setPrivate(
  ctx: ClientContext,
  id: string,
  enabled: boolean,
  signal?: AbortSignal
): Promise<void> {
  ctx.boxes.invalidate(id)
  await ctx.api.request({
    method: 'PUT',
    path: boxPath(id, '/private'),
    body: { json: { enabled } },
    signal
  })
}

export async function deleteBox(
  ctx: ClientContext,
  id: string,
  archive: boolean | undefined,
  signal?: AbortSignal
): Promise<void> {
  ctx.boxes.invalidate(id)
  await ctx.api.request({
    method: 'DELETE',
    path: boxPath(id),
    ...(archive === undefined ? {} : { body: { json: { archiveBeforeDestroy: archive } } }),
    signal
  })
}

/** The retry delay for the n-th consecutive failure of a resumable stream. */
export function retryDelay(ctx: ClientContext, attempt: number): number {
  return backoffDelay({
    attempt,
    baseMs: ctx.retryBaseDelayMs,
    capMs: ctx.retryMaxDelayMs,
    random: ctx.random()
  })
}
