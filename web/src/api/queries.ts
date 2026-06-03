import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from './client'

export function useMe() {
  return useQuery({ queryKey: ['me'], queryFn: api.me })
}

export function useLibraries() {
  return useQuery({ queryKey: ['libraries'], queryFn: api.libraries })
}

export function useFiles(libraryId: string, path: string) {
  return useQuery({
    queryKey: ['files', libraryId, path],
    queryFn: () => api.files(libraryId, path),
  })
}

export function useLogin() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (vars: { username: string; password: string }) =>
      api.login(vars.username, vars.password),
    onSuccess: (user) => qc.setQueryData(['me'], user),
  })
}

export function useLogout() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => api.logout(),
    onSuccess: () => qc.clear(),
  })
}
