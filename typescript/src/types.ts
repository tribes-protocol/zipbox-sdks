/** A JSON value as the API returns it. */
export type JsonValue =
  | string
  | number
  | boolean
  | null
  | JsonValue[]
  | { [key: string]: JsonValue }

/** A JSON object whose shape the SDK does not pin (billing summaries, usage). */
export type JsonObject = Record<string, JsonValue>

/** Box size. `enterprise` boxes are created by Zipbox staff only. */
export type SandboxTier = 'xs' | 's' | 'm' | 'l' | 'xl' | 'xxl' | 'enterprise'

/** Lifecycle state of a box. */
export type SandboxStatus =
  | 'provisioning'
  | 'running'
  | 'stopping'
  | 'stopped'
  | 'archived'
  | 'destroyed'

/** The documented fields of a box row. Other fields may be present and are internal. */
export interface SandboxInfo {
  id: string
  name?: string | null
  domain?: string | null
  ipv6?: string | null
  status: SandboxStatus
  tier?: SandboxTier | null
  pendingTier?: SandboxTier | null
  machineSize?: { vcpus: number; memMib: number; diskGb: number } | null
  isArchived?: boolean | null
  cpuPct?: number | null
  memPct?: number | null
  diskPct?: number | null
  netRxBps?: number | null
  netTxBps?: number | null
  statsUpdatedAt?: string | null
  startFailedAt?: string | null
  claimedAt?: string | null
  createdAt: string
  updatedAt: string
}

/** `GET /sandbox/:id`: the row plus the box's connectability. */
export interface SandboxDetail extends SandboxInfo {
  /** Running and serving the owner's generation: the box answers on `baseUrl`. */
  ready: boolean
  /**
   * Whether `baseUrl` completes a TLS handshake with a trusted certificate yet. A
   * fresh name's certificate can trail `ready` by seconds to minutes; the SDK's own
   * box calls retry the handshake meanwhile. Absent from an older control plane.
   */
  httpsReady?: boolean
  /** The box's HTTPS origin, once ready. */
  baseUrl?: string | null
  /** Per-key, per-box bearer token the box accepts. Held inside the client; never on a returned row. */
  boxToken?: string | null
}

/** Options for {@link Sandboxes.create}. */
export interface CreateSandboxOptions {
  /** The box's permanent subdomain. Absent = the account's default box. */
  name?: string
  tier?: SandboxTier
  /** One OpenSSH public key line to authorize for `ssh root@<ipv6>`. */
  sshPublicKey?: string
  signal?: AbortSignal
}

/** Options for {@link Sandboxes.delete}. */
export interface DeleteSandboxOptions {
  /** Keep a recoverable copy for 30 days. The server default is true. */
  archive?: boolean
  signal?: AbortSignal
}

/** Common per-call options. */
export interface CallOptions {
  signal?: AbortSignal
}

/** Options for running a command on a box. */
export interface ExecOptions extends CallOptions {
  /** Absolute, or relative to /root/workspace. Default /root/workspace. */
  cwd?: string
  env?: Record<string, string>
  /** Opt-in deadline. Absent = the command runs until it exits. */
  timeoutMs?: number
  /** Run as another local user. Default: the box user (root). */
  user?: string
}

/** Options for {@link Sandbox.execBackground}. */
export interface ExecBackgroundOptions extends ExecOptions {
  /** Open a stdin pipe fed by {@link Command.stdin}. Default: stdin is /dev/null. */
  stdin?: boolean
}

/** The collected result of a command. A non-zero `exitCode` is a result, not an error. */
export interface ExecResult {
  id: string
  exitCode: number | null
  stdout: string
  stderr: string
}

export type ExecSignal = 'TERM' | 'KILL' | 'INT' | 'HUP' | 'QUIT'

export type ExecState = 'running' | 'exited' | 'unknown'

/** A command record on the box. */
export interface CommandRecord {
  id: string
  state: ExecState
  argv: string[]
  cwd: string
  background: boolean
  exitCode?: number | null
  signal?: ExecSignal | null
  timedOut: boolean
  startedAt: string
  endedAt?: string | null
  stdoutBytes: number
  stderrBytes: number
}

/** One event of a command's output stream. `cursor` resumes the stream after this event. */
export type ExecEvent =
  | { type: 'start'; cursor: string; id: string }
  | { type: 'stdout'; cursor: string; data: string }
  | { type: 'stderr'; cursor: string; data: string }
  | { type: 'gap'; cursor: string; stream: 'stdout' | 'stderr'; message: string }
  | {
      type: 'exit'
      cursor: string
      state: ExecState
      exitCode: number | null
      signal: ExecSignal | null
      timedOut: boolean
    }

/** Options for {@link Commands.events}. */
export interface CommandEventsOptions extends CallOptions {
  /** Resume after this cursor (`<stdoutBytes>-<stderrBytes>`). Default: from the start. */
  since?: string
}

export type RunCodeLanguage = 'python' | 'node' | 'bash'

export type FileKind = 'directory' | 'file' | 'symlink' | 'other'

/** One directory entry. */
export interface FileEntry {
  name: string
  kind: FileKind
  sizeBytes: number
  modifiedAtMs: number
}

/** Options for {@link Preview.create}. */
export interface PreviewOptions extends CallOptions {
  /** Subdomain label under the box's host. Default `p<port>`. */
  label?: string
}

/** A published port. */
export interface PreviewLink {
  url: string
  label: string
}

/** Options for {@link Sandbox.logs}. */
export interface LogsOptions extends CallOptions {
  /** journalctl `--since` expression, e.g. `"10 min ago"` or an ISO timestamp. */
  since?: string
  /** Most recent N lines; everything when unset (as the other SDKs and the CLI). */
  lines?: number
}

/** How to SSH into a box. */
export interface SshInfo {
  host: string
  user: 'root'
  command: string
}

/** Options for {@link PtyApi.create}. */
export interface PtyCreateOptions {
  cols?: number
  rows?: number
}

// Method-shorthand inside an indexed type keeps the handler parameter bivariant,
// so the WHATWG WebSocket (whose handlers take a full Event) is assignable here.
type Handler<E> = { bivarianceHack(event: E): void }['bivarianceHack']

/** The subset of the WHATWG WebSocket the SDK uses (Node 22+, browsers, or the `ws` package). */
export interface WebSocketLike {
  readonly readyState: number
  onopen: Handler<unknown> | null
  onmessage: Handler<{ data: unknown }> | null
  onclose: Handler<{ code: number; reason: string }> | null
  onerror: Handler<unknown> | null
  send(data: string): void
  close(code?: number, reason?: string): void
}

/** A WebSocket constructor. */
export type WebSocketConstructor = new (url: string, protocols?: string | string[]) => WebSocketLike

/** A `fetch` implementation. */
export type FetchLike = (input: string, init?: RequestInit) => Promise<Response>

/** Constructor options for {@link Zipbox}. */
export interface ZipboxOptions {
  /** Account API key (`zb_…`). Default: `ZIPBOX_API_KEY`. */
  apiKey?: string
  /** Control plane URL. Default: `ZIPBOX_API_URL`, else `https://api.tribes.xyz`. */
  baseUrl?: string
  /**
   * Retries for retryable failures of one request (GET and create). Default 3.
   * Followed command streams and PTYs are live surfaces and never give up.
   */
  maxRetries?: number
  /** First retry delay; doubles per attempt, capped at 30 s, with jitter. Default 500 ms. */
  retryBaseDelayMs?: number
  /** First delay between readiness checks in `create`; grows to 10 s. Default 1000 ms. */
  pollIntervalMs?: number
  fetch?: FetchLike
  /** WebSocket constructor for PTYs (needed on Node 20; Node 22+ has one built in). */
  WebSocket?: WebSocketConstructor
}
