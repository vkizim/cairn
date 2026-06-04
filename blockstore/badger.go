package blockstore

import (
	"encoding/binary"
	"errors"
	"fmt"

	badger "github.com/dgraph-io/badger/v4"
)

// BadgerStore is the default BlockStore: a single embedded Badger key/value
// directory holding both blocks and their refcounts.
//
// Key layout (namespace plumbed in as a key prefix):
//
//	<namespace>/b/<32 raw hash bytes>   -> block contents
//	<namespace>/r/<32 raw hash bytes>   -> refcount (8-byte big-endian uint64)
type BadgerStore struct {
	db *badger.DB
}

// compile-time check that BadgerStore satisfies the full Store interface.
var _ Store = (*BadgerStore)(nil)

// BadgerOption customizes how the Badger store is opened.
type BadgerOption func(*badger.Options)

// WithBypassLockGuard opens Badger without acquiring the directory LOCK file.
//
// ONLY safe when the caller can guarantee no other process has the same
// directory open — two processes writing one Badger directory corrupt it.
// Long-running servers must NOT use this (the lock guard is their protection
// against double-starts). It exists for single-user CLI tools on Windows, where
// an interrupted `go run` can leave an orphaned child holding the LOCK handle
// and every subsequent run fails with "process cannot access the file".
func WithBypassLockGuard() BadgerOption {
	return func(o *badger.Options) { *o = o.WithBypassLockGuard(true) }
}

// NewBadgerStore opens (creating if needed) a Badger-backed block store at dir.
func NewBadgerStore(dir string, options ...BadgerOption) (*BadgerStore, error) {
	if dir == "" {
		return nil, errors.New("blockstore: BadgerStore dir must not be empty")
	}
	opts := badger.DefaultOptions(dir).WithLogger(noopLogger{})
	for _, apply := range options {
		apply(&opts)
	}
	db, err := badger.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("blockstore: open badger at %q: %w", dir, err)
	}
	return &BadgerStore{db: db}, nil
}

func blockKey(ns string, h Hash) []byte {
	key := make([]byte, 0, len(ns)+3+HashSize)
	key = append(key, ns...)
	key = append(key, "/b/"...)
	key = append(key, h[:]...)
	return key
}

func refKey(ns string, h Hash) []byte {
	key := make([]byte, 0, len(ns)+3+HashSize)
	key = append(key, ns...)
	key = append(key, "/r/"...)
	key = append(key, h[:]...)
	return key
}

func blockPrefix(ns string) []byte {
	return append([]byte(ns), "/b/"...)
}

func (s *BadgerStore) Put(ns string, h Hash, data []byte) (bool, error) {
	if err := validateNamespace(ns); err != nil {
		return false, err
	}
	key := blockKey(ns, h)

	var isNew bool
	err := s.updateWithRetry(func(txn *badger.Txn) error {
		isNew = false // reset on each attempt
		_, err := txn.Get(key)
		switch {
		case err == nil:
			// Already present — idempotent, do not rewrite.
			return nil
		case errors.Is(err, badger.ErrKeyNotFound):
			isNew = true
			return txn.Set(key, data)
		default:
			return err
		}
	})
	if err != nil {
		return false, fmt.Errorf("blockstore: put block %s: %w", h, err)
	}
	return isNew, nil
}

func (s *BadgerStore) Get(ns string, h Hash) ([]byte, error) {
	if err := validateNamespace(ns); err != nil {
		return nil, err
	}
	var out []byte
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(blockKey(ns, h))
		if err != nil {
			return err
		}
		out, err = item.ValueCopy(nil)
		return err
	})
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("blockstore: get block %s: %w", h, err)
	}
	return out, nil
}

func (s *BadgerStore) Exists(ns string, h Hash) (bool, error) {
	if err := validateNamespace(ns); err != nil {
		return false, err
	}
	err := s.db.View(func(txn *badger.Txn) error {
		_, err := txn.Get(blockKey(ns, h))
		return err
	})
	if errors.Is(err, badger.ErrKeyNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("blockstore: exists block %s: %w", h, err)
	}
	return true, nil
}

func (s *BadgerStore) Delete(ns string, h Hash) error {
	if err := validateNamespace(ns); err != nil {
		return err
	}
	err := s.updateWithRetry(func(txn *badger.Txn) error {
		return txn.Delete(blockKey(ns, h))
	})
	if err != nil {
		return fmt.Errorf("blockstore: delete block %s: %w", h, err)
	}
	return nil
}

func (s *BadgerStore) Iterate(ns string, fn func(Hash) error) error {
	if err := validateNamespace(ns); err != nil {
		return err
	}
	prefix := blockPrefix(ns)
	return s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false // keys only
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Rewind(); it.Valid(); it.Next() {
			key := it.Item().Key()
			raw := key[len(prefix):]
			if len(raw) != HashSize {
				continue // defensive: skip anything that isn't a hash-keyed block
			}
			var h Hash
			copy(h[:], raw)
			if err := fn(h); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *BadgerStore) Incr(ns string, h Hash) (uint64, error) {
	return s.adjustRef(ns, h, +1)
}

func (s *BadgerStore) Decr(ns string, h Hash) (uint64, error) {
	return s.adjustRef(ns, h, -1)
}

func (s *BadgerStore) Refs(ns string, h Hash) (uint64, error) {
	if err := validateNamespace(ns); err != nil {
		return 0, err
	}
	var n uint64
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(refKey(ns, h))
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			n = decodeRef(val)
			return nil
		})
	})
	if err != nil {
		return 0, fmt.Errorf("blockstore: refs %s: %w", h, err)
	}
	return n, nil
}

// adjustRef performs an atomic read-modify-write of the refcount key inside a
// single Badger transaction. Concurrent writers to the same key conflict on
// commit and are retried by updateWithRetry. The count is clamped at zero.
func (s *BadgerStore) adjustRef(ns string, h Hash, delta int64) (uint64, error) {
	if err := validateNamespace(ns); err != nil {
		return 0, err
	}
	key := refKey(ns, h)

	var next uint64
	err := s.updateWithRetry(func(txn *badger.Txn) error {
		var cur uint64
		item, err := txn.Get(key)
		switch {
		case err == nil:
			if err := item.Value(func(val []byte) error {
				cur = decodeRef(val)
				return nil
			}); err != nil {
				return err
			}
		case errors.Is(err, badger.ErrKeyNotFound):
			cur = 0
		default:
			return err
		}

		v := int64(cur) + delta
		if v < 0 {
			v = 0
		}
		next = uint64(v)
		if next == 0 {
			return txn.Delete(key)
		}
		return txn.Set(key, encodeRef(next))
	})
	if err != nil {
		return 0, fmt.Errorf("blockstore: adjust ref %s: %w", h, err)
	}
	return next, nil
}

func (s *BadgerStore) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("blockstore: close badger: %w", err)
	}
	return nil
}

// updateWithRetry runs fn in an Update transaction, retrying on transaction
// conflicts (ErrConflict), which Badger raises when two transactions touch the
// same key concurrently.
func (s *BadgerStore) updateWithRetry(fn func(txn *badger.Txn) error) error {
	for {
		err := s.db.Update(fn)
		if errors.Is(err, badger.ErrConflict) {
			continue
		}
		return err
	}
}

func encodeRef(n uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], n)
	return b[:]
}

func decodeRef(b []byte) uint64 {
	if len(b) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(b)
}

// noopLogger silences Badger's chatty INFO/DEBUG output. It satisfies
// badger.Logger; all levels are dropped to keep CLI output clean.
type noopLogger struct{}

func (noopLogger) Errorf(string, ...interface{})   {}
func (noopLogger) Warningf(string, ...interface{}) {}
func (noopLogger) Infof(string, ...interface{})    {}
func (noopLogger) Debugf(string, ...interface{})   {}
