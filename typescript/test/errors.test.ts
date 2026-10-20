import { describe, expect, it } from 'vitest'

import { codeForStatus, errorFromResponse, parseRetryAfter, ZipboxError } from '../src/errors.js'

describe('errors', () => {
  it('maps statuses without an envelope', () => {
    expect([400, 401, 402, 403, 404, 409, 413, 429, 502, 503, 500].map(codeForStatus)).toEqual([
      'invalid_request',
      'unauthorized',
      'insufficient_credits',
      'forbidden',
      'not_found',
      'conflict',
      'invalid_request',
      'rate_limited',
      'box_not_running',
      'unavailable',
      'unavailable'
    ])
  })

  it('reads the envelope and Retry-After', () => {
    const error = errorFromResponse({
      status: 429,
      headers: new Headers({ 'retry-after': '2' }),
      body: { error: { code: 'rate_limited', message: 'slow', requestId: 'r1', retryable: true } }
    })
    expect(error).toMatchObject({
      code: 'rate_limited',
      status: 429,
      requestId: 'r1',
      retryAfterMs: 2000
    })
    expect(error.docsUrl).toBe('https://zipbox.ai/docs/errors#rate_limited')
  })

  it("keeps a box route's plain message", () => {
    const error = errorFromResponse({
      status: 404,
      headers: new Headers(),
      body: { error: 'no such file' }
    })
    expect(error).toMatchObject({ code: 'not_found', message: 'no such file', retryable: false })
  })

  it('parses Retry-After dates', () => {
    expect(parseRetryAfter(new Date(10_000).toUTCString(), 4_000)).toBe(6_000)
    expect(parseRetryAfter(null, 0)).toBeNull()
  })

  it('is an Error', () => {
    expect(new ZipboxError({ code: 'conflict', message: 'x' })).toBeInstanceOf(Error)
  })
})
