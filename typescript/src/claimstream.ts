import { isRecord } from './guards.js'

/** The media type `create` asks for: the answer streamed once the box is placed. */
export const CLAIM_STREAM_MEDIA_TYPE = 'application/x-ndjson'

/**
 * Read a create's answer. A server that streamed it (conformance README, streamed create)
 * sends `{"type":"placed","baseUrl"}` the moment the box is placed, and then
 * `{"type":"answer","status","headers","body"}`, the answer it would have given
 * unstreamed. The placed line is informational and never dialled: its origin is handed
 * out before its DNS record is known to be live, and an early lookup caches a wrong or
 * negative answer. The last line is returned as the Response it stands for, so it is
 * read exactly like an unstreamed answer. Any other response (an unstreamed answer, a
 * refusal) is returned as it is.
 *
 * A stream that ends before its answer line throws, as a dropped connection does: the
 * create is retried with its Idempotency-Key and gets the same box.
 */
export async function readClaimStream(response: Response): Promise<Response> {
  const type = (response.headers.get('content-type') ?? '').split(';')[0]?.trim().toLowerCase()
  if (type !== CLAIM_STREAM_MEDIA_TYPE || response.body === null) return response
  const decoder = new TextDecoder()
  const reader = response.body.getReader()
  let buffer = ''
  for (;;) {
    const chunk = await reader.read()
    buffer += chunk.done ? decoder.decode() : decoder.decode(chunk.value, { stream: true })
    for (;;) {
      const end = buffer.indexOf('\n')
      if (end === -1) break
      const line = buffer.slice(0, end).trim()
      buffer = buffer.slice(end + 1)
      if (line === '') continue
      const parsed: unknown = JSON.parse(line)
      if (!isRecord(parsed)) continue
      if (parsed.type === 'answer' && typeof parsed.status === 'number') {
        // Read to its end (the server closes right after) so the connection goes back
        // to the pool.
        let rest = await reader.read()
        while (!rest.done) rest = await reader.read()
        return answerResponse(parsed.status, parsed.headers, parsed.body)
      }
    }
    if (chunk.done) throw new Error('the create stream ended before its answer')
  }
}

function answerResponse(status: number, headers: unknown, body: unknown): Response {
  const answerHeaders = new Headers({ 'content-type': 'application/json' })
  if (isRecord(headers)) {
    for (const [name, value] of Object.entries(headers)) {
      if (typeof value === 'string') answerHeaders.set(name, value)
    }
  }
  return new Response(body === null ? null : JSON.stringify(body), {
    status,
    headers: answerHeaders
  })
}
