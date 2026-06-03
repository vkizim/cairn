package repo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/vkizim/cairn/blockstore"
)

// Canonical serialization of file, tree, and commit objects.
//
// An object's address is sha256 over the EXACT bytes produced here, and those
// bytes are what we persist (fs_objects.content as BYTEA). This is a permanent
// addressing contract: two logically-identical objects must serialize to
// identical bytes on every machine and every version, or cross-version dedup of
// identical objects silently breaks. The golden-vector test pins this, exactly
// like the chunker's golden boundaries.
//
// Encoding rules:
//   - JSON over fixed structs (never maps), so field order is the struct order.
//   - HTML escaping disabled, and the single trailing newline that
//     json.Encoder appends is trimmed, so output is minimal and stable.
//   - Hashes are lowercase hex strings.
//   - Tree entries are sorted by name (bytewise) before encoding.

type blockWire struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

type fileObjWire struct {
	Size   int64       `json:"size"`
	Blocks []blockWire `json:"blocks"`
}

type entryWire struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

type treeObjWire struct {
	Entries []entryWire `json:"entries"`
}

type commitWire struct {
	LibraryID     string  `json:"library_id"`
	Parent        *string `json:"parent"`
	RootTree      string  `json:"root_tree"`
	CtimeUnixNano int64   `json:"ctime_unix_nano"`
	Description   string  `json:"description"`
}

// canonicalBytes marshals v as canonical JSON (no HTML escaping, no trailing
// newline).
func canonicalBytes(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// encodeFileObject returns the canonical bytes and address of a file object.
func encodeFileObject(fo FileObject) ([]byte, blockstore.Hash, error) {
	w := fileObjWire{Size: fo.Size, Blocks: make([]blockWire, len(fo.Blocks))}
	for i, b := range fo.Blocks {
		w.Blocks[i] = blockWire{Hash: b.Hash.String(), Size: b.Size}
	}
	content, err := canonicalBytes(w)
	if err != nil {
		return nil, blockstore.Hash{}, fmt.Errorf("repo: encode file object: %w", err)
	}
	return content, blockstore.HashData(content), nil
}

// encodeTreeObject returns the canonical bytes and address of a tree object.
// Entries are sorted by name so the encoding is independent of insertion order.
func encodeTreeObject(to TreeObject) ([]byte, blockstore.Hash, error) {
	entries := append([]TreeEntry(nil), to.Entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })

	w := treeObjWire{Entries: make([]entryWire, len(entries))}
	for i, e := range entries {
		w.Entries[i] = entryWire{Name: e.Name, Type: e.Type, Hash: e.Hash.String(), Size: e.Size}
	}
	content, err := canonicalBytes(w)
	if err != nil {
		return nil, blockstore.Hash{}, fmt.Errorf("repo: encode tree object: %w", err)
	}
	return content, blockstore.HashData(content), nil
}

// encodeCommit returns the canonical bytes and address of a commit object. The
// commit row stores its fields in columns, not as bytea; these bytes exist only
// to derive and verify commit_hash.
func encodeCommit(c Commit) ([]byte, blockstore.Hash, error) {
	w := commitWire{
		LibraryID:     c.LibraryID.String(),
		RootTree:      c.RootTree.String(),
		CtimeUnixNano: c.Ctime.UTC().UnixNano(),
		Description:   c.Description,
	}
	if c.Parent != nil {
		p := c.Parent.String()
		w.Parent = &p
	}
	content, err := canonicalBytes(w)
	if err != nil {
		return nil, blockstore.Hash{}, fmt.Errorf("repo: encode commit: %w", err)
	}
	return content, blockstore.HashData(content), nil
}

// decodeFileObject parses canonical file-object bytes back into a FileObject.
func decodeFileObject(content []byte) (FileObject, error) {
	var w fileObjWire
	if err := json.Unmarshal(content, &w); err != nil {
		return FileObject{}, fmt.Errorf("repo: decode file object: %w", err)
	}
	fo := FileObject{Size: w.Size, Blocks: make([]FileBlock, len(w.Blocks))}
	for i, b := range w.Blocks {
		h, err := blockstore.ParseHash(b.Hash)
		if err != nil {
			return FileObject{}, fmt.Errorf("repo: decode file object block %d: %w", i, err)
		}
		fo.Blocks[i] = FileBlock{Hash: h, Size: b.Size}
	}
	return fo, nil
}

// decodeTreeObject parses canonical tree-object bytes back into a TreeObject.
func decodeTreeObject(content []byte) (TreeObject, error) {
	var w treeObjWire
	if err := json.Unmarshal(content, &w); err != nil {
		return TreeObject{}, fmt.Errorf("repo: decode tree object: %w", err)
	}
	to := TreeObject{Entries: make([]TreeEntry, len(w.Entries))}
	for i, e := range w.Entries {
		h, err := blockstore.ParseHash(e.Hash)
		if err != nil {
			return TreeObject{}, fmt.Errorf("repo: decode tree entry %q: %w", e.Name, err)
		}
		to.Entries[i] = TreeEntry{Name: e.Name, Type: e.Type, Hash: h, Size: e.Size}
	}
	return to, nil
}

// VerifyContent reports whether content hashes to want (the self-verification
// check behind rule C).
func VerifyContent(content []byte, want blockstore.Hash) bool {
	return blockstore.HashData(content) == want
}

// parseUUID is a small helper used when reading rows.
func parseUUID(s string) (uuid.UUID, error) { return uuid.Parse(s) }
