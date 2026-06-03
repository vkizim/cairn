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
func (db *DB) CommitFiles(ctx context.Context, libID uuid.UUID, inputs []FileInput, description string) (CommitResult, error) {
	lib, err := db.GetLibrary(ctx, libID)
	if err != nil {
		return CommitResult{}, err
	}

	prep, err := db.prepareCommit(ctx, lib, inputs, description)
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
// metadata writes.
func (db *DB) prepareCommit(ctx context.Context, lib Library, inputs []FileInput, description string) (preparedCommit, error) {
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
	prep.stats.UniqueBlocks = len(prep.blocks)

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
