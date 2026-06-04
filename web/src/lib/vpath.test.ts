import { describe, it, expect } from 'vitest'
import {
  pathToSplat,
  splatToPath,
  joinPath,
  breadcrumbs,
  parentPath,
  validateFilename,
} from './vpath'

// These names exercise spaces, Cyrillic, and URL-reserved characters. The
// per-segment encode/decode contract must round-trip them losslessly between
// the virtual path, the route splat, and (by extension) the API ?path= value.
const trickyNames = ['отчёт #2', 'a?b', '100% done', 'x&y', 'with space', 'plus+plus', 'a/b is impossible']

describe('vpath per-segment round-trip', () => {
  it('round-trips tricky single names through splat', () => {
    for (const name of trickyNames) {
      const path = joinPath('/docs', name)
      const splat = pathToSplat(path)
      // The splat must be safe: no raw spaces or reserved chars leaked.
      expect(splat).not.toMatch(/[ #?&]/)
      expect(splatToPath(splat)).toBe(path)
    }
  })

  it('round-trips nested tricky paths', () => {
    const path = joinPath(joinPath('/', 'отчёт #2'), 'a?b')
    expect(path).toBe('/отчёт #2/a?b')
    expect(splatToPath(pathToSplat(path))).toBe(path)
  })

  it('breadcrumbs decode to the true segment names', () => {
    const path = '/отчёт #2/sub dir/x&y'
    const crumbs = breadcrumbs(path)
    expect(crumbs.map((c) => c.name)).toEqual(['отчёт #2', 'sub dir', 'x&y'])
    // each crumb path itself round-trips
    for (const c of crumbs) {
      expect(splatToPath(pathToSplat(c.path))).toBe(c.path)
    }
  })

  it('handles root and parent', () => {
    expect(splatToPath(undefined)).toBe('/')
    expect(splatToPath('')).toBe('/')
    expect(pathToSplat('/')).toBe('')
    expect(parentPath('/a/b')).toBe('/a')
    expect(parentPath('/a')).toBe('/')
    expect(parentPath('/')).toBe('/')
  })

  it('encodes percent and reserved chars distinctly', () => {
    // "100% done" must not collide with a literal percent-escape after encoding.
    const path = '/100% done'
    const splat = pathToSplat(path)
    expect(splat).toBe('100%25%20done')
    expect(splatToPath(splat)).toBe(path)
  })
})

describe('validateFilename (mirror of repo.ValidateFilename)', () => {
  it('accepts normal and tricky-but-legal names', () => {
    for (const name of ['report.pdf', 'отчёт #2.docx', '100% done', 'a?b', 'x&y']) {
      expect(validateFilename(name)).toBeNull()
    }
  })

  it('rejects invalid names with a reason', () => {
    const bad = ['', '   ', 'a/b', 'a\\b', '.', '..', 'x'.repeat(256), 'tab\tname', '\u0007bell']
    for (const name of bad) {
      expect(validateFilename(name), `name ${JSON.stringify(name)}`).not.toBeNull()
    }
  })
})
