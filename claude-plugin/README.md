# Zipbox for Claude Code

One plugin puts your [Zipbox](https://zipbox.ai) boxes inside Claude Code: a pane of
your boxes and the agent tasks running on them, `/zipbox` commands, the Zipbox MCP
server and the `zipbox-sdk` skill. Full guide: [zipbox.ai/docs/integrations](https://zipbox.ai/docs/integrations).

## Install

Claude Code 2.1.308 or later.

```sh
claude plugin marketplace add tribes-protocol/zipbox-sdks
claude plugin install zipbox@zipbox-sdks
```

At the prompt of a session: `/plugin install zipbox --marketplace tribes-protocol/zipbox-sdks`.

**Claude Desktop** (Code tab), where `/plugin` does not run: click **+** next to the
prompt box, then **Plugins**, **Add marketplace** with `tribes-protocol/zipbox-sdks`,
then **Add plugin**. A plugin installed from a terminal at the user scope loads there
too.

## Sign in

- `/zipbox login` runs the [zipbox CLI](https://zipbox.ai/docs/cli)'s browser sign-in
  and uses the key it stores, so a computer already signed in to the CLI is signed in
  here. Without the CLI, set `ZIPBOX_API_KEY` (it always wins).
- The MCP server signs in separately with OAuth: run `/mcp`, pick **zipbox**, approve.

## Use

| Command                      | Does                                                     |
| ---------------------------- | -------------------------------------------------------- |
| `/zipbox`                    | Open the pane: boxes, and each box's agent sessions      |
| `/zipbox use <box>`          | The box the commands act on                              |
| `/zipbox new [box] <prompt>` | Start an agent task on the box and follow it here        |
| `/zipbox cancel` / `resume`  | The task this window follows                             |
| `/zipbox leave`              | Stop following; prompts go to Claude here again          |
| `/zipbox run <command>`      | Run a shell command on the box                           |
| `/zipbox files [path]`       | List a folder (relative paths are under /root/workspace) |
| `/zipbox read <path>`        | Show a text file                                         |
| `/zipbox save <path>`        | Copy a file of the box into this folder                  |
| `/zipbox preview <port>`     | A public HTTPS URL for a port                            |
| `/zipbox screenshot`         | Save the box's desktop as `zipbox-screenshot.png`        |
| `/zipbox open`               | Open the box's desktop in the browser                    |
| `/zipbox whoami` / `logout`  | The account in use; forget the stored key                |

Press a task in the pane to follow it: its conversation so far is drawn in this
window, new replies and tool calls appear as the box writes them, and what you type
goes to that task. Sessions started in the box's own terminal are listed but cannot
be followed.

## Develop

```sh
claude plugin validate .   # manifest, hooks module, state contract
claude plugin test .       # tests/*.test.ts(x) against the engine
```

The source lives in the private Zipbox monorepo (`sdks/claude-plugin`); the skill is
copied in from `harnesses/setup/skills/zipbox-sdk` when the public mirror is exported.
