import { useRef, useState } from 'react'
import type { DragEvent, ReactNode } from 'react'

// DropZone wraps the file listing: dragging files over it shows an overlay and
// dropping hands them to onFiles. A depth counter keeps the highlight stable
// across child enter/leave churn.
export function DropZone({
  label,
  onFiles,
  children,
}: {
  label: string
  onFiles: (files: File[]) => void
  children: ReactNode
}) {
  const [over, setOver] = useState(false)
  const depth = useRef(0)

  const enter = (e: DragEvent) => {
    e.preventDefault()
    depth.current++
    if (e.dataTransfer.types.includes('Files')) setOver(true)
  }
  const leave = (e: DragEvent) => {
    e.preventDefault()
    depth.current = Math.max(0, depth.current - 1)
    if (depth.current === 0) setOver(false)
  }
  const drop = (e: DragEvent) => {
    e.preventDefault()
    depth.current = 0
    setOver(false)
    const files = Array.from(e.dataTransfer.files)
    if (files.length > 0) onFiles(files)
  }

  return (
    <div
      className="relative flex min-h-0 flex-1 flex-col"
      onDragEnter={enter}
      onDragLeave={leave}
      onDragOver={(e) => e.preventDefault()}
      onDrop={drop}
    >
      {children}
      {over && (
        <div className="pointer-events-none absolute inset-0 z-10 flex items-center justify-center rounded-lg border-2 border-dashed border-slate-500 bg-slate-100/80">
          <p className="text-sm font-medium text-slate-700">{label}</p>
        </div>
      )}
    </div>
  )
}
