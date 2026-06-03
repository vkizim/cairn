package repo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// UploadSession is an in-progress resumable upload backed by a server-named temp
// file.
type UploadSession struct {
	ID            uuid.UUID
	LibraryID     uuid.UUID
	UserID        uuid.UUID
	TargetPath    string
	Filename      string
	DeclaredSize  *int64
	ReceivedBytes int64
	TempPath      string
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

// OffsetConflictError reports that a PATCH arrived at the wrong offset; Current
// is the server's authoritative received_bytes the client should resume from.
type OffsetConflictError struct{ Current int64 }

func (e *OffsetConflictError) Error() string {
	return fmt.Sprintf("repo: upload offset conflict, current offset is %d", e.Current)
}

// ErrUploadTooLarge is returned when a chunk would push the upload past the
// configured maximum total size.
var ErrUploadTooLarge = errors.New("repo: upload exceeds maximum size")

// CreateUploadSession inserts a new upload session row (the temp file is created
// by the caller).
func (db *DB) CreateUploadSession(ctx context.Context, libID, userID uuid.UUID, targetPath, filename string, declaredSize *int64, tempPath string, expiresAt time.Time) (UploadSession, error) {
	u := UploadSession{
		ID: uuid.New(), LibraryID: libID, UserID: userID,
		TargetPath: targetPath, Filename: filename, DeclaredSize: declaredSize,
		TempPath: tempPath, ExpiresAt: expiresAt,
	}
	err := db.pool.QueryRow(ctx, `
		INSERT INTO upload_sessions
			(id, library_id, user_id, target_path, filename, declared_size, temp_path, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING created_at, received_bytes`,
		u.ID, u.LibraryID, u.UserID, u.TargetPath, u.Filename, u.DeclaredSize, u.TempPath, u.ExpiresAt,
	).Scan(&u.CreatedAt, &u.ReceivedBytes)
	if err != nil {
		return UploadSession{}, fmt.Errorf("repo: create upload session: %w", err)
	}
	return u, nil
}

// GetUploadSession loads a non-expired upload session scoped to a library.
// Expired or absent sessions return ErrNotFound (left for the sweep to delete).
func (db *DB) GetUploadSession(ctx context.Context, id, libID uuid.UUID) (UploadSession, error) {
	var u UploadSession
	err := db.pool.QueryRow(ctx, `
		SELECT id, library_id, user_id, target_path, filename, declared_size,
		       received_bytes, temp_path, created_at, expires_at
		FROM upload_sessions WHERE id = $1 AND library_id = $2 AND expires_at > now()`,
		id, libID).
		Scan(&u.ID, &u.LibraryID, &u.UserID, &u.TargetPath, &u.Filename, &u.DeclaredSize,
			&u.ReceivedBytes, &u.TempPath, &u.CreatedAt, &u.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return UploadSession{}, ErrNotFound
	}
	if err != nil {
		return UploadSession{}, fmt.Errorf("repo: get upload session: %w", err)
	}
	return u, nil
}

// AppendUploadChunk appends a chunk to an upload, serialized per session.
//
// The offset check, the positional write, and the counter update all happen
// while holding a row lock (SELECT ... FOR UPDATE) on the upload_sessions row, so
// two concurrent PATCHes at the same offset cannot both succeed: one wins, the
// other blocks and then sees the advanced offset and gets an OffsetConflictError.
// This is DB-level serialization — correct across multiple server processes,
// unlike an in-memory mutex.
//
// body should already be wrapped by the caller in an http.MaxBytesReader for the
// per-chunk cap; maxTotal bounds the cumulative size. Bytes are streamed to the
// temp file (never fully buffered) at the validated offset, so a failed/rolled-
// back write leaves only harmless trailing bytes that the next write overwrites
// and complete ignores (it reads exactly received_bytes).
func (db *DB) AppendUploadChunk(ctx context.Context, id, libID uuid.UUID, clientOffset int64, body io.Reader, maxTotal int64) (int64, error) {
	var newOffset int64
	err := db.inTx(ctx, func(tx pgx.Tx) error {
		var (
			received int64
			tempPath string
		)
		err := tx.QueryRow(ctx, `
			SELECT received_bytes, temp_path FROM upload_sessions
			WHERE id = $1 AND library_id = $2 AND expires_at > now()
			FOR UPDATE`, id, libID).Scan(&received, &tempPath)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}

		if clientOffset != received {
			return &OffsetConflictError{Current: received}
		}
		remaining := maxTotal - received
		if remaining <= 0 {
			return ErrUploadTooLarge
		}

		f, err := os.OpenFile(tempPath, os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("repo: open temp file: %w", err)
		}
		defer f.Close()

		// Read at most remaining+1 bytes: more than remaining means the chunk
		// pushes past the cap.
		n, copyErr := io.Copy(io.NewOffsetWriter(f, received), io.LimitReader(body, remaining+1))
		if copyErr != nil {
			return copyErr // e.g. *http.MaxBytesError -> mapped to 413 by the caller
		}
		if n > remaining {
			return ErrUploadTooLarge
		}
		if err := f.Sync(); err != nil {
			return fmt.Errorf("repo: fsync temp file: %w", err)
		}

		newOffset = received + n
		if _, err := tx.Exec(ctx,
			`UPDATE upload_sessions SET received_bytes = $1 WHERE id = $2`, newOffset, id); err != nil {
			return err
		}
		return nil
	})
	return newOffset, err
}

// DeleteUploadSession removes an upload session row and its temp file. A missing
// temp file is not an error (tolerant of an already-cleaned file).
func (db *DB) DeleteUploadSession(ctx context.Context, u UploadSession) error {
	if err := os.Remove(u.TempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("repo: remove temp file: %w", err)
	}
	if _, err := db.pool.Exec(ctx, `DELETE FROM upload_sessions WHERE id = $1`, u.ID); err != nil {
		return fmt.Errorf("repo: delete upload session: %w", err)
	}
	return nil
}

// SweepExpiredUploads deletes expired upload sessions and their temp files,
// returning the number of sessions removed. Intended to run periodically (e.g.
// `cairn-server sweep-uploads` from cron/a systemd timer, or a future in-process
// ticker). Missing temp files are ignored.
func (db *DB) SweepExpiredUploads(ctx context.Context) (int, error) {
	rows, err := db.pool.Query(ctx, `SELECT id, temp_path FROM upload_sessions WHERE expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("repo: sweep uploads: %w", err)
	}
	type expired struct {
		id   uuid.UUID
		path string
	}
	var stale []expired
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.id, &e.path); err != nil {
			rows.Close()
			return 0, err
		}
		stale = append(stale, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	n := 0
	for _, e := range stale {
		if rmErr := os.Remove(e.path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return n, fmt.Errorf("repo: remove temp %s: %w", e.path, rmErr)
		}
		if _, err := db.pool.Exec(ctx, `DELETE FROM upload_sessions WHERE id = $1`, e.id); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
