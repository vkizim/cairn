package blockstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFSStore(t *testing.T) {
	runConformance(t, func(t *testing.T) Store {
		s, err := NewFSStore(t.TempDir())
		if err != nil {
			t.Fatalf("NewFSStore: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

// TestFSStoreSharding verifies blocks land at the expected ab/cd/<hash> path so
// the on-disk layout (and thus the dev-inspection workflow) stays as documented.
func TestFSStoreSharding(t *testing.T) {
	root := t.TempDir()
	s, err := NewFSStore(root)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	defer s.Close()

	data := []byte("shard me")
	h := HashData(data)
	if _, err := s.Put(DefaultNamespace, h, data); err != nil {
		t.Fatalf("Put: %v", err)
	}

	hexHash := h.String()
	want := filepath.Join(root, DefaultNamespace, "blocks", hexHash[0:2], hexHash[2:4], hexHash)
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected block at %s: %v", want, err)
	}
}

// TestFSStoreNoTempLeftovers verifies a successful write leaves no temp files
// behind in the shard directory.
func TestFSStoreNoTempLeftovers(t *testing.T) {
	root := t.TempDir()
	s, _ := NewFSStore(root)
	defer s.Close()

	data := []byte("clean up after yourself")
	h := HashData(data)
	if _, err := s.Put(DefaultNamespace, h, data); err != nil {
		t.Fatalf("Put: %v", err)
	}

	hexHash := h.String()
	dir := filepath.Join(root, DefaultNamespace, "blocks", hexHash[0:2], hexHash[2:4])
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != hexHash {
			t.Fatalf("unexpected leftover file in shard dir: %s", e.Name())
		}
	}
}
