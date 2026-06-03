package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Session is a cookie session. The raw token is never stored; sessions are keyed
// by sha256(token), which the api layer computes.
type Session struct {
	UserID    uuid.UUID
	CreatedAt time.Time
	ExpiresAt time.Time
}

// CreateSession inserts a session keyed by tokenHash (the sha256 of the raw
// token), expiring at expiresAt. userAgent/ip are optional audit fields.
func (db *DB) CreateSession(ctx context.Context, tokenHash []byte, userID uuid.UUID, expiresAt time.Time, userAgent, ip string) error {
	_, err := db.pool.Exec(ctx, `
		INSERT INTO sessions (token_hash, user_id, expires_at, user_agent, ip)
		VALUES ($1, $2, $3, $4, $5)`,
		tokenHash, userID, expiresAt, nullIfEmpty(userAgent), nullIfEmpty(ip))
	if err != nil {
		return fmt.Errorf("repo: create session: %w", err)
	}
	return nil
}

// LookupSession returns the session for tokenHash if it exists and has not
// expired; expired or absent sessions return ErrNotFound. Expired rows are left
// for the sweep to delete.
func (db *DB) LookupSession(ctx context.Context, tokenHash []byte) (Session, error) {
	var s Session
	err := db.pool.QueryRow(ctx, `
		SELECT user_id, created_at, expires_at FROM sessions
		WHERE token_hash = $1 AND expires_at > now()`, tokenHash).
		Scan(&s.UserID, &s.CreatedAt, &s.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("repo: lookup session: %w", err)
	}
	return s, nil
}

// DeleteSession removes a session by token hash (idempotent).
func (db *DB) DeleteSession(ctx context.Context, tokenHash []byte) error {
	if _, err := db.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, tokenHash); err != nil {
		return fmt.Errorf("repo: delete session: %w", err)
	}
	return nil
}

// SweepExpiredSessions deletes expired session rows and returns the count.
func (db *DB) SweepExpiredSessions(ctx context.Context) (int64, error) {
	tag, err := db.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("repo: sweep sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
