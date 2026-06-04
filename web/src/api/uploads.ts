// Typed wrappers for the step-3 resumable upload protocol (Upload-Offset).
// All calls go through rawFetch/request, so credentials and the CSRF header on
// mutating requests are applied centrally. PATCH bodies are raw Blob slices —
// never JSON, never a full file read.
import { ApiError, errorMessage, rawFetch, request } from './client'

export interface CreateUploadResponse {
  upload_id: string
  offset: number
}

export interface CompleteResponse {
  commit_hash: string
  files: number
  logical_bytes: number
  physical_new_bytes: number
  unique_blocks: number
  total_blocks: number
  dedup_ratio: number
}

// OffsetConflictError (HTTP 409) carries the server's authoritative offset so
// the uploader can resync without restarting the file.
export class OffsetConflictError extends Error {
  serverOffset: number
  constructor(serverOffset: number) {
    super(`offset conflict; server is at ${serverOffset}`)
    this.name = 'OffsetConflictError'
    this.serverOffset = serverOffset
  }
}

export function createUpload(
  libId: string,
  opts: { path: string; filename: string; size: number },
): Promise<CreateUploadResponse> {
  return request<CreateUploadResponse>('POST', `/api/libraries/${libId}/uploads`, opts)
}

// patchChunk sends one raw chunk at offset and returns the server-acknowledged
// new offset. 409 → OffsetConflictError (resync point), 413 → size limit.
export async function patchChunk(
  libId: string,
  uploadId: string,
  offset: number,
  chunk: Blob,
  signal?: AbortSignal,
): Promise<number> {
  const resp = await rawFetch('PATCH', `/api/libraries/${libId}/uploads/${uploadId}`, {
    headers: { 'Upload-Offset': String(offset) },
    body: chunk,
    signal,
  })
  if (resp.status === 409) {
    const server = Number(resp.headers.get('Upload-Offset') ?? '0')
    resp.body?.cancel?.()
    throw new OffsetConflictError(server)
  }
  if (resp.status === 413) {
    throw new ApiError(413, 'File exceeds the server upload size limit')
  }
  if (!resp.ok) {
    throw new ApiError(resp.status, await errorMessage(resp))
  }
  return Number(resp.headers.get('Upload-Offset') ?? String(offset + chunk.size))
}

// headUpload returns the server's current received offset (for resume).
export async function headUpload(libId: string, uploadId: string): Promise<number> {
  const resp = await rawFetch('HEAD', `/api/libraries/${libId}/uploads/${uploadId}`)
  if (!resp.ok) {
    throw new ApiError(resp.status, `upload session unavailable (${resp.status})`)
  }
  return Number(resp.headers.get('Upload-Offset') ?? '0')
}

// completeBatch lands N finished upload sessions as ONE merged commit.
export function completeBatch(libId: string, uploadIds: string[]): Promise<CompleteResponse> {
  return request<CompleteResponse>('POST', `/api/libraries/${libId}/uploads/complete-batch`, {
    upload_ids: uploadIds,
  })
}

// deleteUpload aborts a session (idempotent on the server). Errors are
// swallowed: abort is best-effort cleanup, the sweep reclaims leftovers.
export async function deleteUpload(libId: string, uploadId: string): Promise<void> {
  try {
    await rawFetch('DELETE', `/api/libraries/${libId}/uploads/${uploadId}`)
  } catch {
    // best-effort
  }
}
