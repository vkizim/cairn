# Cairn

Content-addressable file storage in the spirit of Seafile.

- **Step 1** — the storage foundation: a content-defined chunker, a block store
  with two backends, and global dedup.
- **Step 2** — the Git-like FS engine: immutable file/tree/commit objects plus a
  derived path index in Postgres, layered on the step-1 block store, with fault
  tolerance designed in. The block store is now a dumb byte store; the
  authoritative refcount lives in Postgres.
- **Step 3** — the HTTP API: cookie-session auth (argon2id), CSRF protection, an
  owner-only access seam, REST/JSON metadata endpoints, file download with HTTP
  Range, and a resumable upload protocol — served by `cmd/cairn-server`.
- **Step 4a** — the web UI shell (`web/`): a Vite + React + TypeScript SPA
  (login → libraries → virtualized file manager → download), embedded into the
  binary. Upload and history/rollback come in 4b.

No encryption yet.

## Packages

| Package            | Responsibility                                                              |
|--------------------|-----------------------------------------------------------------------------|
| `chunker`          | FastCDC content-defined chunking + SHA-256 addressing. Streaming, bounded memory. Pure Go / WASM-clean. |
| `blockstore`       | `Hash` type + `Store` interface (blocks). Badger and FS backends. (In-store refcount deprecated — see below.) |
| `ingest`           | Streams a reader through the chunker into the store. `Build` (refcount-free) feeds the FS engine; `Ingest` is the step-1 demo path. |
| `repo`             | FS engine: libraries, users, sessions, uploads, file/tree/commit objects, commits, path index, fsck/GC, file reads — all in Postgres on top of the block store. |
| `api`              | HTTP layer: session auth, CSRF, access middleware, library/file/upload endpoints (stdlib `net/http`). |
| `cmd/cairn-ingest` | Step-1 CLI: ingest a file/dir and report dedup ratios.                     |
| `cmd/cairn-fs`     | Step-2 demo CLI: create libraries, commit, list, log, fsck.                |
| `cmd/cairn-server` | Step-3 server: `serve`, `create-user`, `sweep-uploads`; serves the embedded SPA. |
| `web/`             | Step-4a SPA (Vite + React + TS); nested Go module that embeds `web/dist`.   |

## Quick start (step 1 — no database)

```sh
go build ./...

go run ./cmd/cairn-ingest -backend badger -store ./store ./path/to/folder
go run ./cmd/cairn-ingest -backend fs     -store ./store ./path/to/folder
```

Per file and in total it prints unique/total blocks, logical vs physical-new
bytes, dedup ratio, and elapsed time.

## Step 2 — the FS engine (needs Postgres)

### Configure the database

The `repo` package and `cmd/cairn-fs` connect to Postgres via
`CAIRN_TEST_DATABASE_URL` (or the `-db` flag for the CLI). A local `.env` in the
repo root is auto-loaded (and is gitignored — keep credentials out of git):

```sh
# .env
CAIRN_TEST_DATABASE_URL=postgres://cairn:cairn@localhost:5552/cairn_test?sslmode=disable
```

### Demo CLI

```sh
go build -o cairn-fs ./cmd/cairn-fs

LIB=$(./cairn-fs -backend fs -store ./store create-library photos alice)
./cairn-fs -backend fs -store ./store commit "$LIB" ./path/to/folder   # ingest + new commit
./cairn-fs -backend fs -store ./store ls   "$LIB"            # list root from path_index
./cairn-fs -backend fs -store ./store ls   "$LIB" /sub       # list a subdirectory
./cairn-fs -backend fs -store ./store log  "$LIB"            # commit history (newest first)
./cairn-fs -backend fs -store ./store fsck "$LIB"            # verify + report
```

A second `commit` of an overlapping fileset reuses blocks (physical-new bytes
stay small) and chains onto the previous commit.

Only `commit` opens the block store (`-store`/`-backend` matter just for it);
`create-library`, `ls`, `log`, and `fsck` are metadata-only and talk solely to
Postgres. For `commit`, `cairn-fs` opens Badger with the directory lock guard
bypassed — fine for a single-user CLI, and it sidesteps Windows LOCK conflicts
from interrupted `go run` children — but do **not** point `commit` at a store
directory a running `cairn-server` is actively using (the server keeps the lock
guard precisely to prevent two writers).

### Running the tests

The step-1 packages need no database. The `repo` tests pick their Postgres in
this order:

1. **`CAIRN_TEST_DATABASE_URL`** set (or in `.env`) — used directly; each test
   runs in its own isolated, freshly-migrated schema, dropped on cleanup.
2. Otherwise, if **Docker** is available, a throwaway Postgres is started via
   testcontainers.
3. Otherwise the `repo` tests **skip** with a message (no hard failure).

```sh
go test ./...                       # all packages; repo tests use .env / env var, else skip
CAIRN_TEST_DATABASE_URL=postgres://cairn:cairn@localhost:5552/cairn_test?sslmode=disable go test ./repo/
```

## Design notes

### Chunker: FastCDC via `PlakarKorp/go-cdc-chunkers`

- Algorithm pinned to **`fastcdc-v1.0.0`** with **Min 2 MiB / Normal (avg) 8 MiB
  / Max 16 MiB**, normalization level 2 (NC=2, hardcoded by the library).
- We deliberately do **not** use the library's bare `"fastcdc"` name: it selects
  the *legacy* variant whose `Setup()` hardcodes masks for an 8 KiB `NormalSize`
  and silently ignores ours, collapsing every chunk to ~`MinSize`. The
  version-suffixed name derives masks from our `NormalSize` and pins the
  derivation so future library changes ship under a new name.
- Chunking is **deterministic** (fixed gear table, no key) — identical input
  yields identical boundaries and hashes across runs and machines. This is the
  basis of stable block addressing and is enforced by `TestChunkerGolden`
  (a golden/reference-vector contract) and `TestSplitDeterminism`.

### Browser / WASM portability (forward-looking)

The `chunker` package imports only the chunker library, the standard library,
and `blockstore.Hash` — no cgo, no server-only dependency. It compiles to WASM
unchanged:

```sh
CGO_ENABLED=0 GOOS=js GOARCH=wasm go build ./chunker/
```

Rationale: a future encrypted-library mode will chunk **in the browser** so
plaintext never reaches the server. Client and server must produce identical
chunk boundaries, which is only safe if both run the same implementation (this
Go code, compiled to WASM). The golden vectors are the cross-environment
contract. No WASM/browser code exists yet — this only keeps the door open.

### Block store

- `Hash` is a defined type `[32]byte` (not an alias), so it can't be confused
  with an arbitrary byte array.
- `Put` is idempotent and content-addressed: an existing block is never
  rewritten (`isNew` reports false), giving server-side global dedup.
- **BadgerStore** (default): single embedded KV directory, keys
  `<ns>/b/<hash>` and `<ns>/r/<hash>`.
- **FSStore** (dev/fallback): sharded `ab/cd/<hash>` paths, atomic writes
  (temp file → fsync → rename), inspectable with `ls`.
- A **`namespace`** parameter is plumbed through every method (always `"global"`
  today) to reserve a future per-library mode for encrypted libraries.

### Refcount

The block store still has an in-store refcount (`Incr`/`Decr`/`Refs`), but as of
step 2 it is **deprecated and no longer authoritative**: the `blocks` table in
Postgres owns the refcount, updated transactionally with each commit. The FS
engine writes blocks via `ingest.Build`, which does not touch the in-store
counter. `cmd/cairn-ingest` keeps using the legacy `ingest.Ingest` for its
standalone demo.

## Step-2 design notes

### Storage split

- **Blocks** (large, content-addressed bytes) stay in the block store.
- **Objects** (file/tree/commit) live in Postgres, addressed by the SHA-256 of
  their canonical serialized form, **immutable** (insert-only). File and tree
  objects dedup globally by hash. The block store stays a dumb byte store.
- **`path_index`** is a *derived cache*, never the source of truth — the commit
  chain is. It is rebuilt from a commit's tree (`RebuildPathIndex`), and the
  per-commit refresh is isolated behind `updatePathIndex` so a future
  incremental diff-apply can replace the full rebuild without touching the
  commit transaction boundary.

### Canonical object encoding

Objects are stored as the exact canonical bytes their hash is computed over
(`BYTEA`, not `JSONB` — Postgres would normalize `JSONB` and break the hash).
The encoding (JSON over fixed structs, sorted tree entries, hex hashes) is a
**permanent addressing contract**, pinned by `TestCanonicalGolden` exactly like
the chunker's golden chunk boundaries.

### Fault tolerance

- **Rule A — write order.** Blocks are written first (by ingest), then file/tree
  objects and the commit row, and *only then* does the library head advance. Two
  foreign keys make a violation impossible at the database level:
  `commits.root_tree_hash → fs_objects` and `libraries.head_commit_hash →
  commits`. The worst reachable partial failure is orphan blocks/objects/commits
  (reclaimed by GC) — never a head pointing at a missing commit.
- **Rule B — atomicity.** Advancing the head and rebuilding that library's
  `path_index` happen in a single transaction, with a compare-and-swap on the
  head to reject concurrent races.
- **Rule C — self-verification.** Objects are keyed by `sha256` of their
  canonical bytes; `Fsck` recomputes and checks them, validates that every
  reachable block is present, and locates the last fully-consistent commit.
  `Recover` rolls the head back to it and rebuilds `path_index`.

`GarbageCollect` reclaims commits not on the head's ancestor chain (e.g. orphans
from a crash between the two transactions, or commits abandoned by a rollback),
decrementing their blocks' refcounts and deleting blocks that reach zero. The
refcount may briefly over-count orphans — the safe direction, since a reachable
block is never under-counted.

### Schema

`libraries`, `blocks` (authoritative refcount), `fs_objects` (tree/file,
`BYTEA` content), `commits`, `path_index` (derived). Migrations are embedded SQL
applied by a tiny dependency-light runner (`repo/migrations/*.sql`). The driver
is pgx native (`pgxpool`); we target Postgres only this step.

## Step 3 — the HTTP API server

`cmd/cairn-server` exposes the engine over HTTP (Go 1.22 stdlib `net/http`
ServeMux; no web framework). It runs migrations (incl. `0002_auth.sql`) on
startup.

### Run it

```sh
go build -o cairn-server ./cmd/cairn-server

# Create the first user (password from CAIRN_NEW_USER_PASSWORD or the terminal;
# never logged). There is no open signup endpoint.
CAIRN_NEW_USER_PASSWORD=... ./cairn-server create-user alice

# Serve (CAIRN_DEV relaxes the Secure cookie for http://localhost).
CAIRN_DEV=1 ./cairn-server -addr :8080 -store ./cairn-store -backend badger serve

# Periodically reclaim expired sessions + stale upload temp files (cron/timer).
./cairn-server sweep-uploads
```

Config via flags or env: `-addr`/`CAIRN_LISTEN_ADDR`, `-db`/`CAIRN_DATABASE_URL`
(falls back to `CAIRN_TEST_DATABASE_URL`), `-store`/`CAIRN_STORE`,
`-backend`/`CAIRN_STORE_BACKEND`, `CAIRN_DEV`. See `.env.example`.

### Endpoints

| Method | Path | Notes |
|---|---|---|
| POST | `/api/login` | `{username,password}` → session + CSRF cookies |
| POST | `/api/logout` | clears session |
| GET | `/api/me` | current user |
| GET | `/api/libraries` | current user's libraries |
| POST | `/api/libraries` | `{name}` → create (owned by you) |
| GET | `/api/libraries/{id}` | library metadata |
| GET | `/api/libraries/{id}/files?path=/dir` | directory listing (path_index) |
| GET | `/api/libraries/{id}/files/download?path=/a.pdf` | stream file; supports HTTP **Range** (206) |
| GET | `/api/libraries/{id}/commits` | commit history |
| GET | `/api/libraries/{id}/fsck` | verify report |
| POST | `/api/libraries/{id}/uploads` | `{path,filename,size?}` → start resumable upload |
| HEAD | `/api/libraries/{id}/uploads/{uploadId}` | current `Upload-Offset` |
| PATCH | `/api/libraries/{id}/uploads/{uploadId}` | append chunk at `Upload-Offset` |
| POST | `/api/libraries/{id}/uploads/{uploadId}/complete` | chunk + commit; returns commit hash + stats |
| DELETE | `/api/libraries/{id}/uploads/{uploadId}` | abort |

### Auth, CSRF, access

- **Sessions:** argon2id password hashes; on login the server stores only
  `sha256(token)` and sets an `HttpOnly; SameSite=Lax; Secure` session cookie
  (Secure relaxed only under `CAIRN_DEV`). Expired sessions are rejected.
- **CSRF:** double-submit — a readable `cairn_csrf` cookie plus a required
  `X-CSRF-Token` header on every mutating request (POST/PATCH/DELETE). GETs are
  exempt.
- **Access:** every `{id}` route funnels through the single
  `checkLibraryAccess(user, lib, perm)` seam (currently owner-only). This is the
  growth point for sharing/ACLs — endpoints never check ownership themselves. A
  library you don't own returns **404** (existence is not revealed); `403` is
  reserved for the future "visible but not writable" case.

### Resumable uploads (protocol)

Create → returns `upload_id`. `PATCH` each chunk with an `Upload-Offset` header;
the server validates `offset == received_bytes` under a per-session row lock
(`SELECT … FOR UPDATE`), so concurrent chunks can't race or corrupt — the loser
gets **409** with the authoritative `Upload-Offset` to resume from. `HEAD`
returns the current offset for resuming after an interruption. `complete` runs
the assembled temp file through the CDC chunker and commits it (one upload = one
file = one commit today; `repo.CommitFiles` takes a set, so batching is a clean
future extension). Limits: per-chunk `http.MaxBytesReader` cap and a total
`CAIRN_MAX_UPLOAD_BYTES`-style cap (→ **413**); virtual paths/filenames are
validated through one `repo.CleanLibraryPath`/`ValidateFilename` (rejects
traversal, separators, control chars, over-long names).

### Notes / TODO

- Login **rate-limiting / brute-force protection** is out of scope for this step
  (tracked as a TODO in `auth.go`).
- The `server.go` static fall-through now serves the embedded SPA (step 4a).

## Step 4a — the web UI (`web/`)

A Vite + React + TypeScript SPA: **login → libraries → file manager (virtualized
directory listing) → download**. (Upload and history/rollback are step 4b.)
Stack: TanStack Query + TanStack Virtual, Tailwind v4, react-router, hand-rolled
components.

### Dev (hot reload, two processes)

```sh
# 1) backend with dev cookies (Secure relaxed for http://localhost)
CAIRN_DEV=1 go run ./cmd/cairn-server -addr :8080

# 2) Vite dev server on :5173, proxying /api → :8080 (same-origin cookies)
cd web && npm install && npm run dev
```
Open http://localhost:5173. The proxy keeps the session/CSRF cookies same-origin.

### Prod (embed into the binary)

```sh
cd web && npm install && npm run build      # → web/dist
cd .. && go build -tags embed_spa -o cairn-server ./cmd/cairn-server
CAIRN_DEV=1 ./cairn-server -addr :8080       # serves SPA + API on one origin
```
Open http://localhost:8080. Unknown non-`/api` paths fall back to `index.html`
so client-side routing survives refresh and deep links.

### How it's wired

- **Embed via build tag.** `web/embed_spa.go` (`//go:build embed_spa`) embeds
  `web/dist`; `web/embed_nospa.go` is the default no-op, so plain `go build ./...`
  / `go test ./...` compile **without** needing `web/dist`. Only
  `go build -tags embed_spa` embeds the SPA. `cmd/cairn-server` wires
  `web.FS()` into `api.Config.Static` via `api.SPAFileServer`.
- **`web/` is a nested Go module** (`web/go.mod`, resolved by a `replace` in the
  root `go.mod`) so the root module's `./...` never descends into
  `web/node_modules`.
- **API client** (`web/src/api/client.ts`) sends cookies (`credentials: include`)
  and attaches the `X-CSRF-Token` header (read from the `cairn_csrf` cookie) on
  every mutating request — centralized so no call site forgets. A 401 clears
  caches and routes to login. The session token is never read by JS.
- **Paths** (`web/src/lib/vpath.ts`) are encoded/decoded **per segment** so names
  with spaces, Cyrillic, or `# ? % &` round-trip losslessly between the route
  splat, breadcrumb, and the API `?path=` (covered by `vpath.test.ts`).

### Frontend tooling

TypeScript strict; `npm run lint` (eslint flat config), `npm run format`
(prettier), `npm run test` (vitest). `web/node_modules` and `web/dist` are
gitignored — build locally or in CI; `dist` is not committed.
