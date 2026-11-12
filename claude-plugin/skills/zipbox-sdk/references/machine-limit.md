> Zipbox docs, markdown edition of https://zipbox.ai/docs/machine-limit
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# Machine limit and reusing names

Why an account holds 250 machine names, what happens at the limit, and how to keep a pool of names and reuse them from every SDK, the CLI and MCP.

An account holds **250 machine names**. Reuse them: a fleet that needs many boxes keeps a fixed pool of names and gets-or-creates from it, instead of creating a box under a fresh name for every job.

## What counts

A machine **name** is permanent. The first time you create a box with a name, its subdomain (`worker-07.zbox.sh`) and its mailbox address (`worker-07@zbox.sh`) are reserved to your account, and so are its mailbox and wallets once they are first used. Deleting the box keeps all of them. That is what lets `getOrCreate(name)` bring a deleted box back under the same address.

Every name your account has ever created counts toward the 250, whether its box is running, paused or deleted. A name you already own never counts again: creating, resuming or rebooting it uses nothing up.

## What happens at the limit

Once the account holds 250 names, a `create` with a **new** name answers `403 machine_limit_reached`:

```json
{
  "error": {
    "code": "machine_limit_reached",
    "message": "You've reached this account's limit of 250 machines. …",
    "requestId": "req_…",
    "retryable": false,
    "retryAfterMs": null,
    "docsUrl": "https://zipbox.ai/docs/errors#machine_limit_reached"
  }
}
```

The full message also says that deleted names stay yours, links this page and gives the support address. Nothing is created or charged, and the SDKs do not retry it. Every name you already own keeps working: get-or-create it, resume it, or create it again after deleting its box. The dashboard shows the same message under the **Boot machine** button.

## Keep a pool of names and reuse them

Choose a fixed set of names up front (`worker-00` … `worker-49`) and always get-or-create one of them. Never use a fresh UUID or timestamp per job: each one spends a name for good. Get-or-create returns the box that has the name, creates it if there is none, and brings a deleted one back under the same name.

The create options (`tier`, `sshPublicKey`, `mailbox`, `private`) apply only when the box is created; a box that already has the name comes back as it is. See [Reuse a box by name](https://zipbox.ai/docs/lifecycle.md#reuse-a-box-by-name).

#### TypeScript

```ts
const POOL = 50
for (const [i, job] of jobs.entries()) {
  const name = `worker-${String(i % POOL).padStart(2, '0')}`
  const box = await zb.sandboxes.getOrCreate(name, { tier: 's' })
  await box.exec(job)
}
```

#### Python

```python
POOL = 50
for i, job in enumerate(jobs):
    box = zb.sandboxes.get_or_create(f"worker-{i % POOL:02d}", tier="s")
    box.exec(job)
```

#### Go

```go
const pool = 50
for i, job := range jobs {
	name := fmt.Sprintf("worker-%02d", i%pool)
	box, err := c.Sandboxes.GetOrCreate(ctx, name, zipbox.CreateOptions{Tier: "s"})
	if err != nil {
		return err
	}
	if _, err := box.Exec(ctx, job); err != nil {
		return err
	}
}
```

#### Rust

```rust
const POOL: usize = 50;
for (i, job) in jobs.iter().enumerate() {
    let name = format!("worker-{:02}", i % POOL);
    let opts = zipbox::CreateOptions {
        tier: Some("s".into()),
        ..Default::default()
    };
    let sbx = zb.sandboxes().get_or_create_with(&name, opts).await?;
    sbx.exec(job).await?;
}
```

#### CLI

```sh
# look the box up by name
zipbox sandbox get worker-07
# no box yet: create it (a deleted box's name comes back)
zipbox sandbox create --name worker-07 --tier s
```

#### curl

```sh
# the box with the name, if there is one
curl -s https://api.tribes.xyz/sandbox \
  -H "Authorization: Bearer $ZIPBOX_API_KEY" \
  | jq '.[] | select(.name == "worker-07")'
# none: claim the name (a deleted box's name boots again under the same address)
curl -X POST https://api.tribes.xyz/sandbox/identity/claim \
  -H "Authorization: Bearer $ZIPBOX_API_KEY" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"name":"worker-07","tier":"s"}'
```

**MCP:** call `list_sandboxes` and use a box you already have. To get a box you deleted back, call `create_sandbox` with its name. Ask for a new name only when no existing one fits.

## Pick names up front

`GET /sandbox` (`list()` in the SDKs) shows live boxes only; a deleted box's name is not in it, but it is still yours. Keep the pool in your own config or code, so a name whose box you deleted is chosen again on purpose rather than forgotten and replaced with a new one.

## Need more than 250?

Email support@tribes.xyz with your account email and what the fleet does.
