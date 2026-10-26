import type { EngineInterface, On, Timer } from 'claude-code'
import { atom, read, update } from 'claude-code'

import type { BoxTree, Here, PaneBox, PaneSession } from '../types'
import { connect, listBoxes, listSessions } from './client'
import { follow } from './mirror'

// The Zipbox pane: the account's boxes and, under each running one, its agent
// sessions. A press on a task follows it in this window; a press on a box makes
// it the box the /zipbox commands act on.

export const PANE = 'zipbox'
const here = atom({ plugin: 'zipbox', key: 'here' } as const, { box: null, task: null } as Here)
const tree = atom({ plugin: 'zipbox', key: 'tree' } as const, null as BoxTree | null)
/** How many sessions a box lists. */
const FEW = 8
/** The status of a session that works, or waits on the person. */
const MARK: Record<string, string> = {
  working: 'claude',
  running: 'claude',
  queued: 'claude',
  retrying: 'warning',
  needs_input: 'permission',
  failed: 'error',
  interrupted: 'warning'
}
/** The timer that reads the boxes again while the pane is open. */
let poll: Timer | null = null
let isRefreshing = false

async function boxRow($: EngineInterface, box: PaneBox): Promise<PaneBox> {
  if (box.status !== 'running') return box
  try {
    const sessions = await listSessions($, box.id)
    return {
      ...box,
      sessions: sessions.map(
        (one): PaneSession => ({
          id: one.id,
          title: one.title || one.firstPrompt?.split('\n')[0] || one.id,
          status: one.taskState ?? one.status,
          taskId: one.taskId ?? null
        })
      )
    }
  } catch (error) {
    return { ...box, error: (error as Error).message }
  }
}

export async function refresh($: EngineInterface) {
  // One read at a time: a slow box must not pile reads up behind the timer.
  if (isRefreshing) return
  isRefreshing = true
  try {
    await readBoxes($)
  } finally {
    isRefreshing = false
  }
}

async function readBoxes($: EngineInterface) {
  if (!(await connect($))) {
    await update($, tree, () => ({ boxes: [], error: 'Not signed in. /zipbox login signs in.' }))
    return
  }
  try {
    const listed = await listBoxes($)
    const boxes = await Promise.all(
      listed.map((box) =>
        boxRow($, {
          id: box.id,
          name: box.name ?? box.id,
          status: box.status,
          sessions: [],
          error: null
        })
      )
    )
    await update($, tree, () => ({ boxes, error: null }))
  } catch (error) {
    await update($, tree, (last) => ({ boxes: last?.boxes ?? [], error: (error as Error).message }))
  }
}

export function registerPane(on: On) {
  on('ui.close', (_, e, next) => {
    if (e.id === PANE) {
      poll?.cancel()
      poll = null
    }
    return next(e)
  })

  on('ui.render', { component: 'Pane', requestId: PANE }, async ($, e) => {
    const { Box, Text, Button } = $.ui.resolve(e)
    // The first drawing starts the refresh. Nothing on a box pushes to a box
    // token (the event socket needs the browser's ownership of the machine), so
    // the pane reads the boxes again every 10 s while it is open.
    if (!poll) {
      void refresh($)
      poll = $.clock.every(10_000, () => void refresh($))
    }
    const data = await read($, tree)
    const at = await read($, here)
    if (!data) return <Text dimColor>Reading your boxes…</Text>

    const open = async (box: PaneBox, session: PaneSession) => {
      if (!session.taskId) {
        $.ui.toast('This session runs in the box’s own terminal. /zipbox open shows its desktop.')
        return
      }
      await follow($, {
        box: box.id,
        boxName: box.name,
        task: session.taskId,
        title: session.title
      })
    }

    const row = (box: PaneBox, session: PaneSession) => {
      const isHere = session.taskId !== null && session.taskId === at.task
      const color = MARK[session.status]
      return (
        <Box
          key={`row-${session.id}`}
          flexDirection="row"
          gap={1}
          paddingLeft={2}
          {...(isHere ? { backgroundColor: 'userMessageBackground' } : {})}
        >
          <Box width={1} flexShrink={0}>
            {color ? <Text color={color}>●</Text> : null}
          </Box>
          <Box flexGrow={1} flexShrink={1} minWidth={0}>
            <Button key={`s-${session.id}`} plain onPress={() => open(box, session)}>
              {session.title} <Text dimColor>{session.status}</Text>
            </Button>
          </Box>
        </Box>
      )
    }

    return (
      <Box flexDirection="column">
        {data.error ? (
          <Text color="error" wrap="wrap">
            {data.error}
          </Text>
        ) : null}
        {data.boxes.length === 0 && !data.error ? (
          <Text dimColor wrap="wrap">
            No boxes yet. Create one at zipbox.ai.
          </Text>
        ) : null}
        {data.boxes.map((box) => (
          <Box key={box.id} flexDirection="column" marginTop={1}>
            <Button
              key={`box-${box.id}`}
              plain
              onPress={() => update($, here, (last) => ({ box: box.id, task: last?.task ?? null }))}
            >
              <Text bold>{box.name}</Text>{' '}
              <Text dimColor>
                {box.status}
                {box.id === at.box ? ' · in use' : ''}
              </Text>
            </Button>
            {box.error ? (
              <Box paddingLeft={2}>
                <Text color="warning" wrap="wrap">
                  {box.error}
                </Text>
              </Box>
            ) : null}
            {box.sessions.slice(0, FEW).map((session) => row(box, session))}
            {box.status === 'running' && box.sessions.length === 0 && !box.error ? (
              <Box paddingLeft={2}>
                <Text dimColor>No agent sessions. /zipbox new starts one.</Text>
              </Box>
            ) : null}
          </Box>
        ))}
      </Box>
    )
  })
}
