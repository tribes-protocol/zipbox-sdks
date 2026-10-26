/** One agent session of a box, as the pane lists it. */
export type PaneSession = { id: string; title: string; status: string; taskId: string | null }
/** One box and its agent sessions, most recent first. */
export type PaneBox = {
  id: string
  name: string
  status: string
  sessions: PaneSession[]
  error: string | null
}
/** What the pane draws: the account's boxes, or why it cannot list them. */
export type BoxTree = { boxes: PaneBox[]; error: string | null }
/** The box the /zipbox commands act on, and the task this window follows. */
export type Here = { box: string | null; task: string | null }

declare module 'claude-code' {
  interface PluginState {
    zipbox: { tree: BoxTree | null; here: Here }
  }
}
