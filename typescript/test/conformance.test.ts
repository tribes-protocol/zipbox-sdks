// Runs every shared fixture in sdks/conformance/fixtures against a mock server.
// The rules this runner implements are sdks/conformance/README.md.
import { readdirSync, readFileSync } from 'node:fs'
import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http'
import { createServer as createTcpServer } from 'node:net'
import { join } from 'node:path'

import { afterAll, beforeAll, describe, expect, it } from 'vitest'

import {
  type CreateSandboxOptions,
  type ForkSandboxOptions,
  Zipbox,
  ZipboxError
} from '../src/index.js'

const FIXTURE_DIR = join(import.meta.dirname, '..', '..', 'conformance', 'fixtures')
const API_KEY = `zb_${'T'.repeat(43)}`

interface FixtureRequest {
  method: string
  path: string
  headers?: Record<string, string>
  body?: unknown
  bodyText?: string
}

interface FixtureResponse {
  status: number
  headers?: Record<string, string>
  body?: unknown
  bodyText?: string
}

interface Fixture {
  name: string
  description: string
  call: { op: string; args: Record<string, unknown> }
  exchanges: { request: FixtureRequest; response: FixtureResponse }[]
  expect: { result?: unknown; error?: Record<string, unknown> }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function isFixture(value: unknown): value is Fixture {
  return isRecord(value) && typeof value.name === 'string' && Array.isArray(value.exchanges)
}

function loadFixture(file: string, base: string, dead: string): Fixture {
  const raw = readFileSync(join(FIXTURE_DIR, file), 'utf8')
    .replaceAll('{{BASE}}', base)
    .replaceAll('{{HOST}}', new URL(base).hostname)
    .replaceAll('{{DEAD}}', dead)
  const parsed: unknown = JSON.parse(raw)
  if (!isFixture(parsed)) throw new Error(`bad fixture ${file}`)
  return parsed
}

// `{{ANY}}` inside a fixture body matches any non-null value at that position.
function bodyMatcher(value: unknown): unknown {
  if (value === '{{ANY}}') return expect.anything()
  if (Array.isArray(value)) return value.map(bodyMatcher)
  if (isRecord(value)) {
    return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, bodyMatcher(item)]))
  }
  return value
}

/** `{{DEAD}}`: an origin on a port bound and closed again, so every dial is refused. */
async function deadOrigin(): Promise<string> {
  const server = createTcpServer()
  await new Promise<void>((resolve) => {
    server.listen(0, '127.0.0.1', resolve)
  })
  const address = server.address()
  if (address === null || typeof address === 'string') throw new Error('no address')
  await new Promise<void>((resolve) => {
    server.close(() => {
      resolve()
    })
  })
  return `http://127.0.0.1:${address.port}`
}

function readBody(req: IncomingMessage): Promise<string> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = []
    req.on('data', (chunk: Buffer) => chunks.push(chunk))
    req.on('end', () => {
      resolve(Buffer.concat(chunks).toString('utf8'))
    })
    req.on('error', reject)
  })
}

class MockServer {
  private server: Server | null = null
  private queue: Fixture['exchanges'] = []
  // The previous request's headers, for `{{SAME}}`.
  private previous: IncomingMessage['headers'] = {}
  mismatches: string[] = []
  base = ''

  async start(): Promise<void> {
    this.server = createServer((req, res) => {
      void this.handle(req, res)
    })
    await new Promise<void>((resolve) => this.server?.listen(0, '127.0.0.1', resolve))
    const address = this.server.address()
    if (address === null || typeof address === 'string') throw new Error('no address')
    this.base = `http://127.0.0.1:${address.port}`
  }

  load(exchanges: Fixture['exchanges']): void {
    this.queue = [...exchanges]
    this.previous = {}
    this.mismatches = []
  }

  remaining(): number {
    return this.queue.length
  }

  async stop(): Promise<void> {
    await new Promise<void>((resolve) => {
      this.server?.close(() => {
        resolve()
      })
    })
  }

  private async handle(req: IncomingMessage, res: ServerResponse): Promise<void> {
    const text = await readBody(req)
    const next = this.queue.shift()
    const label = `${req.method ?? '?'} ${req.url ?? '?'}`
    if (next === undefined) {
      this.mismatches.push(`unexpected request ${label}`)
      res.writeHead(599).end()
      return
    }
    const problem = this.match(next.request, req, text)
    this.previous = req.headers
    if (problem !== null) {
      this.mismatches.push(`${label}: ${problem}`)
      res.writeHead(599).end()
      return
    }
    const { response } = next
    const headers: Record<string, string> = { ...response.headers }
    if (response.bodyText !== undefined) {
      res.writeHead(response.status, headers).end(response.bodyText)
    } else {
      headers['content-type'] ??= 'application/json'
      res
        .writeHead(response.status, headers)
        .end(response.body === undefined ? '' : JSON.stringify(response.body))
    }
  }

  private match(expected: FixtureRequest, req: IncomingMessage, text: string): string | null {
    if (req.method !== expected.method) return `method ${req.method ?? ''} ≠ ${expected.method}`
    if (req.url !== expected.path) return `path ≠ ${expected.path}`
    const header = (headers: IncomingMessage['headers'], name: string): string | undefined => {
      const actual = headers[name]
      return Array.isArray(actual) ? actual.join(',') : actual
    }
    for (const [name, value] of Object.entries(expected.headers ?? {})) {
      const got = header(req.headers, name)
      const previous = header(this.previous, name)
      const ok =
        value === '{{ANY}}'
          ? Boolean(got)
          : value === '{{SAME}}'
            ? Boolean(got) && got === previous
            : got === value
      if (!ok)
        return `header ${name}=${got ?? '∅'} ≠ ${value === '{{SAME}}' ? `${previous ?? '∅'} (same)` : value}`
    }
    if (expected.body !== undefined) {
      let parsed: unknown
      try {
        parsed = JSON.parse(text)
      } catch {
        return `body is not JSON: ${text}`
      }
      try {
        expect(parsed).toStrictEqual(bodyMatcher(expected.body))
      } catch {
        return `body ${text} ≠ ${JSON.stringify(expected.body)}`
      }
    }
    if (expected.bodyText !== undefined && text !== expected.bodyText) {
      return `bodyText ${text} ≠ ${expected.bodyText}`
    }
    return null
  }
}

function arg(args: Record<string, unknown>, key: string): string {
  const value = args[key]
  if (typeof value !== 'string') throw new Error(`fixture arg ${key} must be a string`)
  return value
}

function optionalArg(args: Record<string, unknown>, key: string): string | undefined {
  const value = args[key]
  return typeof value === 'string' ? value : undefined
}

function createOptions(args: Record<string, unknown>): Omit<CreateSandboxOptions, 'name'> {
  const tier = optionalArg(args, 'tier')
  const sshPublicKey = optionalArg(args, 'sshPublicKey')
  const mailbox = args.mailbox
  const privateBox = args.private
  return {
    ...(tier === 's' || tier === 'm' || tier === 'l' ? { tier } : {}),
    ...(sshPublicKey === undefined ? {} : { sshPublicKey }),
    ...(typeof mailbox === 'boolean' ? { mailbox } : {}),
    ...(typeof privateBox === 'boolean' ? { private: privateBox } : {})
  }
}

function forkOptions(args: Record<string, unknown>): ForkSandboxOptions {
  const tailscaleAuthKey = optionalArg(args, 'tailscaleAuthKey')
  const wait = args.wait
  const privateBox = args.private
  return {
    name: arg(args, 'name'),
    sourcePolicy:
      arg(args, 'sourcePolicy') === 'pause_and_resume' ? 'pause_and_resume' : 'require_paused',
    ...(typeof privateBox === 'boolean' ? { private: privateBox } : {}),
    ...(tailscaleAuthKey === undefined ? {} : { tailscaleAuthKey }),
    ...(typeof wait === 'boolean' ? { wait } : {})
  }
}

async function run(zb: Zipbox, op: string, args: Record<string, unknown>): Promise<unknown> {
  const box = (): ReturnType<Zipbox['sandbox']> => zb.sandbox(arg(args, 'id'))
  switch (op) {
    case 'sandboxes.list':
      return zb.sandboxes.list()
    case 'sandboxes.get':
      return zb.sandboxes.get(arg(args, 'id'))
    case 'sandboxes.create': {
      const name = optionalArg(args, 'name')
      return zb.sandboxes.create({
        ...(name === undefined ? {} : { name }),
        ...createOptions(args)
      })
    }
    case 'sandboxes.getOrCreate':
      return zb.sandboxes.getOrCreate(arg(args, 'name'), createOptions(args))
    case 'sandboxes.waitReady':
      return zb.sandboxes.waitReady(arg(args, 'id'))
    case 'sandboxes.start':
      return zb.sandboxes.start(arg(args, 'id'))
    case 'sandboxes.stop':
      return zb.sandboxes.stop(arg(args, 'id'))
    case 'sandboxes.restart':
      return zb.sandboxes.restart(arg(args, 'id'))
    case 'sandboxes.cancelTransition':
      return zb.sandboxes.cancelTransition(arg(args, 'id'))
    case 'sandboxes.fork':
      return zb.sandboxes.fork(arg(args, 'id'), forkOptions(args))
    case 'sandboxes.getFork':
      return zb.sandboxes.getFork(arg(args, 'id'))
    case 'sandboxes.setTier':
      return zb.sandboxes.setTier(arg(args, 'id'), arg(args, 'tier') === 'm' ? 'm' : 's')
    case 'sandboxes.delete': {
      const archive = args.archive
      return zb.sandboxes.delete(arg(args, 'id'), typeof archive === 'boolean' ? { archive } : {})
    }
    case 'sandbox.exec':
      return box().exec(arg(args, 'cmd'))
    case 'sandbox.execBackground':
      return box().execBackground(arg(args, 'cmd'))
    case 'commands.events': {
      const events = []
      const since = optionalArg(args, 'since')
      for await (const event of box().commands.events(
        arg(args, 'commandId'),
        since === undefined ? {} : { since }
      )) {
        events.push(event)
      }
      return { events }
    }
    case 'commands.closeStdin':
      return box().commands.closeStdin(arg(args, 'commandId'))
    case 'sandbox.logs': {
      const since = optionalArg(args, 'since')
      const unit = optionalArg(args, 'unit')
      const lines = args.lines
      return box().logs({
        ...(since === undefined ? {} : { since }),
        ...(typeof lines === 'number' ? { lines } : {}),
        ...(unit === undefined ? {} : { unit })
      })
    }
    case 'commands.signal':
      return box().commands.signal(
        arg(args, 'commandId'),
        arg(args, 'signal') === 'KILL' ? 'KILL' : 'TERM'
      )
    case 'files.read':
      return box().files.read(arg(args, 'path'))
    case 'files.write':
      return box().files.write(arg(args, 'path'), arg(args, 'content'))
    case 'files.list':
      return box().files.list(arg(args, 'path'))
    case 'files.stat':
      return box().files.stat(arg(args, 'path'))
    case 'files.mkdir':
      return box().files.mkdir(arg(args, 'path'))
    case 'files.move':
      return box().files.move(arg(args, 'path'), arg(args, 'destination'))
    case 'files.copy':
      return box().files.copy(arg(args, 'path'), arg(args, 'destination'))
    case 'files.delete':
      return box().files.delete(arg(args, 'path'))
    case 'preview.create': {
      const port = args.port
      if (typeof port !== 'number') throw new Error('port must be a number')
      const label = optionalArg(args, 'label')
      return box().preview.create(port, label === undefined ? {} : { label })
    }
    case 'account.balance':
      return zb.account.balance()
    case 'account.whoami':
      return zb.account.whoami()
    case 'sandbox.setPrivate':
      return box().setPrivate(args.enabled === true)
    case 'tailscale.status':
      return box().tailscale.status()
    case 'tailscale.connect':
      return box().tailscale.connect(arg(args, 'authKey'))
    case 'tailscale.disconnect':
      return box().tailscale.disconnect()
    default:
      throw new Error(`unmapped op ${op}`)
  }
}

function toJson(value: unknown): unknown {
  if (value === undefined) return null
  if (value instanceof Uint8Array) return new TextDecoder().decode(value)
  return JSON.parse(JSON.stringify(value))
}

function assertResult(actual: unknown, expected: unknown): void {
  if (expected === null) {
    expect(actual).toBeNull()
    return
  }
  if (isRecord(expected) && 'length' in expected && Array.isArray(actual)) {
    expect(actual).toHaveLength(Number(expected.length))
    const items = Array.isArray(expected.items) ? expected.items : []
    items.forEach((item, index) => {
      expect(actual[index]).toMatchObject(item)
    })
    return
  }
  if (isRecord(expected) && 'text' in expected && typeof actual === 'string') {
    expect(actual).toBe(expected.text)
    return
  }
  expect(actual).toMatchObject(isRecord(expected) ? expected : {})
}

const files = readdirSync(FIXTURE_DIR)
  .filter((file) => file.endsWith('.json'))
  .sort()

describe('conformance', () => {
  const server = new MockServer()
  let dead = ''

  beforeAll(async () => {
    await server.start()
    dead = await deadOrigin()
  })

  afterAll(async () => {
    await server.stop()
  })

  it('has fixtures', () => {
    expect(files.length).toBeGreaterThanOrEqual(88)
  })

  for (const file of files) {
    it(file.replace(/\.json$/, ''), async () => {
      const fixture = loadFixture(file, server.base, dead)
      server.load(fixture.exchanges)
      const zb = new Zipbox({
        apiKey: API_KEY,
        baseUrl: server.base,
        retryBaseDelayMs: 1,
        pollIntervalMs: 1
      })
      let result: unknown
      let error: unknown = null
      try {
        result = await run(zb, fixture.call.op, fixture.call.args)
      } catch (caught) {
        error = caught
      }
      expect(server.mismatches).toEqual([])
      expect(server.remaining()).toBe(0)
      if (fixture.expect.error !== undefined) {
        expect(error).toBeInstanceOf(ZipboxError)
        expect(
          isRecord(error) ? { ...error, code: Reflect.get(error, 'code') } : error
        ).toMatchObject(fixture.expect.error)
        return
      }
      if (error instanceof Error) throw error
      expect(error).toBeNull()
      assertResult(toJson(result), fixture.expect.result)
    })
  }
})
