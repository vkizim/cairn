import { useMemo, useRef } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { useVirtualizer } from '@tanstack/react-virtual'
import { useFiles } from '../api/queries'
import { api, ApiError } from '../api/client'
import type { PathEntry } from '../api/types'
import { joinPath, pathToSplat, splatToPath } from '../lib/vpath'
import { formatMtime, humanSize } from '../lib/format'
import { Breadcrumb } from './Breadcrumb'
import { FullSpinner } from '../components/Spinner'
import { ErrorBanner } from '../components/ErrorBanner'
import { EmptyState } from '../components/EmptyState'

const ROW_HEIGHT = 36

export function FileManagerPage() {
  const params = useParams()
  const libraryId = params.id as string
  const path = splatToPath(params['*'])
  const navigate = useNavigate()

  const { data, isLoading, isError, error } = useFiles(libraryId, path)

  // Directories first, then by name.
  const entries = useMemo(() => {
    const list = data ? [...data] : []
    list.sort((a, b) => (a.is_dir === b.is_dir ? a.name.localeCompare(b.name) : a.is_dir ? -1 : 1))
    return list
  }, [data])

  const scrollRef = useRef<HTMLDivElement>(null)
  const virtualizer = useVirtualizer({
    count: entries.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => ROW_HEIGHT,
    overscan: 12,
  })

  const openDir = (childPath: string) =>
    navigate(`/libraries/${libraryId}/files/${pathToSplat(childPath)}`)

  return (
    <div className="flex h-full flex-col gap-3">
      <Breadcrumb libraryId={libraryId} path={path} />

      {isLoading ? (
        <FullSpinner />
      ) : isError ? (
        <ErrorBanner message={error instanceof ApiError ? error.message : 'Failed to load files.'} />
      ) : entries.length === 0 ? (
        <EmptyState title="This folder is empty" />
      ) : (
        <div
          ref={scrollRef}
          className="flex-1 overflow-auto rounded-lg border border-slate-200 bg-white"
        >
          <div style={{ height: virtualizer.getTotalSize(), position: 'relative' }}>
            {virtualizer.getVirtualItems().map((vi) => {
              const entry = entries[vi.index] as PathEntry
              return (
                <Row
                  key={vi.key}
                  entry={entry}
                  offset={vi.start}
                  libraryId={libraryId}
                  dir={path}
                  onOpenDir={openDir}
                />
              )
            })}
          </div>
        </div>
      )}
    </div>
  )
}

function Row({
  entry,
  offset,
  libraryId,
  dir,
  onOpenDir,
}: {
  entry: PathEntry
  offset: number
  libraryId: string
  dir: string
  onOpenDir: (childPath: string) => void
}) {
  const fullPath = joinPath(dir, entry.name)
  // Fixed-height rows: the name MUST stay single-line (truncate) so wrapping
  // never breaks the virtualizer's row height. Full name is in the tooltip.
  const style: React.CSSProperties = {
    position: 'absolute',
    top: 0,
    left: 0,
    right: 0,
    height: ROW_HEIGHT,
    transform: `translateY(${offset}px)`,
  }
  const rowClass =
    'flex items-center gap-3 border-b border-slate-100 px-3 text-sm hover:bg-slate-50 focus:bg-slate-50 focus:outline-none'

  if (entry.is_dir) {
    return (
      <button
        type="button"
        style={style}
        className={`${rowClass} text-left`}
        onClick={() => onOpenDir(fullPath)}
        title={entry.name}
      >
        <FolderIcon />
        <span className="flex-1 truncate font-medium">{entry.name}</span>
        <span className="w-20 shrink-0 text-right text-xs text-slate-300">—</span>
        <span className="hidden w-44 shrink-0 truncate text-right text-xs text-slate-400 sm:block">
          {formatMtime(entry.mtime)}
        </span>
      </button>
    )
  }

  return (
    <a
      style={style}
      className={rowClass}
      href={api.downloadUrl(libraryId, fullPath)}
      download={entry.name}
      title={`Download ${entry.name}`}
    >
      <FileIcon />
      <span className="flex-1 truncate">{entry.name}</span>
      <span className="w-20 shrink-0 text-right text-xs tabular-nums text-slate-500">
        {humanSize(entry.size)}
      </span>
      <span className="hidden w-44 shrink-0 truncate text-right text-xs text-slate-400 sm:block">
        {formatMtime(entry.mtime)}
      </span>
    </a>
  )
}

function FolderIcon() {
  return (
    <svg className="h-4 w-4 shrink-0 text-amber-500" viewBox="0 0 20 20" fill="currentColor" aria-hidden>
      <path d="M2 5a2 2 0 0 1 2-2h4l2 2h6a2 2 0 0 1 2 2v6a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5Z" />
    </svg>
  )
}

function FileIcon() {
  return (
    <svg className="h-4 w-4 shrink-0 text-slate-400" viewBox="0 0 20 20" fill="currentColor" aria-hidden>
      <path d="M5 3a2 2 0 0 0-2 2v10a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8l-5-5H5Z" />
    </svg>
  )
}
