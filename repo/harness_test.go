package repo

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/vkizim/cairn/blockstore"
)

// baseURL is a Postgres connection we can create per-test schemas in. It comes
// from CAIRN_TEST_DATABASE_URL if set, otherwise from a testcontainers Postgres
// (when Docker is available). When neither is available, tests skip.
var (
	baseURL       string
	skipReason    string
	schemaCounter int64
)

// TestMain resolves where Postgres comes from. Per the agreed policy:
//   - CAIRN_TEST_DATABASE_URL set  -> use it; never start a container.
//   - else Docker available        -> start a throwaway Postgres container.
//   - else                         -> tests t.Skip with guidance (no hard fail).
func TestMain(m *testing.M) {
	// Convenience for local runs: load .env from the package dir or repo root.
	_ = LoadDotEnv(".env")
	_ = LoadDotEnv("../.env")

	var cleanup func()
	if url := os.Getenv("CAIRN_TEST_DATABASE_URL"); url != "" {
		baseURL = url
	} else if url, term, err := startContainer(); err == nil {
		baseURL, cleanup = url, term
	} else {
		skipReason = "set CAIRN_TEST_DATABASE_URL to a Postgres DSN " +
			"(e.g. postgres://user:pass@localhost:5552/cairn_test?sslmode=disable) " +
			"or start Docker for testcontainers; container start failed: " + err.Error()
	}

	code := m.Run()
	if cleanup != nil {
		cleanup()
	}
	os.Exit(code)
}

func startContainer() (string, func(), error) {
	ctx := context.Background()
	ctr, err := postgres.Run(ctx, "postgres:18-alpine",
		postgres.WithDatabase("cairn_test"),
		postgres.WithUsername("cairn"),
		postgres.WithPassword("cairn"),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort("5432/tcp").WithStartupTimeout(90*time.Second)),
	)
	if err != nil {
		return "", nil, err
	}
	url, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = ctr.Terminate(ctx)
		return "", nil, err
	}
	return url, func() { _ = ctr.Terminate(context.Background()) }, nil
}

// newTestDB returns a DB backed by an isolated, freshly-migrated schema on the
// shared Postgres, plus an FS block store in a temp dir. Everything is dropped on
// test cleanup. Tests skip cleanly when no Postgres is configured.
//
// The clock is pinned to deterministic, strictly-increasing timestamps so commit
// hashes are stable within a test.
func newTestDB(t *testing.T) *DB {
	t.Helper()
	if baseURL == "" {
		t.Skip(skipReason)
	}
	ctx := context.Background()

	schema := fmt.Sprintf("ct_%d_%d", os.Getpid(), atomic.AddInt64(&schemaCounter, 1))

	admin, err := pgxpool.New(ctx, baseURL)
	if err != nil {
		t.Fatalf("connect admin pool: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatalf("create schema %s: %v", schema, err)
	}

	cfg, err := pgxpool.ParseConfig(baseURL)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	// All connections in this pool resolve unqualified names to the test schema.
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open scoped pool: %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	store, err := blockstore.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("block store: %v", err)
	}

	db := fromPool(pool, store)
	tick := int64(0)
	clockBase := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	db.now = func() time.Time {
		tick++
		return clockBase.Add(time.Duration(tick) * time.Second)
	}

	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Logf("cleanup: drop schema %s: %v", schema, err)
		}
		admin.Close()
		_ = store.Close()
	})
	return db
}
