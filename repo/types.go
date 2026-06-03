package repo

import (
	"time"

	"github.com/google/uuid"
	"github.com/vkizim/cairn/blockstore"
)

// Library is a content namespace with a moving head pointer into its commit chain.
type Library struct {
	ID         uuid.UUID
	Name       string
	Owner      string
	HeadCommit *blockstore.Hash // nil when the library has no commits yet
	Encrypted  bool
	CreatedAt  time.Time
}

// Commit is one immutable snapshot: a root tree plus a link to its parent.
type Commit struct {
	Hash        blockstore.Hash
	LibraryID   uuid.UUID
	Parent      *blockstore.Hash // nil for the root commit
	RootTree    blockstore.Hash
	Ctime       time.Time
	Description string
}

// FileBlock is one block reference inside a file object (address + size).
type FileBlock struct {
	Hash blockstore.Hash
	Size int64
}

// FileObject is the content of a single file: its total size and the ordered
// list of blocks that reconstruct it.
type FileObject struct {
	Size   int64
	Blocks []FileBlock
}

// TreeEntry is one child of a directory: a file or a subtree, referenced by hash.
type TreeEntry struct {
	Name string
	Type string // ObjTypeTree or ObjTypeFile
	Hash blockstore.Hash
	Size int64
}

// TreeObject is the content of a directory: its entries (canonically sorted by
// name when serialized).
type TreeObject struct {
	Entries []TreeEntry
}

const (
	ObjTypeTree = "tree"
	ObjTypeFile = "file"
)

// PathEntry is one row of the derived path_index: a directory listing entry.
type PathEntry struct {
	Path        string
	Name        string
	IsDir       bool
	Size        int64
	Mtime       *time.Time
	FileObjHash *blockstore.Hash // nil for directories
}
