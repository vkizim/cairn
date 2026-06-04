import { Link, useNavigate } from 'react-router-dom'
import type { ReactNode } from 'react'
import { useLogout, useMe } from '../api/queries'
import { Button } from './Button'

export function Layout({ children }: { children: ReactNode }) {
  const navigate = useNavigate()
  const { data: me } = useMe()
  const logout = useLogout()

  return (
    <div className="flex h-full flex-col">
      <header className="flex items-center justify-between border-b border-slate-200 bg-white px-4 py-3">
        <Link to="/libraries" className="flex items-center gap-2 text-lg font-semibold tracking-tight">
          <img src="/logo.png" alt="" className="-my-2 h-12 w-12" />
          Cairn
        </Link>
        <div className="flex items-center gap-3">
          {me && <span className="text-sm text-slate-500">{me.username}</span>}
          <Button
            variant="ghost"
            onClick={() => logout.mutate(undefined, { onSettled: () => navigate('/login') })}
          >
            Log out
          </Button>
        </div>
      </header>
      <main className="mx-auto w-full max-w-5xl flex-1 overflow-hidden p-4">{children}</main>
    </div>
  )
}
