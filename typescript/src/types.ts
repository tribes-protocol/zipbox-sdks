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

/**
 * Lifecycle state of a box. `pausing` reads as paused: the VM is stopped and its disk
 * is safe on its host; its backup is still uploading; `start` boots it at once.
 */
export type SandboxStatus =
  | 'provisioning'
  | 'running'
  | 'stopping'
  | 'pausing'
  | 'stopped'
  | 'archived'
  | 'deleting'
  | 'destroyed'

/**
 * What a box is doing now: `starting`, `running`, `stopping`, `paused`, `resizing`,
 * `forking`, `deleting`, `delete_failed` (a delete was refused; the box still runs),
 * `start_failed` or `gone`. The box's `transition` says how far a change has got.
 */
export type SandboxPhase =
  | 'starting'
  | 'running'
  | 'stopping'
  | 'paused'
  | 'resizing'
  | 'forking'
  | 'deleting'
  | 'delete_failed'
  | 'start_failed'
  | 'gone'

/**
 * Whether a box will answer, which is what every wait reads. `ready`: it answers.
 * `settling`: coming up, mid-change or not connectable yet (every transient, a host
 * offline or no room included). `awaits_tailnet`: a private box running with its key
 * that only an auth key can join. `paused`, `start_failed`, `deleting` and `gone`: it
 * will not become ready by itself.
 */
export type SandboxReadiness =
  | 'ready'
  | 'settling'
  | 'awaits_tailnet'
  | 'paused'
  | 'start_failed'
  | 'deleting'
  | 'gone'

/** A lifecycle verb a box can accept: `start`, `stop`, `restart`, `delete`, `cancel_transition`, `fork`. */
export type SandboxAction = 'start' | 'stop' | 'restart' | 'delete' | 'cancel_transition' | 'fork'

/**
 * The box's managed Tailscale daemon: `joining` (key accepted, coming up),
 * `connected`, `needsApproval` (a tailnet admin must approve the device),
 * `needsLogin` (the key was refused or the node key expired), `stopped`, `failed`.
 */
export type TailscaleState =
  | 'joining'
  | 'connected'
  | 'needsApproval'
  | 'needsLogin'
  | 'stopped'
  | 'failed'

/** One of the owner's other devices on the tailnet, as the box sees it. */
export interface TailscalePeer {
  /** The device's own name, e.g. `lucys-macbook`. */
  name: string
  /** Its MagicDNS name. */
  dnsName?: string | null
  /** `macOS`, `linux`, `iOS`, `windows`, `android`, … */
  os?: string | null
  /** Connected to Tailscale right now. */
  online: boolean
  /** Its tailnet IPv4 (100.x). */
  ipv4?: string | null
  /**
   * `direct` (peer to peer), `relay` (through a Tailscale DERP relay), or null (no
   * recent traffic). Kept as a string: a later value must not break parsing.
   */
  connection?: string | null
}

/** A box's tailnet status (`GET /sandbox/:id/tailscale`). */
export interface TailscaleStatus {
  /** Null when only the last known identity is known (the box is stopped). */
  state?: TailscaleState | null
  /** The tailnet's name, e.g. `example.com`. */
  tailnet?: string | null
  /** The box's tailnet IPv4 (100.x). */
  ipv4?: string | null
  ipv6?: string | null
  /** The box's MagicDNS name. */
  dnsName?: string | null
  /** When the node key expires; null when expiry is off (recommended). */
  keyExpiry?: string | null
  /**
   * True while an auth key is on its way to the box. It dropping without `state`
   * advancing means the key never arrived: connect again with a new key.
   */
  pending: boolean
  /** The owner's other devices (online first, at most 50); null while not reporting. */
  peers?: TailscalePeer[] | null
}

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
  /** Private mode: reachable only from devices on the owner's tailnet. */
  privateMode?: boolean | null
  /** The box's tailnet status; null when it never joined one. */
  tailscale?: TailscaleStatus | null
  /**
   * False when the box's ZipOS predates Tailscale: `box.tailscale.connect` fails
   * with `tailscale_requires_update` until the ZipOS update is accepted. Null until
   * the box has reported since it booted.
   */
  tailscaleSupported?: boolean | null
  /**
   * The box's own P-256 public key (SPKI DER, base64). Credentials are sealed to
   * it; it changes when a box is claimed, so it is read right before sealing.
   */
  agentPublicKey?: string | null
  /**
   * The fork still making this box: its phase, source recovery, waits and error.
   * Null on every box that is not being forked into.
   */
  fork?: SandboxFork | null
  /** What the box is doing now. Absent from an older control plane. */
  phase?: SandboxPhase | null
  /**
   * The lifecycle verbs the box accepts in its current state. A listed verb can
   * still be refused for a reason outside the box (credit, capacity, an argument).
   * Absent from an older control plane.
   */
  allowedActions?: SandboxAction[] | null
  createdAt: string
  updatedAt: string
}

/** `GET /sandbox/:id`: the row plus the box's connectability. */
export interface SandboxDetail extends SandboxInfo {
  /** Running and serving the owner's generation: the box answers on `baseUrl`. */
  ready: boolean
  /**
   * Whether the box will answer, read from the same connection gate as `ready`:
   * what every wait reads. Only on `GET /sandbox/:id`; absent from an older control
   * plane, where the SDK falls back to `ready` and `status`.
   */
  readiness?: SandboxReadiness | null
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

/**
 * Options for {@link Sandboxes.create}. {@link Sandboxes.getOrCreate} takes the same
 * options minus `name`, and applies them only when it creates the box.
 */
export interface CreateSandboxOptions {
  /** The box's permanent subdomain. Absent = the account's default box. */
  name?: string
  tier?: SandboxTier
  /** One OpenSSH public key line to authorize for `ssh root@<ipv6>`. */
  sshPublicKey?: string
  /** Create the box's mailbox at creation. Default: created on first use. */
  mailbox?: boolean
  /**
   * Start the box private: never public, reachable only from devices on your
   * tailnet once it joins. `create` then returns as soon as the box is running
   * with its key (it is not `ready` until it joins); call `box.tailscale.connect`.
   */
  private?: boolean
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

/**
 * What {@link Sandboxes.cancelTransition} did. `cleared`: nothing was executing on a
 * host, so the queued change was dropped and the box is already back where it was.
 * `cancelling`: the host is executing it; its abort is on the way. `none`: the box
 * had no change in flight. A change past its point of no return is refused with a
 * `conflict` error carrying the reason.
 */
export type TransitionCancelOutcome = 'cleared' | 'cancelling' | 'none'

/**
 * How a fork treats its source. `require_paused` refuses a running source
 * (`pause_required`); `pause_and_resume` lets a running source pause for its
 * capture: its sessions disconnect and it resumes right after. A paused source
 * stays paused either way.
 */
export type SandboxForkSourcePolicy = 'require_paused' | 'pause_and_resume'

/**
 * Where a fork stands: `preparing_identity` (the new name, mailbox and wallets are
 * minted), `queued`, `capturing` (the source's disk is captured), `copying` (its
 * files are copied into the new box's fresh filesystem), `starting`, then
 * `complete` (the new box has its own disk and serves), `cancelling`, `cancelled`
 * or `failed`.
 */
export type SandboxForkPhase =
  | 'preparing_identity'
  | 'queued'
  | 'capturing'
  | 'copying'
  | 'starting'
  | 'complete'
  | 'cancelling'
  | 'cancelled'
  | 'failed'

/**
 * Where the source stands: `not_needed` (it was paused and stays paused),
 * `pending` (the fork paused it and owes its resume), `resumed`, or `superseded`
 * (a newer pause or delete replaced the resume).
 */
export type SandboxForkSourceRecovery = 'not_needed' | 'pending' | 'resumed' | 'superseded'

/** Why a fork ended without a new box. Each is final: the same fork fails the same way. */
export type SandboxForkErrorCode =
  | 'source_unsupported'
  | 'source_gone'
  | 'identity_reused'
  | 'copy_failed'
  | 'start_failed'

/** A fork's terminal error. */
export interface SandboxForkError {
  code: SandboxForkErrorCode
  message: string
}

/**
 * One fork operation (`GET /sandbox/:id/fork`, and `fork` on the new box's row
 * while it is being made). Kept as strings where the server may add a value later.
 */
export interface SandboxFork {
  operationId: string
  sourceSandboxId: string
  childSandboxId: string
  sourcePolicy: SandboxForkSourcePolicy
  phase: SandboxForkPhase
  sourceRecovery: SandboxForkSourceRecovery
  /** How the source disk was captured: `reflink` (copy-on-write) or `copy`. */
  captureMode?: 'reflink' | 'copy' | null
  /**
   * Why the current step waits: `software_install`, `legacy_chisel`, `busy`,
   * `host_offline` or `capacity`. Null while it moves.
   */
  waitingOn?: string | null
  /** The retry ladder's position for the current step. */
  attempt: number
  nextAttemptAt?: string | null
  cancelRequestedAt?: string | null
  /** Set when the fork `failed`. */
  error?: SandboxForkError | null
  createdAt: string
  updatedAt: string
  completedAt?: string | null
}

/** Options for {@link Sandboxes.fork} and {@link Sandbox.fork}. */
export interface ForkSandboxOptions {
  /** The new box's permanent name. It stays reserved even if the fork is cancelled. */
  name: string
  /** Required: whether a running source may pause for its capture. */
  sourcePolicy: SandboxForkSourcePolicy
  /**
   * Start the new box private: never public, reachable only from devices on your
   * tailnet once it joins. Default: public, whatever the source's privacy. Pass
   * `tailscaleAuthKey` to join it as part of the fork.
   */
  private?: boolean
  /**
   * A Tailscale auth key for the new box. Used on this machine only: once the new
   * box runs, it is sealed to that box's own key and delivered as
   * `box.tailscale.connect` does. Never sent in the fork request. Needs `wait`.
   */
  tailscaleAuthKey?: string
  /**
   * Wait until the new box is ready (a private one: running with its key, then
   * connected when `tailscaleAuthKey` is given). Default true. With false, the
   * accepted box is returned at once, still `provisioning`, with `info.fork`.
   */
  wait?: boolean
  /** Stops the local wait only; the fork keeps running. Cancel it with `cancelTransition`. */
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
  /** Only this systemd unit (`journalctl -u`). */
  unit?: string
}

/** How to SSH into a box. */
export interface SshInfo {
  host: string
  user: 'root'
  command: string
}

/** Options for {@link PtyApi.create} and {@link PtyApi.attach}. */
export interface PtyCreateOptions {
  /** Initial size. Absent: the box's default. */
  cols?: number
  rows?: number
  /** Names this client in the box's session list. Default `zipbox-sdk`. */
  clientLabel?: string
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
  /** First retry delay; doubles per attempt up to `retryMaxDelayMs`, with jitter. Default 500 ms. */
  retryBaseDelayMs?: number
  /** Cap on the retry delay. Default 30 s. */
  retryMaxDelayMs?: number
  /**
   * First delay between readiness checks in `create`; grows 1.25× per check up to
   * `pollMaxIntervalMs`, with jitter. Default 1000 ms.
   */
  pollIntervalMs?: number
  /** Cap on the delay between readiness checks. Default 10 s. */
  pollMaxIntervalMs?: number
  /** First PTY reconnect delay; doubles up to `ptyMaxDelayMs`, with jitter. Default 500 ms. */
  ptyBaseDelayMs?: number
  /** Cap on the PTY reconnect delay. Default 30 s. */
  ptyMaxDelayMs?: number
  fetch?: FetchLike
  /** WebSocket constructor for PTYs (needed on Node 20; Node 22+ has one built in). */
  WebSocket?: WebSocketConstructor
}
