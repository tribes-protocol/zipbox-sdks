// Create (or reuse) a box, run a command, write and read a file, publish a port,
// and pause the box. Run: ZIPBOX_API_KEY=zb_… npx tsx quickstart.ts
import { Zipbox } from '@zipbox/sdk'

const zb = new Zipbox()
// Reuse names, never mint one per job: an account holds 250 machine names and a deleted
// box keeps its name. For a fleet, keep a fixed pool and get-or-create from it:
//   const worker = await zb.sandboxes.getOrCreate(`worker-${i % 50}`, { tier: 's' })
// https://zipbox.ai/docs/machine-limit
const box = await zb.sandboxes.getOrCreate('quickstart', { tier: 's' })

const { stdout } = await box.exec('uname -a')
console.log(stdout)

await box.files.write('hello.txt', 'hello from the SDK\n')
console.log(await box.files.readText('hello.txt'))

await box.execBackground('python3 -m http.server 8000', { cwd: '/root/workspace' })
const { url } = await box.preview.create(8000)
console.log(`serving /root/workspace at ${url}`)

await box.pause()
