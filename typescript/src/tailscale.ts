import { boxPath, type ClientContext, getDetail } from './context.js'
import { sealBoxEnvelope, tailscaleEnvelopeInfo } from './envelope.js'
import { ZipboxError } from './errors.js'
import { isRecord, readJson } from './guards.js'
import type { CallOptions, SandboxDetail, TailscaleStatus } from './types.js'

interface TailscaleResponse {
  tailscale: TailscaleStatus | null
}

function isTailscaleResponse(value: unknown): value is TailscaleResponse {
  return (
    isRecord(value) &&
    (value.tailscale === null ||
      (isRecord(value.tailscale) && typeof value.tailscale.pending === 'boolean'))
  )
}

// The box seals to the key it holds now; one it does not hold yet cannot open it.
function sealableKey(id: string, detail: SandboxDetail): string {
  const key = detail.agentPublicKey
  const started = detail.status === 'provisioning' || detail.status === 'running'
  if (!started || typeof key !== 'string' || key === '') {
    throw new ZipboxError({
      code: 'box_not_running',
      message: `box ${id} has not started (status: ${detail.status}); start it, then connect it to Tailscale`,
      retryable: started
    })
  }
  return key
}

/**
 * The box on your tailnet. The auth key is sealed on this machine to the box's own
 * key and only the box can open it: Zipbox relays it once and never stores or sees
 * it. Use a one-off, pre-approved key with expiry turned off.
 */
export class Tailscale {
  constructor(
    private readonly ctx: ClientContext,
    private readonly sandboxId: string
  ) {}

  /** The box's tailnet status, or null when it never joined one and no key is pending. */
  async status(options: CallOptions = {}): Promise<TailscaleStatus | null> {
    const response = await this.ctx.api.request({
      method: 'GET',
      path: boxPath(this.sandboxId, '/tailscale'),
      signal: options.signal
    })
    return (await readJson(response, isTailscaleResponse, 'GET /sandbox/:id/tailscale')).tailscale
  }

  /**
   * Join the box to your tailnet with a Tailscale auth key. Resolves once Zipbox has
   * accepted the sealed key for delivery; watch `status()` for the join. Reads the
   * box's key right before sealing and re-seals once if it changed meanwhile.
   */
  async connect(authKey: string, options: CallOptions = {}): Promise<void> {
    if (authKey.trim() === '') {
      throw new ZipboxError({ code: 'invalid_request', message: 'the Tailscale auth key is empty' })
    }
    for (let attempt = 0; ; attempt++) {
      const detail = await getDetail(this.ctx, this.sandboxId, options.signal)
      const agentPublicKey = sealableKey(this.sandboxId, detail)
      const envelope = await sealBoxEnvelope({
        agentPublicKey,
        info: tailscaleEnvelopeInfo(this.sandboxId),
        plaintext: authKey.trim()
      })
      try {
        await this.ctx.api.request({
          method: 'PUT',
          path: boxPath(this.sandboxId, '/tailscale'),
          body: { json: { envelope, agentPublicKey } },
          signal: options.signal
        })
        return
      } catch (error) {
        // A claim re-minted the box key between the read and the send.
        if (attempt === 0 && error instanceof ZipboxError && error.code === 'box_key_changed') {
          continue
        }
        throw error
      }
    }
  }

  /** Take the box off your tailnet: it signs out and forgets its Tailscale state. */
  async disconnect(options: CallOptions = {}): Promise<void> {
    await this.ctx.api.request({
      method: 'DELETE',
      path: boxPath(this.sandboxId, '/tailscale'),
      signal: options.signal
    })
  }
}
