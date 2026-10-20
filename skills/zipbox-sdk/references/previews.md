> Zipbox docs, markdown edition of https://zipbox.ai/docs/previews
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# Previews

Give any port on a box a public HTTPS URL - preview(port), the URL shape, public by default, remove.

A preview is a public HTTPS URL for a port on your box. Start a dev server on port 3000, call `preview.create(3000)`, and `https://<label>.<name>.zbox.sh` answers with it - certificate included.

## Create a preview

`create(port, { label })` runs the box's own `tribes-caddy add <label>.<box host> 127.0.0.1:<port>` and returns the URL. The label defaults to `p<port>`. The box already holds a wildcard certificate and DNS for every name under its own, so the URL is live as soon as Caddy reloads (about a second).

#### TypeScript

```ts
await box.execBackground('npx vite --port 3000 --host 127.0.0.1')
const preview = await box.preview.create(3000, { label: 'web' })
console.log(preview.url) // https://web.demo.zbox.sh
```

#### Python

```python
box.exec_background("npx vite --port 3000 --host 127.0.0.1")
preview = box.preview.create(3000, label="web")
print(preview.url)  # https://web.demo.zbox.sh
```

#### Go

```go
_, _ = box.ExecBackground(ctx, "npx vite --port 3000 --host 127.0.0.1")
preview, err := box.Preview.Create(ctx, 3000, "web")
fmt.Println(preview.URL)
```

#### Rust

```rust
sbx.exec_background("npx vite --port 3000 --host 127.0.0.1").await?;
let preview = sbx.preview().create(3000, Some("web")).await?;
println!("{}", preview.url);
```

#### CLI

```sh
zipbox preview $ID 3000 --label web
```

#### curl

```sh
curl -N -X POST "$BASE/exec" -H "Authorization: Bearer $BOX_TOKEN" \
  -d '{"argv":["tribes-caddy","add","web.demo.zbox.sh","127.0.0.1:3000"]}'
# → https://web.demo.zbox.sh
```

## URL shape

`https://<label>.<box host>`, where the box host is the box's own name (`demo.zbox.sh`). Labels are DNS labels: lower case letters, digits and hyphens. Each label is one site; creating the same label again points it at the new port.

## Previews are public

Anyone with the URL can open it - there is no sign-in in front of it. That matches how sites you serve from the box with `tribes-caddy` have always worked. Don't preview something you would not put on the internet; private or signed previews are not in this version ([Errors and limits](https://zipbox.ai/docs/errors.md#out-of-v1)).

Bind your dev server to `127.0.0.1`; Caddy proxies to it, so it never needs to listen publicly.

## Remove a preview

#### TypeScript

```ts
await box.preview.remove('web')
```

#### Python

```python
box.preview.remove("web")
```

#### Go

```go
err = box.Preview.Remove(ctx, "web")
```

#### Rust

```rust
sbx.preview().remove("web").await?;
```

#### CLI

```sh
zipbox preview rm $ID web
```

#### curl

```sh
curl -N -X POST "$BASE/exec" -H "Authorization: Bearer $BOX_TOKEN" \
  -d '{"argv":["tribes-caddy","rm","web.demo.zbox.sh"]}'
```

`tribes-caddy list` (via `exec`) shows every site the box serves. A Caddy reload drops open websockets for about a second, including PTYs; they reconnect by themselves.
