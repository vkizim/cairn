import { Link } from 'react-router-dom'
import { useLibraries } from '../api/queries'
import { ApiError } from '../api/client'
import { FullSpinner } from '../components/Spinner'
import { ErrorBanner } from '../components/ErrorBanner'
import { EmptyState } from '../components/EmptyState'

export function LibrariesPage() {
  const { data, isLoading, isError, error } = useLibraries()

  if (isLoading) return <FullSpinner />
  if (isError) {
    return <ErrorBanner message={error instanceof ApiError ? error.message : 'Failed to load libraries.'} />
  }

  const libraries = data ?? []
  return (
    <div className="space-y-4">
      <h1 className="text-lg font-semibold">Libraries</h1>
      {libraries.length === 0 ? (
        <EmptyState title="No libraries yet" hint="Libraries you own will appear here." />
      ) : (
        <ul className="divide-y divide-slate-200 overflow-hidden rounded-lg border border-slate-200 bg-white">
          {libraries.map((lib) => (
            <li key={lib.id}>
              <Link
                to={`/libraries/${lib.id}/files/`}
                className="flex items-center justify-between px-4 py-3 hover:bg-slate-50 focus:bg-slate-50 focus:outline-none"
              >
                <span className="font-medium">{lib.name}</span>
                <span className="text-xs text-slate-400">
                  {new Date(lib.created_at).toLocaleDateString()}
                </span>
              </Link>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}
