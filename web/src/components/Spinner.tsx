export function Spinner({ label }: { label?: string }) {
  return (
    <div className="flex items-center gap-2 text-slate-500" role="status" aria-live="polite">
      <span className="h-4 w-4 animate-spin rounded-full border-2 border-slate-300 border-t-slate-600" />
      {label && <span className="text-sm">{label}</span>}
    </div>
  )
}

export function FullSpinner({ label = 'Loading…' }: { label?: string }) {
  return (
    <div className="flex h-full items-center justify-center p-12">
      <Spinner label={label} />
    </div>
  )
}
