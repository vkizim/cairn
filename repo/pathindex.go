package repo

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vkizim/cairn/blockstore"
)

// walkTree walks the object graph rooted at rootTree, invoking visit for every
// entry with the path of its parent directory ("/" for the root's children).
// It loads tree objects from q (a pool or transaction).
func walkTree(ctx context.Context, q querier, rootTree blockstore.Hash, visit func(parentPath string, e TreeEntry) error) error {
	return walkTreeRec(ctx, q, "/", rootTree, visit)
}

func walkTreeRec(ctx context.Context, q querier, parentPath string, treeHash blockstore.Hash, visit func(string, TreeEntry) error) error {
	_, content, err := loadObject(ctx, q, treeHash)
	if err != nil {
		return err
	}
	tree, err := decodeTreeObject(content)
	if err != nil {
		return err
	}
	for _, e := range tree.Entries {
		if err := visit(parentPath, e); err != nil {
			return err
		}
		if e.Type == ObjTypeTree {
			if err := walkTreeRec(ctx, q, joinPath(parentPath, e.Name), e.Hash, visit); err != nil {
				return err
			}
		}
	}
	return nil
}

func joinPath(parent, name string) string {
	if parent == "/" {
		return "/" + name
	}
	return parent + "/" + name
}

// updatePathIndex refreshes the derived path_index for a library to match the
// given commit, inside the caller's transaction. It is called from publishCommit
// in the SAME transaction as the head advance (rule B): both apply or neither.
//
// Implementation note: this is a FULL rebuild — O(total files in the library)
// per commit. It is deliberately isolated behind this single function so a
// future incremental diff-apply (parent tree vs new tree -> only the changed
// rows) can replace it WITHOUT changing the tx2 atomicity boundary or rule B.
func (db *DB) updatePathIndex(ctx context.Context, tx pgx.Tx, libID uuid.UUID, c Commit) error {
	return rebuildPathIndexFromCommit(ctx, tx, libID, c)
}

// rebuildPathIndexFromCommit deletes the library's path_index rows and re-inserts
// them by walking the commit's tree. This is the correctness-canonical operation
// and the shared core behind both updatePathIndex and the exported
// RebuildPathIndex repair entry point.
func rebuildPathIndexFromCommit(ctx context.Context, tx pgx.Tx, libID uuid.UUID, c Commit) error {
	if _, err := tx.Exec(ctx, `DELETE FROM path_index WHERE library_id = $1`, libID); err != nil {
		return fmt.Errorf("repo: clear path_index: %w", err)
	}

	batch := &pgx.Batch{}
	err := walkTree(ctx, tx, c.RootTree, func(parentPath string, e TreeEntry) error {
		isDir := e.Type == ObjTypeTree
		var fileObjHash *string
		if !isDir {
			s := e.Hash.String()
			fileObjHash = &s
		}
		batch.Queue(
			`INSERT INTO path_index (library_id, path, name, is_dir, size, mtime, file_obj_hash)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			libID, parentPath, e.Name, isDir, e.Size, c.Ctime, fileObjHash)
		return nil
	})
	if err != nil {
		return err
	}
	if batch.Len() == 0 {
		return nil // empty commit: no entries
	}
	br := tx.SendBatch(ctx, batch)
	if err := br.Close(); err != nil {
		return fmt.Errorf("repo: rebuild path_index: %w", err)
	}
	return nil
}

// RebuildPathIndex rebuilds a library's path_index from the given commit, in its
// own transaction. This is the fsck/repair entry point; the normal commit path
// uses updatePathIndex inside the head-advance transaction.
func (db *DB) RebuildPathIndex(ctx context.Context, libID uuid.UUID, commitHash blockstore.Hash) error {
	c, err := db.getCommit(ctx, db.pool, commitHash)
	if err != nil {
		return err
	}
	return db.inTx(ctx, func(tx pgx.Tx) error {
		return rebuildPathIndexFromCommit(ctx, tx, libID, c)
	})
}

// ListDir returns the entries of a directory from the derived path_index,
// ordered by name. dir defaults to the root ("/" or "").
func (db *DB) ListDir(ctx context.Context, libID uuid.UUID, dir string) ([]PathEntry, error) {
	dir = normalizeDir(dir)
	rows, err := db.pool.Query(ctx, `
		SELECT path, name, is_dir, size, mtime, file_obj_hash
		FROM path_index WHERE library_id = $1 AND path = $2
		ORDER BY name`, libID, dir)
	if err != nil {
		return nil, fmt.Errorf("repo: list %q: %w", dir, err)
	}
	defer rows.Close()

	var out []PathEntry
	for rows.Next() {
		var (
			pe       PathEntry
			fileHash *string
		)
		if err := rows.Scan(&pe.Path, &pe.Name, &pe.IsDir, &pe.Size, &pe.Mtime, &fileHash); err != nil {
			return nil, fmt.Errorf("repo: scan path_index row: %w", err)
		}
		if fileHash != nil {
			h, perr := blockstore.ParseHash(*fileHash)
			if perr != nil {
				return nil, perr
			}
			pe.FileObjHash = &h
		}
		out = append(out, pe)
	}
	return out, rows.Err()
}

// normalizeDir cleans a directory path to its canonical path_index form: a
// leading slash, no trailing slash, root is "/".
func normalizeDir(d string) string {
	if d == "" {
		return "/"
	}
	return path.Clean("/" + strings.TrimPrefix(strings.ReplaceAll(d, `\`, "/"), "/"))
}
