// Wire types mirroring the step-3 api package response shapes exactly.

export interface User {
  id: string
  username: string
}

export interface Library {
  id: string
  name: string
  owner_id: string
  head_commit: string | null
  encrypted: boolean
  created_at: string
}

export interface PathEntry {
  name: string
  is_dir: boolean
  size: number
  mtime: string | null
}
