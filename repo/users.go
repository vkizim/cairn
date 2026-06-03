package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// User is an authenticated account. password_hash is an argon2id PHC string; the
// repo layer stores and returns it opaquely (hashing/verification live in the
// api layer so repo stays free of crypto policy).
type User struct {
	ID           uuid.UUID
	Username     string
	PasswordHash string
	CreatedAt    time.Time
}

// ErrUsernameTaken is returned by CreateUser on a unique-violation.
var ErrUsernameTaken = errors.New("repo: username already exists")

// CreateUser inserts a user with a pre-computed password hash.
func (db *DB) CreateUser(ctx context.Context, username, passwordHash string) (User, error) {
	u := User{ID: uuid.New(), Username: username, PasswordHash: passwordHash}
	err := db.pool.QueryRow(ctx,
		`INSERT INTO users (id, username, password_hash) VALUES ($1, $2, $3) RETURNING created_at`,
		u.ID, u.Username, u.PasswordHash,
	).Scan(&u.CreatedAt)
	if isUniqueViolation(err) {
		return User{}, ErrUsernameTaken
	}
	if err != nil {
		return User{}, fmt.Errorf("repo: create user: %w", err)
	}
	return u, nil
}

// GetUserByUsername loads a user by username, or ErrNotFound.
func (db *DB) GetUserByUsername(ctx context.Context, username string) (User, error) {
	return scanUser(db.pool.QueryRow(ctx,
		`SELECT id, username, password_hash, created_at FROM users WHERE username = $1`, username))
}

// GetUserByID loads a user by id, or ErrNotFound.
func (db *DB) GetUserByID(ctx context.Context, id uuid.UUID) (User, error) {
	return scanUser(db.pool.QueryRow(ctx,
		`SELECT id, username, password_hash, created_at FROM users WHERE id = $1`, id))
}

func scanUser(row rowScanner) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("repo: scan user: %w", err)
	}
	return u, nil
}
