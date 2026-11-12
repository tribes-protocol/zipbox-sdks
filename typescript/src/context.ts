import { backoffDelay, sleep } from './backoff.js'
import { ZipboxError } from './errors.js'
import { isSandboxDetail, isTransitionCancelResult, readJson } from './guards.js'
import { endedError, pauseEndedError, readinessOf } from './readiness.js'
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

/** Cache a detail's address and token, and return the row without the token. */
export function remember(ctx: ClientContext, detail: SandboxDetail): SandboxDetail {
  ctx.boxes.seed(detail)
  // The token lives in the transport only, so logging or serializing a Sandbox never leaks it.
  const { boxToken, ...row } = detail
  void boxToken
  return row
}

export async function getDetail(
  ctx: ClientContext,
  id: string,
  signal?: AbortSignal
): Promise<SandboxDetail> {
  return remember(ctx, await ctx.api.getDetail(id, signal))
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
 * What a wait waits for: the box answering (`ready`), or its Pause taking effect
 * (`paused`). It is the `until` of `GET /sandbox/:id/wait`.
 */
export type WaitTarget = 'ready' | 'paused'

/** Whether a read already shows what the caller waits for. */
function reached(detail: SandboxDetail, until: WaitTarget): boolean {
  return readinessOf(detail) === until
}

/**
 * One read inside a wait: the detail once the wait is over, null while the box may
 * still get there. A box that will not get there by itself raises its answered
 * verdict. A wait for `ready` also ends on `awaits_tailnet` (a private box running
 * with its key: the caller seals an auth key to it).
 */
function settle(id: string, detail: SandboxDetail, until: WaitTarget): SandboxDetail | null {
  const ended = until === 'ready' ? endedError(id, detail) : pauseEndedError(id, detail)
  if (ended !== null) throw ended
  if (until === 'ready') return readinessOf(detail) === 'settling' ? null : detail
  return reached(detail, until) ? detail : null
}

async function readWait(
  ctx: ClientContext,
  id: string,
  until: WaitTarget,
  signal: AbortSignal | undefined
): Promise<SandboxDetail> {
  const response = await ctx.api.request({
    method: 'GET',
    path: boxPath(id, `/wait?until=${until}`),
    signal
  })
  return remember(ctx, await readJson(response, isSandboxDetail, 'GET /sandbox/:id/wait'))
}

/**
 * Wait until the box reaches `until`, on `GET /sandbox/:id/wait`: the server holds
 * the request until the box gets there (pushed when the host answers, no polling),
 * until it cannot get there by itself, or until its hold limit passes. A held
 * answer that is still settling is asked again at once: the server did the
 * waiting. There is no deadline; abort with the signal. A read that fails with a
 * retryable error goes on the poll ladder, never sooner than its `retryAfterMs`.
 * An API that predates `/wait` answers it 404 on a box that exists: then this is
 * the poll on `GET /sandbox/:id` it replaced, whose 404 is the box's own.
 */
export async function waitUntil(
  ctx: ClientContext,
  id: string,
  until: WaitTarget,
  signal?: AbortSignal
): Promise<SandboxDetail> {
  for (let attempt = 0; ; ) {
    let retryAfterMs = 0
    let detail: SandboxDetail | null = null
    try {
      detail = await readWait(ctx, id, until, signal)
    } catch (error) {
      if (!(error instanceof ZipboxError)) throw error
      if (error.code === 'not_found') return pollUntil(ctx, id, until, signal)
      if (!error.retryable) throw error
      retryAfterMs = error.retryAfterMs ?? 0
    }
    if (detail !== null) {
      const done = settle(id, detail, until)
      if (done !== null) return done
      attempt = 0
      // No delay: the server held this request. The zero sleep only honours an abort.
      await sleep(0, signal)
      continue
    }
    await sleep(Math.max(retryAfterMs, pollDelay(ctx, attempt++)), signal)
  }
}

/** {@link waitUntil} for `ready`: what `create`, `getOrCreate`, `waitReady` and `fork` wait on. */
export function waitUntilReady(
  ctx: ClientContext,
  id: string,
  signal?: AbortSignal
): Promise<SandboxDetail> {
  return waitUntil(ctx, id, 'ready', signal)
}

/**
 * The wait for an API without `/wait`: re-read `GET /sandbox/:id` with backoff,
 * never on a deadline, until the box reaches `until` or will not get there by
 * itself. A read that fails with a retryable error (a control-plane deploy) goes on
 * the same ladder, never sooner than its `retryAfterMs`.
 */
async function pollUntil(
  ctx: ClientContext,
  id: string,
  until: WaitTarget,
  signal: AbortSignal | undefined
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
      const done = settle(id, detail, until)
      if (done !== null) return done
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

function parseBody(text: string): unknown {
  try {
    return JSON.parse(text)
  } catch {
    return undefined
  }
}

/**
 * Send a lifecycle verb. The answer is the box's detail: its final state (200) when
 * the host answered within the server's budget, or its state in transition (202).
 * Null for an answer that carries no detail.
 */
export async function lifecycle(
  ctx: ClientContext,
  id: string,
  action: 'pause' | 'resume' | 'reset',
  signal?: AbortSignal
): Promise<SandboxDetail | null> {
  ctx.boxes.invalidate(id)
  const response = await ctx.api.request({
    method: 'POST',
    path: boxPath(id, `/${action}`),
    signal
  })
  const body = parseBody(await response.text())
  return isSandboxDetail(body) ? remember(ctx, body) : null
}

/** The accepted detail when it already shows `until`, else the wait for it. */
function finish(
  ctx: ClientContext,
  id: string,
  until: WaitTarget,
  accepted: SandboxDetail | null,
  signal: AbortSignal | undefined
): Promise<SandboxDetail> {
  if (accepted !== null && reached(accepted, until)) return Promise.resolve(accepted)
  return waitUntil(ctx, id, until, signal)
}

/**
 * Pause the box and return it paused: at once when the server answered with the
 * final state (200), else after waiting for the Pause to take effect (202).
 */
export async function pauseBox(
  ctx: ClientContext,
  id: string,
  signal?: AbortSignal
): Promise<SandboxDetail> {
  return finish(ctx, id, 'paused', await lifecycle(ctx, id, 'pause', signal), signal)
}

/**
 * Send Resume, and return the detail it answered with (null when the box came back
 * by itself). A Pause the host has not answered yet leaves the box `stopping`, and
 * Resume answers `conflict` meanwhile. Wait that out, client-side with backoff, and
 * resume the box once it reads `paused`. A box that comes out of the wait already
 * starting or running (a resize restores itself, a failed pause leaves it running)
 * is not resumed again. Any other `conflict` (a running box that was never
 * stopping, a box that will not resume) is raised as it came.
 */
export async function requestResume(
  ctx: ClientContext,
  id: string,
  signal?: AbortSignal
): Promise<SandboxDetail | null> {
  // In parallel with the resume, never waited for: the first call after it reuses
  // the connection instead of paying TCP + TLS (conformance README, pre-connect).
  const preconnect = ctx.boxes.preconnect(id)
  try {
    const accepted = await sendResume(ctx, id, signal)
    ctx.boxes.settlePreconnect(preconnect, accepted)
    return accepted
  } catch (error) {
    preconnect?.controller.abort()
    throw error
  }
}

async function sendResume(
  ctx: ClientContext,
  id: string,
  signal?: AbortSignal
): Promise<SandboxDetail | null> {
  let waited = false
  for (;;) {
    try {
      return await lifecycle(ctx, id, 'resume', signal)
    } catch (error) {
      if (!(error instanceof ZipboxError) || error.code !== 'conflict') throw error
      let detail = await readDetailInWait(ctx, id, signal)
      if (detail.status !== 'stopping') {
        // A second conflict after a pause was waited out: the box came back by itself,
        // or it is paused and will not resume (that answer is final).
        if (waited && (detail.status === 'provisioning' || detail.status === 'running')) {
          return null
        }
        throw error
      }
      for (let attempt = 0; detail.status === 'stopping'; attempt++) {
        await sleep(pollDelay(ctx, attempt), signal)
        detail = await readDetailInWait(ctx, id, signal)
      }
      waited = true
      if (detail.status === 'provisioning' || detail.status === 'running') return null
      if (detail.status !== 'paused') throw error
    }
  }
}

/**
 * Resume the box and return it ready: at once when the server answered with the
 * final state (200), else after waiting for it (202, or a Pause still landing, see
 * {@link requestResume}). Credit-gated: `insufficient_credits` when the balance is dry.
 */
export async function resumeBox(
  ctx: ClientContext,
  id: string,
  signal?: AbortSignal
): Promise<SandboxDetail> {
  return finish(ctx, id, 'ready', await requestResume(ctx, id, signal), signal)
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
