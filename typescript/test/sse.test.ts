import { describe, expect, it } from 'vitest'

import { parseSse, type RawSseEvent } from '../src/sse.js'

function streamOf(chunks: Uint8Array[]): ReadableStream<Uint8Array> {
  return new ReadableStream({
    start(controller) {
      for (const chunk of chunks) controller.enqueue(chunk)
      controller.close()
    }
  })
}

async function all(chunks: Uint8Array[]): Promise<RawSseEvent[]> {
  const out: RawSseEvent[] = []
  for await (const event of parseSse(streamOf(chunks))) out.push(event)
  return out
}

const encode = (text: string): Uint8Array => new TextEncoder().encode(text)

describe('parseSse', () => {
  it('parses id, event and multi-line data', async () => {
    const events = await all([encode('id: 1-0\nevent: stdout\ndata: {"a":1}\ndata: x\n\n')])
    expect(events).toEqual([{ id: '1-0', event: 'stdout', data: '{"a":1}\nx' }])
  })

  it('survives chunk boundaries anywhere, including inside a multi-byte character', async () => {
    const text =
      'id: 3-0\nevent: stdout\ndata: {"data":"héllo 🌍"}\n\nid: 3-0\nevent: exit\ndata: {}\n\n'
    const bytes = encode(text)
    for (let split = 1; split < bytes.length; split++) {
      const events = await all([bytes.slice(0, split), bytes.slice(split)])
      expect(events.map((event) => event.data)).toEqual(['{"data":"héllo 🌍"}', '{}'])
    }
  })

  it('accepts CRLF and CR line endings and ignores comments', async () => {
    const events = await all([
      encode(': keepalive\r\nevent: a\r\ndata: 1\r\n\r\nevent: b\rdata: 2\r\r')
    ])
    expect(events.map((event) => [event.event, event.data])).toEqual([
      ['a', '1'],
      ['b', '2']
    ])
  })

  it('keeps the last event id and discards an unterminated final event (a cut stream)', async () => {
    // A connection dropped mid-event leaves a half `data:` behind. Handing it on
    // would surface as a decode error instead of a resumable drop.
    const events = await all([encode('id: 9-9\nevent: a\ndata: 1\n\nevent: b\ndata: {"da')])
    expect(events).toEqual([{ id: '9-9', event: 'a', data: '1' }])
  })
})
