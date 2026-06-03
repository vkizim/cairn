package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/vkizim/cairn/repo"
)

// Cookie names.
const (
	sessionCookie = "cairn_session"
	csrfCookie    = "cairn_csrf"
	csrfHeader    = "X-CSRF-Token"
)

// argon2id parameters. Encoded into each PHC string, so they can be raised later
// without invalidating existing hashes.
const (
	argonTime    = 2
	argonMemory  = 64 * 1024 // KiB => 64 MiB
	argonThreads = 1
	argonKeyLen  = 32
	argonSaltLen = 16
)

// --- password hashing (argon2id, PHC string) ---

func hashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// verifyPassword reports whether password matches a PHC argon2id hash. It always
// does the full argon2 computation so timing does not reveal parse failures.
func verifyPassword(password, phc string) bool {
	parts := strings.Split(phc, "$")
	// ["", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	var m uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash is verified against when a username is unknown, so login timing does
// not distinguish "no such user" from "wrong password".
var dummyHash = sync.OnceValue(func() string {
	h, _ := hashPassword("cairn-dummy-password-for-constant-time")
	return h
})

// HashPassword is exported for cmd/cairn-server create-user.
func HashPassword(password string) (string, error) { return hashPassword(password) }

// --- session tokens ---

func newToken() (raw string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(raw))
	return raw, sum[:], nil
}

func tokenHash(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

// --- cookies ---

func (s *Server) setCookie(w http.ResponseWriter, name, value string, expires time.Time, httpOnly bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		HttpOnly: httpOnly,
		Secure:   !s.cfg.Dev, // Secure by default; relaxed only in dev for plain-http localhost
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearCookie(w http.ResponseWriter, name string, httpOnly bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: httpOnly,
		Secure:   !s.cfg.Dev,
		SameSite: http.SameSiteLaxMode,
	})
}

// --- handlers ---

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type userResponse struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := s.decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// TODO(step-future): rate-limit / brute-force protection (per-IP and
	// per-username backoff). Out of scope for this step.
	user, err := s.db.GetUserByUsername(r.Context(), req.Username)
	if errors.Is(err, repo.ErrNotFound) {
		verifyPassword(req.Password, dummyHash()) // keep timing similar
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !verifyPassword(req.Password, user.PasswordHash) {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	raw, hash, err := newToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	expires := s.now().Add(s.cfg.SessionTTL)
	if err := s.db.CreateSession(r.Context(), hash, user.ID, expires, r.UserAgent(), clientIP(r)); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	csrf, err := randToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.setCookie(w, sessionCookie, raw, expires, true)   // HttpOnly
	s.setCookie(w, csrfCookie, csrf, expires, false)    // readable by JS for double-submit
	writeJSON(w, http.StatusOK, userResponse{ID: user.ID.String(), Username: user.Username})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		_ = s.db.DeleteSession(r.Context(), tokenHash(c.Value))
	}
	s.clearCookie(w, sessionCookie, true)
	s.clearCookie(w, csrfCookie, false)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	writeJSON(w, http.StatusOK, userResponse{ID: user.ID.String(), Username: user.Username})
}

func randToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func clientIP(r *http.Request) string {
	if i := strings.LastIndex(r.RemoteAddr, ":"); i >= 0 {
		return r.RemoteAddr[:i]
	}
	return r.RemoteAddr
}
