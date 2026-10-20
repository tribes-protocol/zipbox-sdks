> Zipbox docs, markdown edition of https://zipbox.ai/docs/quickstart
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# Quickstart

An API key, one install line, and ten lines of code that open a box and run a command.

> **Agents:** read [/llms.txt](https://zipbox.ai/llms.txt) for every page and section, or use the `search_docs` tool on `https://mcp.zipbox.ai`. The SDKs move faster than your training data.

## Get an API key

Sign in at [zipbox.ai](https://zipbox.ai/dashboard), open **Settings → API keys**, and press **Add API key** at the top right. It is shown once; copy it and export it:

```sh
export ZIPBOX_API_KEY=zb_...
```

Booting a box needs a credit balance above $0. New accounts can claim free credit when they finish setup.

## Install

#### TypeScript

```sh
npm install @zipbox/sdk
```

#### Python

```sh
pip install zipbox
```

#### Go

```sh
go get github.com/tribes-protocol/zipbox-go
```

#### Rust

```sh
cargo add zipbox tokio --features tokio/full
```

#### CLI

```sh
curl -fsSL https://zipbox.ai/install.sh | sh
```

#### curl

```sh
# Nothing to install. Control plane: https://api.tribes.xyz
```

## Open a box and run a command

`getOrCreate` returns the box with that name, creating it (and waiting until it is ready) the first time.

#### TypeScript

```ts
import { Zipbox } from '@zipbox/sdk'

const zb = new Zipbox() // reads ZIPBOX_API_KEY
const box = await zb.sandboxes.getOrCreate('quickstart')
const result = await box.exec('python3 --version')
console.log(result.exitCode, result.stdout)
```

#### Python

```python
from zipbox import Zipbox

zb = Zipbox()  # reads ZIPBOX_API_KEY
box = zb.sandboxes.get_or_create("quickstart")
result = box.exec("python3 --version")
print(result.exit_code, result.stdout)
```

#### Go

```go
package main

import ("context"; "fmt"; "github.com/tribes-protocol/zipbox-go")

func main() {
	ctx := context.Background()
	c := zipbox.New("") // "" reads ZIPBOX_API_KEY
	box, _ := c.Sandboxes.GetOrCreate(ctx, "quickstart")
	res, _ := box.Exec(ctx, "python3 --version")
	fmt.Println(res.ExitCode, res.Stdout)
}
```

#### Rust

```rust
use zipbox::Zipbox;

#[tokio::main]
async fn main() -> Result<(), zipbox::Error> {
    let zb = Zipbox::from_env()?;
    let sbx = zb.sandboxes().get_or_create("quickstart").await?;
    let res = sbx.exec("python3 --version").await?;
    println!("{:?} {}", res.exit_code, res.stdout);
    Ok(())
}
```

#### CLI

```sh
# create the box and wait until it is ready
zipbox sandbox create --name quickstart
# run a command in it
zipbox exec quickstart -- python3 --version
```

#### curl

```sh
AUTH="Authorization: Bearer $ZIPBOX_API_KEY"
# claim a box
ID=$(curl -s -X POST https://api.tribes.xyz/sandbox/identity/claim \
  -H "$AUTH" -H "Idempotency-Key: $(uuidgen)" \
  --json '{"name":"quickstart"}' | jq -r .sandboxId)
# repeat these two until "ready" is true
BOX=$(curl -s https://api.tribes.xyz/sandbox/$ID -H "$AUTH")
echo "$BOX" | jq .ready
# then take baseUrl and boxToken
BASE=$(echo "$BOX" | jq -r .baseUrl)
TOKEN=$(echo "$BOX" | jq -r .boxToken)
# run a command
curl -N -X POST "$BASE/exec" -H "Authorization: Bearer $TOKEN" \
  -H 'Accept: text/event-stream' -d '{"cmd":"python3 --version"}'
```

The exit code is a result, not an error: a command that exits `1` returns `exitCode: 1` and raises nothing.

## Stop it when you are done

Nothing stops a box on a clock. A stopped box archives its disk and bills nothing; starting it again brings the same disk back.

#### TypeScript

```ts
await zb.sandboxes.stop(box.id)
```

#### Python

```python
zb.sandboxes.stop(box.id)
```

#### Go

```go
err := c.Sandboxes.Stop(ctx, box.ID)
```

#### Rust

```rust
zb.sandboxes().stop(sbx.id()).await?;
```

#### CLI

```sh
zipbox sandbox stop quickstart
```

#### curl

```sh
curl -X POST https://api.tribes.xyz/sandbox/$ID/pause -H "$AUTH"
```

## Next

- Run code, stream output, re-attach to a background command: [Run commands](https://zipbox.ai/docs/commands.md).
- Write and read files: [Files](https://zipbox.ai/docs/files.md).
- Put a dev server on a public URL: [Previews](https://zipbox.ai/docs/previews.md).
- Let Claude or Codex drive the box: [MCP server](https://zipbox.ai/docs/mcp.md).
- The full method list for your language: [TypeScript](https://zipbox.ai/docs/sdk-ts.md), [Python](https://zipbox.ai/docs/sdk-python.md), [Go](https://zipbox.ai/docs/sdk-go.md), [Rust](https://zipbox.ai/docs/sdk-rust.md).
