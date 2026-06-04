import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useLogin } from '../api/queries'
import { ApiError } from '../api/client'
import { Button } from '../components/Button'
import { ErrorBanner } from '../components/ErrorBanner'

export function LoginPage() {
  const navigate = useNavigate()
  const login = useLogin()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')

  const onSubmit = (e: React.FormEvent) => {
    e.preventDefault()
    login.mutate(
      { username, password },
      { onSuccess: () => navigate('/libraries', { replace: true }) },
    )
  }

  // Never leak detail: any failure shows the same generic message.
  const errorMessage =
    login.isError &&
    (login.error instanceof ApiError && login.error.status === 401
      ? 'Invalid username or password.'
      : 'Could not sign in. Please try again.')

  return (
    <div className="flex h-full items-center justify-center p-6">
      <form
        onSubmit={onSubmit}
        className="w-full max-w-sm space-y-4 rounded-xl border border-slate-200 bg-white p-6 shadow-sm"
      >
        <div className="flex justify-center">
          <img src="/logo.png" alt="Cairn" className="h-28 w-28 rounded-2xl shadow-md" />
        </div>
        <h1 className="text-center text-xl font-semibold">Sign in to Cairn</h1>

        <label className="block space-y-1">
          <span className="text-sm text-slate-600">Username</span>
          <input
            className="w-full rounded-md border border-slate-300 px-3 py-2 text-sm focus:border-slate-500 focus:outline-none"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            autoComplete="username"
            autoFocus
            required
          />
        </label>

        <label className="block space-y-1">
          <span className="text-sm text-slate-600">Password</span>
          <input
            type="password"
            className="w-full rounded-md border border-slate-300 px-3 py-2 text-sm focus:border-slate-500 focus:outline-none"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="current-password"
            required
          />
        </label>

        {errorMessage && <ErrorBanner message={errorMessage} />}

        <Button type="submit" className="w-full" disabled={login.isPending}>
          {login.isPending ? 'Signing in…' : 'Sign in'}
        </Button>
      </form>
    </div>
  )
}
