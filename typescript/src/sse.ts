/** One server-sent event as it came off the wire. */
export interface RawSseEvent {
  id: string | null
  event: string
  data: string
}

/**
 * Parse a `text/event-stream` body. Bytes are decoded with one streaming
 * TextDecoder, so a multi-byte character split across chunks survives, and lines
 * may end in LF, CRLF or CR.
 */
export async function* parseSse(body: ReadableStream<Uint8Array>): AsyncGenerator<RawSseEvent> {
  const decoder = new TextDecoder()
  const reader = body.getReader()
  let buffer = ''
  let id: string | null = null
  let event = ''
  let data: string[] = []
  let sawField = false
  let finished = false

  const dispatch = (): RawSseEvent | null => {
    if (!sawField) return null
    const out: RawSseEvent = { id, event: event === '' ? 'message' : event, data: data.join('\n') }
    event = ''
    data = []
    sawField = false
    return out
  }

  const takeLines = function* (final: boolean): Generator<string> {
    for (;;) {
      const match = /\r\n|\r|\n/.exec(buffer)
      if (match === null) break
      // A trailing CR may be the first half of a CRLF still in flight.
      if (match[0] === '\r' && match.index === buffer.length - 1 && !final) break
      yield buffer.slice(0, match.index)
      buffer = buffer.slice(match.index + match[0].length)
    }
  }

  const handleLine = (line: string): RawSseEvent | null => {
    if (line === '') return dispatch()
    if (line.startsWith(':')) return null
    const colon = line.indexOf(':')
    const field = colon === -1 ? line : line.slice(0, colon)
    let value = colon === -1 ? '' : line.slice(colon + 1)
    if (value.startsWith(' ')) value = value.slice(1)
    switch (field) {
      case 'id':
        id = value
        sawField = true
        break
      case 'event':
        event = value
        sawField = true
        break
      case 'data':
        data.push(value)
        sawField = true
        break
      default:
        break
    }
    return null
  }

  try {
    for (;;) {
      const { done, value } = await reader.read()
      buffer += done ? decoder.decode() : decoder.decode(value, { stream: true })
      for (const line of takeLines(done)) {
        const out = handleLine(line)
        if (out !== null) yield out
      }
      if (done) {
        // An event not yet terminated by a blank line is incomplete (the stream
        // was cut mid-event): discard it, per the SSE spec, rather than hand a
        // truncated `data:` to the caller. The box always ends a finished stream
        // with a terminated `exit`; a follower resumes from the last cursor.
        buffer = ''
        finished = true
        return
      }
    }
  } finally {
    // A consumer that stops early closes the connection rather than leaving it open.
    if (!finished) await reader.cancel().catch(() => undefined)
    reader.releaseLock()
  }
}
