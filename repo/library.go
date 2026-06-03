package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vkizim/cairn/blockstore"
)

// ErrNotFound is returned when a library or commit does not exist.
var ErrNotFound = errors.New("repo: not found")

// CreateLibrary inserts a new, empty library (no commits, nil head) owned by the
// given user.
func (db *DB) CreateLibrary(ctx context.Context, name string, ownerID uuid.UUID) (Library, error) {
	lib := Library{ID: uuid.New(), Name: name, OwnerID: ownerID}
	err := db.pool.QueryRow(ctx,
		`INSERT INTO libraries (id, name, owner_id) VALUES ($1, $2, $3) RETURNING created_at`,
		lib.ID, lib.Name, lib.OwnerID,
	).Scan(&lib.CreatedAt)
	if err != nil {
		return Library{}, fmt.Errorf("repo: create library: %w", err)
	}
	return lib, nil
}

// GetLibrary loads a library by id.
func (db *DB) GetLibrary(ctx context.Context, id uuid.UUID) (Library, error) {
	return scanLibrary(db.pool.QueryRow(ctx, `
		SELECT id, name, owner_id, head_commit_hash, encrypted, created_at
		FROM libraries WHERE id = $1`, id))
}

// ListLibrariesByOwner returns the libraries owned by a user, oldest first.
func (db *DB) ListLibrariesByOwner(ctx context.Context, ownerID uuid.UUID) ([]Library, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id, name, owner_id, head_commit_hash, encrypted, created_at
		FROM libraries WHERE owner_id = $1 ORDER BY created_at`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("repo: list libraries: %w", err)
	}
	defer rows.Close()

	var out []Library
	for rows.Next() {
		lib, err := scanLibrary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, lib)
	}
	return out, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanLibrary(row rowScanner) (Library, error) {
	var (
		lib  Library
		head *string
	)
	err := row.Scan(&lib.ID, &lib.Name, &lib.OwnerID, &head, &lib.Encrypted, &lib.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Library{}, ErrNotFound
	}
	if err != nil {
		return Library{}, fmt.Errorf("repo: scan library: %w", err)
	}
	if head != nil {
		h, perr := blockstore.ParseHash(*head)
		if perr != nil {
			return Library{}, fmt.Errorf("repo: corrupt head hash for library %s: %w", lib.ID, perr)
		}
		lib.HeadCommit = &h
	}
	return lib, nil
}

// History returns the commit chain of a library, newest first, by following
// parent links from the head. The commit chain is the source of truth for
// history (path_index is only a derived cache).
func (db *DB) History(ctx context.Context, id uuid.UUID) ([]Commit, error) {
	lib, err := db.GetLibrary(ctx, id)
	if err != nil {
		return nil, err
	}
	var history []Commit
	cur := lib.HeadCommit
	for cur != nil {
		c, err := db.getCommit(ctx, db.pool, *cur)
		if err != nil {
			return nil, err
		}
		history = append(history, c)
		cur = c.Parent
	}
	return history, nil
}

// getCommit loads a single commit row via q (a pool or transaction).
func (db *DB) getCommit(ctx context.Context, q querier, h blockstore.Hash) (Commit, error) {
	var (
		c          Commit
		parent     *string
		rootTree   string
		commitHash string
	)
	err := q.QueryRow(ctx, `
		SELECT commit_hash, library_id, parent_hash, root_tree_hash, ctime, description
		FROM commits WHERE commit_hash = $1`, h.String()).
		Scan(&commitHash, &c.LibraryID, &parent, &rootTree, &c.Ctime, &c.Description)
	if errors.Is(err, pgx.ErrNoRows) {
		return Commit{}, ErrNotFound
	}
	if err != nil {
		return Commit{}, fmt.Errorf("repo: get commit %s: %w", h, err)
	}
	if c.Hash, err = blockstore.ParseHash(commitHash); err != nil {
		return Commit{}, err
	}
	if c.RootTree, err = blockstore.ParseHash(rootTree); err != nil {
		return Commit{}, err
	}
	if parent != nil {
		p, perr := blockstore.ParseHash(*parent)
		if perr != nil {
			return Commit{}, perr
		}
		c.Parent = &p
	}
	return c, nil
}

// querier is satisfied by both *pgxpool.Pool and pgx.Tx, so helpers can run
// against either a connection pool or an open transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}
