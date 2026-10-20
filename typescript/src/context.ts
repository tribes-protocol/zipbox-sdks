import { backoffDelay, sleep } from './backoff.js'
import { ZipboxError } from './errors.js'
import type { BoxTransport, ControlPlane } from './transport.js'
import type { SandboxDetail, SandboxTier, WebSocketConstructor } from './types.js'

/** Everything a handle needs from its client. Internal. */
export interface ClientContext {
  api: ControlPlane
  boxes: BoxTransport
  retryBaseDelayMs: number
  pollIntervalMs: number
  random: () => number
  WebSocket: WebSocketConstructor | undefined
}

const POLL_CAP_MS = 10_000
const RETRY_CAP_MS = 30_000

function boxPath(id: string, suffix = ''): string {
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
 * Re-read the box until it is ready. Client-side polling with backoff, never on a
 * deadline: a box that is still booting is still booting. Abort with the signal.
 */
export async function waitUntilReady(
  ctx: ClientContext,
  id: string,
  signal?: AbortSignal
): Promise<SandboxDetail> {
  let delay = ctx.pollIntervalMs
  for (;;) {
    const detail = await getDetail(ctx, id, signal)
    if (detail.ready) return detail
    if (detail.status === 'destroyed' || detail.status === 'stopped') {
      throw new ZipboxError({
        code: 'conflict',
        message: `box ${id} is ${detail.status} and will not become ready`
      })
    }
    await sleep(Math.round(delay * (0.75 + ctx.random() * 0.5)), signal)
    delay = Math.min(POLL_CAP_MS, delay * 1.5)
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
    capMs: RETRY_CAP_MS,
    random: ctx.random()
  })
}
