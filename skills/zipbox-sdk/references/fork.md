> Zipbox docs, markdown edition of https://zipbox.ai/docs/fork
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# Fork a box

Copy a box into a new box with the same files, software and settings, its own name and a fresh identity - from REST, every SDK, the CLI and MCP.

A fork makes a new box from a consistent capture of another box's disk. The new box keeps the source's files, installed software, repositories (uncommitted and untracked changes included), configuration and local application data, and boots with its own name, keys, mailbox and wallets. It belongs to the same account, runs at the same size, and starts public unless you ask for a private one.

A fork copies the **disk**, not the running machine: RAM, running processes, open connections, terminal sessions and unsaved editor buffers are not copied. The new box boots fresh, and whatever starts on boot starts there too.

## Fork a box

Name the new box and say what may happen to the source. `pause_and_resume` lets a running source pause for its capture and resume right after; `require_paused` refuses a running source with `409 pause_required` and touches nothing. A paused source stays paused either way.

#### TypeScript

```ts
const child = await zb.sandboxes.fork(source.id, {
  name: 'rent-jesse-2',
  sourcePolicy: 'pause_and_resume'
})
// child is the new box, ready: child.id, child.name, child.status
```

#### Python

```python
child = zb.sandboxes.fork(
    source.id,
    name="rent-jesse-2",
    source_policy="pause_and_resume",
)
```

#### Go

```go
child, err := c.Sandboxes.Fork(ctx, source.ID, zipbox.ForkOptions{
	Name:         "rent-jesse-2",
	SourcePolicy: zipbox.ForkPauseAndResume,
})
```

#### Rust

```rust
let options = zipbox::ForkOptions::new(
    "rent-jesse-2",
    zipbox::ForkSourcePolicy::PauseAndResume,
);
let child = zb.sandboxes().fork(source.id(), options).await?;
```

#### CLI

```sh
# --pause: a running source pauses for the capture, then resumes
zipbox fork rent-jesse --name rent-jesse-2 --pause
```

#### curl

```sh
KEY=$(uuidgen)
curl -X POST https://api.tribes.xyz/sandbox/$ID/fork \
  -H "Authorization: Bearer $ZIPBOX_API_KEY" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $KEY" \
  -d '{"name":"rent-jesse-2","sourcePolicy":"pause_and_resume"}'
# 202 {"operationId":"…","sandbox":{…"status":"provisioning"},"fork":{…}}
```

The SDKs return a handle on the **new** box once it is ready, the same handle `create` returns. A new box you asked to be private is returned once it is running with its own key, as `create` does for a private box ([Private boxes and Tailscale](#private-boxes-and-tailscale)). The handle on the source is unchanged.

The new name follows the usual name rules and is reserved for good the moment the fork is accepted: it stays yours, on hold, even if the fork is cancelled, and it counts toward the account's [250 machine names](https://zipbox.ai/docs/machine-limit.md). It can also be a name you already hold on hold, one whose box you deleted: the new box then boots as that name, with its subdomain, its mailbox and the mail in it, and its wallets and their funds, and nothing new counts toward the limit. A name a box is using, yours or anyone's, is `409 name_taken`. The new box is billed like any box of its size from the moment it starts; preparing, capturing and copying are not billed, and no usage or charge is copied from the source.

## Running and paused sources

| Source  | `require_paused`                      | `pause_and_resume`                                                        |
| ------- | ------------------------------------- | ------------------------------------------------------------------------- |
| Paused  | Forked; it stays paused               | Forked; it stays paused                                                   |
| Running | `409 pause_required`, nothing touched | Paused for the capture, then resumed right after; its sessions disconnect |

A running source has to stop writing for its disk to be captured consistently; a fork never copies a disk that is still being written to. While it is paused, its terminals, desktop and SSH sessions disconnect and its running processes stop, exactly as a pause. It resumes as soon as the capture is taken, without waiting for the copy or the new box, and whatever starts on boot starts again. A pause or delete you send to the source in that window wins over the fork's resume.

The capture is crash-consistent: what was on disk when the source paused, like a power cut right after a `sync`. A local database with its write-ahead log on the same disk recovers on the new box. An application that needs a clean shutdown, or state on a remote volume, should be stopped or quiesced before you fork.

Every fork reports where its source stands in `sourceRecovery`: `not_needed` (it was paused and stays paused), `pending` (the fork paused it and owes the resume), `resumed`, or `superseded` (your own pause or delete replaced the resume).

## Copied, new, and left out

**Copied from the source**

| What                                                                                    | How                                                                                                                                                                                                  |
| --------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Files, repositories, uncommitted changes, installed packages, configuration, local data | Every live file on the box's disk, with numeric owners, permissions, ACLs, extended attributes, capabilities, hardlinks, symlinks and sparse files. Docker images and volumes on the disk come along |
| Size, harness, timezone, display and taskbar preferences, repository selection          | Copied as settings                                                                                                                                                                                   |
| Private mode                                                                            | From the fork request (`private`), default public, whatever the source is. A private fork is private from its first packet                                                                           |
| Egress policy                                                                           | Inherited before the new box's first packet                                                                                                                                                          |
| Machine-scoped provider keys and custom API membership                                  | Copied inside the control plane and re-encrypted for the new box. Needs a signed-in owner ([API keys and machine-scoped credentials](#api-keys-and-machine-scoped-credentials))                      |
| Included-service off switches                                                           | Always copied: they only restrict                                                                                                                                                                    |
| Your owner SSH keys on the source                                                       | Added to the new box once, as create adds them                                                                                                                                                       |
| Custom HTTPS sites                                                                      | The services they point at are kept; platform names are rebuilt for the new name                                                                                                                     |

Account-wide provider keys and custom APIs are not copied: the new box gets them from your account settings like any other box, including a later revocation.

**New for the new box**

| What                                                  | Notes                                                                                                                                                           |
| ----------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Box id, permanent name, subdomain and mailbox address | The mailbox starts empty. A name you held on hold keeps its own mailbox and mail                                                                                |
| Wallets                                               | New wallets, created on the new box's first wallet use; no funds, permissions or signers are inherited. A name you held on hold keeps its own wallets and funds |
| The box's own key (`agentPublicKey`) and API tokens   | Credentials are sealed to the new key; tokens for the source do not work on the new box, nor the other way round                                                |
| SSH host key and the managed `~/.ssh/id_ed25519`      | Both are new. If you use the managed key as a deploy key, authorize the new public key. Your other SSH keys and config are copied                               |
| HTTPS certificate                                     | Issued for the new name; it can trail the box's readiness by seconds to minutes, like any new name                                                              |
| Tailscale identity                                    | The new box starts off any tailnet and joins as a new device with a key you give it                                                                             |
| Network addresses, OS machine id, swap                | Fresh                                                                                                                                                           |
| Usage, billing, error history                         | Start empty                                                                                                                                                     |

**Left out**

| What                                                                                    | Why                                                                                        |
| --------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------ |
| RAM, running processes, open sockets, terminal sessions, unsaved buffers                | A fork copies the disk; the new box boots fresh                                            |
| SSH access grants between boxes                                                         | Each grant is signed by you for one box; a copy would be access nobody signed. Grant again |
| Platform secrets: box sessions, token lists, command history, Tailscale state, TLS keys | They identify the source. The new box gets its own                                         |
| Generated provider and GitHub credential files                                          | Regenerated from what is authorized for the new box now                                    |
| Mailbox and calendar contents, wallet funds                                             | They belong to the source's identity                                                       |
| Archives, pending resizes, software update state                                        | The new box starts with its own                                                            |

A fork does not remove credentials you copied into your own files, shell history, browser profiles or databases: those are user data and stay, within the same account.

## Where your data goes

1. **Your machine → control plane.** Your browser or SDK sends the new name and the source policy. A Tailscale auth key, if you gave one, stays on your machine.
2. **Control plane → source host.** The control plane checks ownership, the name, credit and the machine limit, reserves the new name, and sends the capture to the host the source runs on.
3. **Source disk → capture.** On that host, the source's disk is captured while nothing writes to it. The raw capture is host-only staging: it never leaves the source host, is never uploaded, exported or attached to any box, and is deleted once the new box no longer needs it. A paused source whose disk is only in cold storage is restored from its own backup into that staging.
4. **Capture → copy VM → fresh disk.** An isolated copy VM with **no network** reads the capture and copies the allowed files into a new, empty filesystem. It runs nothing from your disk.
5. **Fresh disk → new box.** The fresh disk boots as the new box on the same host. A host with no room for it makes the fork wait (`waitingOn: capacity`) rather than move anything; neither disk ever leaves the host.
6. **Tailscale key → new box.** Once the new box runs and publishes its own public key, your machine seals the auth key to that key. The control plane relays the ciphertext once, and only the new box can open it.

| Boundary      | Crosses it                                       | Never crosses it                                     |
| ------------- | ------------------------------------------------ | ---------------------------------------------------- |
| Your machine  | Name, source policy; later, the sealed auth key  | The plaintext auth key                               |
| Source host   | Nothing: the new box boots on it                 | The raw capture: deleted files, journal, swap        |
| Copy VM       | Allowed file contents, into the fresh filesystem | Network traffic: it has none                         |
| Control plane | The sealed auth key, relayed once                | The auth key in the clear; it is never stored either |

## Timeline

A fork moves through `phase` in this order. The source is down only during `capturing`; the copy and the new box's boot happen after it has resumed.

| `phase`              | What happens                                                                                | The source                                         |
| -------------------- | ------------------------------------------------------------------------------------------- | -------------------------------------------------- |
| `preparing_identity` | The new name and its mailbox address are reserved, or a name you held on hold is taken back | Untouched                                          |
| `capturing`          | The source's disk is captured with nothing writing to it                                    | Paused, when it was running: its only downtime     |
| `copying`            | Allowed files are copied into the fresh filesystem                                          | Resumed right after the capture (`sourceRecovery`) |
| `starting`           | The new box boots on the normal start path                                                  | Running                                            |
| `complete`           | The new box has its own disk and serves (`status: running`)                                 | Running                                            |

Off the critical path: the new box's HTTPS certificate, its Tailscale enrollment and its wallet signer binding settle after `complete` and never hold it up. A fork that waits says why in `waitingOn` (`capacity`, `busy`, `host_offline`, `software_install`, `legacy_chisel`), with `attempt` and `nextAttemptAt`; a temporary outage makes it wait and retry, never fail. It ends in `complete`, `cancelled`, or `failed` with an `error`.

## Why the disk is not cloned

A block-for-block clone of the source's disk would be faster, and it would be wrong. Deleting a file removes its name, not its data: a raw clone carries the blocks of every deleted file, the filesystem journal and the swap file, and the old box's signing keys and sessions can still be read from them by whoever has root on the clone. Overwriting them in place is unreliable on journaled and copy-on-write storage.

| Approach                                           | Live files | Deleted files' blocks | Journal | Swap   | Free space    |
| -------------------------------------------------- | ---------- | --------------------- | ------- | ------ | ------------- |
| Raw clone of the disk                              | Copied     | Copied                | Copied  | Copied | Old contents  |
| Fresh filesystem, allowed files (what Zipbox does) | Copied     | Not copied            | New     | New    | Reads as zero |

So the new box never receives the source's raw disk, its unallocated bytes or its journal: only the live files a fork allows, written into a filesystem that starts empty.

## Follow and cancel a fork

A fork runs on the control plane, not in your request. A client that stops waiting, times out or disconnects stops only its own wait; the fork keeps going. Pass `wait: false` to get the new box back at once, still `provisioning`, and follow it yourself.

#### TypeScript

```ts
const child = await zb.sandboxes.fork(source.id, {
  name: 'rent-jesse-2',
  sourcePolicy: 'require_paused',
  wait: false
})
const fork = await child.forkStatus() // phase, sourceRecovery, waitingOn
await zb.sandboxes.cancelTransition(child.id) // cancel the fork
```

#### Python

```python
child = zb.sandboxes.fork(
    source.id,
    name="rent-jesse-2",
    source_policy="require_paused",
    wait=False,
)
fork = child.fork_status()  # phase, source_recovery, waiting_on
zb.sandboxes.cancel_transition(child.id)  # cancel the fork
```

#### Go

```go
child, err := c.Sandboxes.Fork(ctx, source.ID, zipbox.ForkOptions{
	Name:         "rent-jesse-2",
	SourcePolicy: zipbox.ForkRequirePaused,
	NoWait:       true,
})
fork, err := child.ForkStatus(ctx) // Phase, SourceRecovery, WaitingOn
outcome, err := c.Sandboxes.CancelTransition(ctx, child.ID)
```

#### Rust

```rust
let mut options = zipbox::ForkOptions::new(
    "rent-jesse-2",
    zipbox::ForkSourcePolicy::RequirePaused,
);
options.wait = false;
let child = zb.sandboxes().fork(source.id(), options).await?;
let fork = child.fork_status().await?; // phase, source_recovery
zb.sandboxes().cancel_transition(child.id()).await?;
```

#### CLI

```sh
# prints the new box and its operation, then returns
zipbox fork rent-jesse --name rent-jesse-2 --no-wait
# its fork field shows the phase
zipbox sandbox get rent-jesse-2
zipbox sandbox cancel rent-jesse-2
```

#### curl

```sh
curl https://api.tribes.xyz/sandbox/$CHILD_ID/fork \
  -H "Authorization: Bearer $ZIPBOX_API_KEY"
# cancel the fork
curl -X POST https://api.tribes.xyz/sandbox/$CHILD_ID/transition/cancel \
  -H "Authorization: Bearer $ZIPBOX_API_KEY"
```

`GET /sandbox/:id/fork` takes the **new** box's id and answers the fork; `404 not_found` when that box was not made by a fork or is not yours. While the fork runs, the new box's own row (`GET /sandbox/:id`, `GET /sandbox`) carries the same object in `fork`; it is null once the fork is over.

Cancelling uses the same [cancel](https://zipbox.ai/docs/lifecycle.md#cancel-a-change-in-flight) as any other change, on the new box (or on the source while it is being captured), with the same answers. A fork cancelled before the new box boots discards it; its name stays reserved under your account, on hold, like the name of a deleted box. If the fork had paused the source, the source is resumed. Once a fork is `complete` the new box is an ordinary box: delete it to remove it.

When the SDKs wait and the fork removes the new box instead (it was cancelled or failed, so `GET /sandbox/:id` answers `not_found`), they read the fork once and raise its `error.code` (`copy_failed`, `start_failed`, …), or `conflict` when the fork was cancelled.

## Private boxes and Tailscale

A fork starts public unless you ask for a private one with `private: true`, whether or not its source is private. A private fork, with or without a key, behaves exactly as a private create: it is private from its first packet, never public, not even while it boots. A fork does **not** inherit the source's tailnet membership. Tailscale identities are never copied (two devices with one identity would share an address), and the source stays on your tailnet unchanged.

The new box joins your tailnet as a new device with an auth key you give it. Pass it to the SDK, which keeps it on your machine: the fork request never carries it. Once the new box is running with its own key, the SDK seals the auth key to that key and delivers it exactly as [`tailscale.connect`](https://zipbox.ai/docs/tailscale.md#connect-a-box) does. This is end-to-end to the box: the control plane relays ciphertext it cannot open and never stores, and only the new box can read the key. It does not protect against an attacker who controls the control plane itself and hands your machine a key of their own ([How the key is sealed](https://zipbox.ai/docs/tailscale.md#how-the-key-is-sealed)).

#### TypeScript

```ts
const child = await zb.sandboxes.fork(source.id, {
  name: 'build-2',
  sourcePolicy: 'require_paused',
  private: true,
  tailscaleAuthKey: process.env.TS_AUTHKEY!
})
```

#### Python

```python
child = zb.sandboxes.fork(
    source.id,
    name="build-2",
    source_policy="require_paused",
    private=True,
    tailscale_auth_key=os.environ["TS_AUTHKEY"],
)
```

#### Go

```go
child, err := c.Sandboxes.Fork(ctx, source.ID, zipbox.ForkOptions{
	Name:             "build-2",
	SourcePolicy:     zipbox.ForkRequirePaused,
	Private:          true,
	TailscaleAuthKey: os.Getenv("TS_AUTHKEY"),
})
```

#### Rust

```rust
let mut options = zipbox::ForkOptions::new(
    "build-2",
    zipbox::ForkSourcePolicy::RequirePaused,
);
options.private = Some(true);
options.tailscale_auth_key = Some(std::env::var("TS_AUTHKEY")?);
let child = zb.sandboxes().fork(source.id(), options).await?;
```

#### CLI

```sh
# the key is read from the file (or stdin with -), never from argv
zipbox fork build --name build-2 --private --tailscale-auth-key-file ./ts.key
```

#### curl

```sh
curl -X POST https://api.tribes.xyz/sandbox/$ID/fork \
  -H "Authorization: Bearer $ZIPBOX_API_KEY" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"name":"build-2","sourcePolicy":"require_paused","private":true}'
# then wait for the NEW box to be running with an agentPublicKey,
# seal to it, and PUT /sandbox/$CHILD_ID/tailscale
```

The auth key needs the wait, since it is sealed to the new box once that box runs: an SDK refuses `tailscaleAuthKey` with `wait: false` as `invalid_request` before sending anything. Over plain HTTP the fork and the enrollment are two steps; follow [Tailscale](https://zipbox.ai/docs/tailscale.md#connect-a-box) for the second.

Which key to use: a one-off key is spent by the device it enrolls, so the source's original key cannot enroll the new box and Zipbox never keeps or reuses it. Use a new one-off key, or enter a reusable key again. Prefer a pre-approved, non-ephemeral key; the key decides which tailnet the new box joins and with which tags, and approval, ACLs and Tailnet Lock stay with your tailnet. A key that is refused after the fork leaves a running, private box that is not on your tailnet yet: connect it again with another key. A private fork without a key stays private and waits for you to connect it from its **Settings → Tailscale** in the dashboard. A key without `private` joins a public box to your tailnet. The dashboard's fork dialog asks for private only beside a key, as **New machine** does.

## API keys and machine-scoped credentials

A source can hold provider keys scoped to that one machine, or be a selected member of a custom API. Copying those into the new box is a credential change, and credential changes need a signed-in owner, never an API key. So an **API-key** fork of such a source answers `403 requires_interactive` before anything is reserved and before the source is touched: fork it from the dashboard instead, signed in as its owner, and everything is copied.

An API key can fork every other source. Account-wide provider keys and custom APIs reach the new box through your account settings, and restrictions (included-service off switches, egress policy) always copy, whoever forks. Private mode is the request's own (`private`), never copied.

## How long a fork takes

A fork has no fixed duration, and nothing in it times out. Three intervals add up differently:

| Interval                 | Made of                                              | Depends on                                                                                                  |
| ------------------------ | ---------------------------------------------------- | ----------------------------------------------------------------------------------------------------------- |
| Acceptance               | Checks and the reservation                           | One request                                                                                                 |
| Source downtime          | Stop, capture, restart (running sources only)        | `captureMode`: `reflink` is metadata work, `copy` copies the whole disk image while the source stays paused |
| Until the new box serves | Identity, capture, copying live files, a normal boot | How much data is on the disk and how many files                                                             |

The copy into a fresh filesystem scales with the live data and file count even when the capture itself is instant, so a box with a large repository or millions of small files takes longer to fork than a nearly empty one. Fork a paused source to avoid any downtime at all.

## What a fork does not reproduce

- **Memory and processes.** No RAM, process, socket or terminal state is copied. Services and agents that start on boot start fresh.
- **Work that runs on its own runs twice.** Cron jobs, timers, Docker restart policies, queue workers, CI runners and agents on the new box run alongside the source's. Turn off what must run once before you fork, or on the new box right after.
- **External services are shared, not copied.** Remote databases, buckets, queues and third-party accounts the box talks to are the same ones on both boxes. A fork copies nothing outside the box.
- **Application identities.** Node ids, license keys, cluster membership and other identities an application keeps in its own files are copied as files; reconfigure them where two copies would clash.
- **Custom domains.** A custom domain stays with the source; the new box serves its own name.
- **A byte-identical disk.** The files are the same; filesystem metadata, platform keys and identity files differ.

## MCP

`fork_sandbox` takes `id` (the source), `name`, `pause?` and `private?`. `pause: true` is `pause_and_resume`; without it a running source answers `pause_required`. It returns at once with the new box, its operation id and its phase: the model follows it with `get_sandbox` (the row's `fork` field) and cancels it with `cancel_sandbox_transition` on the new box.

There is no Tailscale key parameter, by design: a key passed to a tool would sit in plain text in the model's transcript. The new box starts public unless `private: true`; a private one is reached only from the owner's tailnet, which they connect from the dashboard (`tailscale_connect_link`). An API-key MCP session gets the same `requires_interactive` answer as REST. See [MCP](https://zipbox.ai/docs/mcp.md).

## REST reference

`POST /sandbox/:id/fork`, with the **source** id. Headers: `Authorization`, `Content-Type: application/json`, and a required `Idempotency-Key` (1 to 255 characters, unique per fork you mean to make). The body is exactly:

| Field          | Type                                   | Meaning                                            |
| -------------- | -------------------------------------- | -------------------------------------------------- |
| `name`         | string                                 | The new box's permanent name                       |
| `sourcePolicy` | `require_paused` or `pause_and_resume` | Whether a running source may pause for its capture |
| `private`      | boolean, optional                      | Start the new box private; absent or false, public |

Any other field is refused with `400 invalid_request`: the body takes no SSH key, Tailscale key, setup script or credential. The answer is `202` with `{operationId, sandbox, fork}`: the operation's id, the new box's row (`status: provisioning`) and the fork. Sending the same `Idempotency-Key` with the same body again returns the same `202`, the same operation and the same new box, so a fork whose answer you lost is always safe to resend; a cancelled fork answers as cancelled and is not made again. The same key with a different body answers `409 idempotency_mismatch`.

`GET /sandbox/:id/fork`, with the **new** box's id, answers the fork:

| Field                                   | Meaning                                                             |
| --------------------------------------- | ------------------------------------------------------------------- |
| `operationId`                           | The fork's id                                                       |
| `sourceSandboxId`, `childSandboxId`     | The source and the new box                                          |
| `sourcePolicy`                          | As sent                                                             |
| `phase`                                 | [Timeline](#timeline)                                               |
| `sourceRecovery`                        | `not_needed`, `pending`, `resumed` or `superseded`                  |
| `captureMode`                           | `reflink` or `copy`; null before the capture                        |
| `waitingOn`, `attempt`, `nextAttemptAt` | Why the current step waits and when it retries; null while it moves |
| `cancelRequestedAt`                     | Set once a cancel was asked for                                     |
| `error`                                 | `{code, message}` once `failed`                                     |
| `createdAt`, `updatedAt`, `completedAt` | Timestamps                                                          |

## Errors

Refused before anything is reserved or the source is touched:

| Status | Code                                                          | When                                                                                                                                                 |
| ------ | ------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------- |
| 400    | [`invalid_request`](https://zipbox.ai/docs/errors.md#invalid_request)             | A bad body, or no `Idempotency-Key`                                                                                                                  |
| 402    | [`insufficient_credits`](https://zipbox.ai/docs/errors.md#insufficient_credits)   | No credit balance                                                                                                                                    |
| 403    | [`forbidden`](https://zipbox.ai/docs/errors.md#forbidden)                         | A reserved name, or the account cannot create boxes yet                                                                                              |
| 403    | [`machine_limit_reached`](https://zipbox.ai/docs/errors.md#machine_limit_reached) | The account already holds 250 machine names                                                                                                          |
| 403    | [`requires_interactive`](https://zipbox.ai/docs/errors.md#requires_interactive)   | The source holds machine-scoped credentials and an API key asked: fork it from the dashboard                                                         |
| 404    | [`not_found`](https://zipbox.ai/docs/errors.md#not_found)                         | No such source, or not yours                                                                                                                         |
| 409    | [`name_taken`](https://zipbox.ai/docs/errors.md#name_taken)                       | A box is using the new name, or another account holds it                                                                                             |
| 409    | [`source_unsupported`](https://zipbox.ai/docs/errors.md#source_unsupported)       | The source is a kind of box a fork does not support                                                                                                  |
| 409    | [`source_busy`](https://zipbox.ai/docs/errors.md#source_busy)                     | The source is in another change (a pause, a resize, another fork)                                                                                    |
| 409    | [`pause_required`](https://zipbox.ai/docs/errors.md#pause_required)               | The source is running and `sourcePolicy` is `require_paused`                                                                                         |
| 409    | [`idempotency_mismatch`](https://zipbox.ai/docs/errors.md#idempotency_mismatch)   | The `Idempotency-Key` was used for a different fork                                                                                                  |
| 503    | [`unavailable`](https://zipbox.ai/docs/errors.md#unavailable)                     | Maintenance, no room on the source's host, the host out of reach, or another request accepting this fork: resend with the **same** `Idempotency-Key` |

None of these is retried by the SDKs except `unavailable` and `rate_limited`, which they resend with the same key.

A fork that was accepted ends `failed` only for a reason that would fail again; its `error.code` is one of:

| Code                                                    | Meaning                                                                                           |
| ------------------------------------------------------- | ------------------------------------------------------------------------------------------------- |
| [`source_unsupported`](https://zipbox.ai/docs/errors.md#source_unsupported) | The source's filesystem layout is one the copy does not support                                   |
| [`source_gone`](https://zipbox.ai/docs/errors.md#source_gone)               | The source was deleted before it was captured                                                     |
| [`identity_reused`](https://zipbox.ai/docs/errors.md#identity_reused)       | The new box came up with one of the source's keys, so it was stopped before serving and discarded |
| [`copy_failed`](https://zipbox.ai/docs/errors.md#copy_failed)               | The copy could not produce a verified filesystem                                                  |
| [`start_failed`](https://zipbox.ai/docs/errors.md#start_failed)             | The new box failed to boot                                                                        |

A failed fork resumes a source it paused and discards the new box; its name stays reserved under your account.
