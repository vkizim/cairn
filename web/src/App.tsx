import { useEffect } from 'react'
import { Navigate, Route, Routes, useNavigate } from 'react-router-dom'
import { useQueryClient } from '@tanstack/react-query'
import { setUnauthorizedHandler } from './api/client'
import { RequireAuth } from './auth/RequireAuth'
import { LoginPage } from './auth/LoginPage'
import { Layout } from './components/Layout'
import { LibrariesPage } from './libraries/LibrariesPage'
import { FileManagerPage } from './files/FileManagerPage'

export default function App() {
  const navigate = useNavigate()
  const qc = useQueryClient()

  // Any API 401 clears caches and routes to login — one place, every screen.
  useEffect(() => {
    setUnauthorizedHandler(() => {
      qc.clear()
      navigate('/login', { replace: true })
    })
    return () => setUnauthorizedHandler(() => {})
  }, [navigate, qc])

  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route
        path="/libraries"
        element={
          <RequireAuth>
            <Layout>
              <LibrariesPage />
            </Layout>
          </RequireAuth>
        }
      />
      <Route
        path="/libraries/:id/files/*"
        element={
          <RequireAuth>
            <Layout>
              <FileManagerPage />
            </Layout>
          </RequireAuth>
        }
      />
      <Route path="/" element={<Navigate to="/libraries" replace />} />
      <Route path="*" element={<Navigate to="/libraries" replace />} />
    </Routes>
  )
}
