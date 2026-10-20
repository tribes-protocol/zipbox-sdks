import { describe, expect, it } from 'vitest'

import { resolveBoxPath, shellQuote, splitBoxPath } from '../src/paths.js'

describe('paths', () => {
  it('joins relative paths to /root/workspace', () => {
    expect(resolveBoxPath('a/b.txt')).toBe('/root/workspace/a/b.txt')
    expect(resolveBoxPath('./a')).toBe('/root/workspace/a')
    expect(resolveBoxPath('.')).toBe('/root/workspace')
    expect(resolveBoxPath('/etc/hosts')).toBe('/etc/hosts')
  })

  it('splits into directory and name', () => {
    expect(splitBoxPath('/etc/hosts')).toEqual({ dir: '/etc', name: 'hosts' })
    expect(splitBoxPath('/top')).toEqual({ dir: '/', name: 'top' })
    expect(() => splitBoxPath('/etc/')).toThrow()
  })

  it('quotes for a shell', () => {
    expect(shellQuote("it's")).toBe(`'it'\\''s'`)
  })
})
