import { boxPath, type ClientContext, waitUntilReady } from './context.js'
import { isErrorCode, ZipboxError } from './errors.js'
import { isForkAccepted, isSandboxFork, readJson } from './guards.js'
import { Tailscale } from './tailscale.js'
import type { ForkSandboxOptions, SandboxDetail, SandboxFork } from './types.js'

/** `GET /sandbox/:id/fork`: the fork that is making box `id`. */
export async function getFork(
  ctx: ClientContext,
  id: string,
  signal?: AbortSignal
): Promise<SandboxFork> {
  const response = await ctx.api.request({ method: 'GET', path: boxPath(id, '/fork'), signal })
  return readJson(response, isSandboxFork, 'GET /sandbox/:id/fork')
}

// The error a fork that ended without its box answers: the fork's own code (a code
// this SDK does not know yet as `conflict`, with the server's message), or
// `conflict` for a cancel. Null while the fork has not ended that way.
function forkEndedError(id: string, fork: SandboxFork): ZipboxError | null {
  const failure = fork.error
  if (failure !== null && failure !== undefined) {
    // An answered verdict, whatever its code: never retryable, as in every SDK.
    return new ZipboxError({
      code: isErrorCode(failure.code) ? failure.code : 'conflict',
      message: failure.message,
      retryable: false
    })
  }
  if (fork.phase === 'cancelled') {
    return new ZipboxError({
      code: 'conflict',
      message: `the fork making box ${id} was cancelled`,
      retryable: false
    })
  }
  return null
}

/**
 * A new box that went away while it was waited on: a cancelled or failed fork
 * removes it, so the box reads `not_found` while its fork still says which. A
 * failure is raised with the fork's own code, a cancel as `conflict`. A fork that
 * could not be read (a retryable error) is no verdict: that error is raised.
 */
async function vanishedError(
  ctx: ClientContext,
  id: string,
  notFound: ZipboxError,
  signal: AbortSignal | undefined
): Promise<ZipboxError> {
  let fork: SandboxFork
  try {
    fork = await getFork(ctx, id, signal)
  } catch (error) {
    if (signal?.aborted === true) throw error
    if (error instanceof ZipboxError && error.retryable) return error
    return notFound
  }
  return forkEndedError(id, fork) ?? notFound
}

/**
 * Fork `sourceId` into a new box and return the new box's row. One Idempotency-Key
 * per call, reused on every retry, so a retried request never makes two boxes. The
 * Tailscale auth key never leaves this machine in the clear: it is not part of the
 * fork request, and is sealed to the new box's own key once that box runs.
 */
export async function forkBox(
  ctx: ClientContext,
  sourceId: string,
  options: ForkSandboxOptions
): Promise<SandboxDetail> {
  const wait = options.wait ?? true
  const authKey = options.tailscaleAuthKey
  if (authKey !== undefined) {
    if (authKey.trim() === '') {
      throw new ZipboxError({ code: 'invalid_request', message: 'the Tailscale auth key is empty' })
    }
    if (!wait) {
      throw new ZipboxError({
        code: 'invalid_request',
        message: 'tailscaleAuthKey needs wait: the key is sealed to the new box once it runs'
      })
    }
  }
  // A running source pauses for its capture: its cached address goes with it.
  ctx.boxes.invalidate(sourceId)
  const response = await ctx.api.request({
    method: 'POST',
    path: boxPath(sourceId, '/fork'),
    headers: { 'idempotency-key': crypto.randomUUID() },
    body: {
      json: {
        name: options.name,
        sourcePolicy: options.sourcePolicy,
        ...(options.private === true ? { private: true } : {})
      }
    },
    retry: true,
    signal: options.signal
  })
  const accepted = await readJson(response, isForkAccepted, 'POST /sandbox/:id/fork')
  const childId = accepted.fork.childSandboxId
  if (accepted.sandbox === null) {
    // The key found a fork that already ended and removed its box: that fork's
    // verdict, never an "unexpected response".
    throw (
      forkEndedError(childId, accepted.fork) ??
      new ZipboxError({ code: 'conflict', message: `the fork making box ${childId} ended` })
    )
  }
  // Accepted, not started: a provisioning box is never ready.
  if (!wait) return { ...accepted.sandbox, fork: accepted.fork, ready: false }
  let detail: SandboxDetail
  try {
    detail = await waitUntilReady(ctx, childId, options.signal)
  } catch (error) {
    if (!(error instanceof ZipboxError) || error.code !== 'not_found') throw error
    throw await vanishedError(ctx, childId, error, options.signal)
  }
  if (authKey !== undefined) {
    await new Tailscale(ctx, childId).connect(
      authKey,
      options.signal === undefined ? {} : { signal: options.signal }
    )
  }
  return detail
}
