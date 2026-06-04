import type { ReactNode } from 'react'
import { Navigate } from 'react-router-dom'
import { useMe } from '../api/queries'
import { FullSpinner } from '../components/Spinner'

// RequireAuth bootstraps the app by calling GET /api/me. While it resolves we
// show a spinner; on failure (no/expired session) we route to /login.
export function RequireAuth({ children }: { children: ReactNode }) {
  const { data, isLoading, isError } = useMe()

  if (isLoading) return <FullSpinner />
  if (isError || !data) return <Navigate to="/login" replace />
  return <>{children}</>
}
