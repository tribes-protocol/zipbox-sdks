import { describe, expect, it } from 'vitest'

import { backoffDelay, sleep } from '../src/backoff.js'

describe('backoffDelay', () => {
  it('doubles per attempt, within [50%, 100%) of the step', () => {
    expect(backoffDelay({ attempt: 0, baseMs: 500, capMs: 30_000, random: 0 })).toBe(250)
    expect(backoffDelay({ attempt: 0, baseMs: 500, capMs: 30_000, random: 0.999 })).toBe(500)
    expect(backoffDelay({ attempt: 3, baseMs: 500, capMs: 30_000, random: 0.999 })).toBe(3998)
  })

  it('never exceeds the cap, however many attempts', () => {
    for (const attempt of [10, 50, 1000]) {
      expect(
        backoffDelay({ attempt, baseMs: 500, capMs: 30_000, random: 0.999 })
      ).toBeLessThanOrEqual(30_000)
    }
  })
})

describe('sleep', () => {
  it('rejects when the signal aborts', async () => {
    const controller = new AbortController()
    const pending = sleep(60_000, controller.signal)
    controller.abort(new Error('stop'))
    await expect(pending).rejects.toThrow('stop')
  })
})
