package repo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vkizim/cairn/blockstore"
	"github.com/vkizim/cairn/ingest"
)

// FileInput is one file to include in a commit: its repository path and a reader
// over its bytes.
type FileInput struct {
	Path   string
	Reader io.Reader
}

// CommitStats summarizes a commit's ingest, including cross-file dedup within the
// commit (a block shared by two files in the same commit is written once).
type CommitStats struct {
	Files int
	ingest.Stats
}

// CommitResult is what CommitFiles returns: the new commit and its stats.
type CommitResult struct {
	Commit Commit
	Stats  CommitStats
}

// preparedCommit holds everything computed before any metadata is written:
// blocks are already in the store, objects are serialized, hashes are final.
type preparedCommit struct {
	libID    uuid.UUID
	commit   Commit
	parent   *blockstore.Hash
	fileObjs []objectToInsert
	treeObjs []objectToInsert
	blocks   map[blockstore.Hash]int64 // distinct blocks referenced -> size
	stats    CommitStats
}

// CommitFiles ingests the given files into a new commit and advances the library
// head, enforcing the fault-tolerance rules:
//
//	prepare      : blocks written to the store (rule A: blocks first)
//	commitObjects: tx1 inserts fs_objects + the commit row, increments refcounts
//	publishCommit: tx2 advances head AND rebuilds path_index atomically (rule B)
//
// A crash between tx1 and tx2 leaves an orphan commit (reclaimed by GC) but never
// a head pointing at a missing commit.
//
// SNAPSHOT semantics: the new commit's tree is built from EXACTLY the given
// inputs — files present in the head but absent from inputs disappear from the
// new head (they stay reachable through history). This is what the cairn-fs CLI
// wants ("commit this directory" = full snapshot). For incremental additions
// (web upload), use CommitFilesMerged.
func (db *DB) CommitFiles(ctx context.Context, libID uuid.UUID, inputs []FileInput, description string) (CommitResult, error) {
	return db.commitFiles(ctx, libID, inputs, description, false)
}

// CommitFilesMerged commits inputs ON TOP of the library's current head: the new
// commit's tree is the head tree with the inputs overlaid. An input whose path
// already exists REPLACES that entry — the path gets a new version, and the
// previous file object stays reachable through the parent commit (no "file(1)"
// copies). Files not mentioned in inputs are carried over unchanged. With no
// head it behaves exactly like CommitFiles.
//
// This is the semantics the web upload uses; one batch of uploads = one merged
// commit. Rules A/B/C are identical to CommitFiles (same tx1/tx2 path).
func (db *DB) CommitFilesMerged(ctx context.Context, libID uuid.UUID, inputs []FileInput, description string) (CommitResult, error) {
	return db.commitFiles(ctx, libID, inputs, description, true)
}

func (db *DB) commitFiles(ctx context.Context, libID uuid.UUID, inputs []FileInput, description string, mergeWithHead bool) (CommitResult, error) {
	lib, err := db.GetLibrary(ctx, libID)
	if err != nil {
		return CommitResult{}, err
	}

	prep, err := db.prepareCommit(ctx, lib, inputs, description, mergeWithHead)
	if err != nil {
		return CommitResult{}, err
	}
	if err := db.commitObjects(ctx, prep); err != nil {
		return CommitResult{}, err
	}
	if err := db.publishCommit(ctx, prep); err != nil {
		return CommitResult{}, err
	}
	return CommitResult{Commit: prep.commit, Stats: prep.stats}, nil
}

// prepareCommit runs the chunker over each input (writing blocks to the store),
// builds the file and tree objects, and computes the commit hash. It performs NO
// metadata writes. With mergeWithHead, the head commit's files are carried over
// into the new tree (inputs override matching paths) and their blocks join the
// refcount set.
func (db *DB) prepareCommit(ctx context.Context, lib Library, inputs []FileInput, description string, mergeWithHead bool) (preparedCommit, error) {
	prep := preparedCommit{
		libID:  lib.ID,
		parent: lib.HeadCommit,
		blocks: map[blockstore.Hash]int64{},
		stats:  CommitStats{Files: len(inputs)},
	}

	var files []committedFile
	for _, in := range inputs {
		// Rule A, step 1: block bytes are written to the store first.
		manifest, st, err := ingest.Build(db.store, db.ns, in.Reader, in.Path)
		if err != nil {
			return preparedCommit{}, fmt.Errorf("repo: ingest %q: %w", in.Path, err)
		}

		fileObj, size, err := buildFileObject(manifest)
		if err != nil {
			return preparedCommit{}, err
		}
		prep.fileObjs = append(prep.fileObjs, fileObj)
		files = append(files, committedFile{path: in.Path, objHash: fileObj.hash, size: size})

		for _, b := range manifest.Blocks {
			prep.blocks[b.Hash] = int64(b.Size)
		}
		prep.stats.LogicalBytes += st.LogicalBytes
		prep.stats.PhysicalNewBytes += st.PhysicalNewBytes
		prep.stats.TotalBlocks += st.TotalBlocks
	}
	// Stats reflect the NEW inputs only (the upload), even when merging.
	prep.stats.UniqueBlocks = len(prep.blocks)

	if mergeWithHead && lib.HeadCommit != nil {
		carried, err := db.carriedHeadFiles(ctx, *lib.HeadCommit, files, prep.blocks)
		if err != nil {
			return preparedCommit{}, err
		}
		files = append(carried, files...)
	}

	rootTree, treeObjs, err := buildTrees(files)
	if err != nil {
		return preparedCommit{}, err
	}
	prep.treeObjs = treeObjs

	// Microsecond precision matches Postgres timestamptz, so the commit hash we
	// compute here still verifies after a DB round-trip (rule C).
	ctime := db.now().UTC().Truncate(time.Microsecond)
	commit := Commit{
		LibraryID:   lib.ID,
		Parent:      lib.HeadCommit,
		RootTree:    rootTree,
		Ctime:       ctime,
		Description: description,
	}
	_, commitHash, err := encodeCommit(commit)
	if err != nil {
		return preparedCommit{}, err
	}
	commit.Hash = commitHash
	prep.commit = commit
	return prep, nil
}

// carriedHeadFiles loads the head commit's file entries, drops those whose path
// is overridden by a new input (the override = a new version of that path), and
// returns the survivors. The survivors' blocks are merged into blocks — the
// commit's refcount set.
//
// REFCOUNT SYMMETRY (critical): tx1 must increment EVERY distinct block of the
// new tree — carried-over and newly-ingested alike — because GarbageCollect
// decrements by walking a commit's FULL tree. If only the new manifests were
// counted, rolling back and GC'ing this commit would decrement carried-over
// blocks that were never incremented for it, potentially driving a block still
// referenced by an ancestor head to refcount 0 and deleting it from the store.
func (db *DB) carriedHeadFiles(ctx context.Context, headHash blockstore.Hash, newFiles []committedFile, blocks map[blockstore.Hash]int64) ([]committedFile, error) {
	override := make(map[string]bool, len(newFiles))
	for _, f := range newFiles {
		rel, err := normalizeRelPath(f.path)
		if err != nil {
			return nil, err
		}
		override[rel] = true
	}

	head, err := db.getCommit(ctx, db.pool, headHash)
	if err != nil {
		return nil, fmt.Errorf("repo: load head for merge: %w", err)
	}

	var carried []committedFile
	err = walkTree(ctx, db.pool, head.RootTree, func(parentPath string, e TreeEntry) error {
		if e.Type != ObjTypeFile {
			return nil
		}
		rel := strings.TrimPrefix(joinPath(parentPath, e.Name), "/")
		if override[rel] {
			return nil // replaced by a new version from inputs
		}
		carried = append(carried, committedFile{path: rel, objHash: e.Hash, size: e.Size})
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Merge the survivors' blocks into the refcount set (see symmetry note).
	for _, cf := range carried {
		_, content, err := loadObject(ctx, db.pool, cf.objHash)
		if err != nil {
			return nil, fmt.Errorf("repo: load carried file object %s: %w", cf.objHash, err)
		}
		fo, err := decodeFileObject(content)
		if err != nil {
			return nil, err
		}
		for _, b := range fo.Blocks {
			blocks[b.Hash] = b.Size
		}
	}
	return carried, nil
}

// normalizeRelPath cleans a repository path into the canonical relative form
// walkTree produces ("docs/a.txt"), so override matching can't miss on
// formatting differences.
func normalizeRelPath(p string) (string, error) {
	comps, err := splitPath(p)
	if err != nil {
		return "", err
	}
	return strings.Join(comps, "/"), nil
}

// commitObjects is tx1: insert all file and tree objects, then the commit row,
// then (only if the commit is newly inserted) increment block refcounts. The
// commits.root_tree_hash FK guarantees the root tree object is already present.
func (db *DB) commitObjects(ctx context.Context, prep preparedCommit) error {
	return db.inTx(ctx, func(tx pgx.Tx) error {
		for _, o := range prep.fileObjs {
			if err := insertObject(ctx, tx, o); err != nil {
				return err
			}
		}
		for _, o := range prep.treeObjs {
			if err := insertObject(ctx, tx, o); err != nil {
				return err
			}
		}

		isNew, err := insertCommitRow(ctx, tx, prep.commit)
		if err != nil {
			return err
		}
		// Only count refs once per commit; a re-committed identical hash must not
		// double-count.
		if isNew {
			if err := incrBlockRefs(ctx, tx, prep.blocks); err != nil {
				return err
			}
		}
		return nil
	})
}

// publishCommit is tx2: advance the head with a compare-and-swap against the
// expected parent, AND rebuild path_index — both in one transaction (rule B).
// The head FK guarantees the commit row already exists.
func (db *DB) publishCommit(ctx context.Context, prep preparedCommit) error {
	return db.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE libraries SET head_commit_hash = $1
			WHERE id = $2 AND head_commit_hash IS NOT DISTINCT FROM $3`,
			prep.commit.Hash.String(), prep.libID, hashPtrToText(prep.parent))
		if err != nil {
			return fmt.Errorf("repo: advance head: %w", err)
		}
		if tag.RowsAffected() == 0 {
			// Head moved since we read it (concurrent commit) or library vanished.
			return fmt.Errorf("repo: head advance rejected: library %s head changed concurrently", prep.libID)
		}
		return db.updatePathIndex(ctx, tx, prep.libID, prep.commit)
	})
}

// insertCommitRow inserts the commit, returning whether it was newly created
// (false means an identical commit hash already existed — idempotent re-commit).
func insertCommitRow(ctx context.Context, tx pgx.Tx, c Commit) (bool, error) {
	var inserted string
	err := tx.QueryRow(ctx, `
		INSERT INTO commits (commit_hash, library_id, parent_hash, root_tree_hash, ctime, description)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (commit_hash) DO NOTHING
		RETURNING commit_hash`,
		c.Hash.String(), c.LibraryID, hashPtrToText(c.Parent), c.RootTree.String(), c.Ctime, c.Description,
	).Scan(&inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // already existed
	}
	if err != nil {
		return false, fmt.Errorf("repo: insert commit %s: %w", c.Hash, err)
	}
	return true, nil
}

// hashPtrToText converts a nullable hash to a *string for SQL (NULL when nil).
func hashPtrToText(h *blockstore.Hash) *string {
	if h == nil {
		return nil
	}
	s := h.String()
	return &s
}
