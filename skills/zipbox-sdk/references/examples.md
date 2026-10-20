> Zipbox docs, markdown edition of https://zipbox.ai/docs/examples
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# Examples

Runnable projects - an agent running tests, CI, a dev server with a preview, Claude over MCP, and one page per agent framework.

Every example is a runnable project in [`examples`](https://github.com/tribes-protocol/zipbox-sdks/tree/main/examples) of the public SDK repository. Each one needs only `ZIPBOX_API_KEY`.

## An agent runs your tests in a box

Clone the repository into a box once, then every run is a pull, an install and a test - one command each, run in the clone. The box keeps its `node_modules` between runs, so the second run is fast.

#### TypeScript

```ts
const box = await zb.sandboxes.getOrCreate('tests')
// clone on the first run only
const cloned = await box.exec('test -d app')
if (cloned.exitCode !== 0) {
  await box.exec('git clone https://github.com/acme/app app')
}
// pull, install and test inside the clone
await box.exec('git pull', { cwd: 'app' })
await box.exec('npm ci', { cwd: 'app' })
const run = await box.exec('npm test', { cwd: 'app' })
console.log(run.exitCode === 0 ? 'green' : run.stdout + run.stderr)
```

#### Python

```python
box = zb.sandboxes.get_or_create("tests")
# clone on the first run only
if box.exec("test -d app").exit_code != 0:
    box.exec("git clone https://github.com/acme/app app")
# pull, install and test inside the clone
box.exec("git pull", cwd="app")
box.exec("npm ci", cwd="app")
run = box.exec("npm test", cwd="app")
print("green" if run.exit_code == 0 else run.stdout + run.stderr)
```

#### Go

```go
box, _ := c.Sandboxes.GetOrCreate(ctx, "tests")
// clone on the first run only
if res, _ := box.Exec(ctx, "test -d app"); res.ExitCode != 0 {
	_, _ = box.Exec(ctx, "git clone https://github.com/acme/app app")
}
// pull, install and test inside the clone
inApp := zipbox.WithCwd("app")
_, _ = box.Exec(ctx, "git pull", inApp)
_, _ = box.Exec(ctx, "npm ci", inApp)
run, _ := box.Exec(ctx, "npm test", inApp)
fmt.Println(run.ExitCode, run.Stdout)
```

#### Rust

```rust
let sbx = zb.sandboxes().get_or_create("tests").await?;
// clone on the first run only
if sbx.exec("test -d app").await?.exit_code != Some(0) {
    sbx.exec("git clone https://github.com/acme/app app").await?;
}
// pull, install and test inside the clone
let in_app = || zipbox::ExecOptions {
    cwd: Some("app".into()),
    ..Default::default()
};
sbx.exec_with("git pull", in_app()).await?;
sbx.exec_with("npm ci", in_app()).await?;
let run = sbx.exec_with("npm test", in_app()).await?;
println!("{:?} {}", run.exit_code, run.stdout);
```

#### CLI

```sh
# clone on the first run only (fails harmlessly once app/ exists)
zipbox exec tests -- git clone https://github.com/acme/app app
# pull, install and test inside the clone
zipbox exec tests --cwd app -- git pull
zipbox exec tests --cwd app -- npm ci
zipbox exec tests --cwd app -- npm test
```

#### curl

```sh
# one request per command, each run in the clone
curl -N -X POST "$BASE/exec" -H "Authorization: Bearer $BOX_TOKEN" \
  -d '{"cmd":"git pull","cwd":"app"}'
curl -N -X POST "$BASE/exec" -H "Authorization: Bearer $BOX_TOKEN" \
  -d '{"cmd":"npm ci","cwd":"app"}'
curl -N -X POST "$BASE/exec" -H "Authorization: Bearer $BOX_TOKEN" \
  -d '{"cmd":"npm test","cwd":"app"}'
```

Private repositories clone with no token in your code: connect GitHub once in zipbox.ai Settings and the box already holds the credentials.

## GitHub Actions

```yaml
name: test-on-zipbox
on: [push]
jobs:
  test:
    runs-on: ubuntu-latest
    env:
      ZIPBOX_API_KEY: ${{ secrets.ZIPBOX_API_KEY }}
    steps:
      - run: curl -fsSL https://zipbox.ai/install.sh | sh
      - run: zipbox exec ci-runner --cwd app -- git fetch
      - run: zipbox exec ci-runner --cwd app -- git checkout ${{ github.sha }}
      - run: zipbox exec ci-runner --cwd app -- make test
```

`zipbox exec` exits with the remote exit code, so the step fails when the tests fail. Project: `examples/typescript/github-actions`.

## A long-running dev server with a preview

Start the server in the background, give its port a public URL, and come back to its output whenever you like - nothing stops it on a clock.

```ts
const box = await zb.sandboxes.getOrCreate('preview-demo')
// start the dev server in the clone, in the background
const dev = 'npm run dev -- --port 3000 --host 127.0.0.1'
const server = await box.execBackground(dev, { cwd: 'app' })
const { url } = await box.preview.create(3000, { label: 'app' })
console.log('open', url)
for await (const event of box.commands.events(server.id))
  if (event.type === 'stdout') process.stdout.write(event.data)
```

Project: `examples/typescript/long-running-dev-server`.

## Claude and MCP

1. In Claude, open Settings → Connectors → Add custom connector and enter `https://mcp.zipbox.ai`.
2. Sign in to Zipbox and approve "Claude".
3. Ask: _"Create a box called scratch, clone github.com/acme/app into it, run the tests and tell me what fails."_

Claude calls `create_sandbox`, `git_clone` and `execute_command`; long output is read with `get_command_output`. Delete "Claude (OAuth)" in Settings → API keys to disconnect it. See [MCP server](https://zipbox.ai/docs/mcp.md).

## Vercel AI SDK

A `runCommand` tool backed by a box, for any model the AI SDK supports. Project: `examples/typescript/vercel-ai-sdk`.

```ts
import { tool } from 'ai'
import { z } from 'zod'

const runCommand = tool({
  description: 'Run a shell command in a persistent Linux box',
  inputSchema: z.object({ cmd: z.string() }),
  execute: async ({ cmd }) => {
    const box = await zb.sandboxes.getOrCreate('ai-sdk')
    const { exitCode, stdout, stderr } = await box.exec(cmd)
    return { exitCode, output: (stdout + stderr).slice(-8000) }
  }
})
```

## OpenAI Agents SDK

The same tool as a function tool for `@openai/agents`. Project: `examples/typescript/openai-agents-sdk`.

```ts
import { Agent, run, tool } from '@openai/agents'
import { z } from 'zod'

const shell = tool({
  name: 'shell',
  description: 'Run a shell command in a persistent Linux box',
  parameters: z.object({ cmd: z.string() }),
  execute: async ({ cmd }) => {
    const box = await zb.sandboxes.getOrCreate('agents')
    return (await box.exec(cmd)).stdout
  }
})
const result = await run(
  new Agent({ name: 'builder', tools: [shell] }),
  'What Python is installed?'
)
```

## Claude Agent SDK

Give a Claude agent its own machine through the hosted MCP server - no tool code at all. Project: `examples/typescript/claude-agent-sdk`.

```ts
import { query } from '@anthropic-ai/claude-agent-sdk'

for await (const message of query({
  prompt: 'Create a box called scratch and report its kernel version.',
  options: {
    mcpServers: {
      zipbox: {
        type: 'http',
        url: 'https://mcp.zipbox.ai',
        headers: { Authorization: `Bearer ${process.env.ZIPBOX_API_KEY}` }
      }
    }
  }
}))
  console.log(message)
```

## LangChain

A LangChain tool in Python. Project: `examples/python/langchain_tool.py`.

```python
from langchain_core.tools import tool
from zipbox import Zipbox

zb = Zipbox()

@tool
def run_in_box(cmd: str) -> str:
    """Run a shell command in a persistent Linux box and return its output."""
    result = zb.sandboxes.get_or_create("langchain").exec(cmd)
    return f"exit {result.exit_code}\n{result.stdout}{result.stderr}"[-8000:]
```

## LangGraph

The same tool in a prebuilt ReAct agent. Project: `examples/python/langgraph_agent.py`.

```python
from langgraph.prebuilt import create_react_agent

agent = create_react_agent("anthropic:claude-sonnet-4-5", tools=[run_in_box])
task = "Install ripgrep in the box and count TODOs in /etc"
agent.invoke({"messages": [("user", task)]})
```

## Go

The [Go SDK quickstart](https://zipbox.ai/docs/sdk-go.md#quickstart) is a complete program. Project: `examples/go/quickstart`.

## Claude Code inside a box, driving another box

Every box already runs a coding agent and carries the Zipbox skill, so an agent on box A can create, use and stop box B.

1. On box A, make a key available to the agent: create one in Settings → API keys and add it to the box as a variable named `ZIPBOX_API_KEY` (box **Settings → Provider keys → Key = value**).
2. Install the CLI on box A: `curl -fsSL https://zipbox.ai/install.sh | sh`.
3. Ask the agent on box A: _"Use the zipbox-sdk skill. Create a box called worker, clone our repo there, run the full test suite, and summarise the failures. Stop worker when you are done."_

The agent reads the skill, runs `zipbox sandbox create --name worker`, `zipbox exec worker -- …` and `zipbox sandbox stop worker`, and box A's own terminal stays free. Delete the key when you no longer want box A to drive others.
