> Zipbox docs, markdown edition of https://zipbox.ai/docs/lifecycle
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# Boxes

Create, getOrCreate, pause, resume, restart, fork, resize and delete boxes - and what each costs.

A box is a long-lived machine, not a disposable sandbox. It runs until you pause it. Nothing pauses, stops or deletes a box on a clock.

| Verb               | Route                                 | What happens                                                                                         |
| ------------------ | ------------------------------------- | ---------------------------------------------------------------------------------------------------- |
| `create`           | `POST /sandbox/identity/claim`        | A new box with that name, returned once it is ready                                                  |
| `getOrCreate`      | `GET /sandbox`, then claim            | The box with that name (resumed if paused), created the first time                                   |
| `list` / `get`     | `GET /sandbox`, `GET /sandbox/:id`    | Your boxes, with status and the latest CPU / memory / disk sample                                    |
| `pause`            | `POST /sandbox/:id/pause`             | The box is paused and bills nothing; returned once it reads `paused`                                 |
| `resume`           | `POST /sandbox/:id/resume`            | The paused box runs again with its files and installed software; returned once it is ready           |
| `waitReady`        | `GET /sandbox/:id/wait?until=ready`   | Returns once the box is ready; the server answers when it is                                         |
| `restart`          | `POST /sandbox/:id/reset`             | Hard reboot of a running box; files survive, running processes do not                                |
| `cancelTransition` | `POST /sandbox/:id/transition/cancel` | Cancel the pause, resume, restart, resize or fork in flight; the box goes back where it was          |
| `fork`             | `POST /sandbox/:id/fork`              | A new box with this box's files and settings, its own name and a fresh identity ([Fork](https://zipbox.ai/docs/fork.md)) |
| `setTier`          | `POST /sandbox/:id/tier`              | Move to a bigger or smaller machine; files survive                                                   |
| `delete`           | `DELETE /sandbox/:id`                 | The box is deleted; a recoverable copy is kept for 30 days by default                                |
| `setPrivate`       | `PUT /sandbox/:id/private`            | Only your tailnet can reach the box ([Tailscale](https://zipbox.ai/docs/tailscale.md))                                   |

`stop` and `start` are aliases of `pause` and `resume` in every SDK, the CLI and the MCP server: the same request and the same result. `POST /sandbox/:id/restore` is the same route as `/resume`.

## Create a box

`create` takes a `name` (the box's permanent subdomain, `<name>.zbox.sh`), an optional `tier`, and an optional `sshPublicKey`. It sends an `Idempotency-Key`, so a retried create never makes two boxes. When the server binds an already-booted machine inside the request, its answer says `ready: true` with the box's `baseUrl`, and `create` returns at once. Otherwise it waits on `GET /sandbox/:id/wait?until=ready`, which the server answers as soon as the box's [`readiness`](https://zipbox.ai/docs/http-api.md#status-phase-and-readiness) is no longer `settling` ([Waiting](https://zipbox.ai/docs/http-api.md#waiting-for-a-box)). That wait is the SDK's, never the box's: the box boots as fast as it can. It has no deadline, and a box that will not come up by itself ends it with an error, not retryable: paused `box_not_running`, a failed start `start_failed`, deleting or gone `conflict`. `getOrCreate`, `waitReady` and `fork` wait the same way. The SDKs ask for the create's answer streamed but never resolve or connect to the address its first line names: that name's DNS record may not exist yet, and an early lookup caches a wrong or negative answer. The box's address is the `baseUrl` in the final answer. A created box answers first on its machine's boot address and its `baseUrl` moves once, to `box--<name>`, a few minutes later; a box call that fails before anything was sent runs this same wait again and retries on the new address, so a client created once keeps working.

#### TypeScript

```ts
const box = await zb.sandboxes.create({ name: 'ci-runner', tier: 'm' })
// mailbox: true  — create the mailbox now
```

#### Python

```python
box = zb.sandboxes.create(name="ci-runner", tier="m")
# mailbox=True  — create the mailbox now
```

#### Go

```go
box, err := c.Sandboxes.Create(ctx, zipbox.CreateOptions{
	Name: "ci-runner",
	Tier: "m",
	// Mailbox: true — create the mailbox now
})
```

#### Rust

```rust
let options = zipbox::CreateOptions {
    name: Some("ci-runner".into()),
    tier: Some("m".into()),
    // mailbox: Some(true) — create the mailbox now
    ..Default::default()
};
let sbx = zb.sandboxes().create(options).await?;
```

#### CLI

```sh
zipbox sandbox create --name ci-runner --tier m
# --mailbox  — create the mailbox now
```

#### curl

```sh
curl -X POST https://api.tribes.xyz/sandbox/identity/claim \
  -H "Authorization: Bearer $ZIPBOX_API_KEY" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"name":"ci-runner","tier":"m"}'
# add "mailbox":true to create the mailbox now
```

Tiers are `xs`, `s`, `m`, `l`, `xl` and `xxl`; prices are on the [pricing page](/pricing). With no credit balance, `create`, `resume` and `setTier` answer `402 insufficient_credits`.

A box is created without a mailbox. Its address `<name>@zbox.sh` is reserved with the name; the mailbox is created the first time the box or its owner uses mail (`tribes-email` inside the box, opening the box in the dashboard, or a `/sandbox/:id/email` request). Pass `mailbox: true` to create it at claim time instead, for a box that must receive mail before anything on it has asked. That runs in the background: the create answers without waiting for it.

A box is also created without wallets, and nothing about a create waits on them. Its EVM and Solana wallet pair is created once, the first time it is used: `tribes-wallet` inside the box, or the owner opening the box's Wallet app in the dashboard. From then on the box can receive funds. Spending needs the box's signer, which the owner's browser attaches while the dashboard is open and the box is running; until then `tribes-wallet` refuses to sign and says so. A box driven only through the API can therefore hold and receive funds but cannot spend them until its owner opens the dashboard once.

## Reuse a box by name

`getOrCreate(name, options)` is the "keep my environment" pattern: there are no templates or snapshots, because a box _is_ its environment. Install your toolchain once and every later run picks up where the last one left off.

It takes every `create` option except the name (`tier`, `sshPublicKey`, `mailbox`, `private`) in every SDK, and uses them only when it creates the box. A box that already has the name is returned as it is, even when it differs from the options (another tier, not private): that is not an error, and nothing is resized. Check its tier and call `setTier` if it matters.

Reusing a name also reuses its mailbox and wallets. A recurring job should claim one name and keep it, not create a box per run: every fresh name reserves an address, and every mailbox and wallet you then use is kept for you. An account holds 250 names; see [Machine limit](https://zipbox.ai/docs/machine-limit.md).

#### TypeScript

```ts
// the options apply only the first time, when the box is created
const box = await zb.sandboxes.getOrCreate('ci-runner', { tier: 'l' })
```

#### Python

```python
# the options apply only the first time, when the box is created
box = zb.sandboxes.get_or_create("ci-runner", tier="l")
```

#### Go

```go
// the options apply only the first time, when the box is created
opts := zipbox.CreateOptions{Tier: "l"}
box, err := c.Sandboxes.GetOrCreate(ctx, "ci-runner", opts)
```

#### Rust

```rust
// the options apply only the first time, when the box is created
let opts = zipbox::CreateOptions {
    tier: Some("l".into()),
    ..Default::default()
};
let sbx = zb.sandboxes().get_or_create_with("ci-runner", opts).await?;
```

#### CLI

```sh
# look the box up by name
zipbox sandbox get ci-runner
# no box yet: create it
zipbox sandbox create --name ci-runner --tier l
```

#### curl

```sh
curl -s https://api.tribes.xyz/sandbox \
  -H "Authorization: Bearer $ZIPBOX_API_KEY" \
  | jq '.[] | select(.name == "ci-runner")'
```

If the box with that name is paused, `getOrCreate` resumes it first; either way it returns once the box is ready. A box whose `pause` has not landed yet is resumed as soon as it has, so `getOrCreate` never hands back a box that is on its way down and never creates a second one with the same name. The CLI and curl forms above do not: a paused box stays paused until you `resume` it, and calling it before then raises `box_not_running`.

### Keep a pool for recurring jobs

For work that runs again and again, keep a small pool of named boxes (`worker-1` … `worker-n`, one per run that can happen at the same time). Each run calls `getOrCreate` with a name from the pool and `pause`s the box when it ends, instead of deleting it.

- **Setup runs once.** Packages, browsers, caches and cloned repositories are still on the disk, so the next run starts at the work, not the install.
- **A paused box bills nothing**, and keeps its name, mailbox and wallets.

The first run creates `worker-1`; every later run resumes that same box with everything it installed. In the samples, `setup.sh` is your install script: have it exit at once when `~/.setup-done` exists and create that file when it finishes, so it installs only once.

#### TypeScript

```ts
// first run: creates worker-1; later runs: resume the paused worker-1
const box = await zb.sandboxes.getOrCreate('worker-1', { tier: 'm' })
// installs on the first run, exits at once after that
await box.exec('./setup.sh')
await box.exec('./job.sh')
// pause, not delete: the disk is kept
await zb.sandboxes.pause(box.id)
```

#### Python

```python
# first run: creates worker-1; later runs: resume the paused worker-1
box = zb.sandboxes.get_or_create("worker-1", tier="m")
# installs on the first run, exits at once after that
box.exec("./setup.sh")
box.exec("./job.sh")
# pause, not delete: the disk is kept
zb.sandboxes.pause(box.id)
```

#### Go

```go
// first run: creates worker-1; later runs: resume the paused worker-1
opts := zipbox.CreateOptions{Tier: "m"}
box, err := c.Sandboxes.GetOrCreate(ctx, "worker-1", opts)
if err != nil {
	return err
}
// installs on the first run, exits at once after that
if _, err := box.Exec(ctx, "./setup.sh"); err != nil {
	return err
}
if _, err := box.Exec(ctx, "./job.sh"); err != nil {
	return err
}
// pause, not delete: the disk is kept
_, err = c.Sandboxes.Pause(ctx, box.ID)
return err
```

#### Rust

```rust
// first run: creates worker-1; later runs: resume the paused worker-1
let opts = zipbox::CreateOptions {
    tier: Some("m".into()),
    ..Default::default()
};
let sbx = zb.sandboxes().get_or_create_with("worker-1", opts).await?;
// installs on the first run, exits at once after that
sbx.exec("./setup.sh").await?;
sbx.exec("./job.sh").await?;
// pause, not delete: the disk is kept
zb.sandboxes().pause(sbx.id()).await?;
```

#### CLI

```sh
# first run only: create the box (waits until it is ready)
zipbox sandbox create --name worker-1 --tier m
# later runs: resume the paused box (waits for a pause still landing,
# then until the box is ready)
zipbox sandbox resume worker-1
# every run: set up (exits at once after the first) and work
zipbox exec worker-1 -- ./setup.sh
zipbox exec worker-1 -- ./job.sh
# pause, not delete: the disk is kept
zipbox sandbox pause worker-1
```

#### curl

```sh
AUTH="Authorization: Bearer $ZIPBOX_API_KEY"
# first run only: create the box
ID=$(curl -s -X POST https://api.tribes.xyz/sandbox/identity/claim \
  -H "$AUTH" -H "Idempotency-Key: $(uuidgen)" \
  --json '{"name":"worker-1","tier":"m"}' | jq -r .sandboxId)
# later runs: find the paused box by name and resume it
ID=$(curl -s https://api.tribes.xyz/sandbox -H "$AUTH" \
  | jq -r '.[] | select(.name == "worker-1") | .id')
# a box whose last pause is still landing reads "stopping":
# wait until it reads "paused", then resume it
curl -s "https://api.tribes.xyz/sandbox/$ID/wait?until=paused" \
  -H "$AUTH" | jq -r .status
curl -s -X POST https://api.tribes.xyz/sandbox/$ID/resume -H "$AUTH"
# unless that answered "ready": true, wait until it does (repeat
# while "ready" is false); then run setup.sh and job.sh with its
# baseUrl and boxToken (see Run commands)
curl -s "https://api.tribes.xyz/sandbox/$ID/wait?until=ready" \
  -H "$AUTH" | jq .ready
# pause, not delete: the disk is kept
curl -s -X POST https://api.tribes.xyz/sandbox/$ID/pause -H "$AUTH"
```

`pause` returns the box once it reads `status: paused` (`ready: false`). A box whose pause is still landing reads `status: stopping`: the SDKs' `resume` and `getOrCreate` and the CLI's `resume` wait for it and then resume the box, so the next run on that name can begin as soon as the last one has called `pause`. Over plain HTTP, `resume` on a `stopping` box answers `409 conflict`: wait with `GET /sandbox/:id/wait?until=paused`, then resume it. Runs that overlap take different names from the pool.

`resume` also answers `409 conflict` for a box that is running, for example one whose last run ended before its `pause`; go straight to the job.

How fast `resume` is depends on where the box is. A paused box stays on its host for as long as the host has room, and comes back in well under a second, its processes where they were. If the host needed the room, the box was moved to cold storage and `resume` boots it from there: under a minute for a Tiny box, minutes for larger disks. While `resume` is on its way, the SDKs open the connection to the box's address, so the first call after it does not wait for a TCP and TLS handshake. A `create` is a fresh machine in seconds either way, so a pool pays off when your setup takes longer than that.

## Pause, resume, restart

#### TypeScript

```ts
// each returns the box: paused, then ready
await zb.sandboxes.pause(box.id)
await zb.sandboxes.resume(box.id)
await zb.sandboxes.restart(box.id)
```

#### Python

```python
# each returns the box: paused, then ready
zb.sandboxes.pause(box.id)
zb.sandboxes.resume(box.id)
zb.sandboxes.restart(box.id)
```

#### Go

```go
// each returns the box: paused, then ready
_, _ = c.Sandboxes.Pause(ctx, box.ID)
_, _ = c.Sandboxes.Resume(ctx, box.ID)
_ = c.Sandboxes.Restart(ctx, box.ID)
```

#### Rust

```rust
// each returns the box: paused, then ready
zb.sandboxes().pause(sbx.id()).await?;
zb.sandboxes().resume(sbx.id()).await?;
zb.sandboxes().restart(sbx.id()).await?;
```

#### CLI

```sh
# pause the box
zipbox sandbox pause ci-runner
# resume it
zipbox sandbox resume ci-runner
# or restart it in one step
zipbox sandbox restart ci-runner
```

#### curl

```sh
# pause the box
curl -X POST https://api.tribes.xyz/sandbox/$ID/pause \
  -H "Authorization: Bearer $ZIPBOX_API_KEY"
# resume it
curl -X POST https://api.tribes.xyz/sandbox/$ID/resume \
  -H "Authorization: Bearer $ZIPBOX_API_KEY"
# or restart it in one step
curl -X POST https://api.tribes.xyz/sandbox/$ID/reset \
  -H "Authorization: Bearer $ZIPBOX_API_KEY"
```

Over HTTP, `pause` and `resume` answer `200` with the box's final state when its host answered within the request, and `202` with the box in transition otherwise (a cold resume is a boot, so it answers `202`); both carry the same body as `GET /sandbox/:id`. The SDKs return at once on a final answer and otherwise wait on `GET /sandbox/:id/wait` ([Waiting](https://zipbox.ai/docs/http-api.md#waiting-for-a-box)), with no deadline. `restart` returns once it is accepted; on a paused box it raises `conflict` (409): resume it first. A box's `status` is one of `provisioning`, `running`, `stopping`, `paused`, `deleting` and `destroyed`. A paused box reads `status: paused`, `phase: paused`, and bills nothing. Behind the pause its disk is backed up to cold storage; the box keeps reading `paused` throughout, and a `resume` never waits for that backup. A box whose pause is still landing reads `stopping`; `resume` waits that out in the SDKs and the CLI.

## Cancel a change in flight

A pause, resume, restart, resize or fork that has not finished can be cancelled, and the box goes back where it was: a cancelled pause leaves it running (an SDK `pause` waiting on it raises `conflict`), a cancelled resume leaves it paused. Cancel a fork on the new box: before it boots it is discarded and its name stays yours, and a source the fork paused is resumed. The answer says what happened:

| Outcome      | Meaning                                                                                  |
| ------------ | ---------------------------------------------------------------------------------------- |
| `cleared`    | Nothing was running on a machine yet; the change was dropped and the box is already back |
| `cancelling` | A machine is executing it; its abort is on the way and the box comes back on its own     |
| `none`       | The box had no change in flight                                                          |

A change past its point of no return (an upload that has finished, a box already booting from its restored disk, a restart already rebooting) is refused with `409 conflict` and the reason in the error message; the change then completes normally. Cancel is not retried by the SDKs.

#### TypeScript

```ts
// 'cleared' | 'cancelling' | 'none'
const outcome = await zb.sandboxes.cancelTransition(box.id)
```

#### Python

```python
# "cleared" | "cancelling" | "none"
outcome = zb.sandboxes.cancel_transition(box.id)
```

#### Go

```go
// zipbox.TransitionCleared, TransitionCancelling or TransitionNone
outcome, err := c.Sandboxes.CancelTransition(ctx, box.ID)
```

#### Rust

```rust
// TransitionCancelOutcome::Cleared, ::Cancelling or ::None
let outcome = zb.sandboxes().cancel_transition(sbx.id()).await?;
```

#### CLI

```sh
zipbox sandbox cancel ci-runner
```

#### curl

```sh
curl -X POST https://api.tribes.xyz/sandbox/$ID/transition/cancel \
  -H "Authorization: Bearer $ZIPBOX_API_KEY"
# {"outcome":"cleared","reason":null}
```

## Fork a box

A fork is a new box made from a consistent capture of this one's disk: the same files, installed software, repositories and settings, with its own name, keys, mailbox and wallets. RAM and running processes are not copied. A running source pauses for the capture only with `pause_and_resume` and resumes right after; with `require_paused` it answers `409 pause_required`. What is copied, the private-box flow and the errors: [Fork a box](https://zipbox.ai/docs/fork.md).

#### TypeScript

```ts
const child = await zb.sandboxes.fork(box.id, {
  name: 'ci-runner-2',
  sourcePolicy: 'pause_and_resume'
})
```

#### Python

```python
child = zb.sandboxes.fork(
    box.id, name="ci-runner-2", source_policy="pause_and_resume"
)
```

#### Go

```go
child, err := c.Sandboxes.Fork(ctx, box.ID, zipbox.ForkOptions{
	Name:         "ci-runner-2",
	SourcePolicy: zipbox.ForkPauseAndResume,
})
```

#### Rust

```rust
let options = zipbox::ForkOptions::new(
    "ci-runner-2",
    zipbox::ForkSourcePolicy::PauseAndResume,
);
let child = zb.sandboxes().fork(sbx.id(), options).await?;
```

#### CLI

```sh
zipbox fork ci-runner --name ci-runner-2 --pause
```

#### curl

```sh
curl -X POST https://api.tribes.xyz/sandbox/$ID/fork \
  -H "Authorization: Bearer $ZIPBOX_API_KEY" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"name":"ci-runner-2","sourcePolicy":"pause_and_resume"}'
```

## Resize

#### TypeScript

```ts
await zb.sandboxes.setTier(box.id, 'l')
```

#### Python

```python
zb.sandboxes.set_tier(box.id, "l")
```

#### Go

```go
err = c.Sandboxes.SetTier(ctx, box.ID, "l")
```

#### Rust

```rust
zb.sandboxes().set_tier(sbx.id(), "l").await?;
```

#### CLI

```sh
zipbox sandbox tier ci-runner l
```

#### curl

```sh
curl -X POST https://api.tribes.xyz/sandbox/$ID/tier \
  -H "Authorization: Bearer $ZIPBOX_API_KEY" \
  -H 'Content-Type: application/json' -d '{"tier":"l"}'
```

A running box is moved into the new size; its files come with it.

## Delete

#### TypeScript

```ts
await zb.sandboxes.delete(box.id)
```

#### Python

```python
zb.sandboxes.delete(box.id)
```

#### Go

```go
err = c.Sandboxes.Delete(ctx, box.ID, zipbox.DeleteOptions{})
```

#### Rust

```rust
zb.sandboxes().delete(sbx.id()).await?;
```

#### CLI

```sh
zipbox sandbox delete ci-runner
```

#### curl

```sh
curl -X DELETE https://api.tribes.xyz/sandbox/$ID \
  -H "Authorization: Bearer $ZIPBOX_API_KEY"
```

By default a deleted box keeps a recoverable copy for 30 days. To delete it outright, pass `{ archive: false }` (TypeScript), `archive=False` (Python), `zipbox.DeleteOptions{Archive: &no}` with `no := false` (Go) or `delete_with(id, DeleteOptions { archive: Some(false) })` (Rust); over HTTP, send `{"archiveBeforeDestroy": false}`.

**A box on your tailnet is signed out first**, so it leaves your Tailscale admin with it. A running box signs out, retried until it succeeds, and is then deleted. A paused box that joined a tailnet is booted in a minimal mode that runs none of your services, signs out, and is then deleted; meanwhile its `status` is `deleting`, that boot is never billed, and the dashboard shows its stages (Restoring with a percentage, Starting, Signing out of Tailscale, Deleting). `delete` returns once the delete is accepted either way. A box that never joined deletes as before ([Tailscale](https://zipbox.ai/docs/tailscale.md#deleting-a-box-signs-it-out)).

## Credits and cost

A running box bills by the hour at its tier's rate; a paused box bills nothing. Pricing is identical to the dashboard - code is just another way to press the same buttons. Read your balance with `zb.account.balance()` (`GET /billing/summary`).
