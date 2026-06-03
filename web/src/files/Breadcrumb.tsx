import { Link } from 'react-router-dom'
import { breadcrumbs, pathToSplat } from '../lib/vpath'

export function Breadcrumb({ libraryId, path }: { libraryId: string; path: string }) {
  const crumbs = breadcrumbs(path)
  const base = `/libraries/${libraryId}/files/`

  return (
    <nav className="flex flex-wrap items-center gap-1 text-sm text-slate-500" aria-label="Breadcrumb">
      <Link to={base} className="rounded px-1 hover:text-slate-900 hover:underline">
        root
      </Link>
      {crumbs.map((c) => (
        <span key={c.path} className="flex items-center gap-1">
          <span className="text-slate-300">/</span>
          <Link
            to={base + pathToSplat(c.path)}
            className="max-w-[16rem] truncate rounded px-1 hover:text-slate-900 hover:underline"
            title={c.name}
          >
            {c.name}
          </Link>
        </span>
      ))}
    </nav>
  )
}
