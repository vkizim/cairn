// Package blockstore defines the content-addressable block storage layer for
// Cairn: a set of blocks keyed by the SHA-256 of their contents, with global
// dedup (a block is stored at most once) and a temporary refcount.
//
// Two implementations live in this package: BadgerStore (the default, a single
// embedded key/value directory) and FSStore (a plain sharded directory tree,
// useful for development and inspection). Both satisfy the Store interface.
package blockstore

import "errors"

// DefaultNamespace is the only namespace used today. The namespace parameter is
// plumbed through every method to reserve a future per-library mode (e.g. one
// namespace per encrypted library, so dedup never crosses an encryption
// boundary). Until that exists, callers pass DefaultNamespace everywhere.
const DefaultNamespace = "global"

// ErrNotFound is returned by Get when no block exists for the given hash.
var ErrNotFound = errors.New("blockstore: block not found")

// BlockStore is the core content-addressable store: blocks keyed by their hash.
type BlockStore interface {
	// Put stores data under its hash. It is idempotent and content-addressed:
	// if a block with the same hash already exists it is NOT rewritten, and
	// isNew reports false. The caller is responsible for passing data whose
	// hash actually equals h (Ingest always does via the chunker).
	//
	// data is only read during the call; the store does not retain the slice.
	Put(namespace string, h Hash, data []byte) (isNew bool, err error)

	// Get returns the bytes of the block addressed by h, or ErrNotFound.
	Get(namespace string, h Hash) ([]byte, error)

	// Exists reports whether a block with hash h is present.
	Exists(namespace string, h Hash) (bool, error)

	// Delete removes the block addressed by h. Deleting a missing block is not
	// an error (idempotent). Delete does NOT consult the refcount; callers that
	// care about safe reclamation should check Refs first (see GC, future work).
	Delete(namespace string, h Hash) error

	// Iterate calls fn once per stored block hash, in unspecified order. If fn
	// returns an error, iteration stops and that error is returned. Intended
	// for a future garbage collector.
	Iterate(namespace string, fn func(Hash) error) error
}

// RefStore tracks how many manifests reference each block.
//
// TEMPORARY: in production the authoritative refcount will live in a Postgres
// `blocks` table (a row per (namespace, hash) with a ref count column), updated
// transactionally alongside manifest writes. The in-store counter here exists
// only so the demo and a future GC have something to consult; it is not the
// source of truth and should not be relied on once the database lands.
type RefStore interface {
	// Incr increases the refcount for h by one and returns the new value.
	// Incrementing happens on every reference, including dedup hits (a block
	// that already exists still gains a reference).
	Incr(namespace string, h Hash) (uint64, error)

	// Decr decreases the refcount for h by one and returns the new value. It
	// never goes below zero. Reaching zero makes the block eligible for GC but
	// does NOT delete it.
	Decr(namespace string, h Hash) (uint64, error)

	// Refs returns the current refcount for h (zero if absent).
	Refs(namespace string, h Hash) (uint64, error)
}

// Store is the full interface used by the rest of Cairn: a block store with a
// refcount, that can be closed. Both BadgerStore and FSStore satisfy it.
type Store interface {
	BlockStore
	RefStore

	// Close releases any resources (file handles, the Badger LSM tree, etc.).
	Close() error
}
