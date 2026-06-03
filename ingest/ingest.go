// Package ingest ties the chunker and blockstore together: it streams a reader
// through the chunker, stores each block (with global dedup), bumps refcounts,
// and produces a manifest plus dedup statistics.
package ingest

import (
	"io"
	"math"

	"github.com/vkizim/cairn/blockstore"
	"github.com/vkizim/cairn/chunker"
)

// BlockRef is one entry in a manifest: a block's address and its size.
type BlockRef struct {
	Hash blockstore.Hash
	Size uint
}

// Manifest is the ordered recipe to reconstruct an ingested stream: its name,
// total logical size, and the blocks in stream order. (In a later step this
// will be persisted; for now it is returned to the caller.)
type Manifest struct {
	Name      string
	TotalSize uint64
	Blocks    []BlockRef
}

// Stats summarizes what an ingest did, for reporting dedup effectiveness.
type Stats struct {
	// LogicalBytes is the sum of all chunk sizes (the stream's real size).
	LogicalBytes uint64
	// PhysicalNewBytes is the bytes actually written to the store for the first
	// time (chunks whose Put reported isNew). Bytes deduped against an existing
	// block — whether from this stream or a previous one — are excluded.
	PhysicalNewBytes uint64
	// UniqueBlocks is the number of distinct block addresses in this stream.
	UniqueBlocks int
	// TotalBlocks is the number of chunks emitted (including repeats).
	TotalBlocks int
}

// DedupRatio reports logical bytes per physical byte newly written. A ratio of
// 1.0 means no dedup happened; higher means more sharing. It is +Inf when
// everything deduped (physical == 0, logical > 0) and 0 for an empty stream.
func (s Stats) DedupRatio() float64 {
	switch {
	case s.LogicalBytes == 0:
		return 0
	case s.PhysicalNewBytes == 0:
		return math.Inf(1)
	default:
		return float64(s.LogicalBytes) / float64(s.PhysicalNewBytes)
	}
}

// SavedFraction is the fraction of logical bytes avoided by dedup, in [0,1].
func (s Stats) SavedFraction() float64 {
	if s.LogicalBytes == 0 {
		return 0
	}
	return 1 - float64(s.PhysicalNewBytes)/float64(s.LogicalBytes)
}

// Build streams r through the chunker and stores each resulting block under the
// given namespace (deduping globally), returning the manifest and stats. It
// never buffers the whole stream.
//
// Unlike Ingest, Build does NOT touch the block store's in-store refcount: as of
// step 2 the authoritative refcount lives in Postgres (see package repo, the
// blocks table), updated transactionally with the commit. Build only needs to
// write block bytes, so it takes a BlockStore rather than the full Store.
//
// On the first error (read or store) it stops and returns that error along with
// the partial manifest/stats accumulated so far.
func Build(store blockstore.BlockStore, namespace string, r io.Reader, name string) (Manifest, Stats, error) {
	m := Manifest{Name: name}
	var stats Stats
	unique := make(map[blockstore.Hash]struct{})

	err := chunker.Split(r, chunker.DefaultConfig(), func(ck chunker.Chunk) error {
		isNew, err := store.Put(namespace, ck.Hash, ck.Data)
		if err != nil {
			return err
		}

		stats.LogicalBytes += uint64(ck.Length)
		stats.TotalBlocks++
		if isNew {
			stats.PhysicalNewBytes += uint64(ck.Length)
		}
		unique[ck.Hash] = struct{}{}

		m.Blocks = append(m.Blocks, BlockRef{Hash: ck.Hash, Size: ck.Length})
		return nil
	})

	m.TotalSize = stats.LogicalBytes
	stats.UniqueBlocks = len(unique)
	return m, stats, err
}

// Ingest is the step-1 entry point: it Builds the manifest (writing blocks) and
// then increments the block store's in-store refcount once per block reference.
//
// Deprecated: the in-store refcount is no longer authoritative — Postgres owns
// it as of step 2 (see package repo). Ingest is retained for the standalone
// cmd/cairn-ingest dedup demo; new code should use Build and let repo manage
// refcounts transactionally.
func Ingest(store blockstore.Store, namespace string, r io.Reader, name string) (Manifest, Stats, error) {
	m, stats, err := Build(store, namespace, r, name)
	if err != nil {
		return m, stats, err
	}
	for _, b := range m.Blocks {
		if _, err := store.Incr(namespace, b.Hash); err != nil {
			return m, stats, err
		}
	}
	return m, stats, nil
}
