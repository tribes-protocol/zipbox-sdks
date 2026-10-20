// Give a model a computer: a Zipbox box as a Vercel AI SDK tool.
// Needs ZIPBOX_API_KEY and ANTHROPIC_API_KEY.
import { anthropic } from '@ai-sdk/anthropic'
import { Zipbox } from '@zipbox/sdk'
import { generateText, stepCountIs, tool } from 'ai'
import { z } from 'zod'

const zb = new Zipbox()
const box = await zb.sandboxes.getOrCreate('ai-sdk-agent')

const runCommand = tool({
  description: 'Run a shell command on a Linux machine (Debian, root, /root/workspace).',
  inputSchema: z.object({ cmd: z.string().describe('bash command line') }),
  execute: async ({ cmd }) => {
    const { exitCode, stdout, stderr } = await box.exec(cmd)
    // Keep what the model reads bounded; the box keeps the full output.
    return { exitCode, stdout: stdout.slice(-16_000), stderr: stderr.slice(-4_000) }
  }
})

const { text } = await generateText({
  model: anthropic('claude-sonnet-4-5'),
  tools: { runCommand },
  stopWhen: stepCountIs(8),
  prompt: 'Find out which Python version is installed and write a one-line summary.'
})
console.log(text)
