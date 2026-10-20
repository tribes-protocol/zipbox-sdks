// Create (or reuse) a box, run a command, write and read a file, publish a port,
// and stop the box. Run: ZIPBOX_API_KEY=zb_… npx tsx quickstart.ts
import { Zipbox } from '@zipbox/sdk'

const zb = new Zipbox()
const box = await zb.sandboxes.getOrCreate('quickstart')

const { stdout } = await box.exec('uname -a')
console.log(stdout)

await box.files.write('hello.txt', 'hello from the SDK\n')
console.log(await box.files.readText('hello.txt'))

await box.execBackground('python3 -m http.server 8000', { cwd: '/root/workspace' })
const { url } = await box.preview.create(8000)
console.log(`serving /root/workspace at ${url}`)

await box.stop()
