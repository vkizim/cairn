package blockstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// FSStore is a filesystem-backed BlockStore: a plain sharded directory tree.
// It is intended for development and inspection (you can ls the blocks) and as
// a fallback when an embedded KV store is undesirable.
//
// Layout (per namespace):
//
//	<root>/<namespace>/blocks/ab/cd/<full-hex-hash>   block contents
//	<root>/<namespace>/refs/ab/cd/<full-hex-hash>     decimal refcount
//
// Writes are atomic: data is written to a temp file in the destination
// directory, fsync'd, then renamed into place (rename is atomic on a single
// filesystem). Concurrent access is serialized per-hash by a stripe of mutexes.
type FSStore struct {
	root   string
	stripe [256]sync.Mutex // serialize check-and-write / refcount RMW per hash
}

// compile-time check that FSStore satisfies the full Store interface.
var _ Store = (*FSStore)(nil)

// NewFSStore opens (creating if needed) a filesystem block store rooted at root.
func NewFSStore(root string) (*FSStore, error) {
	if root == "" {
		return nil, errors.New("blockstore: FSStore root must not be empty")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("blockstore: create root %q: %w", root, err)
	}
	return &FSStore{root: root}, nil
}

// lockFor returns the mutex guarding operations on h. Striping by the first
// byte gives 256 independent locks — enough to keep contention low while making
// per-hash check-and-write and refcount updates race-free.
func (s *FSStore) lockFor(h Hash) *sync.Mutex {
	return &s.stripe[h[0]]
}

func (s *FSStore) blockPath(ns string, h Hash) (string, error) {
	if err := validateNamespace(ns); err != nil {
		return "", err
	}
	return filepath.Join(s.root, ns, "blocks", filepath.FromSlash(h.ShardPath())), nil
}

func (s *FSStore) refPath(ns string, h Hash) (string, error) {
	if err := validateNamespace(ns); err != nil {
		return "", err
	}
	return filepath.Join(s.root, ns, "refs", filepath.FromSlash(h.ShardPath())), nil
}

func (s *FSStore) Put(ns string, h Hash, data []byte) (bool, error) {
	path, err := s.blockPath(ns, h)
	if err != nil {
		return false, err
	}

	lock := s.lockFor(h)
	lock.Lock()
	defer lock.Unlock()

	// Idempotent: never rewrite an existing block.
	if fileExists(path) {
		return false, nil
	}
	if err := atomicWriteFile(path, data); err != nil {
		return false, err
	}
	return true, nil
}

func (s *FSStore) Get(ns string, h Hash) ([]byte, error) {
	path, err := s.blockPath(ns, h)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("blockstore: read block %s: %w", h, err)
	}
	return data, nil
}

func (s *FSStore) Exists(ns string, h Hash) (bool, error) {
	path, err := s.blockPath(ns, h)
	if err != nil {
		return false, err
	}
	return fileExists(path), nil
}

func (s *FSStore) Delete(ns string, h Hash) error {
	path, err := s.blockPath(ns, h)
	if err != nil {
		return err
	}
	lock := s.lockFor(h)
	lock.Lock()
	defer lock.Unlock()

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("blockstore: delete block %s: %w", h, err)
	}
	return nil
}

func (s *FSStore) Iterate(ns string, fn func(Hash) error) error {
	if err := validateNamespace(ns); err != nil {
		return err
	}
	blocksDir := filepath.Join(s.root, ns, "blocks")
	entries, err := os.Stat(blocksDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil // nothing stored yet
	}
	if err != nil {
		return fmt.Errorf("blockstore: stat %q: %w", blocksDir, err)
	}
	if !entries.IsDir() {
		return fmt.Errorf("blockstore: %q is not a directory", blocksDir)
	}

	return filepath.WalkDir(blocksDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Leaf filenames are the full hex hash; ignore anything that isn't
		// (e.g. stray temp files from an interrupted write).
		h, perr := ParseHash(d.Name())
		if perr != nil {
			return nil
		}
		return fn(h)
	})
}

func (s *FSStore) Incr(ns string, h Hash) (uint64, error) {
	return s.adjustRef(ns, h, +1)
}

func (s *FSStore) Decr(ns string, h Hash) (uint64, error) {
	return s.adjustRef(ns, h, -1)
}

func (s *FSStore) Refs(ns string, h Hash) (uint64, error) {
	path, err := s.refPath(ns, h)
	if err != nil {
		return 0, err
	}
	return readRefFile(path)
}

// adjustRef performs a locked read-modify-write of the refcount file. The
// counter is clamped at zero and never written negative.
func (s *FSStore) adjustRef(ns string, h Hash, delta int64) (uint64, error) {
	path, err := s.refPath(ns, h)
	if err != nil {
		return 0, err
	}

	lock := s.lockFor(h)
	lock.Lock()
	defer lock.Unlock()

	cur, err := readRefFile(path)
	if err != nil {
		return 0, err
	}
	next := int64(cur) + delta
	if next < 0 {
		next = 0
	}
	if next == 0 {
		// Drop the file entirely when the count hits zero to keep the tree tidy.
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return 0, fmt.Errorf("blockstore: remove ref %s: %w", h, rmErr)
		}
		return 0, nil
	}
	if err := atomicWriteFile(path, []byte(strconv.FormatUint(uint64(next), 10))); err != nil {
		return 0, err
	}
	return uint64(next), nil
}

// Close is a no-op for the filesystem store; it holds no long-lived handles.
func (s *FSStore) Close() error { return nil }

// --- helpers ---

func readRefFile(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("blockstore: read ref %q: %w", path, err)
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("blockstore: corrupt ref file %q: %w", path, err)
	}
	return n, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// atomicWriteFile writes data to path durably: temp file in the same directory,
// fsync, rename into place, then a best-effort fsync of the directory.
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("blockstore: create dir %q: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("blockstore: create temp in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before a successful rename.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("blockstore: write temp %q: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("blockstore: fsync temp %q: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("blockstore: close temp %q: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("blockstore: rename %q -> %q: %w", tmpName, path, err)
	}
	syncDir(dir) // best-effort; not supported on all platforms (e.g. Windows)
	return nil
}

// syncDir best-effort fsyncs a directory so a rename is durable. Directory
// fsync is unsupported on some platforms (notably Windows), so errors are
// intentionally ignored rather than failing the write.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// validateNamespace rejects names that could escape the store root or break the
// on-disk layout. Today only DefaultNamespace is used, but this guards the
// future per-library mode against path traversal.
func validateNamespace(ns string) error {
	if ns == "" {
		return errors.New("blockstore: namespace must not be empty")
	}
	if ns == "." || ns == ".." || strings.ContainsAny(ns, `/\`) || strings.Contains(ns, "..") {
		return fmt.Errorf("blockstore: invalid namespace %q", ns)
	}
	return nil
}
