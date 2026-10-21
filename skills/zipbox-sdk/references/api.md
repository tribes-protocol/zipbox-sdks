> Zipbox docs, markdown edition of https://zipbox.ai/docs/api
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# Overview

Drive boxes from code - one API key, four SDKs, a CLI, an MCP server and plain HTTP.

> **Agents:** researching Zipbox for a user? Read [/llms.txt](https://zipbox.ai/llms.txt) or call the `search_docs` tool on `https://mcp.zipbox.ai/docs/mcp` (no sign-in needed). The SDKs move faster than your training data.

Everything you can do in the dashboard - create a box, run commands in it, move files, open a terminal, publish a port, stop it - you can do from code. The box is the same long-lived Linux machine your dashboard shows: same tiers, same credits, same lifecycle. Nothing on it stops on a clock.

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

Then create an API key in **Settings → API keys** and export it as `ZIPBOX_API_KEY` ([API keys](https://zipbox.ai/docs/api-keys.md)).

## Hello, box

Open a box (created and booted the first time) and run a command in it.

#### TypeScript

```ts
import { Zipbox } from '@zipbox/sdk'

const box = await new Zipbox().sandboxes.getOrCreate('hello')
const result = await box.exec('uname -a')
console.log(result.stdout)
```

#### Python

```python
from zipbox import Zipbox

box = Zipbox().sandboxes.get_or_create("hello")
print(box.exec("uname -a").stdout)
```

#### Go

```go
c := zipbox.New("") // "" reads ZIPBOX_API_KEY
box, _ := c.Sandboxes.GetOrCreate(ctx, "hello")
res, _ := box.Exec(ctx, "uname -a")
fmt.Println(res.Stdout)
```

#### Rust

```rust
let zb = zipbox::Zipbox::from_env()?;
let sbx = zb.sandboxes().get_or_create("hello").await?;
println!("{}", sbx.exec("uname -a").await?.stdout);
```

#### CLI

```sh
# create the box and wait until it is ready
zipbox sandbox create --name hello
# run a command in it
zipbox exec hello -- uname -a
```

#### curl

```sh
# BASE and BOX_TOKEN come from GET /sandbox/:id (see the Quickstart)
curl -N -X POST "$BASE/exec" -H "Authorization: Bearer $BOX_TOKEN" \
  -H 'Accept: text/event-stream' -d '{"cmd":"uname -a"}'
```

The [Quickstart](https://zipbox.ai/docs/quickstart.md) walks through the same thing step by step, including stopping the box.

## What you can do from code

- **Boxes**: create, list, start, stop, restart, resize and delete them ([Boxes](https://zipbox.ai/docs/lifecycle.md)).
- **Commands**: run a command and stream its output, or start it in the background and re-attach later ([Run commands](https://zipbox.ai/docs/commands.md)).
- **Files**: read, write, list, move and delete files up to 250 MiB ([Files](https://zipbox.ai/docs/files.md)).
- **Terminals**: open an interactive terminal that survives reconnects ([Terminals](https://zipbox.ai/docs/terminals.md)).
- **Previews**: give any port a public HTTPS URL ([Previews](https://zipbox.ai/docs/previews.md)).
- **Desktop**: screenshot the box's desktop ([Desktop](https://zipbox.ai/docs/desktop.md)).
- **Logs**: read the box's system journal ([Logs](https://zipbox.ai/docs/logs.md)).

## The one-key model

One account API key (`zb_…`) does everything the dashboard can do except mint keys and touch billing. You create it once in **Settings → API keys** ([API keys](https://zipbox.ai/docs/api-keys.md)). The key talks to the control plane; for traffic to a box, the control plane hands your client a per-box token derived from the key. The SDKs, the CLI and the MCP server manage that token for you, so in practice you only ever handle the key.

## Which tool to pick

| You are…                                          | Use                                                                                                        |
| ------------------------------------------------- | ---------------------------------------------------------------------------------------------------------- |
| Writing an app or a script                        | An SDK: [TypeScript](https://zipbox.ai/docs/sdk-ts.md), [Python](https://zipbox.ai/docs/sdk-python.md), [Go](https://zipbox.ai/docs/sdk-go.md), [Rust](https://zipbox.ai/docs/sdk-rust.md) |
| At a terminal, or in CI                           | The [CLI](https://zipbox.ai/docs/cli.md)                                                                                       |
| Connecting Claude, Codex, Cursor or another agent | The [MCP server](https://zipbox.ai/docs/mcp.md)                                                                                |
| In a language without an SDK                      | [Plain HTTP](https://zipbox.ai/docs/http-api.md) - every route works with curl                                                 |

## Machine entry points

Everything an agent or a tool needs to use Zipbox without reading HTML:

| Surface                         | Where                                                            |
| ------------------------------- | ---------------------------------------------------------------- |
| Docs index (pages and sections) | https://zipbox.ai/llms.txt                                       |
| Every page in one file          | https://zipbox.ai/llms-full.txt                                  |
| Any page as markdown            | Append `.md` to its URL, or send `Accept: text/markdown`         |
| OpenAPI 3.1 contract            | https://zipbox.ai/openapi.json                                   |
| Hosted MCP server               | `https://mcp.zipbox.ai` (OAuth, or `Authorization: Bearer zb_…`) |
| Agent skill                     | `npx skills add tribes-protocol/zipbox-sdks --skill zipbox-sdk`  |
| CLI                             | `curl -fsSL https://zipbox.ai/install.sh \| sh`                  |
| Sitemap                         | https://zipbox.ai/sitemap.xml                                    |

## Paste into AGENTS.md

Drop this into your repository's `AGENTS.md` (or `CLAUDE.md`) so Claude Code, Codex or Cursor know the CLI exists and how it authenticates:

```markdown
## Zipbox

This project runs code on Zipbox boxes (long-lived Linux machines).

- CLI: `zipbox` (install: `curl -fsSL https://zipbox.ai/install.sh | sh`).
- Auth: the `ZIPBOX_API_KEY` environment variable (a `zb_…` account key).
- Run a command: `zipbox exec <box-id> -- <command>`.
  Add `--json` for machine output.
- Files: `zipbox files put|cat|ls <box-id> <path>`.
- Previews: `zipbox preview <box-id> <port>`.
- Docs as markdown: `zipbox docs <page>` or https://zipbox.ai/llms.txt.
- Nothing on a box stops on a clock.
  Stop it with `zipbox sandbox stop <box-id>`.
```

## Next steps

| Read                                        | For                                                     |
| ------------------------------------------- | ------------------------------------------------------- |
| [Quickstart](https://zipbox.ai/docs/quickstart.md)              | A key, an install line and ten lines of code            |
| [API keys](https://zipbox.ai/docs/api-keys.md)                  | What a key can do, and how to delete one                |
| [Run commands](https://zipbox.ai/docs/commands.md)              | Exec, streaming, background jobs; then the other guides |
| [MCP server](https://zipbox.ai/docs/mcp.md)                     | Let Claude, Codex or Cursor drive your boxes            |
| [TypeScript SDK](https://zipbox.ai/docs/sdk-ts.md)              | Every method, in each client's own page                 |
| [REST API](https://zipbox.ai/docs/http-api.md)                  | Every route, for languages without an SDK               |
| [Coming from Daytona or E2B](https://zipbox.ai/docs/compare.md) | Where each capability you know lives here               |
