// Package repo is Cairn's Git-like FS engine: file/tree/commit objects and a
// derived path index in Postgres, layered on the step-1 content-addressed block
// store. Blocks (large, content-addressed bytes) stay in the block store; the
// object graph and the authoritative block refcount live in Postgres.
//
// Fault tolerance is designed in, not optional:
//
//   - Rule A (write order): blocks are written first (by ingest), then file/tree
//     objects and the commit row, and ONLY THEN does the library head advance.
//     Two foreign keys make a violation impossible at the database level:
//     commits.root_tree_hash -> fs_objects, and libraries.head_commit_hash ->
//     commits. The worst reachable partial failure is orphan blocks/objects/
//     commits, which GC reclaims — never a head pointing at a missing commit.
//   - Rule B (atomicity): advancing the head and rebuilding that library's
//     path_index happen in a single transaction (publishCommit), with a
//     compare-and-swap on the head to reject concurrent races.
//   - Rule C (self-verification): objects are keyed by sha256 of their canonical
//     bytes; Fsck recomputes and checks them, validates reachable blocks, and can
//     roll the head back to the last fully consistent commit.
package repo

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vkizim/cairn/blockstore"
)

// DB is the FS engine handle: a Postgres pool plus the block store the objects
// reference.
type DB struct {
	pool  *pgxpool.Pool
	store blockstore.Store
	ns    string

	// now returns the commit timestamp. It is a field so tests can pin time for
	// deterministic commit hashes; production uses time.Now.
	now func() time.Time
}

// Open connects to Postgres at url, applies migrations, and returns a DB bound to
// the given block store. The block store and pool are owned by the caller for
// the store and by DB for the pool (closed via Close).
func Open(ctx context.Context, url string, store blockstore.Store) (*DB, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("repo: connect %q: %w", url, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("repo: ping: %w", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return fromPool(pool, store), nil
}

// fromPool builds a DB around an already-open pool (used by Open and by tests
// that manage their own schema-isolated pool).
func fromPool(pool *pgxpool.Pool, store blockstore.Store) *DB {
	return &DB{
		pool:  pool,
		store: store,
		ns:    blockstore.DefaultNamespace,
		now:   func() time.Time { return time.Now() },
	}
}

// Close closes the underlying connection pool. It does not close the block store.
func (db *DB) Close() { db.pool.Close() }

// inTx runs fn inside a transaction, committing on success and rolling back on
// error (or panic). The deferred Rollback after a successful Commit is a no-op.
func (db *DB) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("repo: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("repo: commit tx: %w", err)
	}
	return nil
}
