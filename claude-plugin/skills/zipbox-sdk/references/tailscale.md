> Zipbox docs, markdown edition of https://zipbox.ai/docs/tailscale
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# Tailscale and private boxes

Put a box on your own tailnet with a one-off auth key sealed on your machine, read its tailnet status, and make it private so only your tailnet can reach it.

A box can join **your own** [Tailscale](https://tailscale.com) tailnet. You create a one-off auth key in your Tailscale admin and hand it to the box; the box shows up as a device next to your laptop and phone, and keeps its tailnet identity across reboots, stops and restarts. Once it is connected you can make it **private**: every public way into it closes, and its usual names (`demo.zbox.sh`, its previews, its terminal, `ssh root@demo.zbox.sh`) answer only to devices on your tailnet.

The auth key is sealed on your machine to the box's own key and opened only inside the box. Zipbox relays it once and never stores, decrypts or logs it.

## Create a one-off auth key

In the Tailscale admin console, open **Settings → Keys → Generate auth key**, and choose:

| Setting      | Choose | Why                                                                           |
| ------------ | ------ | ----------------------------------------------------------------------------- |
| Reusable     | Off    | A one-off key is spent when the box joins, so a leaked copy is useless        |
| Ephemeral    | Off    | An ephemeral node is removed when it goes offline; a stopped box would vanish |
| Pre-approved | On     | Otherwise the box waits as `needsApproval` until you approve it               |
| Tags         | None   | Not required                                                                  |

The key looks like `tskey-auth-…`. Copy it once and use it once. Keep it out of chat, issue trackers and agent transcripts.

After the box joins, turn off **key expiry** for it in the admin (Machines → the box → Disable key expiry). While expiry is on, `status()` reports `keyExpiry`, and the dashboard warns you a week before it.

## Connect a box

`connect(authKey)` reads the box's `agentPublicKey` with `GET /sandbox/:id`, seals the key on your machine, and sends only the sealed envelope. The call returns once the key is on its way (`202`); the box reports its tailnet state a few seconds later. The box must be running.

#### TypeScript

```ts
const box = await zb.sandboxes.get(id)
await box.tailscale.connect(process.env.TS_AUTHKEY!)
```

#### Python

```python
box = zb.sandboxes.get(box_id)
box.tailscale.connect(os.environ["TS_AUTHKEY"])
```

#### Go

```go
box := c.Sandbox(id)
err := box.Tailscale.Connect(ctx, os.Getenv("TS_AUTHKEY"))
```

#### Rust

```rust
let sbx = zb.sandboxes().get(id).await?;
sbx.tailscale().connect(&std::env::var("TS_AUTHKEY")?).await?;
```

#### CLI

```sh
# the key comes from stdin or a file, never from the command line
zipbox tailscale connect demo --auth-key-file ./ts.key
pbpaste | zipbox tailscale connect demo
```

#### curl

```sh
# curl cannot seal: build envelope.json as described below, then
curl -X PUT https://api.tribes.xyz/sandbox/$ID/tailscale \
  -H "Authorization: Bearer $ZIPBOX_API_KEY" \
  -H 'Content-Type: application/json' -d @envelope.json
```

The box joins with `--accept-dns=false --accept-routes=false`, under its own name as the hostname. If the key never reaches the box (the box stopped before it arrived), `pending` drops back to false without the state advancing: send a new key. A key that reached the box is spent, so a later failure shows the real Tailscale state instead, such as `needsApproval`.

The dashboard does the same from **Settings → Tailscale**, or from the "Connect to your tailnet" card when you create a box.

## How the key is sealed

Use this when you call the HTTP API without an SDK. Every SDK implements it, and the one in the dashboard is the same construction.

1. Read `agentPublicKey` from `GET /sandbox/:id` **right before sealing**. It is the box's own P-256 public key, SPKI DER in standard base64. Claiming a box re-mints it, so a key read earlier may be stale. A box with no `agentPublicKey` has not started yet.
2. Generate an ephemeral P-256 key pair and run ECDH against `agentPublicKey`. The shared secret is the 32-byte x coordinate.
3. Derive a 32-byte key with HKDF-SHA256, an empty salt, and `info` = `zipbox-tailscale-v1|<sandbox id>` (UTF-8). The info binds the envelope to this box and this purpose.
4. Encrypt the auth key with AES-256-GCM under a random 12-byte iv and no associated data. Append the 16-byte tag to the ciphertext.
5. Send `{envelope: {ephemeralPublicKey, iv, ciphertext}, agentPublicKey}`. `ephemeralPublicKey` is the ephemeral key as SPKI DER. Every envelope field is base64url without padding, and `agentPublicKey` is the exact value you sealed to.

```json
{
  "envelope": {
    "ephemeralPublicKey": "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE…",
    "iv": "qoG7P5UeAXp8Ks1d",
    "ciphertext": "yHG01V57Go0rjDBV5CYrjnnz…"
  },
  "agentPublicKey": "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAENj+q…"
}
```

If the box's key changed between your read and your request, the answer is `409 box_key_changed`. Read `agentPublicKey` again, seal again, and send once more; the SDKs do this exactly once. The fixed-key test vector every SDK reproduces byte for byte is `conformance/vectors/box_envelope.json` in [tribes-protocol/zipbox-sdks](https://github.com/tribes-protocol/zipbox-sdks).

**What this protects.** The key never sits in Zipbox's database, backups, logs or error reports, and nobody with read access to them can recover it. It is end-to-end to the box. It does not protect you from someone who controls the control plane and serves you a different `agentPublicKey`.

## Read the status

#### TypeScript

```ts
const ts = await box.tailscale.status() // null: never joined, nothing pending
console.log(ts?.state, ts?.ipv4, ts?.dnsName)
```

#### Python

```python
ts = box.tailscale.status()  # None: never joined, nothing pending
if ts is not None:
    print(ts.state, ts.ipv4, ts.dns_name)
```

#### Go

```go
ts, err := box.Tailscale.Status(ctx) // nil: never joined, nothing pending
if ts != nil {
	fmt.Println(ts.State, ts.IPv4, ts.DNSName)
}
```

#### Rust

```rust
if let Some(ts) = sbx.tailscale().status().await? {
    println!("{:?} {:?}", ts.state, ts.ipv4);
}
```

#### CLI

```sh
zipbox tailscale status demo
zipbox tailscale status demo --json
```

#### curl

```sh
curl https://api.tribes.xyz/sandbox/$ID/tailscale \
  -H "Authorization: Bearer $ZIPBOX_API_KEY"
# {"tailscale":{"state":"connected","ipv4":"100.101.32.7",…},
#  "privateMode":false}
```

`GET /sandbox/:id/tailscale` answers `{tailscale, privateMode, supported}`. `tailscale` is null when the box never joined a tailnet and no key is pending. The same object is on every box row as `tailscale`, next to `privateMode` and `tailscaleSupported`.

| Field       | Meaning                                                                        |
| ----------- | ------------------------------------------------------------------------------ |
| `state`     | See below. Null while the box is stopped, or before it reports after a restart |
| `tailnet`   | Your tailnet's name, e.g. `example.com`                                        |
| `ipv4`      | The box's tailnet address, e.g. `100.101.32.7`                                 |
| `ipv6`      | Its tailnet IPv6 address                                                       |
| `dnsName`   | Its MagicDNS name, e.g. `demo.tail1234.ts.net`                                 |
| `keyExpiry` | When the node key expires (ISO 8601); null when expiry is disabled             |
| `pending`   | A key is on its way to the box                                                 |
| `peers`     | Your other devices on the tailnet (see below); null while the box is stopped   |

| `state`         | Meaning                                                                     |
| --------------- | --------------------------------------------------------------------------- |
| `joining`       | The key was accepted and the box is coming up on the tailnet                |
| `connected`     | On the tailnet                                                              |
| `needsApproval` | Your tailnet requires device approval: approve the box in the admin         |
| `needsLogin`    | The node key expired or was removed, or the key was refused: send a new key |
| `stopped`       | Logged in, but turned off (someone ran `tailscale down` on the box)         |
| `failed`        | The box's Tailscale daemon is not answering                                 |

`peers` lists your other devices as the box sees them, online first and then by name, at most 50. It is what the dashboard's Tailscale menu shows under **Your devices**.

| Peer field   | Meaning                                                                         |
| ------------ | ------------------------------------------------------------------------------- |
| `name`       | The device's own name, e.g. `lucys-macbook`                                     |
| `dnsName`    | Its MagicDNS name                                                               |
| `os`         | `macOS`, `linux`, `iOS`, `windows`, `android`, …                                |
| `online`     | Connected to Tailscale right now                                                |
| `ipv4`       | Its tailnet address                                                             |
| `connection` | `direct` (peer to peer), `relay` (through a Tailscale relay), or null when idle |

Tailscale does not tell a device whether its own key is ephemeral, so the box cannot report it. Use a key that is **not** ephemeral (step 1): an ephemeral node is removed from your tailnet whenever it goes offline, for example while the box is stopped.

### Boxes that need a ZipOS update

A box created before Tailscale support reports `tailscaleSupported: false` (and `supported: false` on `GET /sandbox/:id/tailscale`). It refuses a key with `409 tailscale_requires_update`, and the dashboard shows **Update ZipOS to use Tailscale** instead of the key field. Accept the ZipOS software update for the box, let it restart, then connect. `tailscaleSupported` is null for a moment after every boot, until the box first reports; nothing is refused while it is null.

### Notifications

The dashboard's bell tells you, once each, when a box's Tailscale key expires within 7 days ("build's Tailscale key expires in 3 days") and when a **private** box loses its tailnet connection ("build is private but Tailscale is down"). Both open the box's **Settings → Tailscale**. A connection that flaps raises at most one "down" notification per box every 6 hours.

Inside the box, `tailscale status` talks to the same managed daemon.

## Disconnect

`disconnect()` logs the box out of the tailnet, removes the device from your admin, and wipes its Tailscale state. Disconnecting a private box makes it public first, in the same call, so it never ends up private with no way in.

#### TypeScript

```ts
await box.tailscale.disconnect()
```

#### Python

```python
box.tailscale.disconnect()
```

#### Go

```go
err = box.Tailscale.Disconnect(ctx)
```

#### Rust

```rust
sbx.tailscale().disconnect().await?;
```

#### CLI

```sh
zipbox tailscale disconnect demo
```

#### curl

```sh
curl -X DELETE https://api.tribes.xyz/sandbox/$ID/tailscale \
  -H "Authorization: Bearer $ZIPBOX_API_KEY"
```

## Make a box private

A private box can only be reached from devices on your tailnet. That covers everything: its own name and custom names, previews, the dashboard's terminal, IDE and desktop, the Box API, and SSH. Public SSH stops; `ssh root@demo.zbox.sh` keeps working from a tailnet device.

#### TypeScript

```ts
await box.setPrivate(true) // false makes it public again
```

#### Python

```python
box.set_private(True)  # False makes it public again
```

#### Go

```go
err = box.SetPrivate(ctx, true) // false makes it public again
```

#### Rust

```rust
sbx.set_private(true).await?; // false makes it public again
```

#### CLI

```sh
zipbox private on demo
zipbox private off demo
```

#### curl

```sh
curl -X PUT https://api.tribes.xyz/sandbox/$ID/private \
  -H "Authorization: Bearer $ZIPBOX_API_KEY" \
  -H 'Content-Type: application/json' -d '{"enabled":true}'
```

Turning private on needs the box `connected` with a tailnet IPv4; otherwise the answer is `409 tailnet_not_connected`. The switch runs in an order that never locks you out:

1. The box gets its own HTTPS certificate for its names, and starts serving them on its own port 443.
2. Once it reports those names ready, its public DNS records point at its tailnet addresses (`A` → the tailnet IPv4, `AAAA` → the tailnet IPv6, TTL 60).
3. Only then does the host drop the box's public routes and close its public IPv6 to everything but Tailscale's own encrypted traffic.

Until step 3 the box is still public; `privateMode` is true from the start, and the dashboard shows each stage. Turning private off runs the same steps backwards.

The same URLs keep working on your tailnet devices, so nothing in your code or bookmarks changes. A `100.x` address means nothing off your tailnet, so publishing it in public DNS reveals nothing usable. If your router's DNS drops answers that point at private addresses, turn on MagicDNS on your device.

**If Tailscale drops, a private box stays private.** It does not fall back to public. To recover, send a new key with `connect`: it travels over Zipbox's own channel to the host, never the internet, so it works while the box is unreachable. The dashboard's recovery card does the same.

## Start a box private

Pass `private: true` at create. The box is never public, not even for a moment. A private box has no tailnet address until it joins, so it is not `ready` yet: `create` returns once the box is running with its `agentPublicKey`, and you connect it next.

#### TypeScript

```ts
const box = await zb.sandboxes.create({ name: 'build', private: true })
await box.tailscale.connect(process.env.TS_AUTHKEY!)
```

#### Python

```python
box = zb.sandboxes.create(name="build", private=True)
box.tailscale.connect(os.environ["TS_AUTHKEY"])
```

#### Go

```go
box, err := c.Sandboxes.Create(ctx, zipbox.CreateOptions{
	Name:    "build",
	Private: true,
})
err = box.Tailscale.Connect(ctx, os.Getenv("TS_AUTHKEY"))
```

#### Rust

```rust
let options = zipbox::CreateOptions {
    name: Some("build".into()),
    private: Some(true),
    ..Default::default()
};
let sbx = zb.sandboxes().create(options).await?;
sbx.tailscale().connect(&std::env::var("TS_AUTHKEY")?).await?;
```

#### CLI

```sh
zipbox sandbox create --name build --private --tailscale-auth-key-file ./ts.key
```

#### curl

```sh
curl -X POST https://api.tribes.xyz/sandbox/identity/claim \
  -H "Authorization: Bearer $ZIPBOX_API_KEY" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"name":"build","private":true}'
# then wait for status "running" with an agentPublicKey,
# seal, and PUT /sandbox/:id/tailscale
```

The box is reachable from your tailnet once it has joined and its certificate is issued, usually within a minute.

## Reaching a private box

| From                                      | Works?                                                                         |
| ----------------------------------------- | ------------------------------------------------------------------------------ |
| SDKs and the CLI on a tailnet device      | Yes, unchanged                                                                 |
| SDKs and the CLI off the tailnet          | Lifecycle calls only. Box calls fail fast with `box_private_unreachable`       |
| The dashboard on a tailnet device         | Yes. Off the tailnet it shows "Turn on Tailscale on this computer"             |
| Zipbox MCP (hosted or `zipbox mcp start`) | Lifecycle and Tailscale tools only. Box tools return `box_private_unreachable` |
| `ssh root@<name>.zbox.sh`                 | From a tailnet device only                                                     |
| Another box                               | Only if it is on the same tailnet                                              |

**Lifecycle calls always work**, from anywhere: start, stop, restart, status, the private switch, and Tailscale connect and disconnect all go through the control plane.

**Box calls** (exec, files, PTY, previews, logs) go straight to the box. On a box whose row says `privateMode: true`, the SDKs first check that the box answers (a short probe of `<baseUrl>/challenge`, a 5 s deadline, cached once it succeeds). Off the tailnet that check fails and the call raises `box_private_unreachable` instead of hanging. Turn on Tailscale on your device ([tailscale.com/download](https://tailscale.com/download)) and retry.

**MCP does not reach private boxes, and this is deliberate.** The hosted MCP server runs in Zipbox's cloud and can never join your tailnet, and `zipbox mcp start` only bridges to it. There is also no MCP tool that takes an auth key, because the key would land in plain text in a model's transcript; `tailscale_connect_link` returns a link to the box's **Settings → Tailscale** instead.

## Deleting a box signs it out

Deleting a box removes it from your tailnet, so you are never left with a dead device in your admin.

- **A running box** is signed out first, then deleted. Its `status` is `deleting` meanwhile, and the dashboard shows Signing out of Tailscale, then Deleting. A sign-out that fails because Tailscale's servers are unreachable is retried with backoff, with no attempt ceiling, until it succeeds; a node that is already gone from your admin counts as signed out.
- **A stopped box that joined a tailnet** is brought back just long enough to sign out: its disk is restored, it boots in a minimal mode that runs none of your services, signs out of Tailscale, and is deleted. Nothing is billed for that boot. The dashboard shows the stages (Restoring with a percentage, Starting, Signing out of Tailscale, Deleting), and the box reads "Deleting… 42%" in the machines list until it is gone.
- **A box that never joined** deletes exactly as before.

A new box needs a new key; tailnet identities are never copied between boxes.

## What is not supported

- **Exit nodes.** The box never uses an exit node (provider calls would leave through it and fail) and is never offered as one. A setting made by hand inside the box is cleared.
- **Subnet routes and tailnet DNS.** The box joins with `--accept-routes=false` and `--accept-dns=false`.
- **Funnel and `tailscale serve`.** A public box already has [preview URLs](https://zipbox.ai/docs/previews.md); a private box serves the same names on your tailnet.
- **Boxes with company egress lockdown.** They cannot reach Tailscale at all, so `connect` answers `409 tailscale_unavailable_egress_lockdown`.
- **Boxes on an old ZipOS.** `connect` answers `409 tailscale_requires_update` until the ZipOS update is accepted.
- **Tailscale SSH.** SSH stays the box's own `sshd`, with your keys from **Settings → SSH keys**.
