> Zipbox docs, markdown edition of https://zipbox.ai/docs/compare
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# Coming from Daytona or E2B

What you will look for after Daytona or E2B, where Zipbox has it, and the documented gaps.

Coming from another sandbox API? Here is what you will look for and where Zipbox stands. "Have" exists today; "Out" is a documented gap ([Errors and limits](https://zipbox.ai/docs/errors.md#out-of-v1)).

The biggest difference is time: a Zipbox box is a long-lived machine, not a disposable sandbox. Nothing on it stops on a clock, so there is no timeout to extend and no keep-alive to send. Stop it yourself when you are done ([Boxes](https://zipbox.ai/docs/lifecycle.md)).

## Capability by capability

| Capability                       | Zipbox                                                                     |
| -------------------------------- | -------------------------------------------------------------------------- |
| Create / list / get / delete     | Have                                                                       |
| Stop / start (disk preserved)    | Have: stop archives the disk and bills nothing; start restores it          |
| Memory pause / resume            | Out: restart reboots, files survive                                        |
| Auto-stop / idle timeout         | Never, by design. A box runs until you stop it                             |
| Resize                           | Have (`setTier`)                                                           |
| Named snapshots / templates      | Out. A box is its own template; `getOrCreate(name)` reuses it              |
| Run a command, streamed          | Have                                                                       |
| Background command, re-attach    | Have; survives a restart of the box's agent                                |
| Run code (Python, Node, Bash)    | Have (`runCode`)                                                           |
| Stateful code interpreter        | Out: use a PTY or a background `python -i`                                 |
| Files read / write / list / move | Have, up to 250 MiB per request                                            |
| File watch                       | Out                                                                        |
| PTY with reconnect               | Have                                                                       |
| SSH                              | Have: direct over IPv6 (`sshInfo`)                                         |
| Preview URL for a port           | Have, public                                                               |
| Private / signed previews        | Out                                                                        |
| Git clone with credentials       | Have: connect GitHub once in Settings, then `git clone` in the box         |
| LSP, volumes, regions, webhooks  | Out                                                                        |
| Logs and metrics                 | Have: `logs()` and the CPU / memory / disk sample on every box             |
| Outbound network policy          | Out                                                                        |
| Secrets / env injection          | Have: provider keys are injected at the edge, never stored on the box      |
| API keys, many per account       | Have, up to 20                                                             |
| Hosted MCP with OAuth            | Have, plus `zipbox mcp start` for stdio clients                            |
| SDKs                             | TypeScript, Python, Go, Rust                                               |
| `llms.txt`, full text, OpenAPI   | Have all three                                                             |
| Desktop and screenshots          | Have: every box has a desktop; `screenshot()` and MCP `desktop_screenshot` |

## Names that differ

| Daytona / E2B                  | Zipbox                                                                  |
| ------------------------------ | ----------------------------------------------------------------------- |
| Sandbox                        | Box (the SDK namespace is `sandboxes`)                                  |
| `process.exec`, `commands.run` | `exec`, `execStream`, `execBackground` ([Run commands](https://zipbox.ai/docs/commands.md)) |
| `fs`, `files`                  | `files` ([Files](https://zipbox.ai/docs/files.md))                                          |
| `pty`                          | `pty` ([Terminals](https://zipbox.ai/docs/terminals.md))                                    |
| Preview link, public URL       | `preview.create(port)` ([Previews](https://zipbox.ai/docs/previews.md))                     |
| Snapshot / template            | A named box reused with `getOrCreate(name)`                             |
