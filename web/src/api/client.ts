import { readCookie } from '../lib/cookies'
import type { Library, PathEntry, User } from './types'

// ApiError carries the HTTP status and the server's {error} message.
export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
    this.name = 'ApiError'
  }
}

// A single place to react to session loss (set by the app to clear caches and
// route to /login), so every screen handles 401 uniformly.
let unauthorizedHandler: (() => void) | null = null
export function setUnauthorizedHandler(fn: () => void): void {
  unauthorizedHandler = fn
}

const MUTATING = new Set(['POST', 'PATCH', 'PUT', 'DELETE'])

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {}
  let payload: BodyInit | undefined

  if (body !== undefined) {
    headers['Content-Type'] = 'application/json'
    payload = JSON.stringify(body)
  }

  // CSRF: double-submit. Attach the readable cairn_csrf cookie as a header on
  // every mutating request; GET/HEAD omit it. Centralized so no call site forgets.
  if (MUTATING.has(method)) {
    const csrf = readCookie('cairn_csrf')
    if (csrf) headers['X-CSRF-Token'] = csrf
  }

  const resp = await fetch(path, {
    method,
    headers,
    body: payload,
    credentials: 'include', // send the HttpOnly session cookie
  })

  if (resp.status === 401) {
    unauthorizedHandler?.()
    throw new ApiError(401, 'Not authenticated')
  }
  if (!resp.ok) {
    throw new ApiError(resp.status, await errorMessage(resp))
  }
  if (resp.status === 204) return undefined as T
  if ((resp.headers.get('Content-Type') ?? '').includes('application/json')) {
    return (await resp.json()) as T
  }
  return undefined as T
}

async function errorMessage(resp: Response): Promise<string> {
  try {
    const data = (await resp.json()) as { error?: string }
    if (data?.error) return data.error
  } catch {
    // fall through to a generic message
  }
  return `Request failed (${resp.status})`
}

export const api = {
  login: (username: string, password: string) =>
    request<User>('POST', '/api/login', { username, password }),
  logout: () => request<void>('POST', '/api/logout'),
  me: () => request<User>('GET', '/api/me'),

  libraries: () => request<Library[]>('GET', '/api/libraries'),
  library: (id: string) => request<Library>('GET', `/api/libraries/${id}`),

  files: (id: string, path: string) =>
    request<PathEntry[]>('GET', `/api/libraries/${id}/files?path=${encodeURIComponent(path)}`),

  // A same-origin URL for an <a download> element. The full virtual path is
  // encoded as one query value (the server URL-decodes it back to the true path).
  downloadUrl: (id: string, path: string) =>
    `/api/libraries/${id}/files/download?path=${encodeURIComponent(path)}`,
}
