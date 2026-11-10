import { ZipboxError } from './errors.js'
import type { SandboxDetail, SandboxReadiness } from './types.js'

const READINESS: readonly SandboxReadiness[] = [
  'ready',
  'settling',
  'awaits_tailnet',
  'paused',
  'start_failed',
  'deleting',
  'gone'
]

function isReadiness(value: unknown): value is SandboxReadiness {
  return typeof value === 'string' && READINESS.some((known) => known === value)
}

/**
 * A private box that is running with its key but has not joined a tailnet yet. Only
 * the fallback below reads it: the server answers `awaits_tailnet` itself.
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
 * Whether the box will answer, as the server decided it (`readiness`). A control
 * plane that predates the field gets the rule this SDK used before it: ready,
 * private and waiting for a tailnet, ended (`destroyed`, `stopped`, `deleting`),
 * or still settling. The fallback can be removed once every control plane sends
 * `readiness`.
 */
export function readinessOf(detail: SandboxDetail): SandboxReadiness {
  return isReadiness(detail.readiness) ? detail.readiness : fallbackReadiness(detail)
}

function fallbackReadiness(detail: SandboxDetail): SandboxReadiness {
  if (detail.ready) return 'ready'
  if (awaitsTailnet(detail)) return 'awaits_tailnet'
  if (detail.status === 'destroyed' || detail.status === 'stopped') return 'gone'
  if (detail.status === 'deleting') return 'deleting'
  // Every other status, a later one this SDK does not know included, may still come up.
  return 'settling'
}

/** The remedy for a paused box: start it, unless the server says it cannot start now. */
function pausedRemedy(detail: SandboxDetail): string {
  const actions = detail.allowedActions
  return actions === undefined || actions === null || actions.includes('start')
    ? 'start it with sandboxes.start'
    : 'it cannot be started now; see its allowedActions'
}

/**
 * The error a wait raises for a box that will not become ready by itself, or null
 * while it may (`ready`, `settling`, `awaits_tailnet`). Each is an answered verdict,
 * never retryable: nothing but the caller starts the box again.
 */
export function endedError(id: string, detail: SandboxDetail): ZipboxError | null {
  const readiness = readinessOf(detail)
  switch (readiness) {
    case 'ready':
    case 'settling':
    case 'awaits_tailnet':
      return null
    case 'paused':
      return new ZipboxError({
        code: 'box_not_running',
        message: `box ${id} is paused and nothing will start it; ${pausedRemedy(detail)}`,
        retryable: false
      })
    case 'start_failed':
      return new ZipboxError({
        code: 'start_failed',
        message:
          detail.status === 'stopped'
            ? `box ${id} failed to start and its name was released; create it again`
            : `box ${id} failed to start; start it again with sandboxes.start`,
        retryable: false
      })
    case 'deleting':
    case 'gone':
      return new ZipboxError({
        code: 'conflict',
        message: `box ${id} is ${detail.status} and will not become ready`,
        retryable: false
      })
  }
}

/**
 * The error a box call raises on a detail it cannot call: a private box that has not
 * joined a tailnet answers only once the owner's auth key joins it
 * (`tailnet_not_connected`); anything else is `box_not_running`, retryable only
 * while the box is settling.
 */
export function boxNotReadyError(id: string, detail: SandboxDetail): ZipboxError {
  const stated = isReadiness(detail.readiness) ? detail.readiness : null
  const readiness = stated ?? fallbackReadiness(detail)
  if (readiness === 'awaits_tailnet') {
    return new ZipboxError({
      code: 'tailnet_not_connected',
      message: `box ${id} is private and has not joined a tailnet yet; connect it with box.tailscale.connect`,
      retryable: false
    })
  }
  return new ZipboxError({
    code: 'box_not_running',
    message: `box ${id} is not ready (status: ${detail.status}); start it with sandboxes.start`,
    // Without `readiness` (an older control plane), the rule before it: booting or
    // restarting comes back by itself, and nothing else does.
    retryable:
      stated === null
        ? detail.status === 'provisioning' || detail.status === 'running'
        : stated === 'settling'
  })
}
