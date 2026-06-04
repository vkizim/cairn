import type { UploadItem, UploadStatus } from './useUploadQueue'
import { humanSize } from '../lib/format'

const statusLabel: Record<UploadStatus, string> = {
  queued: 'Queued',
  uploading: 'Uploading…',
  resuming: 'Reconnecting…',
  sent: 'Sent',
  committing: 'Committing…',
  done: 'Done',
  error: 'Failed',
  cancelled: 'Cancelled',
}

const cancellable: UploadStatus[] = ['queued', 'uploading', 'resuming']

// UploadPanel is the floating queue: one row per file with a progress bar fed
// only by server-acknowledged bytes, a status, and a cancel control while the
// file is still in flight.
export function UploadPanel({
  items,
  onCancel,
  onClear,
}: {
  items: UploadItem[]
  onCancel: (id: string) => void
  onClear: () => void
}) {
  if (items.length === 0) return null

  const finished = items.filter(
    (i) => i.status === 'done' || i.status === 'error' || i.status === 'cancelled',
  ).length

  return (
    <div className="fixed bottom-4 right-4 z-20 w-96 max-w-[calc(100vw-2rem)] rounded-xl border border-slate-200 bg-white shadow-lg">
      <div className="flex items-center justify-between border-b border-slate-100 px-3 py-2">
        <span className="text-sm font-semibold">
          Uploads ({finished}/{items.length})
        </span>
        {finished > 0 && (
          <button
            type="button"
            onClick={onClear}
            className="rounded px-2 py-0.5 text-xs text-slate-500 hover:bg-slate-100 focus:outline-none focus-visible:ring-2 focus-visible:ring-slate-400"
          >
            Clear finished
          </button>
        )}
      </div>

      <ul className="max-h-72 overflow-auto p-2">
        {items.map((item) => (
          <li key={item.id} className="rounded-lg px-2 py-1.5 hover:bg-slate-50">
            <div className="flex items-center gap-2">
              {/* fixed-height single-line name; full name in the tooltip */}
              <span className="min-w-0 flex-1 truncate text-sm" title={item.name}>
                {item.name}
              </span>
              <span
                className={`shrink-0 text-xs ${
                  item.status === 'error'
                    ? 'text-red-600'
                    : item.status === 'done'
                      ? 'text-emerald-600'
                      : 'text-slate-500'
                }`}
              >
                {statusLabel[item.status]}
              </span>
              {cancellable.includes(item.status) && (
                <button
                  type="button"
                  aria-label={`Cancel upload of ${item.name}`}
                  onClick={() => onCancel(item.id)}
                  className="shrink-0 rounded px-1 text-slate-400 hover:bg-slate-200 hover:text-slate-700 focus:outline-none focus-visible:ring-2 focus-visible:ring-slate-400"
                >
                  ✕
                </button>
              )}
            </div>

            <div className="mt-1 flex items-center gap-2">
              <Progress item={item} />
              <span className="w-20 shrink-0 text-right text-[11px] tabular-nums text-slate-400">
                {humanSize(item.sent)} / {humanSize(item.size)}
              </span>
            </div>

            {item.error && <p className="mt-0.5 text-xs text-red-600">{item.error}</p>}
          </li>
        ))}
      </ul>
    </div>
  )
}

function Progress({ item }: { item: UploadItem }) {
  const pct =
    item.status === 'done'
      ? 100
      : item.size === 0
        ? item.status === 'sent' || item.status === 'committing'
          ? 100
          : 0
        : Math.floor((item.sent / item.size) * 100)
  const color =
    item.status === 'error'
      ? 'bg-red-400'
      : item.status === 'done'
        ? 'bg-emerald-500'
        : 'bg-slate-600'
  return (
    <div
      className="h-1.5 flex-1 overflow-hidden rounded-full bg-slate-100"
      role="progressbar"
      aria-valuenow={pct}
      aria-valuemin={0}
      aria-valuemax={100}
    >
      <div className={`h-full ${color} transition-[width]`} style={{ width: `${pct}%` }} />
    </div>
  )
}
