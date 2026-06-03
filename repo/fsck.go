package repo

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vkizim/cairn/blockstore"
)

// FsckReport is the outcome of validating a library (rule C).
type FsckReport struct {
	LibraryID  uuid.UUID
	HeadCommit *blockstore.Hash // current head (nil = empty library)
	// HeadConsistent is true when the current head is fully verifiable (or the
	// library is empty).
	HeadConsistent bool
	// LastConsistentCommit is the commit the head should roll back to when the
	// head is inconsistent: the most recent fully-verifiable commit. nil means no
	// consistent commit exists (recovery would empty the head).
	LastConsistentCommit *blockstore.Hash
	// Problems lists what was wrong, for human consumption.
	Problems []string
}

// VerifyObject recomputes an object's hash from its stored canonical bytes and
// reports whether it matches its address (the core self-verification of rule C).
func (db *DB) VerifyObject(ctx context.Context, h blockstore.Hash) (bool, error) {
	_, content, err := loadObject(ctx, db.pool, h)
	if errors.Is(err, ErrNotFound) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	return VerifyContent(content, h), nil
}

// Fsck validates a library's head commit and everything reachable from it: every
// object hashes to its address, and every referenced block is present in the
// store. If the head is not fully consistent it locates the most recent commit
// that is, so the caller (or Recover) can roll back to it. Fsck does not mutate.
func (db *DB) Fsck(ctx context.Context, libID uuid.UUID) (FsckReport, error) {
	lib, err := db.GetLibrary(ctx, libID)
	if err != nil {
		return FsckReport{}, err
	}
	report := FsckReport{LibraryID: libID, HeadCommit: lib.HeadCommit}

	if lib.HeadCommit == nil {
		report.HeadConsistent = true // empty library is trivially consistent
		return report, nil
	}

	commits, err := db.loadAllCommits(ctx, libID)
	if err != nil {
		return FsckReport{}, err
	}

	// Determine the order in which to look for the last consistent commit.
	// Normally walk the parent chain from head; if the head row itself is missing
	// (dangling head), consider all commits deepest-first as candidate tips.
	var order []Commit
	if _, ok := commits[*lib.HeadCommit]; ok {
		for cur := lib.HeadCommit; cur != nil; {
			c, ok := commits[*cur]
			if !ok {
				break
			}
			order = append(order, c)
			cur = c.Parent
		}
	} else {
		report.Problems = append(report.Problems,
			fmt.Sprintf("head points at missing commit %s", lib.HeadCommit))
		order = commitsByDepthDesc(commits)
	}

	for _, c := range order {
		probs := db.verifyCommit(ctx, db.pool, c)
		if len(probs) == 0 {
			h := c.Hash
			report.LastConsistentCommit = &h
			break
		}
		// Record problems for the head commit specifically (most useful signal).
		if c.Hash == *lib.HeadCommit {
			report.Problems = append(report.Problems, probs...)
		}
	}

	report.HeadConsistent = report.LastConsistentCommit != nil && *report.LastConsistentCommit == *lib.HeadCommit
	return report, nil
}

// verifyCommit returns the list of problems with a commit's snapshot: a bad
// commit hash, any missing/corrupt object, or any missing block. Empty means the
// commit is fully consistent.
func (db *DB) verifyCommit(ctx context.Context, q querier, c Commit) []string {
	var probs []string

	if _, h, err := encodeCommit(c); err != nil {
		probs = append(probs, fmt.Sprintf("commit %s: encode error: %v", c.Hash, err))
	} else if h != c.Hash {
		probs = append(probs, fmt.Sprintf("commit %s: content hash mismatch (recomputed %s)", c.Hash, h))
	}

	probs = append(probs, db.verifyTreeClosure(ctx, q, c.RootTree)...)
	return probs
}

func (db *DB) verifyTreeClosure(ctx context.Context, q querier, treeHash blockstore.Hash) []string {
	typ, content, err := loadObject(ctx, q, treeHash)
	if errors.Is(err, ErrNotFound) {
		return []string{fmt.Sprintf("tree object %s missing", treeHash)}
	}
	if err != nil {
		return []string{fmt.Sprintf("tree object %s: %v", treeHash, err)}
	}

	var probs []string
	if !VerifyContent(content, treeHash) {
		probs = append(probs, fmt.Sprintf("tree object %s: content hash mismatch", treeHash))
	}
	if typ != ObjTypeTree {
		probs = append(probs, fmt.Sprintf("object %s: expected tree, got %q", treeHash, typ))
	}
	tree, err := decodeTreeObject(content)
	if err != nil {
		return append(probs, fmt.Sprintf("tree object %s: %v", treeHash, err))
	}
	for _, e := range tree.Entries {
		switch e.Type {
		case ObjTypeTree:
			probs = append(probs, db.verifyTreeClosure(ctx, q, e.Hash)...)
		case ObjTypeFile:
			probs = append(probs, db.verifyFileObject(ctx, q, e.Hash)...)
		default:
			probs = append(probs, fmt.Sprintf("tree entry %q: unknown type %q", e.Name, e.Type))
		}
	}
	return probs
}

func (db *DB) verifyFileObject(ctx context.Context, q querier, fileHash blockstore.Hash) []string {
	typ, content, err := loadObject(ctx, q, fileHash)
	if errors.Is(err, ErrNotFound) {
		return []string{fmt.Sprintf("file object %s missing", fileHash)}
	}
	if err != nil {
		return []string{fmt.Sprintf("file object %s: %v", fileHash, err)}
	}

	var probs []string
	if !VerifyContent(content, fileHash) {
		probs = append(probs, fmt.Sprintf("file object %s: content hash mismatch", fileHash))
	}
	if typ != ObjTypeFile {
		probs = append(probs, fmt.Sprintf("object %s: expected file, got %q", fileHash, typ))
	}
	fo, err := decodeFileObject(content)
	if err != nil {
		return append(probs, fmt.Sprintf("file object %s: %v", fileHash, err))
	}
	for _, b := range fo.Blocks {
		ok, err := db.store.Exists(db.ns, b.Hash)
		if err != nil {
			probs = append(probs, fmt.Sprintf("block %s: %v", b.Hash, err))
		} else if !ok {
			probs = append(probs, fmt.Sprintf("block %s missing from store", b.Hash))
		}
	}
	return probs
}

// Recover runs Fsck and, if the head is inconsistent, rolls it back to the last
// consistent commit (or empties it if none), rebuilding path_index. It returns
// the report describing what was found.
func (db *DB) Recover(ctx context.Context, libID uuid.UUID) (FsckReport, error) {
	report, err := db.Fsck(ctx, libID)
	if err != nil {
		return FsckReport{}, err
	}
	if report.HeadConsistent {
		return report, nil
	}
	if err := db.RollbackHead(ctx, libID, report.LastConsistentCommit); err != nil {
		return report, err
	}
	return report, nil
}

// RollbackHead forcibly sets the library head to target (nil = empty) and
// rebuilds path_index to match — atomically, in one transaction (rule B). Unlike
// the commit path it does not compare-and-swap; it is an explicit repair action.
func (db *DB) RollbackHead(ctx context.Context, libID uuid.UUID, target *blockstore.Hash) error {
	return db.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE libraries SET head_commit_hash = $1 WHERE id = $2`,
			hashPtrToText(target), libID); err != nil {
			return fmt.Errorf("repo: rollback head: %w", err)
		}
		if target == nil {
			_, err := tx.Exec(ctx, `DELETE FROM path_index WHERE library_id = $1`, libID)
			return err
		}
		c, err := db.getCommit(ctx, tx, *target)
		if err != nil {
			return err
		}
		return rebuildPathIndexFromCommit(ctx, tx, libID, c)
	})
}

// GCResult summarizes a garbage collection pass.
type GCResult struct {
	OrphanCommitsDeleted int
	BlocksDecremented    int
	BlocksDeleted        int // blocks whose refcount reached 0 and were removed
}

// GarbageCollect reclaims commits not reachable from the library head (e.g. those
// orphaned by a crash between tx1 and tx2, or abandoned by a rollback). For each
// orphan commit it decrements its distinct blocks' refcounts and deletes the
// commit row; blocks reaching refcount 0 are removed from the blocks table and
// the store. Ancestors of the head are never collected.
//
// Note: this assumes no concurrent committer is racing the same blocks. Block
// byte deletion happens after the metadata transaction commits; hardening GC
// against concurrent writers (e.g. via locking) is future work.
func (db *DB) GarbageCollect(ctx context.Context, libID uuid.UUID) (GCResult, error) {
	lib, err := db.GetLibrary(ctx, libID)
	if err != nil {
		return GCResult{}, err
	}
	commits, err := db.loadAllCommits(ctx, libID)
	if err != nil {
		return GCResult{}, err
	}

	reachable := map[blockstore.Hash]bool{}
	for cur := lib.HeadCommit; cur != nil; {
		c, ok := commits[*cur]
		if !ok {
			break
		}
		reachable[*cur] = true
		cur = c.Parent
	}

	var orphans []Commit
	for h, c := range commits {
		if !reachable[h] {
			orphans = append(orphans, c)
		}
	}
	// Delete deepest commits first so a parent is never removed before its child
	// (the parent_hash self-FK).
	depth := commitDepths(commits)
	sort.Slice(orphans, func(i, j int) bool {
		return depth[orphans[i].Hash] > depth[orphans[j].Hash]
	})

	var result GCResult
	touched := map[blockstore.Hash]int64{} // block -> size, for zero-ref cleanup
	zeroRef := map[blockstore.Hash]bool{}

	err = db.inTx(ctx, func(tx pgx.Tx) error {
		for _, c := range orphans {
			blocks, err := db.collectCommitBlocks(ctx, tx, c.RootTree)
			if err != nil {
				return err
			}
			hashes := make([]blockstore.Hash, 0, len(blocks))
			for h, sz := range blocks {
				hashes = append(hashes, h)
				touched[h] = sz
			}
			if err := decrBlockRefs(ctx, tx, hashes); err != nil {
				return err
			}
			result.BlocksDecremented += len(hashes)

			if _, err := tx.Exec(ctx, `DELETE FROM commits WHERE commit_hash = $1`, c.Hash.String()); err != nil {
				return fmt.Errorf("repo: delete orphan commit %s: %w", c.Hash, err)
			}
			result.OrphanCommitsDeleted++
		}

		// Within the same tx, drop block rows that fell to zero.
		for h := range touched {
			var rc int64
			if err := tx.QueryRow(ctx, `SELECT refcount FROM blocks WHERE block_hash = $1`, h.String()).Scan(&rc); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					continue
				}
				return err
			}
			if rc == 0 {
				if _, err := tx.Exec(ctx, `DELETE FROM blocks WHERE block_hash = $1`, h.String()); err != nil {
					return err
				}
				zeroRef[h] = true
			}
		}
		return nil
	})
	if err != nil {
		return GCResult{}, err
	}

	// After the metadata tx commits, delete the now-unreferenced block bytes.
	for h := range zeroRef {
		if err := db.store.Delete(db.ns, h); err != nil {
			return result, fmt.Errorf("repo: delete block %s from store: %w", h, err)
		}
		result.BlocksDeleted++
	}
	return result, nil
}

// loadAllCommits loads every commit of a library into a map keyed by hash.
func (db *DB) loadAllCommits(ctx context.Context, libID uuid.UUID) (map[blockstore.Hash]Commit, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT commit_hash, library_id, parent_hash, root_tree_hash, ctime, description
		FROM commits WHERE library_id = $1`, libID)
	if err != nil {
		return nil, fmt.Errorf("repo: load commits: %w", err)
	}
	defer rows.Close()

	out := map[blockstore.Hash]Commit{}
	for rows.Next() {
		var (
			c                    Commit
			commitHash, rootTree string
			parent               *string
		)
		if err := rows.Scan(&commitHash, &c.LibraryID, &parent, &rootTree, &c.Ctime, &c.Description); err != nil {
			return nil, err
		}
		if c.Hash, err = blockstore.ParseHash(commitHash); err != nil {
			return nil, err
		}
		if c.RootTree, err = blockstore.ParseHash(rootTree); err != nil {
			return nil, err
		}
		if parent != nil {
			p, perr := blockstore.ParseHash(*parent)
			if perr != nil {
				return nil, perr
			}
			c.Parent = &p
		}
		out[c.Hash] = c
	}
	return out, rows.Err()
}

// commitDepths returns each commit's distance from a root (root = 1), following
// parent links that exist in the map.
func commitDepths(commits map[blockstore.Hash]Commit) map[blockstore.Hash]int {
	depth := map[blockstore.Hash]int{}
	var compute func(h blockstore.Hash) int
	compute = func(h blockstore.Hash) int {
		if d, ok := depth[h]; ok {
			return d
		}
		c, ok := commits[h]
		if !ok {
			return 0
		}
		d := 1
		if c.Parent != nil {
			d = compute(*c.Parent) + 1
		}
		depth[h] = d
		return d
	}
	for h := range commits {
		compute(h)
	}
	return depth
}

// commitsByDepthDesc orders commits deepest-first (ties broken by newer ctime),
// used to pick a recovery tip when the head is dangling.
func commitsByDepthDesc(commits map[blockstore.Hash]Commit) []Commit {
	depth := commitDepths(commits)
	out := make([]Commit, 0, len(commits))
	for _, c := range commits {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		di, dj := depth[out[i].Hash], depth[out[j].Hash]
		if di != dj {
			return di > dj
		}
		return out[i].Ctime.After(out[j].Ctime)
	})
	return out
}
