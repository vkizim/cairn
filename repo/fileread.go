package repo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vkizim/cairn/blockstore"
)

// ErrIsDirectory is returned by OpenFile when the path resolves to a directory.
var ErrIsDirectory = errors.New("repo: path is a directory")

// FileReader streams a file's bytes by reassembling its blocks from the store on
// demand. It implements io.ReadSeeker so handlers can pass it to
// http.ServeContent for free HTTP Range support. One block is cached at a time,
// which suits the sequential reads ServeContent issues within a range.
type FileReader struct {
	ctx     context.Context
	store   blockstore.Store
	ns      string
	spans   []blockSpan
	size    int64
	pos     int64
	cached  []byte
	cacheAt int64 // start offset of the cached block, -1 if none
	name    string
	modTime time.Time
}

type blockSpan struct {
	hash   blockstore.Hash
	start  int64
	length int64
}

// Name is the file's base name (used for content-type sniffing).
func (r *FileReader) Name() string { return r.name }

// ModTime is the file's modification time (the commit ctime), or zero.
func (r *FileReader) ModTime() time.Time { return r.modTime }

// Size is the total file size in bytes.
func (r *FileReader) Size() int64 { return r.size }

// OpenFile resolves a file path in a library via path_index and returns a
// seekable reader over its reassembled bytes. Returns ErrNotFound if the path
// has no entry and ErrIsDirectory if it names a directory.
func (db *DB) OpenFile(ctx context.Context, libID uuid.UUID, fullPath string) (*FileReader, error) {
	dir, name, err := SplitLibraryPath(fullPath)
	if err != nil {
		return nil, err
	}

	var (
		isDir    bool
		fileHash *string
		mtime    *time.Time
	)
	err = db.pool.QueryRow(ctx, `
		SELECT is_dir, file_obj_hash, mtime FROM path_index
		WHERE library_id = $1 AND path = $2 AND name = $3`, libID, dir, name).
		Scan(&isDir, &fileHash, &mtime)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("repo: lookup path %q: %w", fullPath, err)
	}
	if isDir || fileHash == nil {
		return nil, ErrIsDirectory
	}

	h, err := blockstore.ParseHash(*fileHash)
	if err != nil {
		return nil, err
	}
	_, content, err := loadObject(ctx, db.pool, h)
	if err != nil {
		return nil, err
	}
	fo, err := decodeFileObject(content)
	if err != nil {
		return nil, err
	}

	fr := &FileReader{ctx: ctx, store: db.store, ns: db.ns, cacheAt: -1, name: name}
	var off int64
	for _, b := range fo.Blocks {
		fr.spans = append(fr.spans, blockSpan{hash: b.Hash, start: off, length: b.Size})
		off += b.Size
	}
	fr.size = off
	if mtime != nil {
		fr.modTime = *mtime
	}
	return fr, nil
}

func (r *FileReader) Read(p []byte) (int, error) {
	if r.pos >= r.size {
		return 0, io.EOF
	}
	if err := r.ensureBlockFor(r.pos); err != nil {
		return 0, err
	}
	n := copy(p, r.cached[r.pos-r.cacheAt:])
	r.pos += int64(n)
	return n, nil
}

func (r *FileReader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.pos + offset
	case io.SeekEnd:
		abs = r.size + offset
	default:
		return 0, fmt.Errorf("repo: invalid seek whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("repo: negative seek position %d", abs)
	}
	r.pos = abs
	return abs, nil
}

// ensureBlockFor loads the block covering pos into the cache, if not already.
func (r *FileReader) ensureBlockFor(pos int64) error {
	if r.cached != nil && pos >= r.cacheAt && pos < r.cacheAt+int64(len(r.cached)) {
		return nil
	}
	for _, s := range r.spans {
		if pos >= s.start && pos < s.start+s.length {
			data, err := r.store.Get(r.ns, s.hash)
			if err != nil {
				return fmt.Errorf("repo: read block %s: %w", s.hash, err)
			}
			r.cached = data
			r.cacheAt = s.start
			return nil
		}
	}
	return fmt.Errorf("repo: no block covers offset %d", pos)
}
