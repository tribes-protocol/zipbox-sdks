> Zipbox docs, markdown edition of https://zipbox.ai/docs/lifecycle
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# Boxes

Create, getOrCreate, stop, start, restart, resize and delete boxes - and what each costs.

A box is a long-lived machine, not a disposable sandbox. It runs until you stop it. Nothing stops, pauses or deletes a box on a clock.

| Verb           | Route                              | What happens                                                          |
| -------------- | ---------------------------------- | --------------------------------------------------------------------- |
| `create`       | `POST /sandbox/identity/claim`     | A new box with that name; the SDK waits until it is ready             |
| `getOrCreate`  | `GET /sandbox`, then claim         | The box with that name (started if stopped), created the first time   |
| `list` / `get` | `GET /sandbox`, `GET /sandbox/:id` | Your boxes, with status and the latest CPU / memory / disk sample     |
| `stop`         | `POST /sandbox/:id/pause`          | Disk archived to cold storage; the box bills nothing while stopped    |
| `start`        | `POST /sandbox/:id/restore`        | The same disk comes back on a running machine                         |
| `restart`      | `POST /sandbox/:id/reset`          | Hard reboot of a running box; files survive, running processes do not |
| `setTier`      | `POST /sandbox/:id/tier`           | Move to a bigger or smaller machine; files survive                    |
| `delete`       | `DELETE /sandbox/:id`              | The box is deleted; a recoverable copy is kept for 30 days by default |
| `setPrivate`   | `PUT /sandbox/:id/private`         | Only your tailnet can reach the box ([Tailscale](https://zipbox.ai/docs/tailscale.md))    |

## Create a box

`create` takes a `name` (the box's permanent subdomain, `<name>.zbox.sh`), an optional `tier`, and an optional `sshPublicKey`. It sends an `Idempotency-Key`, so a retried create never makes two boxes, and then reads `GET /sandbox/:id` with backoff until `ready` is true. That wait is the SDK's, never the box's: the box boots as fast as it can.

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

Tiers are `xs`, `s`, `m`, `l`, `xl` and `xxl`; prices are on the [pricing page](/pricing). With no credit balance, `create`, `start` and `setTier` answer `402 insufficient_credits`.

A box is created without a mailbox. Its address `<name>@zbox.sh` is reserved with the name; the mailbox is created the first time the box or its owner uses mail (`tribes-email` inside the box, opening the box in the dashboard, or a `/sandbox/:id/email` request). Pass `mailbox: true` to create it at claim time instead, for a box that must receive mail before anything on it has asked.

## Reuse a box by name

`getOrCreate(name, options)` is the "keep my environment" pattern: there are no templates or snapshots, because a box _is_ its environment. Install your toolchain once and every later run picks up where the last one left off.

It takes every `create` option except the name (`tier`, `sshPublicKey`, `mailbox`, `private`) in every SDK, and uses them only when it creates the box. A box that already has the name is returned as it is, even when it differs from the options (another tier, not private): that is not an error, and nothing is resized. Check its tier and call `setTier` if it matters.

Reusing a name also reuses its mailbox and wallets. A recurring job should claim one name and keep it, not create a box per run: every fresh name reserves an address, and every mailbox you then use is kept for you. An account holds 250 names; see [Machine limit](https://zipbox.ai/docs/machine-limit.md).

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

If the box with that name is stopped, `getOrCreate` starts it first; either way it returns once the box is ready. A box whose `stop` has not finished yet is started as soon as it has, so `getOrCreate` never hands back a box that is on its way down and never creates a second one with the same name. The CLI and curl forms above do not: a stopped box stays stopped until you `start` it, and calling it before then raises `box_not_running`.

### Keep a pool for recurring jobs

For work that runs again and again, keep a small pool of named boxes (`worker-1` … `worker-n`, one per run that can happen at the same time). Each run calls `getOrCreate` with a name from the pool and `stop`s the box when it ends, instead of deleting it.

- **Setup runs once.** Packages, browsers, caches and cloned repositories are still on the disk, so the next run starts at the work, not the install.
- **A stopped box bills nothing**, and keeps its name, mailbox and wallets.

The first run creates `worker-1`; every later run starts that same box again with everything it installed. In the samples, `setup.sh` is your install script: have it exit at once when `~/.setup-done` exists and create that file when it finishes, so it installs only once.

#### TypeScript

```ts
// first run: creates worker-1; later runs: start the stopped worker-1
const box = await zb.sandboxes.getOrCreate('worker-1', { tier: 'm' })
// installs on the first run, exits at once after that
await box.exec('./setup.sh')
await box.exec('./job.sh')
// stop, not delete: the disk is kept
await zb.sandboxes.stop(box.id)
```

#### Python

```python
# first run: creates worker-1; later runs: start the stopped worker-1
box = zb.sandboxes.get_or_create("worker-1", tier="m")
# installs on the first run, exits at once after that
box.exec("./setup.sh")
box.exec("./job.sh")
# stop, not delete: the disk is kept
zb.sandboxes.stop(box.id)
```

#### Go

```go
// first run: creates worker-1; later runs: start the stopped worker-1
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
// stop, not delete: the disk is kept
return c.Sandboxes.Stop(ctx, box.ID)
```

#### Rust

```rust
// first run: creates worker-1; later runs: start the stopped worker-1
let opts = zipbox::CreateOptions {
    tier: Some("m".into()),
    ..Default::default()
};
let sbx = zb.sandboxes().get_or_create_with("worker-1", opts).await?;
// installs on the first run, exits at once after that
sbx.exec("./setup.sh").await?;
sbx.exec("./job.sh").await?;
// stop, not delete: the disk is kept
zb.sandboxes().stop(sbx.id()).await?;
```

#### CLI

```sh
# first run only: create the box (waits until it is ready)
zipbox sandbox create --name worker-1 --tier m
# later runs: start the stopped box (waits for a stop still in progress)
zipbox sandbox start worker-1
# later runs: repeat until "ready" is true
zipbox sandbox get worker-1 --json | jq .ready
# every run: set up (exits at once after the first) and work
zipbox exec worker-1 -- ./setup.sh
zipbox exec worker-1 -- ./job.sh
# stop, not delete: the disk is kept
zipbox sandbox stop worker-1
```

#### curl

```sh
AUTH="Authorization: Bearer $ZIPBOX_API_KEY"
# first run only: create the box
ID=$(curl -s -X POST https://api.tribes.xyz/sandbox/identity/claim \
  -H "$AUTH" -H "Idempotency-Key: $(uuidgen)" \
  --json '{"name":"worker-1","tier":"m"}' | jq -r .sandboxId)
# later runs: find the stopped box by name and start it
ID=$(curl -s https://api.tribes.xyz/sandbox -H "$AUTH" \
  | jq -r '.[] | select(.name == "worker-1") | .id')
# a box whose last stop is still finishing reads "stopping":
# repeat until it reads "archived", then start it
curl -s https://api.tribes.xyz/sandbox/$ID -H "$AUTH" | jq -r .status
curl -s -X POST https://api.tribes.xyz/sandbox/$ID/restore -H "$AUTH"
# repeat until "ready" is true, then run setup.sh and job.sh
# with its baseUrl and boxToken (see Run commands)
curl -s https://api.tribes.xyz/sandbox/$ID -H "$AUTH" | jq .ready
# stop, not delete: the disk is kept
curl -s -X POST https://api.tribes.xyz/sandbox/$ID/pause -H "$AUTH"
```

`stop` returns once it is accepted. The disk is then archived, which takes from seconds to minutes depending on how much is on it; until it lands the box reads `status: stopping` and `ready: false`. The SDKs' `start` and `getOrCreate` and the CLI's `start` wait for it and then start the box, so the next run on that name can begin as soon as the last one has called `stop`. Over plain HTTP, `start` on a `stopping` box answers `409 conflict`: read `GET /sandbox/:id` until `status` is `archived`, then start it. Runs that overlap take different names from the pool.

`start` also answers `409 conflict` for a box that is running, for example one whose last run ended before its `stop`; go straight to the job.

How fast `start` is depends on where the disk is. If the host that last ran the box still holds it, the box is back in seconds, about as fast as a `create`. Otherwise it is restored from cold storage: under a minute for a Tiny box, minutes for larger disks. A `create` is a fresh machine in seconds either way, so a pool pays off when your setup takes longer than that restore.

## Stop, start, restart

#### TypeScript

```ts
await zb.sandboxes.stop(box.id)
await zb.sandboxes.start(box.id)
await zb.sandboxes.restart(box.id)
```

#### Python

```python
zb.sandboxes.stop(box.id)
zb.sandboxes.start(box.id)
zb.sandboxes.restart(box.id)
```

#### Go

```go
_ = c.Sandboxes.Stop(ctx, box.ID)
_ = c.Sandboxes.Start(ctx, box.ID)
_ = c.Sandboxes.Restart(ctx, box.ID)
```

#### Rust

```rust
zb.sandboxes().stop(sbx.id()).await?;
zb.sandboxes().start(sbx.id()).await?;
zb.sandboxes().restart(sbx.id()).await?;
```

#### CLI

```sh
# stop the box
zipbox sandbox stop ci-runner
# start it again
zipbox sandbox start ci-runner
# or restart it in one step
zipbox sandbox restart ci-runner
```

#### curl

```sh
# stop the box
curl -X POST https://api.tribes.xyz/sandbox/$ID/pause \
  -H "Authorization: Bearer $ZIPBOX_API_KEY"
# start it again
curl -X POST https://api.tribes.xyz/sandbox/$ID/restore \
  -H "Authorization: Bearer $ZIPBOX_API_KEY"
# or restart it in one step
curl -X POST https://api.tribes.xyz/sandbox/$ID/reset \
  -H "Authorization: Bearer $ZIPBOX_API_KEY"
```

Stop and start are asynchronous on the control plane: the call returns once the change is accepted. Read `get(id)` for `status` (`provisioning`, `running`, `stopping`, `archived`, …) and `ready`. A box reads `stopping` from the moment its `stop` is accepted until its disk has been archived; `start` waits that out in the SDKs and the CLI.

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

A running box is archived and restored into the new size; its files come with it.

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

**A box on your tailnet is signed out first**, so it leaves your Tailscale admin with it. A running box signs out, retried until it succeeds, and is then deleted. A stopped box that joined a tailnet is restored and booted in a minimal mode that runs none of your services, signs out, and is then deleted; meanwhile its `status` is `deleting`, that boot is never billed, and the dashboard shows its stages (Restoring with a percentage, Starting, Signing out of Tailscale, Deleting). `delete` returns once the delete is accepted either way. A box that never joined deletes as before ([Tailscale](https://zipbox.ai/docs/tailscale.md#deleting-a-box-signs-it-out)).

## Credits and cost

A running box bills by the hour at its tier's rate; a stopped box bills nothing. Pricing is identical to the dashboard - code is just another way to press the same buttons. Read your balance with `zb.account.balance()` (`GET /billing/summary`).
