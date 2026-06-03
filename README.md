# Cairn

Content-addressable file storage in the spirit of Seafile. This is **step 1**:
the storage foundation — a content-defined chunker, a block store with two
backends, global dedup, and a refcount. No HTTP, database, frontend, or
encryption yet.

## Packages

| Package            | Responsibility                                                              |
|--------------------|-----------------------------------------------------------------------------|
| `chunker`          | FastCDC content-defined chunking + SHA-256 addressing. Streaming, bounded memory. |
| `blockstore`       | `Hash` type + `Store` interface (blocks + refcount). Badger and FS backends. |
| `ingest`           | Streams a reader through the chunker into the store; returns a manifest + dedup stats. |
| `cmd/cairn-ingest` | CLI that ingests a file or directory and reports dedup ratios.              |

## Quick start

```sh
go build ./...
go test ./...

go run ./cmd/cairn-ingest -backend badger -store ./store ./path/to/folder
go run ./cmd/cairn-ingest -backend fs     -store ./store ./path/to/folder
```

Per file and in total it prints unique/total blocks, logical vs physical-new
bytes, dedup ratio, and elapsed time.

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

### Refcount (temporary)

The store keeps an in-store refcount (`Incr`/`Decr`/`Refs`) for a future GC.
**This is temporary:** in production the authoritative refcount moves to a
Postgres `blocks` table updated transactionally with manifest writes. The
in-store counter is not the source of truth.
