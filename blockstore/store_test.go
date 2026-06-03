package blockstore

import (
	"bytes"
	"errors"
	"sync"
	"testing"
)

// storeFactory builds a fresh, empty Store for one test. Cleanup (Close +
// temp-dir removal) is registered via t.Cleanup by the caller.
type storeFactory func(t *testing.T) Store

// runConformance exercises the full Store contract against any implementation.
// Both BadgerStore and FSStore run through this identical suite.
func runConformance(t *testing.T, newStore storeFactory) {
	const ns = DefaultNamespace

	t.Run("put-get-roundtrip", func(t *testing.T) {
		s := newStore(t)
		cases := []struct {
			name string
			data []byte
		}{
			{"empty", []byte{}},                       // models a zero-length block
			{"small", []byte("hello cairn")},          // far below any chunk size
			{"binary", bytes.Repeat([]byte{0x00, 0xFF, 0xA5}, 1000)},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				h := HashData(tc.data)

				isNew, err := s.Put(ns, h, tc.data)
				if err != nil {
					t.Fatalf("Put: %v", err)
				}
				if !isNew {
					t.Fatal("first Put: isNew = false, want true")
				}

				got, err := s.Get(ns, h)
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				if !bytes.Equal(got, tc.data) {
					t.Fatalf("Get = %q, want %q", got, tc.data)
				}

				exists, err := s.Exists(ns, h)
				if err != nil || !exists {
					t.Fatalf("Exists = %v, %v; want true, nil", exists, err)
				}
			})
		}
	})

	t.Run("put-idempotent-dedup", func(t *testing.T) {
		s := newStore(t)
		data := []byte("identical content stored twice")
		h := HashData(data)

		isNew, err := s.Put(ns, h, data)
		if err != nil || !isNew {
			t.Fatalf("first Put: isNew=%v err=%v, want true,nil", isNew, err)
		}
		isNew, err = s.Put(ns, h, data)
		if err != nil {
			t.Fatalf("second Put: %v", err)
		}
		if isNew {
			t.Fatal("second Put of identical data: isNew = true, want false (dedup)")
		}
		got, err := s.Get(ns, h)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("Get after dedup: %q, %v", got, err)
		}
	})

	t.Run("get-missing-is-ErrNotFound", func(t *testing.T) {
		s := newStore(t)
		_, err := s.Get(ns, HashData([]byte("never stored")))
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get missing: err = %v, want ErrNotFound", err)
		}
		exists, err := s.Exists(ns, HashData([]byte("never stored")))
		if err != nil || exists {
			t.Fatalf("Exists missing = %v, %v; want false, nil", exists, err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		s := newStore(t)
		data := []byte("to be deleted")
		h := HashData(data)

		// Deleting an absent block is not an error.
		if err := s.Delete(ns, h); err != nil {
			t.Fatalf("Delete absent: %v", err)
		}
		if _, err := s.Put(ns, h, data); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if err := s.Delete(ns, h); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := s.Get(ns, h); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get after Delete: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("iterate", func(t *testing.T) {
		s := newStore(t)
		want := map[Hash]bool{}
		for _, b := range [][]byte{[]byte("a"), []byte("bb"), []byte("ccc"), []byte("dddd")} {
			h := HashData(b)
			want[h] = true
			if _, err := s.Put(ns, h, b); err != nil {
				t.Fatalf("Put: %v", err)
			}
		}

		got := map[Hash]bool{}
		err := s.Iterate(ns, func(h Hash) error {
			got[h] = true
			return nil
		})
		if err != nil {
			t.Fatalf("Iterate: %v", err)
		}
		if len(got) != len(want) {
			t.Fatalf("Iterate visited %d blocks, want %d", len(got), len(want))
		}
		for h := range want {
			if !got[h] {
				t.Fatalf("Iterate missed block %s", h)
			}
		}
	})

	t.Run("iterate-empty", func(t *testing.T) {
		s := newStore(t)
		count := 0
		if err := s.Iterate(ns, func(Hash) error { count++; return nil }); err != nil {
			t.Fatalf("Iterate empty: %v", err)
		}
		if count != 0 {
			t.Fatalf("Iterate empty visited %d, want 0", count)
		}
	})

	t.Run("refcount", func(t *testing.T) {
		s := newStore(t)
		h := HashData([]byte("refcounted"))

		if n, _ := s.Refs(ns, h); n != 0 {
			t.Fatalf("initial Refs = %d, want 0", n)
		}
		if n, err := s.Incr(ns, h); err != nil || n != 1 {
			t.Fatalf("Incr#1 = %d, %v; want 1, nil", n, err)
		}
		if n, err := s.Incr(ns, h); err != nil || n != 2 {
			t.Fatalf("Incr#2 = %d, %v; want 2, nil", n, err)
		}
		if n, _ := s.Refs(ns, h); n != 2 {
			t.Fatalf("Refs = %d, want 2", n)
		}
		if n, err := s.Decr(ns, h); err != nil || n != 1 {
			t.Fatalf("Decr#1 = %d, %v; want 1, nil", n, err)
		}
		if n, err := s.Decr(ns, h); err != nil || n != 0 {
			t.Fatalf("Decr#2 = %d, %v; want 0, nil", n, err)
		}
		// Decrement below zero clamps at zero.
		if n, err := s.Decr(ns, h); err != nil || n != 0 {
			t.Fatalf("Decr#3 = %d, %v; want 0, nil", n, err)
		}
	})

	t.Run("concurrent-put-same-hash", func(t *testing.T) {
		s := newStore(t)
		data := bytes.Repeat([]byte("z"), 4096)
		h := HashData(data)

		const goroutines = 16
		var wg sync.WaitGroup
		var mu sync.Mutex
		newCount := 0
		errs := make([]error, 0)

		wg.Add(goroutines)
		for i := 0; i < goroutines; i++ {
			go func() {
				defer wg.Done()
				isNew, err := s.Put(ns, h, data)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs = append(errs, err)
					return
				}
				if isNew {
					newCount++
				}
			}()
		}
		wg.Wait()

		if len(errs) != 0 {
			t.Fatalf("concurrent Put errors: %v", errs)
		}
		// Exactly one writer should observe isNew; the rest dedup.
		if newCount != 1 {
			t.Fatalf("isNew observed %d times, want exactly 1", newCount)
		}
	})

	t.Run("invalid-namespace", func(t *testing.T) {
		s := newStore(t)
		h := HashData([]byte("x"))
		for _, bad := range []string{"", "..", "a/b", `a\b`, "x..y"} {
			if _, err := s.Put(bad, h, []byte("x")); err == nil {
				t.Fatalf("Put with namespace %q: want error, got nil", bad)
			}
		}
	})
}
