// Start a dev server that keeps running after this script exits, publish it, and
// come back to its output later from anywhere with the command id.
//
//   npx tsx long-running-dev-server-preview.ts            # start it
//   npx tsx long-running-dev-server-preview.ts <cmd-id>   # re-attach to its output
import { Zipbox } from '@zipbox/sdk'

const zb = new Zipbox()
const box = await zb.sandboxes.getOrCreate('dev-server')
const commandId = process.argv[2]

if (commandId === undefined) {
  await box.exec('mkdir -p site && echo "<h1>hello</h1>" > site/index.html')
  const server = await box.execBackground('python3 -m http.server 4321', { cwd: 'site' })
  const { url } = await box.preview.create(4321, { label: 'site' })
  console.log(`command ${server.id} serving ${url}`)
  console.log(`re-attach: npx tsx long-running-dev-server-preview.ts ${server.id}`)
} else {
  // Replays everything the server printed, then follows it live. Ctrl-C detaches;
  // the server keeps running.
  for await (const event of box.commands.follow(commandId)) {
    if (event.type === 'stdout' || event.type === 'stderr') process.stdout.write(event.data)
    if (event.type === 'exit') console.log(`server exited with ${event.exitCode ?? 'unknown'}`)
  }
}
