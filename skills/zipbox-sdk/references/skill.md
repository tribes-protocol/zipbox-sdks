> Zipbox docs, markdown edition of https://zipbox.ai/docs/skill
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# Agent skill

The zipbox-sdk skill - what it teaches an agent, how to install it in Claude Code and other harnesses, and how it stays in step with these docs.

The Zipbox skill (`zipbox-sdk`) is a packaged set of instructions an agent loads when a task involves Zipbox: how to authenticate, the SDK essentials in each language, common patterns, limits, and where to read more. Agents that support skills - Claude Code, Codex, and the agents on every Zipbox box - pick it up on their own when it is relevant.

## Install

```sh
npx skills add tribes-protocol/zipbox-sdks --skill zipbox-sdk
```

For Claude Code, add the public repository as a plugin marketplace:

```sh
claude plugin marketplace add tribes-protocol/zipbox-sdks
claude plugin install zipbox@zipbox-sdks
```

Every Zipbox box already has it, so an agent on one box can drive another without installing anything.

## What it covers

- **Preflight**: is an SDK or the CLI installed, is `ZIPBOX_API_KEY` set, and where the user gets a key.
- **SDK essentials**: create or reuse a box, exec, files, background commands, previews and stop - Python first, with the TypeScript, Go and Rust differences.
- **Common patterns**: a long-running job with re-attach, previewing a dev server, an agent on one box driving another.
- **Limits**: spool caps, concurrency, the rate limit, and what is not in this version.
- **"I need to… → read this"**: a table that sends the agent to the right reference page.

## Generated from these docs

The skill's `references/` directory is generated from the markdown of these pages at build time, so the skill and the docs cannot drift: one source, two outputs. Anything the skill says in more detail than its own page links back here.
