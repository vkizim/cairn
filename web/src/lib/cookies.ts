// readCookie reads a non-HttpOnly cookie value (used only for the CSRF token;
// the session cookie is HttpOnly and never read from JS).
export function readCookie(name: string): string | null {
  const prefix = name + '='
  for (const part of document.cookie.split(';')) {
    const c = part.trim()
    if (c.startsWith(prefix)) {
      return decodeURIComponent(c.slice(prefix.length))
    }
  }
  return null
}
