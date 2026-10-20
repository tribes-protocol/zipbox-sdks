> Zipbox docs, markdown edition of https://zipbox.ai/docs/cli
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# CLI

The zipbox command - install, login, every command and flag, --json output, CI use, SSH and the desktop.

`zipbox` drives your boxes from a terminal or a CI job. It is built on the [Go SDK](https://zipbox.ai/docs/sdk-go.md) and talks to the same API.

> This is the CLI for **your** computer. Inside a box, `zipbox` already names the box's own tool (`zipbox keys`, `zipbox-os`, …); the two are unrelated, and this one is not installed in boxes.

## Install

```sh
brew install tribes-protocol/tap/zipbox          # macOS, Linux
curl -fsSL https://zipbox.ai/install.sh | sh      # macOS, Linux
irm https://zipbox.ai/install.ps1 | iex           # Windows PowerShell
```

The installers verify the release checksum and install into a user-writable directory. Builds exist for macOS and Linux (amd64, arm64) and Windows (amd64); every release is on GitHub.

## Log in

```sh
zipbox login
```

`login` opens `zipbox.ai/cli/auth` in your browser. Sign in, and the page mints a key of kind `cli` and hands it to the CLI on a local callback - like `gh auth login`. The key is stored in your user config directory with owner-only permissions. In CI, skip `login` and set `ZIPBOX_API_KEY`; the environment variable always wins.

| Command                     | Does                                                      |
| --------------------------- | --------------------------------------------------------- |
| `zipbox login`              | Browser sign-in; stores a `cli` key                       |
| `zipbox login --no-browser` | Prints the sign-in URL instead of opening it              |
| `zipbox login --with-key`   | Reads a `zb_` key from stdin and stores it                |
| `zipbox logout`             | Forgets the stored key (delete it in Settings to kill it) |
| `zipbox whoami`             | The account and key in use                                |

## Boxes

| Command                                                                      | Does                                        |
| ---------------------------------------------------------------------------- | ------------------------------------------- |
| `zipbox sandbox create [--name <name>] [--tier s] [--ssh-public-key <line>]` | Create a box and wait until it is ready     |
| `zipbox sandbox list`                                                        | Your boxes                                  |
| `zipbox sandbox get <box>`                                                   | One box                                     |
| `zipbox sandbox start <box>`                                                 | Start a stopped box                         |
| `zipbox sandbox stop <box>`                                                  | Stop (disk archived, nothing billed)        |
| `zipbox sandbox restart <box>`                                               | Hard restart; files survive                 |
| `zipbox sandbox tier <box> <tier>`                                           | Resize                                      |
| `zipbox sandbox delete <box> [--yes]`                                        | Delete (30-day recoverable copy by default) |

`<box>` is a box id or its name. `sandbox` also answers to `box`; `sandbox list` to `sandbox ls`. `delete` asks for confirmation unless you pass `--yes` (`-y`).

## Run commands

```sh
# stream output; exit with the remote exit code
zipbox exec <box> -- npm test
# print the command id and return
zipbox exec <box> --background -- npm run dev
# run code from stdin
echo 'print(1 + 1)' | zipbox run <box> --lang python -
# or from a file
zipbox run <box> --lang node script.js
```

`exec` joins its arguments with spaces and runs them with `bash -lc`, like `ssh`. It exits with the command's own status, so a CI step fails when the remote command fails. `run` (`--lang python|node|bash`, required) does the same for code read from a file, or from stdin with `-` or no file.

| `exec` flag       | Does                                                          |
| ----------------- | ------------------------------------------------------------- |
| `--cwd <dir>`     | Working directory (default `/root/workspace`)                 |
| `-e`, `--env K=V` | Environment variable; repeatable                              |
| `--timeout <dur>` | Kill the command after this long, e.g. `10m` (default: never) |
| `--background`    | Start it, print its id, return; it keeps running              |

## Files

| Command                                   | Does             |
| ----------------------------------------- | ---------------- |
| `zipbox files ls <box> [path]`            | List a directory |
| `zipbox files cat <box> <path>`           | Print a file     |
| `zipbox files put <box> <local> <remote>` | Upload           |
| `zipbox files get <box> <remote> [local]` | Download         |
| `zipbox files mkdir <box> <path>`         | Make a directory |
| `zipbox files mv <box> <from> <to>`       | Move or rename   |
| `zipbox files rm <box> <path>`            | Delete           |

Relative remote paths are under `/root/workspace`. `put` reads stdin when `<local>` is `-`, and a `<remote>` ending in `/` keeps the local file name. `get` writes to the remote file's name when `[local]` is omitted, and to stdout when it is `-`.

## Terminal, SSH, previews and logs

| Command                                            | Does                                                                                           |
| -------------------------------------------------- | ---------------------------------------------------------------------------------------------- |
| `zipbox shell <box> [--attach <session>]`          | An interactive terminal on the box (reconnects by itself); `--attach` re-joins a session by id |
| `zipbox ssh <box> [--exec]`                        | Prints the `ssh root@<ipv6>` command; `--exec` runs it                                         |
| `zipbox preview <box> <port> [--label <label>]`    | A public HTTPS URL for a port ([Previews](https://zipbox.ai/docs/previews.md))                                     |
| `zipbox preview rm <box> <label>`                  | Remove a preview                                                                               |
| `zipbox logs <box> [--since …] [-n N] [-u <unit>]` | The system journal ([Logs](https://zipbox.ai/docs/logs.md))                                                        |

**SSH** goes directly to the box over IPv6 with the keys you added in **Settings → SSH keys** (or `--ssh-public-key` at create). Your network needs IPv6; if it has none, use `zipbox shell`, which runs over HTTPS.

**The desktop.** Every box has a full desktop with a browser, at zipbox.ai in the box's window. From code, the SDKs' `screenshot()` and `desktop_screenshot` on the [MCP server](https://zipbox.ai/docs/mcp.md) capture it ([Desktop](https://zipbox.ai/docs/desktop.md)).

## MCP

| Command                    | Does                                                               |
| -------------------------- | ------------------------------------------------------------------ |
| `zipbox mcp start`         | Serve the Zipbox MCP tools over stdio, with the CLI's key          |
| `zipbox mcp init <client>` | Write the MCP config for `cursor`, `windsurf`, `claude` or `codex` |
| `zipbox mcp config`        | Print a JSON config snippet for any other client                   |

## Docs from the terminal

```sh
zipbox docs box-api    # prints https://zipbox.ai/docs/box-api as markdown
zipbox docs            # no slug: prints /llms.txt
```

Any slug from [/llms.txt](https://zipbox.ai/llms.txt) works. Agents in a terminal read docs this way without a browser.

## JSON output

Every command takes `--json` and prints one JSON document with stable field names: the same shapes as the [REST API](https://zipbox.ai/docs/http-api.md) - box rows from `sandbox`, command results `{id, exitCode, stdout, stderr, timedOut}` from `exec` and `run` (collected, then printed once the command exits; `exec --background` prints the command record), file entries `{name, kind, sizeBytes, modifiedAtMs}` from `files ls`. Errors print `{error: {code, message, requestId, retryable, docsUrl, …}}` on stderr and exit non-zero.

## In CI

```yaml
- run: curl -fsSL https://zipbox.ai/install.sh | sh
- run: zipbox exec ci-runner --cwd app -- git pull
- run: zipbox exec ci-runner --cwd app -- make test
  env:
    ZIPBOX_API_KEY: ${{ secrets.ZIPBOX_API_KEY }}
```

Give CI its own key so you can delete it alone. A full workflow is on [Examples](https://zipbox.ai/docs/examples.md#github-actions).
