import { backoffDelay, sleep } from './backoff.js'
import { ZipboxError } from './errors.js'
import type { BoxTransport, ControlPlane } from './transport.js'
import type { SandboxDetail, SandboxTier, WebSocketConstructor } from './types.js'

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

/**
 * A private box that is running with its key but has not joined a tailnet yet: it
 * is never `ready` until it joins, which needs the caller's auth key, sealed to
 * that key.
 */
function awaitsTailnet(detail: SandboxDetail): boolean {
  return (
    detail.privateMode === true &&
    typeof detail.tailscale?.ipv4 !== 'string' &&
    detail.status === 'running' &&
    typeof detail.agentPublicKey === 'string' &&
    detail.agentPublicKey !== ''
  )
}

/**
 * Re-read the box until it is ready. Client-side polling with backoff, never on a
 * deadline: a box that is still booting is still booting. Abort with the signal.
 * A private box that has not joined a tailnet yet is returned once it is running
 * with its own key, so the caller can seal an auth key to it.
 */
export async function waitUntilReady(
  ctx: ClientContext,
  id: string,
  signal?: AbortSignal
): Promise<SandboxDetail> {
  let delay = ctx.pollIntervalMs
  for (;;) {
    const detail = await getDetail(ctx, id, signal)
    if (detail.ready || awaitsTailnet(detail)) return detail
    if (
      detail.status === 'destroyed' ||
      detail.status === 'stopped' ||
      detail.status === 'deleting'
    ) {
      throw new ZipboxError({
        code: 'conflict',
        message: `box ${id} is ${detail.status} and will not become ready`
      })
    }
    await sleep(Math.round(delay * (0.75 + ctx.random() * 0.5)), signal)
    delay = Math.min(ctx.pollMaxIntervalMs, delay * 1.5)
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
      let detail = await getDetail(ctx, id, signal)
      if (detail.status !== 'stopping') {
        // A second conflict after a stop was waited out: the box restored itself,
        // or it is archived and will not restore (that answer is final).
        if (waited && (detail.status === 'provisioning' || detail.status === 'running')) return
        throw error
      }
      let delay = ctx.pollIntervalMs
      while (detail.status === 'stopping') {
        await sleep(Math.round(delay * (0.75 + ctx.random() * 0.5)), signal)
        delay = Math.min(ctx.pollMaxIntervalMs, delay * 1.5)
        detail = await getDetail(ctx, id, signal)
      }
      waited = true
      if (detail.status === 'provisioning' || detail.status === 'running') return
      if (detail.status !== 'archived') throw error
    }
  }
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
