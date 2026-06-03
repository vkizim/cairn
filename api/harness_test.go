package api_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vkizim/cairn/api"
	"github.com/vkizim/cairn/blockstore"
	"github.com/vkizim/cairn/repo"
)

var (
	baseURL       string
	skipReason    string
	schemaCounter int64
)

func TestMain(m *testing.M) {
	_ = repo.LoadDotEnv(".env")
	_ = repo.LoadDotEnv("../.env")
	if url := os.Getenv("CAIRN_TEST_DATABASE_URL"); url != "" {
		baseURL = url
	} else {
		skipReason = "set CAIRN_TEST_DATABASE_URL to a Postgres DSN to run the api tests " +
			"(e.g. postgres://user:pass@localhost:5552/cairn_test?sslmode=disable)"
	}
	os.Exit(m.Run())
}

// testEnv is an isolated server instance: its own migrated schema, FS block
// store, and httptest server. Dev mode is on so cookies are sent over plain http.
type testEnv struct {
	server *httptest.Server
	db     *repo.DB
	pool   *pgxpool.Pool
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	if baseURL == "" {
		t.Skip(skipReason)
	}
	ctx := context.Background()
	schema := fmt.Sprintf("api_%d_%d", os.Getpid(), atomic.AddInt64(&schemaCounter, 1))

	admin, err := pgxpool.New(ctx, baseURL)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(baseURL)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("scoped pool: %v", err)
	}
	if err := repo.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	store, err := blockstore.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	db := repo.OpenPool(pool, store)

	srv, err := api.NewServer(db, store, api.Config{
		Dev:            true,        // plain-http cookies for httptest
		MaxUploadBytes: 1 << 20,     // 1 MiB total cap (easy to exercise 413)
		MaxChunkBytes:  256 << 10,   // 256 KiB per chunk
		UploadTmpDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())

	t.Cleanup(func() {
		ts.Close()
		pool.Close()
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Logf("drop schema: %v", err)
		}
		admin.Close()
		_ = store.Close()
	})
	return &testEnv{server: ts, db: db, pool: pool}
}

// seedUser creates a user with a known password (hashed like production).
func (e *testEnv) seedUser(t *testing.T, username, password string) repo.User {
	t.Helper()
	hash, err := api.HashPassword(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	u, err := e.db.CreateUser(context.Background(), username, hash)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

// client is a cookie-jar HTTP client against the test server that auto-sets the
// CSRF header (read from the csrf cookie) on mutating requests.
type client struct {
	t    *testing.T
	base string
	hc   *http.Client
}

func (e *testEnv) newClient(t *testing.T) *client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	return &client{t: t, base: e.server.URL, hc: &http.Client{Jar: jar}}
}

func (c *client) csrfToken() string {
	u, _ := http.NewRequest("GET", c.base, nil)
	for _, ck := range c.hc.Jar.Cookies(u.URL) {
		if ck.Name == "cairn_csrf" {
			return ck.Value
		}
	}
	return ""
}

// req issues a request; for non-GET/HEAD it attaches the CSRF header unless
// withCSRF is false (to test rejection). Extra headers may be supplied.
func (c *client) req(method, path string, body io.Reader, withCSRF bool, headers map[string]string) *http.Response {
	c.t.Helper()
	r, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		c.t.Fatalf("new request: %v", err)
	}
	if withCSRF && method != http.MethodGet && method != http.MethodHead {
		if tok := c.csrfToken(); tok != "" {
			r.Header.Set("X-CSRF-Token", tok)
		}
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	resp, err := c.hc.Do(r)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func (c *client) login(username, password string) *http.Response {
	body := bytes.NewBufferString(fmt.Sprintf(`{"username":%q,"password":%q}`, username, password))
	return c.req(http.MethodPost, "/api/login", body, false, map[string]string{"Content-Type": "application/json"})
}

func mustLibrary(t *testing.T, c *client, name string) string {
	t.Helper()
	body := bytes.NewBufferString(fmt.Sprintf(`{"name":%q}`, name))
	resp := c.req(http.MethodPost, "/api/libraries", body, true, map[string]string{"Content-Type": "application/json"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create library: status %d", resp.StatusCode)
	}
	var lib struct {
		ID string `json:"id"`
	}
	decode(t, resp, &lib)
	if _, err := uuid.Parse(lib.ID); err != nil {
		t.Fatalf("bad library id %q", lib.ID)
	}
	return lib.ID
}
