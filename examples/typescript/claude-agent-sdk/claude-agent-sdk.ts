// A Zipbox box as an in-process MCP tool for the Claude Agent SDK. The same box is
// also reachable through the hosted MCP server: mcpServers: { zipbox: { type: 'http',
// url: 'https://mcp.zipbox.ai', headers: { Authorization: `Bearer ${key}` } } }.
// Needs ZIPBOX_API_KEY and ANTHROPIC_API_KEY.
import { createSdkMcpServer, query, tool } from '@anthropic-ai/claude-agent-sdk'
import { Zipbox } from '@zipbox/sdk'
import { z } from 'zod'

const zb = new Zipbox()
const box = await zb.sandboxes.getOrCreate('claude-agent')

const zipbox = createSdkMcpServer({
  name: 'zipbox',
  version: '1.0.0',
  tools: [
    tool(
      'exec',
      'Run a bash command on a remote Linux machine (root, /root/workspace).',
      { cmd: z.string() },
      async ({ cmd }) => {
        const { exitCode, stdout, stderr } = await box.exec(cmd)
        const text = `exit ${exitCode ?? 'unknown'}\n${stdout.slice(-16_000)}${stderr.slice(-4_000)}`
        return { content: [{ type: 'text', text }] }
      }
    )
  ]
})

for await (const message of query({
  prompt: 'Clone https://github.com/sindresorhus/slugify into /root/workspace and run its tests.',
  options: { mcpServers: { zipbox }, allowedTools: ['mcp__zipbox__exec'] }
})) {
  if (message.type === 'result' && message.subtype === 'success') console.log(message.result)
}
