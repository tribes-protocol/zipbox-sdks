> Zipbox docs, markdown edition of https://zipbox.ai/docs/mcp
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# MCP server

Connect Claude, Codex, Cursor or any MCP client to your boxes - the hosted server at mcp.zipbox.ai, stdio via the CLI, and every tool.

Zipbox runs one hosted MCP server, `https://mcp.zipbox.ai` (streamable HTTP, with an SSE fallback). Any MCP client can connect with **OAuth** - sign in to Zipbox and approve the app - or with a **bearer API key**. Clients that only speak stdio use `zipbox mcp start` from the [CLI](https://zipbox.ai/docs/cli.md), which serves exactly the same tools.

## Connect Claude

**Claude (web, desktop, mobile):** Settings → Connectors → Add custom connector → `https://mcp.zipbox.ai`. Claude opens the Zipbox sign-in; approve it and the tools appear.

**Claude Code:**

```sh
claude mcp add --transport http zipbox https://mcp.zipbox.ai
# or, with a key instead of OAuth:
claude mcp add --transport http zipbox https://mcp.zipbox.ai \
  --header "Authorization: Bearer zb_..."
```

## Connect Codex

```sh
codex mcp add zipbox --url https://mcp.zipbox.ai
```

Or, with a key, in `~/.codex/config.toml`:

```toml
[mcp_servers.zipbox]
url = "https://mcp.zipbox.ai"
bearer_token_env_var = "ZIPBOX_API_KEY"
```

## Connect Cursor, Windsurf or any stdio client

```sh
zipbox mcp init cursor     # writes the client's MCP config
zipbox mcp config          # prints a JSON snippet for any other client
zipbox mcp start           # what the config runs: the same tools over stdio
```

`zipbox mcp start` authenticates with the CLI's key (`zipbox login`, or `ZIPBOX_API_KEY`).

## Tools

Every tool has a title, a one-line description, read-only or destructive hints, and a typed input schema.

| Tool                       | Parameters                        | Hints                 | Does                                                                    |
| -------------------------- | --------------------------------- | --------------------- | ----------------------------------------------------------------------- |
| `list_sandboxes`           | -                                 | read-only             | Your boxes                                                              |
| `get_sandbox`              | `id`                              | read-only             | One box: status, size, resource sample                                  |
| `create_sandbox`           | `name?`, `tier?`                  |                       | Create a box and wait until it is ready                                 |
| `start_sandbox`            | `id`                              |                       | Start a stopped box                                                     |
| `stop_sandbox`             | `id`                              |                       | Stop a box (disk archived, nothing billed)                              |
| `restart_sandbox`          | `id`                              |                       | Hard-restart a running box                                              |
| `delete_sandbox`           | `id`, `keepCopy?`                 | destructive           | Delete a box; `keepCopy` (default true) keeps a 30-day recoverable copy |
| `execute_command`          | `id`, `cmd`, `cwd?`, `timeoutMs?` | destructive           | Run a command; returns exit code and output (capped)                    |
| `run_code`                 | `id`, `language`, `code`          | destructive           | Run Python, Node or Bash code                                           |
| `start_background_command` | `id`, `cmd`, `cwd?`               | destructive           | Start a long-running command; returns its id                            |
| `get_command_output`       | `id`, `commandId`, `since?`       | read-only             | Output and state of a command, from a cursor                            |
| `read_file`                | `id`, `path`                      | read-only             | A file's contents                                                       |
| `write_file`               | `id`, `path`, `content`           | destructive           | Write a file                                                            |
| `list_files`               | `id`, `path?`                     | read-only             | A directory listing (default `/root/workspace`)                         |
| `get_file_info`            | `id`, `path`                      | read-only             | Kind, size and modification time                                        |
| `create_folder`            | `id`, `path`                      |                       | `mkdir`                                                                 |
| `move_file`                | `id`, `path`, `destination`       | destructive           | Move or rename                                                          |
| `delete_file`              | `id`, `path`                      | destructive           | Delete a file or folder                                                 |
| `git_clone`                | `id`, `url`, `path?`, `branch?`   |                       | Clone a repository; uses your connected GitHub account                  |
| `preview_link`             | `id`, `port`, `label?`            |                       | A public HTTPS URL for a port ([Previews](https://zipbox.ai/docs/previews.md))              |
| `get_logs`                 | `id`, `since?`                    | read-only             | The box's system journal                                                |
| `desktop_screenshot`       | `id`                              | read-only             | A PNG of the box's desktop, returned as an image                        |
| `search_docs`              | `query`                           | read-only (see below) | Search these docs                                                       |
| `read_doc`                 | `slug`                            | read-only (see below) | One docs page as markdown                                               |

**Output cap.** A tool result shows at most 32 KiB of output. `execute_command` returns the command id with its result; `get_command_output` reads the rest from the box, which keeps everything (64 MiB per stream).

**Nothing stops on a clock.** A command `execute_command` starts has no deadline unless you pass `timeoutMs`; use `start_background_command` for dev servers and watchers.

## Resources and prompts

| Resource                | Contents                                         |
| ----------------------- | ------------------------------------------------ |
| `zipbox://sandboxes`    | Your boxes, so a model can ground without a call |
| `zipbox://sandbox/{id}` | One box                                          |
| `zipbox://docs/{slug}`  | A docs page as markdown                          |
| `zipbox://skill`        | The Zipbox agent skill                           |

The `onboard_box` prompt walks a model through opening a box and running a command. The server's `instructions` (sent at initialize) tell the model to prefer `search_docs` over prior knowledge, that `execute_command` output is capped with the rest in `get_command_output`, and that nothing stops on a clock.

## Read the docs before signing in

`search_docs` and `read_doc` also run on a second, docs-only endpoint, `https://mcp.zipbox.ai/docs/mcp`, which needs no OAuth or key, so a client can learn how to connect before it connects. `https://mcp.zipbox.ai` itself always asks for sign-in (`401` with an OAuth challenge), docs tools included.

```sh
claude mcp add --transport http zipbox-docs https://mcp.zipbox.ai/docs/mcp
```

## OAuth and removing access

Connecting with OAuth creates an API key named after the app - "Claude (OAuth)", "Codex (OAuth)", or whatever name the client registered - in **Settings → API keys**. Delete that row to disconnect the app: its next call fails with `401` and it asks you to sign in again. Other apps are untouched. Building your own client? See [OAuth](https://zipbox.ai/docs/oauth.md).
