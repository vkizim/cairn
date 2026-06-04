// Virtual path helpers.
//
// A "virtual path" is an absolute, '/'-rooted path of DECODED segment names,
// e.g. "/", "/docs", "/docs/отчёт #2". In the URL we carry the current
// directory as a router splat where EACH SEGMENT is encodeURIComponent'd
// independently — never the whole string at once — so names containing spaces,
// Cyrillic, or URL-reserved characters (# ? % &) round-trip losslessly between
// the route, the breadcrumb, and the API's ?path= query.

/** splitSegments returns the decoded segments of a virtual path. */
export function splitSegments(path: string): string[] {
  return path.split('/').filter((s) => s.length > 0)
}

/** pathToSplat encodes a virtual path into a route splat (per-segment). */
export function pathToSplat(path: string): string {
  return splitSegments(path).map(encodeURIComponent).join('/')
}

/** splatToPath decodes a route splat back into a virtual path (per-segment). */
export function splatToPath(splat: string | undefined): string {
  if (!splat) return '/'
  const segs = splat
    .split('/')
    .filter((s) => s.length > 0)
    .map(decodeURIComponent)
  return '/' + segs.join('/')
}

/** joinPath appends a (decoded) child name to a directory path. */
export function joinPath(dir: string, name: string): string {
  return dir === '/' ? '/' + name : dir + '/' + name
}

/** parentPath returns the parent directory of a path ('/' for top level). */
export function parentPath(path: string): string {
  const segs = splitSegments(path)
  segs.pop()
  return segs.length === 0 ? '/' : '/' + segs.join('/')
}

// validateFilename mirrors the backend's repo.ValidateFilename so invalid names
// are rejected client-side with a clear message BEFORE any upload starts.
// Returns null when valid, or a human-readable reason.
export function validateFilename(name: string): string | null {
  if (name.length === 0 || name.trim().length === 0) {
    return 'File name is empty'
  }
  if (/[/\\]/.test(name)) {
    return 'File name must not contain path separators'
  }
  if (name === '.' || name === '..') {
    return `"${name}" is not a valid file name`
  }
  if (name.length > 255) {
    return 'File name is too long (max 255 characters)'
  }
  for (const ch of name) {
    const code = ch.codePointAt(0) ?? 0
    if (code < 0x20 || code === 0x7f) {
      return 'File name contains control characters'
    }
  }
  return null
}

export interface Crumb {
  name: string
  path: string
}

/** breadcrumbs returns the {name, path} for each segment of a virtual path. */
export function breadcrumbs(path: string): Crumb[] {
  const crumbs: Crumb[] = []
  let acc = ''
  for (const name of splitSegments(path)) {
    acc = acc + '/' + name
    crumbs.push({ name, path: acc })
  }
  return crumbs
}
