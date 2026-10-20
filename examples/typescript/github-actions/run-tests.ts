// Run this repository's tests in a Zipbox box from CI, and fail the job if they
// fail. Used by workflow.yml. Needs ZIPBOX_API_KEY (a repository secret).
import { Zipbox } from '@zipbox/sdk'

const repo = process.env.GITHUB_REPOSITORY ?? ''
const sha = process.env.GITHUB_SHA ?? 'HEAD'
const zb = new Zipbox()
// One box per repository, reused across runs; nothing stops it on a clock, so stop
// it explicitly when the job is done.
const box = await zb.sandboxes.getOrCreate(
  `ci-${repo.replace(/[^a-z0-9]+/gi, '-').toLowerCase()}`.slice(0, 63)
)

const script = [
  'set -euo pipefail',
  `rm -rf repo && git clone --quiet https://github.com/${repo}.git repo`,
  `cd repo && git checkout --quiet ${sha}`,
  'npm ci && npm test'
].join('\n')

let exitCode: number | null = null
for await (const event of box.execStream(script)) {
  if (event.type === 'stdout') process.stdout.write(event.data)
  if (event.type === 'stderr') process.stderr.write(event.data)
  if (event.type === 'exit') exitCode = event.exitCode
}
await box.stop()
process.exit(exitCode ?? 1)
