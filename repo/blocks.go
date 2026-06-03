package repo

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/vkizim/cairn/blockstore"
)

// Authoritative block refcount, owned by Postgres (the step-1 in-store refcount
// is deprecated). refcount counts distinct blocks once per referencing commit.
// It is incremented in the object transaction (tx1) and may therefore briefly
// include orphan commits (those that crash before the head advances in tx2);
// GarbageCollect reconciles. This over-counts orphans rather than under-counts
// reachable blocks — the safe direction.

// incrBlockRefs inserts/updates one row per distinct block, adding a single
// reference. blocks maps block hash -> size.
func incrBlockRefs(ctx context.Context, tx pgx.Tx, blocks map[blockstore.Hash]int64) error {
	batch := &pgx.Batch{}
	for h, size := range blocks {
		batch.Queue(
			`INSERT INTO blocks (block_hash, size, refcount) VALUES ($1, $2, 1)
			 ON CONFLICT (block_hash) DO UPDATE SET refcount = blocks.refcount + 1`,
			h.String(), size)
	}
	if batch.Len() == 0 {
		return nil
	}
	br := tx.SendBatch(ctx, batch)
	if err := br.Close(); err != nil {
		return fmt.Errorf("repo: increment block refs: %w", err)
	}
	return nil
}

// decrBlockRefs removes a single reference from each distinct block, clamped at
// zero (a block reaching zero is eligible for GC deletion).
func decrBlockRefs(ctx context.Context, tx pgx.Tx, hashes []blockstore.Hash) error {
	batch := &pgx.Batch{}
	for _, h := range hashes {
		batch.Queue(
			`UPDATE blocks SET refcount = GREATEST(refcount - 1, 0) WHERE block_hash = $1`,
			h.String())
	}
	if batch.Len() == 0 {
		return nil
	}
	br := tx.SendBatch(ctx, batch)
	if err := br.Close(); err != nil {
		return fmt.Errorf("repo: decrement block refs: %w", err)
	}
	return nil
}

// BlockRefcount returns the current refcount of a block (0 if absent). Exposed
// for diagnostics and tests.
func (db *DB) BlockRefcount(ctx context.Context, h blockstore.Hash) (int64, error) {
	var n int64
	err := db.pool.QueryRow(ctx, `SELECT refcount FROM blocks WHERE block_hash = $1`, h.String()).Scan(&n)
	if err == pgx.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("repo: block refcount %s: %w", h, err)
	}
	return n, nil
}

// collectCommitBlocks walks a commit's tree (via q) and returns the distinct
// blocks it references, mapped to size.
func (db *DB) collectCommitBlocks(ctx context.Context, q querier, rootTree blockstore.Hash) (map[blockstore.Hash]int64, error) {
	blocks := map[blockstore.Hash]int64{}
	err := walkTree(ctx, q, rootTree, func(_ string, e TreeEntry) error {
		if e.Type != ObjTypeFile {
			return nil
		}
		_, content, err := loadObject(ctx, q, e.Hash)
		if err != nil {
			return err
		}
		fo, err := decodeFileObject(content)
		if err != nil {
			return err
		}
		for _, b := range fo.Blocks {
			blocks[b.Hash] = b.Size
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return blocks, nil
}
