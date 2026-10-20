import { ZipboxError } from './errors.js'

/** The box's workspace: where relative paths and commands land. */
const BOX_WORKSPACE = '/root/workspace'

/** Absolute paths pass through; anything else is joined to /root/workspace. */
export function resolveBoxPath(path: string): string {
  if (path.startsWith('/')) return path
  const relative = path.replace(/^(\.\/)+/, '')
  return relative === '' || relative === '.' ? BOX_WORKSPACE : `${BOX_WORKSPACE}/${relative}`
}

/** Split an absolute file path into its directory and file name. */
export function splitBoxPath(path: string): { dir: string; name: string } {
  const absolute = resolveBoxPath(path)
  const slash = absolute.lastIndexOf('/')
  const name = absolute.slice(slash + 1)
  if (name === '' || name === '.' || name === '..') {
    throw new ZipboxError({ code: 'invalid_request', message: `not a file path: ${path}` })
  }
  return { dir: slash === 0 ? '/' : absolute.slice(0, slash), name }
}

/** Quote a string for a POSIX shell. */
export function shellQuote(value: string): string {
  return `'${value.replace(/'/g, `'\\''`)}'`
}
