// Package api is Cairn's HTTP layer: cookie-session auth, CSRF protection, an
// owner-only access seam, REST/JSON metadata endpoints, file download with HTTP
// Range, and a resumable upload protocol — all built on the repo FS engine and
// the block store. It uses the Go 1.22 net/http ServeMux (method+pattern
// routing); no third-party web framework, consistent with the single-binary,
// dependency-light ethos.
package api

import (
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/vkizim/cairn/blockstore"
	"github.com/vkizim/cairn/repo"
)

// Config holds tunables for the server. Zero values are replaced with defaults
// by NewServer.
type Config struct {
	Dev            bool          // relax the Secure cookie attribute for plain-http localhost
	SessionTTL     time.Duration // session lifetime
	UploadTTL      time.Duration // upload-session lifetime before sweep eligibility
	UploadTmpDir   string        // directory for in-progress upload temp files
	MaxJSONBytes   int64         // cap on JSON request bodies
	MaxChunkBytes  int64         // cap on a single PATCH upload chunk
	MaxUploadBytes int64         // cap on a single upload's total size
}

func (c Config) withDefaults() Config {
	if c.SessionTTL == 0 {
		c.SessionTTL = 7 * 24 * time.Hour
	}
	if c.UploadTTL == 0 {
		c.UploadTTL = 24 * time.Hour
	}
	if c.UploadTmpDir == "" {
		c.UploadTmpDir = filepath.Join(os.TempDir(), "cairn-uploads")
	}
	if c.MaxJSONBytes == 0 {
		c.MaxJSONBytes = 1 << 20 // 1 MiB
	}
	if c.MaxChunkBytes == 0 {
		c.MaxChunkBytes = 64 << 20 // 64 MiB
	}
	if c.MaxUploadBytes == 0 {
		c.MaxUploadBytes = 5 << 30 // 5 GiB
	}
	return c
}

// Server bundles the FS engine, the block store, and config behind an
// http.Handler.
type Server struct {
	db    *repo.DB
	store blockstore.Store
	cfg   Config
	now   func() time.Time
}

// NewServer builds a Server. It creates the upload temp directory if needed.
func NewServer(db *repo.DB, store blockstore.Store, cfg Config) (*Server, error) {
	cfg = cfg.withDefaults()
	if err := os.MkdirAll(cfg.UploadTmpDir, 0o700); err != nil {
		return nil, err
	}
	return &Server{db: db, store: store, cfg: cfg, now: time.Now}, nil
}

// Handler returns the root http.Handler: the API mux behind recover middleware,
// with a fall-through for static assets (a no-op hook today; step 4 mounts the
// embedded SPA here).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.routes(mux)

	root := http.NewServeMux()
	root.Handle("/api/", mux)
	root.Handle("/", s.staticHandler())

	return s.recoverMW(root)
}

func (s *Server) routes(mux *http.ServeMux) {
	// Auth.
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.authdCSRF(s.handleLogout))
	mux.HandleFunc("GET /api/me", s.authd(s.handleMe))

	// Libraries.
	mux.HandleFunc("GET /api/libraries", s.authd(s.handleListLibraries))
	mux.HandleFunc("POST /api/libraries", s.authdCSRF(s.handleCreateLibrary))
	mux.HandleFunc("GET /api/libraries/{id}", s.libRead(s.handleGetLibrary))
	mux.HandleFunc("GET /api/libraries/{id}/files", s.libRead(s.handleListFiles))
	mux.HandleFunc("GET /api/libraries/{id}/files/download", s.libRead(s.handleDownload))
	mux.HandleFunc("GET /api/libraries/{id}/commits", s.libRead(s.handleCommits))
	mux.HandleFunc("GET /api/libraries/{id}/fsck", s.libRead(s.handleFsck))

	// Resumable uploads (writes).
	mux.HandleFunc("POST /api/libraries/{id}/uploads", s.libWrite(s.handleCreateUpload))
	mux.HandleFunc("HEAD /api/libraries/{id}/uploads/{uploadId}", s.libRead(s.handleHeadUpload))
	mux.HandleFunc("PATCH /api/libraries/{id}/uploads/{uploadId}", s.libWrite(s.handlePatchUpload))
	mux.HandleFunc("POST /api/libraries/{id}/uploads/{uploadId}/complete", s.libWrite(s.handleCompleteUpload))
	mux.HandleFunc("DELETE /api/libraries/{id}/uploads/{uploadId}", s.libWrite(s.handleDeleteUpload))
}

// staticHandler is the SPA fall-through hook. Today it returns 404 for non-API
// paths; in step 4 this is where an embed.FS-backed file server (with SPA
// index.html fallback) gets mounted. No frontend assets exist yet.
func (s *Server) staticHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
}
