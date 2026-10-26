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

## Create a box

`create` takes a `name` (the box's permanent subdomain, `<name>.zbox.sh`), an optional `tier`, and an optional `sshPublicKey`. It sends an `Idempotency-Key`, so a retried create never makes two boxes, and then reads `GET /sandbox/:id` with backoff until `ready` is true. That wait is the SDK's, never the box's: the box boots as fast as it can.

#### TypeScript

```ts
const box = await zb.sandboxes.create({ name: 'ci-runner', tier: 'm' })
```

#### Python

```python
box = zb.sandboxes.create(name="ci-runner", tier="m")
```

#### Go

```go
box, err := c.Sandboxes.Create(ctx, zipbox.CreateOptions{
	Name: "ci-runner",
	Tier: "m",
})
```

#### Rust

```rust
let options = zipbox::CreateOptions {
    name: Some("ci-runner".into()),
    tier: Some("m".into()),
    ..Default::default()
};
let sbx = zb.sandboxes().create(options).await?;
```

#### CLI

```sh
zipbox sandbox create --name ci-runner --tier m
```

#### curl

```sh
curl -X POST https://api.tribes.xyz/sandbox/identity/claim \
  -H "Authorization: Bearer $ZIPBOX_API_KEY" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"name":"ci-runner","tier":"m"}'
```

Tiers are `xs`, `s`, `m`, `l`, `xl` and `xxl`; prices are on the [pricing page](/pricing). With no credit balance, `create`, `start` and `setTier` answer `402 insufficient_credits`.

## Reuse a box by name

`getOrCreate(name)` is the "keep my environment" pattern: there are no templates or snapshots, because a box _is_ its environment. Install your toolchain once and every later run picks up where the last one left off.

#### TypeScript

```ts
const box = await zb.sandboxes.getOrCreate('ci-runner')
```

#### Python

```python
box = zb.sandboxes.get_or_create("ci-runner")
```

#### Go

```go
box, err := c.Sandboxes.GetOrCreate(ctx, "ci-runner")
```

#### Rust

```rust
let sbx = zb.sandboxes().get_or_create("ci-runner").await?;
```

#### CLI

```sh
# look the box up by name
zipbox sandbox get ci-runner
# no box yet: create it
zipbox sandbox create --name ci-runner
```

#### curl

```sh
curl -s https://api.tribes.xyz/sandbox \
  -H "Authorization: Bearer $ZIPBOX_API_KEY" \
  | jq '.[] | select(.name == "ci-runner")'
```

If the box with that name is stopped, `getOrCreate` starts it first; either way it returns once the box is ready. The CLI and curl forms above do not: a stopped box stays stopped until you `start` it, and calling it before then raises `box_not_running`.

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

Stop and start are asynchronous on the control plane: the call returns once the change is accepted. Read `get(id)` for `status` (`running`, `provisioning`, `archived`, …) and `ready`.

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
err = c.Sandboxes.Delete(ctx, box.ID)
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

By default a deleted box keeps a recoverable copy for 30 days. Send `{"archiveBeforeDestroy": false}` to delete it outright.

## Credits and cost

A running box bills by the hour at its tier's rate; a stopped box bills nothing. Pricing is identical to the dashboard - code is just another way to press the same buttons. Read your balance with `zb.account.balance()` (`GET /billing/summary`).
