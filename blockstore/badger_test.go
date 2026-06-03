package blockstore

import "testing"

func TestBadgerStore(t *testing.T) {
	runConformance(t, func(t *testing.T) Store {
		s, err := NewBadgerStore(t.TempDir())
		if err != nil {
			t.Fatalf("NewBadgerStore: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}
