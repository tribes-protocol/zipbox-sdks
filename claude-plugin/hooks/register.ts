import type { Register } from 'claude-code'

import { connect } from './client'
import { registerCommands } from './commands'
import { drawReply, endTurn, followed, isDrawing, send, startTurn } from './mirror'
import { registerPane } from './pane'

// Zipbox in Claude Code: the /zipbox command, the pane of boxes and their agent
// sessions, and the window that follows one task. The plugin's MCP server and
// skill are declared beside this module and need none of it.

export const register: Register = (on) => {
  registerPane(on)
  registerCommands(on)

  on('session.start', async ($, e, next) => {
    const started = await next(e)
    await $.command.register({
      name: 'zipbox',
      description: 'Zipbox: your boxes, their agent tasks, files, commands, previews',
      argumentHint: '[help]'
    })
    await connect($)
    return started
  })

  // While the window follows a task, what the person types goes to that task and
  // starts a turn here, which draws the task's reply.
  on('prompt.submit', async ($, e, next) => {
    const person = e.origin?.kind === 'composer' || e.origin?.kind === 'bridge'
    if (!followed() || !person || e.command) return next(e)
    await send($, e.text)
    return next(e)
  }).catch(async ($, e, next) => {
    if (next.called || !followed()) return next(e)
    // Sent nowhere, the prompt goes back to the box so nothing typed is lost.
    void $.prompt.fill({ text: e.text }).catch(() => {})
    return { drop: `Not sent to the task: ${next.error.message || 'no answer'}` }
  })

  on('turn.start', async ($, e, next) => {
    if (!e.agentId) startTurn(e.turnId, e.text)
    return next(e)
  })

  // The task's reply is this turn's answer: no model is called.
  on('turn.step', async function* ($, e, next) {
    if (e.agentId || !isDrawing(e.turnId)) return yield* next(e)
    return yield* drawReply($, e, next.signal)
  })

  on('turn.complete', async ($, e, next) => {
    endTurn(e.turnId)
    return next(e)
  })
}
