// A Zipbox box as a tool for the OpenAI Agents SDK.
// Needs ZIPBOX_API_KEY and OPENAI_API_KEY.
import { Agent, run, tool } from '@openai/agents'
import { Zipbox } from '@zipbox/sdk'
import { z } from 'zod'

const zb = new Zipbox()
const box = await zb.sandboxes.getOrCreate('openai-agent')

const shell = tool({
  name: 'shell',
  description: 'Run a bash command on a Linux machine and return its output.',
  parameters: z.object({ cmd: z.string() }),
  execute: async ({ cmd }) => {
    const { exitCode, stdout, stderr } = await box.exec(cmd)
    return `exit ${exitCode ?? 'unknown'}\n${stdout.slice(-16_000)}\n${stderr.slice(-4_000)}`
  }
})

const agent = new Agent({
  name: 'Box operator',
  instructions: 'You operate a Linux machine through the shell tool. Be concise.',
  tools: [shell]
})

const result = await run(agent, 'How much free disk space does the machine have?')
console.log(result.finalOutput)
