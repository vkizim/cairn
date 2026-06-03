package repo

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/vkizim/cairn/blockstore"
	"github.com/vkizim/cairn/ingest"
)

// objectToInsert is a serialized fs_object awaiting persistence.
type objectToInsert struct {
	hash    blockstore.Hash
	typ     string
	content []byte
}

// committedFile pairs a file's repository path with the file object it produced.
type committedFile struct {
	path    string // forward-slash relative path, e.g. "docs/report.txt"
	objHash blockstore.Hash
	size    int64
}

// buildFileObject converts an ingest manifest into a file object plus its
// canonical bytes and address.
func buildFileObject(m ingest.Manifest) (objectToInsert, int64, error) {
	fo := FileObject{Size: int64(m.TotalSize), Blocks: make([]FileBlock, len(m.Blocks))}
	for i, b := range m.Blocks {
		fo.Blocks[i] = FileBlock{Hash: b.Hash, Size: int64(b.Size)}
	}
	content, h, err := encodeFileObject(fo)
	if err != nil {
		return objectToInsert{}, 0, err
	}
	return objectToInsert{hash: h, typ: ObjTypeFile, content: content}, fo.Size, nil
}

// dirNode is a node in the in-memory directory trie used to build tree objects.
type dirNode struct {
	dirs  map[string]*dirNode
	files map[string]TreeEntry
}

func newDirNode() *dirNode {
	return &dirNode{dirs: map[string]*dirNode{}, files: map[string]TreeEntry{}}
}

// buildTrees assembles the directory hierarchy from a flat list of committed
// files and emits one tree object per directory (bottom-up). It returns the root
// tree hash and every tree object to insert. Identical subtrees naturally
// produce identical hashes (and are deduped on insert).
func buildTrees(files []committedFile) (blockstore.Hash, []objectToInsert, error) {
	root := newDirNode()
	for _, f := range files {
		comps, err := splitPath(f.path)
		if err != nil {
			return blockstore.Hash{}, nil, err
		}
		node := root
		for _, dir := range comps[:len(comps)-1] {
			child, ok := node.dirs[dir]
			if !ok {
				child = newDirNode()
				node.dirs[dir] = child
			}
			node = child
		}
		name := comps[len(comps)-1]
		if _, clash := node.dirs[name]; clash {
			return blockstore.Hash{}, nil, fmt.Errorf("repo: path %q conflicts with a directory of the same name", f.path)
		}
		node.files[name] = TreeEntry{Name: name, Type: ObjTypeFile, Hash: f.objHash, Size: f.size}
	}

	var objs []objectToInsert
	var emit func(n *dirNode) (blockstore.Hash, int64, error)
	emit = func(n *dirNode) (blockstore.Hash, int64, error) {
		var entries []TreeEntry
		var total int64
		for name, child := range n.dirs {
			if _, clash := n.files[name]; clash {
				return blockstore.Hash{}, 0, fmt.Errorf("repo: name %q is both a file and a directory", name)
			}
			ch, sz, err := emit(child)
			if err != nil {
				return blockstore.Hash{}, 0, err
			}
			entries = append(entries, TreeEntry{Name: name, Type: ObjTypeTree, Hash: ch, Size: sz})
			total += sz
		}
		for _, fe := range n.files {
			entries = append(entries, fe)
			total += fe.Size
		}
		content, h, err := encodeTreeObject(TreeObject{Entries: entries})
		if err != nil {
			return blockstore.Hash{}, 0, err
		}
		objs = append(objs, objectToInsert{hash: h, typ: ObjTypeTree, content: content})
		return h, total, nil
	}

	rootHash, _, err := emit(root)
	if err != nil {
		return blockstore.Hash{}, nil, err
	}
	return rootHash, objs, nil
}

// splitPath normalizes a repository path to clean forward-slash components,
// rejecting empties. path.Clean resolves any "." / ".." so a path can never
// escape the library root.
func splitPath(p string) ([]string, error) {
	cleaned := path.Clean("/" + strings.ReplaceAll(p, `\`, "/"))
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "" || cleaned == "." {
		return nil, fmt.Errorf("repo: empty path %q", p)
	}
	comps := strings.Split(cleaned, "/")
	for _, c := range comps {
		if c == "" {
			return nil, fmt.Errorf("repo: invalid path %q", p)
		}
	}
	return comps, nil
}

// insertObject persists one fs_object, idempotently (immutable, content-addressed).
func insertObject(ctx context.Context, tx pgx.Tx, o objectToInsert) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO fs_objects (obj_hash, type, content) VALUES ($1, $2, $3)
		 ON CONFLICT (obj_hash) DO NOTHING`,
		o.hash.String(), o.typ, o.content)
	if err != nil {
		return fmt.Errorf("repo: insert object %s: %w", o.hash, err)
	}
	return nil
}

// loadObject reads an fs_object's type and canonical content by hash.
func loadObject(ctx context.Context, q querier, h blockstore.Hash) (string, []byte, error) {
	var (
		typ     string
		content []byte
	)
	err := q.QueryRow(ctx, `SELECT type, content FROM fs_objects WHERE obj_hash = $1`, h.String()).
		Scan(&typ, &content)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, ErrNotFound
	}
	if err != nil {
		return "", nil, fmt.Errorf("repo: load object %s: %w", h, err)
	}
	return typ, content, nil
}
