import type { EngineInterface, On } from 'claude-code'
import { atom, read, update } from 'claude-code'

import type { Here } from '../types'

import {
  api,
  type BoxInfo,
  boxOrigin,
  boxPath,
  connect,
  createTask,
  exec,
  forget,
  listBoxes,
  listFiles,
  readFile,
  signedIn,
  taskAction,
  WEB_URL
} from './client'
import { follow, followed, leave } from './mirror'
import { PANE, refresh } from './pane'

// `/zipbox <action>`: what the dashboard and the CLI do with a box, done on the
// box this window uses. Bare, it opens the pane.

const here = atom({ plugin: 'zipbox', key: 'here' } as const, { box: null, task: null } as Here)

const HELP = `/zipbox                     open the pane: your boxes and their agent sessions
/zipbox login               sign in in the browser (through the zipbox CLI)
/zipbox logout | whoami
/zipbox use <box>           the box the commands below act on
/zipbox new [box] <prompt>  start an agent task on the box and follow it here
/zipbox cancel | resume     the task this window follows
/zipbox leave               stop following; prompts go to Claude here again
/zipbox run <command>       run a shell command on the box
/zipbox files [path]        list a folder on the box
/zipbox read <path>         show a text file of the box
/zipbox save <path>         copy a file of the box into this folder
/zipbox preview <port>      a public HTTPS URL for a port on the box
/zipbox screenshot          save the box's desktop as zipbox-screenshot.png here
/zipbox open                open the box's desktop in the browser`

/** The most a file or an output shows in the transcript. */
const SHOWN_CHARS = 20_000
const NO_CLI = `/zipbox login signs in through the zipbox CLI, which is not installed here. Install it (brew install tribes-protocol/tap/zipbox, or curl -fsSL ${WEB_URL}/install.sh | sh) and run /zipbox login again, or set ZIPBOX_API_KEY.`

const tail = (text: string) => (text.length > SHOWN_CHARS ? `…${text.slice(-SHOWN_CHARS)}` : text)
const boxName = (box: BoxInfo) => box.name ?? box.id

/** The box named, the one in use, or the only one. */
async function pickBox($: EngineInterface, named?: string): Promise<BoxInfo> {
  const boxes = await listBoxes($)
  const wanted = named ?? (await read($, here)).box ?? undefined
  const found = wanted
    ? boxes.find((box) => box.id === wanted || box.name === wanted)
    : boxes.length === 1
      ? boxes[0]
      : undefined
  if (found) return found
  if (named) throw new Error(`No box ${named}. Yours: ${boxes.map(boxName).join(', ') || 'none'}.`)
  throw new Error(
    boxes.length === 0
      ? `No boxes yet. Create one at ${WEB_URL}.`
      : `Pick a box: /zipbox use <box>. Yours: ${boxes.map(boxName).join(', ')}.`
  )
}

// Writes base64 text into a file of this folder: a file's bytes cross $.http as text.
async function saveBase64($: EngineInterface, name: string, base64: string): Promise<string> {
  const written = await $.process
    .run(['/bin/sh', '-c', 'base64 -d > "$1"', 'sh', name], { stdin: base64 })
    .catch(() => null)
  return written?.exitCode === 0 ? `Saved ${name}.` : `Could not write ${name} here.`
}

async function openInBrowser($: EngineInterface, link: string) {
  for (const opener of ['open', 'xdg-open']) {
    const opened = await $.process.run([opener, link], { timeoutMs: 5000 }).catch(() => null)
    if (opened?.exitCode === 0) return
  }
}

async function onBox($: EngineInterface, action: string, rest: string[]): Promise<string> {
  const arg = rest.join(' ').trim()
  switch (action) {
    case 'use': {
      if (!arg) return 'Name the box: /zipbox use <box>'
      const box = await pickBox($, arg)
      await update($, here, (last) => ({ box: box.id, task: last?.task ?? null }))
      return `Using ${boxName(box)}.`
    }
    case 'new': {
      const boxes = await listBoxes($)
      const named = boxes.find((box) => box.id === rest[0] || box.name === rest[0])
      const prompt = (named ? rest.slice(1) : rest).join(' ').trim()
      if (!prompt) return 'Say what the task is: /zipbox new [box] <prompt>'
      const box = named ?? (await pickBox($))
      const task = await createTask($, box.id, prompt)
      await follow($, {
        box: box.id,
        boxName: boxName(box),
        task: task.id,
        title: prompt.split('\n')[0]?.slice(0, 80) ?? task.id
      })
      return `Started a task on ${boxName(box)}.`
    }
    case 'cancel':
    case 'resume': {
      const at = followed()
      if (!at) return 'This window follows no task. /zipbox opens one.'
      const task = await taskAction($, at.box, at.task, action)
      return `The task is ${task.state}.`
    }
    case 'run': {
      if (!arg) return 'Name the command: /zipbox run <command>'
      const box = await pickBox($)
      const ran = await exec($, box.id, { cmd: arg })
      const output = tail(ran.stdout + ran.stderr)
      return `${output}${output.endsWith('\n') || !output ? '' : '\n'}exit ${ran.exitCode ?? '?'}`
    }
    case 'files': {
      const box = await pickBox($)
      const listed = await listFiles($, box.id, boxPath(arg))
      if (listed.entries.length === 0) return `${listed.path} is empty.`
      const lines = listed.entries.map(
        (entry) => `${entry.name}${entry.kind === 'directory' ? '/' : ''}`
      )
      return [`${listed.path}:`, ...lines, ...(listed.truncated ? ['…more'] : [])].join('\n')
    }
    case 'read': {
      if (!arg) return 'Name the file: /zipbox read <path>'
      const box = await pickBox($)
      const text = await readFile($, box.id, boxPath(arg))
      if (text.includes('\u0000')) return 'Not a text file. /zipbox save copies it here.'
      return text.length > SHOWN_CHARS ? `${text.slice(0, SHOWN_CHARS)}\n…` : text
    }
    case 'save': {
      if (!arg) return 'Name the file: /zipbox save <path>'
      const box = await pickBox($)
      const ran = await exec($, box.id, { argv: ['base64', '-w0', boxPath(arg)] })
      if (ran.exitCode !== 0) return ran.stderr.trim() || `Could not read ${arg} on the box.`
      return saveBase64($, boxPath(arg).split('/').at(-1) || 'zipbox-file', ran.stdout.trim())
    }
    case 'preview': {
      const port = Number(arg)
      if (!Number.isInteger(port) || port < 1 || port > 65535)
        return 'Name the port: /zipbox preview <port>'
      const box = await pickBox($)
      const host = `p${port}.${new URL(await boxOrigin($, box.id)).host}`
      const ran = await exec($, box.id, {
        argv: ['tribes-caddy', 'add', host, `127.0.0.1:${port}`]
      })
      if (ran.exitCode !== 0) return ran.stderr.trim() || 'The box did not publish the port.'
      return `https://${host} (public: anyone with the link reaches port ${port})`
    }
    case 'screenshot': {
      const box = await pickBox($)
      const ran = await exec($, box.id, { cmd: SCREENSHOT_COMMAND })
      if (ran.exitCode !== 0) return ran.stderr.trim() || 'The box took no screenshot.'
      return saveBase64($, 'zipbox-screenshot.png', ran.stdout.trim())
    }
    case 'open': {
      const box = await pickBox($)
      const link = `${WEB_URL}/os/${box.id}`
      await openInBrowser($, link)
      return link
    }
  }
  return HELP
}

// The SDKs' screenshot: the box's newest X display through ffmpeg, as base64.
const SCREENSHOT_COMMAND =
  "set -o pipefail; d=$(ls /tmp/.X11-unix 2>/dev/null | sed -n 's/^X//p' | sort -n | tail -n1); " +
  'ffmpeg -loglevel error -f x11grab -i ":${d:-1}" -frames:v 1 -f image2pipe -vcodec png - | base64 -w0'

/**
 * `/zipbox login`: the CLI's own browser sign-in (zipbox.ai/cli/auth), so the key
 * lands where the CLI keeps it and both read the same one. A key is never typed
 * here: the transcript keeps commands.
 */
async function login($: EngineInterface): Promise<string> {
  if (await $.env.get('ZIPBOX_API_KEY'))
    return 'ZIPBOX_API_KEY is set, and it always wins. Unset it to sign in with the browser.'
  const run = $.process.spawn({ argv: ['zipbox', 'login'] })
  let seen = ''
  let link: string | undefined
  try {
    while (!link) {
      const piece = await run.next()
      if (piece.done) break
      seen += piece.value.text
      link = /https?:\/\/\S+\/cli\/auth\S*/.exec(seen)?.[0]
    }
  } catch {
    return NO_CLI
  }
  if (!link) return `zipbox login stopped: ${seen.trim() || 'no output'}`
  // Waiting on a person has no deadline; the CLI ends when they approve.
  void (async () => {
    let piece = await run.next()
    while (!piece.done) piece = await run.next()
    if (piece.value.code !== 0 || !(await connect($))) {
      $.ui.toast('Zipbox sign-in did not finish. /zipbox login starts again.')
      return
    }
    $.ui.toast('Signed in to Zipbox. /zipbox opens the pane.', { timeoutMs: 8000 })
    await refresh($)
  })().catch(() => {})
  return `Approve this computer in the browser: ${link}`
}

/**
 * `/zipbox logout`: the CLI forgets the key it stored, which is the key this
 * window reads. The CLI knows its own config file on every OS; nothing here
 * guesses at it.
 */
async function logout($: EngineInterface): Promise<string> {
  const ran = await $.process.run(['zipbox', 'logout']).catch(() => null)
  await leave($)
  forget()
  await refresh($)
  if (ran?.exitCode !== 0)
    return `Still signed in: zipbox logout did not run${ran ? `: ${ran.stderr.trim()}` : ' (is the zipbox CLI installed?)'}. Delete the key in Settings → API keys instead.`
  const env = (await $.env.get('ZIPBOX_API_KEY'))
    ? ' ZIPBOX_API_KEY is still set and still signs in.'
    : ''
  return `Signed out here and in the zipbox CLI. The key works until you delete it in Settings → API keys.${env}`
}

async function whoami($: EngineInterface): Promise<string> {
  const who = await api<{
    userId: string
    apiKey?: { name: string; kind: string; last4: string } | null
  }>($, '/account/whoami')
  const key = who.apiKey
    ? `\nkey   ${who.apiKey.name} (${who.apiKey.kind}, …${who.apiKey.last4})`
    : ''
  const from = signedIn()?.source === 'env' ? 'ZIPBOX_API_KEY' : 'the zipbox CLI'
  return `user  ${who.userId}${key}\nfrom  ${from}`
}

export function registerCommands(on: On) {
  on('command.run', { command: 'zipbox' }, async ($, e) => {
    const [action = '', ...rest] = e.args.trim().split(/\s+/)
    try {
      if (action === 'help') return { text: HELP }
      if (action === 'login') return { text: await login($) }
      if (action === 'logout') return { text: await logout($) }
      if (action === 'leave')
        return {
          text: (await leave($))
            ? 'Left the task. Prompts go to Claude here again.'
            : 'Not following a task.'
        }
      if (!(await connect($))) return { text: 'Not signed in. /zipbox login signs in.' }
      if (!action) {
        await $.ui.open({ id: PANE, title: 'Zipbox' })
        return { text: 'Zipbox pane opened. /zipbox help lists the rest.' }
      }
      if (action === 'whoami') return { text: await whoami($) }
      return { text: await onBox($, action, rest) }
    } catch (error) {
      return { text: `Zipbox: ${(error as Error).message}` }
    }
  })
}
