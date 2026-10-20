> Zipbox docs, markdown edition of https://zipbox.ai/docs/desktop
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# Desktop

Every box has a full desktop with a browser. Capture it from code as a PNG, or let an MCP client look at it.

Every box runs a full Linux desktop with a browser. You use it from the dashboard ([Desktop and apps](https://zipbox.ai/docs/rails.md)); your code and your agents can look at it too.

## Take a screenshot

`screenshot()` returns a PNG of the box's desktop. The SDKs capture it on the box with `ffmpeg`, so there is nothing to install.

#### TypeScript

```ts
import { writeFile } from 'node:fs/promises'

const png = await box.screenshot()
await writeFile('desktop.png', png)
```

#### Python

```python
png = box.screenshot()
with open("desktop.png", "wb") as file:
    file.write(png)
```

#### Go

```go
png, err := box.Screenshot(ctx)
if err != nil {
	return err
}
err = os.WriteFile("desktop.png", png, 0o644)
```

#### Rust

```rust
let png = sbx.screenshot().await?;
std::fs::write("desktop.png", png)?;
```

The desktop must be running: open it from the dashboard, or start the browser on the box. With no desktop to capture, the call fails with `conflict`.

## From an MCP client

The [MCP server](https://zipbox.ai/docs/mcp.md) has a `desktop_screenshot` tool. Claude, Codex or Cursor call it with the box id and get the image back, so an agent can check what a web app looks like after it changes the code.

## Drive it

The desktop is an ordinary X session on the box. Anything you can run there, you can run from code: start a browser with a URL through [Run commands](https://zipbox.ai/docs/commands.md), or run your end-to-end tests against a [preview](https://zipbox.ai/docs/previews.md) of the app.
