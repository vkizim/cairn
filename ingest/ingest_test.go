package ingest

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	"github.com/vkizim/cairn/blockstore"
)

func newStore(t *testing.T) blockstore.Store {
	t.Helper()
	s, err := blockstore.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// deterministicBytes mirrors the chunker test generator (xorshift64*), kept
// local so this package has no test-only dependency on chunker internals.
func deterministicBytes(n int, seed uint64) []byte {
	b := make([]byte, n)
	s := seed
	var word [8]byte
	for i := 0; i < n; i += 8 {
		s ^= s >> 12
		s ^= s << 25
		s ^= s >> 27
		binary.LittleEndian.PutUint64(word[:], s*0x2545F4914F6CDD1D)
		copy(b[i:], word[:])
	}
	return b
}

const ns = blockstore.DefaultNamespace

func TestIngestEmpty(t *testing.T) {
	s := newStore(t)
	m, stats, err := Ingest(s, ns, bytes.NewReader(nil), "empty.bin")
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(m.Blocks) != 0 || m.TotalSize != 0 {
		t.Fatalf("empty manifest = %+v, want no blocks", m)
	}
	if stats.LogicalBytes != 0 || stats.PhysicalNewBytes != 0 || stats.UniqueBlocks != 0 {
		t.Fatalf("empty stats = %+v, want zeros", stats)
	}
	if r := stats.DedupRatio(); r != 0 {
		t.Fatalf("empty DedupRatio = %v, want 0", r)
	}
}

func TestIngestSmallFile(t *testing.T) {
	s := newStore(t)
	data := []byte("a small file well below the minimum chunk size")
	m, stats, err := Ingest(s, ns, bytes.NewReader(data), "small.txt")
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(m.Blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(m.Blocks))
	}
	if m.TotalSize != uint64(len(data)) {
		t.Fatalf("TotalSize = %d, want %d", m.TotalSize, len(data))
	}
	if stats.LogicalBytes != uint64(len(data)) || stats.PhysicalNewBytes != uint64(len(data)) {
		t.Fatalf("stats = %+v, want logical=physical=%d", stats, len(data))
	}
	if stats.UniqueBlocks != 1 || stats.TotalBlocks != 1 {
		t.Fatalf("blocks unique=%d total=%d, want 1/1", stats.UniqueBlocks, stats.TotalBlocks)
	}
	if r := stats.DedupRatio(); r != 1.0 {
		t.Fatalf("DedupRatio = %v, want 1.0", r)
	}
	// The single block should be referenced exactly once.
	if n, _ := s.Refs(ns, m.Blocks[0].Hash); n != 1 {
		t.Fatalf("refs = %d, want 1", n)
	}
}

func TestIngestRoundTrip(t *testing.T) {
	s := newStore(t)
	data := deterministicBytes(40<<20, 0x1357) // ~40 MiB, multi-chunk
	m, stats, err := Ingest(s, ns, bytes.NewReader(data), "big.bin")
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(m.Blocks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(m.Blocks))
	}
	if stats.LogicalBytes != uint64(len(data)) {
		t.Fatalf("LogicalBytes = %d, want %d", stats.LogicalBytes, len(data))
	}

	// Reconstruct the file from its manifest and compare byte-for-byte.
	var buf bytes.Buffer
	for _, ref := range m.Blocks {
		block, err := s.Get(ns, ref.Hash)
		if err != nil {
			t.Fatalf("Get %s: %v", ref.Hash, err)
		}
		if uint(len(block)) != ref.Size {
			t.Fatalf("block size = %d, manifest says %d", len(block), ref.Size)
		}
		buf.Write(block)
	}
	if !bytes.Equal(buf.Bytes(), data) {
		t.Fatal("reconstructed data != original")
	}
}

func TestIngestGlobalDedup(t *testing.T) {
	s := newStore(t)
	data := deterministicBytes(30<<20, 0x2468)

	// First ingest: everything is new.
	_, stats1, err := Ingest(s, ns, bytes.NewReader(data), "first.bin")
	if err != nil {
		t.Fatalf("Ingest #1: %v", err)
	}
	if stats1.PhysicalNewBytes != stats1.LogicalBytes {
		t.Fatalf("first ingest: physical=%d logical=%d, want equal", stats1.PhysicalNewBytes, stats1.LogicalBytes)
	}

	// Second ingest of identical content: nothing new should be written.
	m2, stats2, err := Ingest(s, ns, bytes.NewReader(data), "second.bin")
	if err != nil {
		t.Fatalf("Ingest #2: %v", err)
	}
	if stats2.PhysicalNewBytes != 0 {
		t.Fatalf("second ingest physical = %d, want 0 (fully deduped)", stats2.PhysicalNewBytes)
	}
	if r := stats2.DedupRatio(); !math.IsInf(r, 1) {
		t.Fatalf("second ingest DedupRatio = %v, want +Inf", r)
	}
	if f := stats2.SavedFraction(); f != 1.0 {
		t.Fatalf("second ingest SavedFraction = %v, want 1.0", f)
	}

	// Each block is now referenced twice (once per ingest).
	for _, ref := range m2.Blocks {
		if n, _ := s.Refs(ns, ref.Hash); n != 2 {
			t.Fatalf("block %s refs = %d, want 2", ref.Hash, n)
		}
	}
}
