// Upload queue: orchestrates resumable uploads against the step-3 protocol.
//
// - Files are sliced into CHUNK_SIZE pieces with Blob.slice — the file is never
//   read into memory as a whole; fetch streams each slice.
// - Chunks within a file are strictly sequential (the protocol's Upload-Offset
//   is a running cursor); up to MAX_PARALLEL files upload concurrently.
// - Progress counts only server-ACKNOWLEDGED bytes (the offset the server
//   returns), so the bar never lies.
// - On a failed chunk: retry with backoff, resyncing the offset via HEAD first —
//   an interrupted upload resumes where the server actually is, never restarts.
//   A 409 resyncs immediately from the server's Upload-Offset. A 413 fails the
//   file with a clear message and no retries; the rest of the queue continues.
// - One drag-drop / multi-select = ONE batch = ONE merged commit (complete-batch).
//   Files that failed or were cancelled are excluded; the successful remainder
//   still lands as a single commit.
// - Known 4b limitation: the queue lives with the file-manager page — navigating
//   away unmounts it and abandons in-flight uploads (sessions expire and are
//   swept server-side).
import { useCallback, useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { ApiError } from '../api/client'
import {
  completeBatch,
  createUpload,
  deleteUpload,
  headUpload,
  OffsetConflictError,
  patchChunk,
} from '../api/uploads'
import { validateFilename } from '../lib/vpath'

export const CHUNK_SIZE = 8 * 1024 * 1024 // 8 MiB; well under the server's per-chunk cap
const MAX_PARALLEL = 2
const MAX_RETRIES = 3

export type UploadStatus =
  | 'queued'
  | 'uploading'
  | 'resuming'
  | 'sent'
  | 'committing'
  | 'done'
  | 'error'
  | 'cancelled'

export interface UploadItem {
  id: string
  name: string
  dir: string
  size: number
  sent: number // server-acknowledged bytes
  status: UploadStatus
  error?: string
}

interface ItemInternal {
  file: File
  dir: string
  abort: AbortController
  uploadId?: string
}

const ACTIVE: UploadStatus[] = ['queued', 'uploading', 'resuming']

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const t = setTimeout(resolve, ms)
    signal.addEventListener('abort', () => {
      clearTimeout(t)
      resolve()
    })
  })
}

function describeError(e: unknown): string {
  if (e instanceof ApiError) return e.message
  if (e instanceof Error) return e.message
  return 'Upload failed'
}

export function useUploadQueue(libraryId: string) {
  const [items, setItems] = useState<UploadItem[]>([])
  const internals = useRef(new Map<string, ItemInternal>())
  const qc = useQueryClient()

  const update = useCallback((id: string, patch: Partial<UploadItem>) => {
    setItems((prev) => prev.map((i) => (i.id === id ? { ...i, ...patch } : i)))
  }, [])

  // uploadFile pushes one file's bytes; returns true when fully sent.
  const uploadFile = useCallback(
    async (id: string): Promise<boolean> => {
      const meta = internals.current.get(id)
      if (!meta || meta.abort.signal.aborted) return false

      update(id, { status: 'uploading' })
      const created = await createUpload(libraryId, {
        path: meta.dir,
        filename: meta.file.name,
        size: meta.file.size,
      })
      meta.uploadId = created.upload_id

      let offset = created.offset
      let retries = 0
      while (offset < meta.file.size) {
        if (meta.abort.signal.aborted) return false
        const chunk = meta.file.slice(offset, Math.min(offset + CHUNK_SIZE, meta.file.size))
        try {
          offset = await patchChunk(libraryId, created.upload_id, offset, chunk, meta.abort.signal)
          retries = 0
          update(id, { sent: offset })
        } catch (e) {
          if (meta.abort.signal.aborted) return false
          if (e instanceof OffsetConflictError) {
            // Server knows better — resync instantly, no retry consumed.
            offset = e.serverOffset
            update(id, { sent: offset })
            continue
          }
          if (e instanceof ApiError && (e.status === 413 || e.status === 404)) {
            throw e // size limit / session lost: no point retrying
          }
          retries++
          if (retries > MAX_RETRIES) throw e
          // Transient failure: back off, then ask the server where it actually
          // is and resume from there (never restart the file).
          update(id, { status: 'resuming' })
          await sleep(1000 * 2 ** (retries - 1), meta.abort.signal)
          if (meta.abort.signal.aborted) return false
          offset = await headUpload(libraryId, created.upload_id)
          update(id, { sent: offset, status: 'uploading' })
        }
      }
      // 0-byte files skip the loop entirely: create -> sent -> batch complete.
      update(id, { status: 'sent', sent: meta.file.size })
      return true
    },
    [libraryId, update],
  )

  const runBatch = useCallback(
    async (ids: string[]) => {
      const queue = [...ids]
      const sentIds: string[] = []

      const workers = Array.from({ length: Math.min(MAX_PARALLEL, queue.length) }, async () => {
        for (let id = queue.shift(); id !== undefined; id = queue.shift()) {
          try {
            if (await uploadFile(id)) sentIds.push(id)
          } catch (e) {
            update(id, { status: 'error', error: describeError(e) })
            const meta = internals.current.get(id)
            if (meta?.uploadId) void deleteUpload(libraryId, meta.uploadId)
          }
        }
      })
      await Promise.all(workers)

      const uploadIds = sentIds
        .map((id) => internals.current.get(id)?.uploadId)
        .filter((v): v is string => Boolean(v))
      if (uploadIds.length === 0) return

      sentIds.forEach((id) => update(id, { status: 'committing' }))
      try {
        await completeBatch(libraryId, uploadIds)
        sentIds.forEach((id) => update(id, { status: 'done' }))
        // New files must show up immediately in the current (and any cached) dir.
        void qc.invalidateQueries({ queryKey: ['files', libraryId] })
      } catch (e) {
        sentIds.forEach((id) => update(id, { status: 'error', error: describeError(e) }))
      }
    },
    [libraryId, qc, update, uploadFile],
  )

  /** enqueue validates and starts a batch targeting dir (the open directory). */
  const enqueue = useCallback(
    (files: File[], dir: string) => {
      const startable: string[] = []
      const newItems: UploadItem[] = []
      for (const file of files) {
        const id = crypto.randomUUID()
        const invalid = validateFilename(file.name)
        newItems.push({
          id,
          name: file.name,
          dir,
          size: file.size,
          sent: 0,
          status: invalid ? 'error' : 'queued',
          error: invalid ?? undefined,
        })
        if (!invalid) {
          internals.current.set(id, { file, dir, abort: new AbortController() })
          startable.push(id)
        }
      }
      setItems((prev) => [...prev, ...newItems])
      if (startable.length > 0) void runBatch(startable)
    },
    [runBatch],
  )

  /** cancel aborts an in-flight/queued item and deletes its server session. */
  const cancel = useCallback(
    (id: string) => {
      const meta = internals.current.get(id)
      if (!meta) return
      meta.abort.abort()
      update(id, { status: 'cancelled' })
      if (meta.uploadId) void deleteUpload(libraryId, meta.uploadId)
    },
    [libraryId, update],
  )

  /** clearFinished removes done/error/cancelled rows from the panel. */
  const clearFinished = useCallback(() => {
    setItems((prev) =>
      prev.filter((i) => {
        const finished = i.status === 'done' || i.status === 'error' || i.status === 'cancelled'
        if (finished) internals.current.delete(i.id)
        return !finished
      }),
    )
  }, [])

  const active = items.some((i) => ACTIVE.includes(i.status) || i.status === 'committing')
  return { items, active, enqueue, cancel, clearFinished }
}
