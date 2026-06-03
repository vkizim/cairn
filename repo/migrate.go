package repo

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate applies any embedded SQL migrations that have not yet been recorded in
// the schema_migrations table, in filename order. Each migration file is wrapped
// in a single transaction together with its bookkeeping insert, so a file either
// applies completely or not at all.
//
// This is a deliberately dependency-light alternative to golang-migrate: a
// handful of forward-only SQL files plus a version table is enough for Cairn and
// keeps the binary self-contained.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("repo: ensure schema_migrations: %w", err)
	}

	applied, err := appliedVersions(ctx, pool)
	if err != nil {
		return err
	}

	files, err := migrationFiles()
	if err != nil {
		return err
	}

	for _, name := range files {
		if applied[name] {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("repo: read migration %s: %w", name, err)
		}
		if err := applyMigration(ctx, pool, name, string(body)); err != nil {
			return err
		}
	}
	return nil
}

func appliedVersions(ctx context.Context, pool *pgxpool.Pool) (map[string]bool, error) {
	rows, err := pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("repo: read schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := map[string]bool{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

func migrationFiles() ([]string, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("repo: list migrations: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// applyMigration runs one migration file and records it, atomically. The whole
// file may contain many statements, so it is executed via the simple query
// protocol (BEGIN ... COMMIT wrapping). If any statement fails, COMMIT is never
// reached and Postgres rolls the transaction back.
func applyMigration(ctx context.Context, pool *pgxpool.Pool, name, body string) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("repo: acquire conn for migration %s: %w", name, err)
	}
	defer conn.Release()

	// name is an embedded filename we control (no quotes); still escape defensively.
	safeName := strings.ReplaceAll(name, "'", "''")
	batch := "BEGIN;\n" + body +
		"\nINSERT INTO schema_migrations (version) VALUES ('" + safeName + "');\nCOMMIT;\n"

	mrr := conn.Conn().PgConn().Exec(ctx, batch)
	if err := mrr.Close(); err != nil {
		return fmt.Errorf("repo: apply migration %s: %w", name, err)
	}
	return nil
}
