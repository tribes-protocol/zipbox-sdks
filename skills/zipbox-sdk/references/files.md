> Zipbox docs, markdown edition of https://zipbox.ai/docs/files
> Every page and section: https://zipbox.ai/llms.txt · everything in one file: https://zipbox.ai/llms-full.txt
> API contract: https://zipbox.ai/openapi.json

# Files

Read, write, list, move and delete files on a box, and move large files between your machine and the box.

The SDKs resolve a relative path against `/root/workspace`; absolute paths work anywhere on the box. Symlinks are followed. The routes behind these calls are in [Box API](https://zipbox.ai/docs/box-api.md#files).

## Write, read, list and move

#### TypeScript

```ts
await box.files.write('notes/todo.txt', 'ship it\n')
const text = await box.files.readText('notes/todo.txt')
const entries = await box.files.list('notes')
await box.files.move('notes/todo.txt', 'notes/done.txt')
```

#### Python

```python
box.files.write("notes/todo.txt", "ship it\n")
text = box.files.read_text("notes/todo.txt")
entries = box.files.list("notes")
box.files.move("notes/todo.txt", "notes/done.txt")
```

#### Go

```go
_ = box.Files.Write(ctx, "notes/todo.txt", strings.NewReader("ship it\n"))
data, err := box.Files.Read(ctx, "notes/todo.txt")
entries, err := box.Files.List(ctx, "notes")
_ = box.Files.Move(ctx, "notes/todo.txt", "notes/done.txt")
```

#### Rust

```rust
sbx.files().write("notes/todo.txt", "ship it\n").await?;
let text = sbx.files().read_text("notes/todo.txt").await?;
let entries = sbx.files().list("notes").await?;
sbx.files().move_to("notes/todo.txt", "notes/done.txt").await?;
```

#### CLI

```sh
# upload a local file
zipbox files put $ID ./todo.txt notes/todo.txt
# print it
zipbox files cat $ID notes/todo.txt
# list the directory
zipbox files ls $ID notes
# rename it
zipbox files mv $ID notes/todo.txt notes/done.txt
```

#### curl

```sh
# upload
curl -X POST "$BASE/files/write?dir=%2Froot%2Fworkspace%2Fnotes&name=todo.txt" \
  -H "Authorization: Bearer $BOX_TOKEN" --data-binary @todo.txt
# download
curl "$BASE/files/download?path=%2Froot%2Fworkspace%2Fnotes%2Ftodo.txt" \
  -H "Authorization: Bearer $BOX_TOKEN"
# list a directory
curl "$BASE/files?path=%2Froot%2Fworkspace%2Fnotes" \
  -H "Authorization: Bearer $BOX_TOKEN"
# move
curl -X POST "$BASE/files/mutate" -H "Authorization: Bearer $BOX_TOKEN" \
  -d '{"action":"move","path":"/root/workspace/notes/todo.txt",
       "destination":"/root/workspace/notes/done.txt"}'
```

Writes are atomic: the box writes a temporary file and renames it, so a reader never sees half a file. `list` returns `{name, kind, sizeBytes, modifiedAtMs}` per entry, where `kind` is `file`, `directory`, `symlink` or `other`. `stat(path)` returns one entry; `mkdir` and `delete` round out the set.

## Large files

`upload(localPath, remotePath)` and `download(remotePath, localPath)` stream between a file on your machine and the box without holding it in memory (TypeScript on Node, Python, Go and Rust).

One request carries up to 250 MiB; a bigger file answers `413`. Split it, or have the box fetch it itself with a command (`curl` from the box, `git`, `rsync`) - see [Run commands](https://zipbox.ai/docs/commands.md).

## Limits

At most 8 file transfers run at once per box; more answer `429`. A missing path is `not_found`. Every code is in [Errors and limits](https://zipbox.ai/docs/errors.md#box-side-errors).
