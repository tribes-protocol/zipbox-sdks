# TypeScript examples

Each directory is a self-contained project. Copy one out, then:

```sh
npm install
export ZIPBOX_API_KEY=zb_…
npx tsx <file>.ts
```

| Example                                            | What it shows                                                             |
| -------------------------------------------------- | ------------------------------------------------------------------------- |
| [quickstart](quickstart)                           | Create a box, run a command, write and read a file, preview, stop         |
| [long-running-dev-server](long-running-dev-server) | A background dev server, a public preview URL, re-attaching to its output |
| [vercel-ai-sdk](vercel-ai-sdk)                     | A box as a tool for the Vercel AI SDK                                     |
| [openai-agents-sdk](openai-agents-sdk)             | A box as a tool for the OpenAI Agents SDK                                 |
| [claude-agent-sdk](claude-agent-sdk)               | A box as an in-process MCP tool for the Claude Agent SDK                  |
| [github-actions](github-actions)                   | Run a repository's tests in a box from CI                                 |

Inside this repository the examples resolve `@zipbox/sdk` to the workspace package:
build it once (`bun run build` in `sdks/typescript`), then `npm run typecheck` in any
example directory checks it against the SDK.
