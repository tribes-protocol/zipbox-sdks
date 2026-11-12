> Zipbox docs, markdown edition of https://zipbox.ai/docs/integrations
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# Integrations

Zipbox in Claude Code, Claude, Codex, Cursor and any other agent - what each one gets, how to set it up, what is missing and what it sends.

One account, every agent. Each integration below drives the same boxes your dashboard shows, with the same key, and a box keeps running when the agent that started work on it closes. Pick the harness you already use; you can use several at once.

## Pick your harness

| Harness                                    | Install                                | You get                                                                          |
| ------------------------------------------ | -------------------------------------- | -------------------------------------------------------------------------------- |
| Claude Code (terminal, Desktop's Code tab) | The `zipbox` plugin                    | A pane of your boxes and their agent tasks, `/zipbox` commands, MCP tools, skill |
| Claude (web, desktop, mobile)              | A custom connector                     | The [MCP tools](https://zipbox.ai/docs/mcp.md#tools)                                                 |
| Codex                                      | The MCP server and the skill           | The MCP tools and the [skill](https://zipbox.ai/docs/skill.md)                                       |
| Cursor, Windsurf, any stdio MCP client     | `zipbox mcp` from the [CLI](https://zipbox.ai/docs/cli.md) | The MCP tools, served locally                                                    |
| Any other agent                            | The skill, plus an SDK or the CLI      | Instructions the agent follows, and code it runs                                 |

## What every integration shares

- **Your boxes.** The same long-lived machines, tiers and credits as the dashboard. An agent can create, pause, resume and delete them.
- **Nothing stops on a clock.** A command an agent starts has no deadline unless it sets one, and a box runs until someone pauses it.
- **One key.** Every integration holds an account key (`zb_…`) and trades it for a per-box token on each box it talks to. You never handle box tokens. A connector that signs in with OAuth gets a key named after the app ("Claude (OAuth)") in **Settings → API keys**; delete that row to disconnect it.
- **The same tools.** Commands, files, previews, logs, the desktop screenshot and docs search are the hosted MCP server's tools in every harness that speaks MCP ([MCP server](https://zipbox.ai/docs/mcp.md)).

## Claude Code

The `zipbox` plugin bundles three things: the hosted MCP server, the `zipbox-sdk` skill, and a mod that adds the Zipbox pane and the `/zipbox` command. The MCP tools and the skill work in any Claude Code; the pane and `/zipbox` need Claude Code 2.1.308 or later.

### Install in the terminal

```sh
claude plugin marketplace add tribes-protocol/zipbox-sdks
claude plugin install zipbox@zipbox-sdks
```

Inside a session, `/plugin install zipbox --marketplace tribes-protocol/zipbox-sdks` does the same.

### Install in Claude Desktop

Claude Desktop's Code tab runs Claude Code, so the plugin works there too. `/plugin` does not run in that tab: click **+** next to the prompt box, then **Plugins**, **Add marketplace** with `tribes-protocol/zipbox-sdks`, then **Add plugin**. A plugin installed from a terminal at the user scope loads in the Code tab as well.

### Sign in

- **The pane and `/zipbox`:** run `/zipbox login`. It runs the CLI's browser sign-in (`zipbox login`, which opens zipbox.ai/cli/auth) and uses the key the CLI stores, so a computer already signed in to the CLI is already signed in here. `ZIPBOX_API_KEY` always wins when it is set. Without the CLI installed, set `ZIPBOX_API_KEY` instead.
- **The MCP tools:** run `/mcp`, pick **zipbox** and approve the sign-in. This is OAuth and makes its own key.

### The pane

`/zipbox` opens the pane: every box with its status and, under each running box, its agent sessions, newest first, with their state (`working`, `needs_input`, `completed`, …). Press a box to make it the box the commands act on.

Press a task to follow it in this window:

- its conversation so far is drawn here as this window's own turns: each prompt, then its reply, with a line for every tool call;
- new replies appear as the box writes them;
- what you type goes to that task, not to Claude Code's own model, and its reply is drawn under it;
- the status line shows the box, the task and its state.

`/zipbox leave` stops following; your prompts go to Claude Code again. No model is called while following: the plugin draws each reply from the box.

### Commands

| Command                      | Does                                                        |
| ---------------------------- | ----------------------------------------------------------- |
| `/zipbox`                    | Open the pane                                               |
| `/zipbox login`              | Sign in through the CLI's browser flow                      |
| `/zipbox logout`             | Forget the stored key, through `zipbox logout`              |
| `/zipbox whoami`             | The account and key in use                                  |
| `/zipbox use <box>`          | The box the commands below act on                           |
| `/zipbox new [box] <prompt>` | Start an agent task on the box and follow it here           |
| `/zipbox cancel`             | Stop the task this window follows                           |
| `/zipbox resume`             | Continue an interrupted task                                |
| `/zipbox leave`              | Stop following                                              |
| `/zipbox run <command>`      | Run a shell command on the box; prints output and exit code |
| `/zipbox files [path]`       | List a folder; relative paths are under `/root/workspace`   |
| `/zipbox read <path>`        | Show a text file                                            |
| `/zipbox save <path>`        | Copy a file of the box into the current folder              |
| `/zipbox preview <port>`     | A public HTTPS URL for a port ([Previews](https://zipbox.ai/docs/previews.md))  |
| `/zipbox screenshot`         | Save the box's desktop as `zipbox-screenshot.png`           |
| `/zipbox open`               | Open the box's desktop in the browser                       |
| `/zipbox help`               | List the commands                                           |

With one box, every command uses it. With several, `/zipbox use <box>` or a press in the pane picks one.

## Claude web, desktop and mobile

Add Zipbox as a custom connector: **Settings → Connectors → Add custom connector**, URL `https://mcp.zipbox.ai`. Claude opens the Zipbox sign-in; approve it and the tools appear in every chat on web, desktop and mobile.

## Codex

Add the MCP server, then the skill:

```sh
# the MCP server, signed in with OAuth
codex mcp add zipbox --url https://mcp.zipbox.ai
# the skill
npx skills add tribes-protocol/zipbox-sdks --skill zipbox-sdk
```

To use a key instead of OAuth, `zipbox mcp init codex` writes the server into `~/.codex/config.toml` with `bearer_token_env_var = "ZIPBOX_API_KEY"`.

## Cursor, Windsurf and stdio clients

The [CLI](https://zipbox.ai/docs/cli.md) serves the same tools over stdio with its own key:

```sh
# sign in once
zipbox login
# write the client's MCP config
zipbox mcp init cursor
```

`zipbox mcp init windsurf` does the same for Windsurf, and `zipbox mcp config` prints a JSON snippet for any other client.

## Any other agent

An agent without MCP still drives boxes through the skill and an SDK or the CLI. Install the skill where the agent reads skills, give it `ZIPBOX_API_KEY`, and it writes and runs the code itself:

```sh
npx skills add tribes-protocol/zipbox-sdks --skill zipbox-sdk
```

An agent that reads `AGENTS.md` instead can take the snippet on the [Overview](https://zipbox.ai/docs/api.md).

## Feature parity

✓ works, partial works with the limits under the table, - not available.

| Feature                                          | Claude Code plugin | Claude connector | Codex | Cursor and stdio | Skill + SDK/CLI |
| ------------------------------------------------ | ------------------ | ---------------- | ----- | ---------------- | --------------- |
| Create, pause, resume and delete boxes           | ✓                  | ✓                | ✓     | ✓                | ✓               |
| Run commands, background commands                | ✓                  | ✓                | ✓     | ✓                | ✓               |
| Read, write and move files                       | ✓                  | ✓                | ✓     | ✓                | ✓               |
| Public previews                                  | ✓                  | ✓                | ✓     | ✓                | ✓               |
| Desktop screenshot                               | ✓                  | ✓                | ✓     | ✓                | ✓               |
| Docs search                                      | ✓                  | ✓                | ✓     | ✓                | -               |
| List a box's agent sessions and their state      | ✓                  | -                | -     | -                | -               |
| Start, cancel and resume an agent task           | ✓                  | -                | -     | -                | -               |
| Follow a task: its history and new replies       | partial            | -                | -     | -                | -               |
| Send a prompt to a running task                  | ✓                  | -                | -     | -                | -               |
| Replies token by token                           | -                  | -                | -     | -                | -               |
| Approvals and questions in the harness's dialogs | -                  | -                | -     | -                | -               |
| Follow a session started in the box's terminal   | -                  | -                | -     | -                | -               |

What is missing, and why:

- **Following reads on a timer.** The box's event stream belongs to the dashboard tab that owns the machine, so the plugin reads the task instead: every 2 seconds while the agent works, every 6 seconds otherwise, and the pane every 10 seconds while it is open. While a reply is drawn, each read waits on the box for the next row. When a read fails it backs off up to 30 seconds and keeps trying.
- **Whole messages, not tokens.** A reply appears once the box has written it. Thinking is not shown, and a tool call is one line without its result.
- **Esc stops the drawing, not the task.** An interrupt ends the turn here; the task goes on, and what it writes after that shows only in the transcript (ctrl+o). `/zipbox cancel` stops the task.
- **Tasks only.** Tasks are what the box's command center runs (started from Super+K on the box, `/zipbox new` or the API), and they run the box's own agent. Agent sessions opened in the box's terminal are listed with their state but cannot be followed.
- **No approvals.** The box's agent does not stop for approvals. A session that is waiting shows `needs_input`; answer it by sending a prompt.
- **`/zipbox run` waits for the command.** Output shows when it exits. Start long jobs with the MCP tool `start_background_command` or `zipbox exec --background`.
- **`/zipbox save` and `/zipbox screenshot`** write the file with `sh` and `base64`, so they need macOS or Linux.

## Privacy

**What the plugin stores.** Nothing of its own. It reads `ZIPBOX_API_KEY`, or the key `zipbox login` keeps in the CLI's config file (`zipbox/config.json` in your user config directory, readable only by you). `/zipbox logout` runs `zipbox logout`, which deletes that file. The key stays valid until you delete it in **Settings → API keys**.

**What it sends.** Your key goes only to the Zipbox control plane (`api.tribes.xyz`), which answers with each box's address and token; the box token goes only to that box (`<name>.zbox.sh`). The plugin sends nothing else anywhere and has no telemetry.

**What the model sees.** A followed task's prompts and replies are drawn as turns of this window, and stay in your local Claude Code transcript like any other turn: no model is called for them, but after `/zipbox leave` the local model reads them as earlier turns. The task's state changes are notes the local model never reads. The output of a `/zipbox` command (`run`, `read`, `files`) is part of the conversation, so the model reads it. MCP tool results pass through `mcp.zipbox.ai` to the model, as with any MCP server.

**The skill** is text. It sends nothing.
