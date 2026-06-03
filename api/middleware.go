package api

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/google/uuid"

	"github.com/vkizim/cairn/repo"
)

type ctxKey int

const (
	ctxUserKey ctxKey = iota
	ctxLibraryKey
)

func userFromContext(ctx context.Context) repo.User {
	u, _ := ctx.Value(ctxUserKey).(repo.User)
	return u
}

func libraryFromContext(ctx context.Context) repo.Library {
	l, _ := ctx.Value(ctxLibraryKey).(repo.Library)
	return l
}

// recoverMW turns a panic in any handler into a 500 instead of crashing the
// server, and logs it.
func (s *Server) recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic serving %s %s: %v", r.Method, r.URL.Path, rec)
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// session loads the session + user from the cookie into the request context, or
// 401s. It runs after mux routing so wrapped handlers still see path values.
func (s *Server) session(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || c.Value == "" {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		sess, err := s.db.LookupSession(r.Context(), tokenHash(c.Value))
		if errors.Is(err, repo.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "session invalid or expired")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		user, err := s.db.GetUserByID(r.Context(), sess.UserID)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "session invalid or expired")
			return
		}
		ctx := context.WithValue(r.Context(), ctxUserKey, user)
		next(w, r.WithContext(ctx))
	}
}

// csrf enforces the double-submit token on state-changing requests: the
// X-CSRF-Token header must equal the (non-HttpOnly) CSRF cookie. Must be applied
// inside session (after auth). GET/HEAD are never wrapped with it.
func (s *Server) csrf(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(csrfCookie)
		header := r.Header.Get(csrfHeader)
		if err != nil || cookie.Value == "" || header == "" || header != cookie.Value {
			writeError(w, http.StatusForbidden, "invalid or missing CSRF token")
			return
		}
		next(w, r)
	}
}

// libraryAccess resolves {id}, loads the library, runs the single access seam
// (checkLibraryAccess) for perm, and stashes the authorized library in context.
// Must be applied inside session. A library that doesn't exist OR isn't
// accessible both return 404 (don't reveal existence).
func (s *Server) libraryAccess(perm Permission, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusNotFound, "library not found")
			return
		}
		lib, err := s.db.GetLibrary(r.Context(), id)
		if errors.Is(err, repo.ErrNotFound) {
			writeError(w, http.StatusNotFound, "library not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		user := userFromContext(r.Context())
		if err := checkLibraryAccess(user, lib, perm); err != nil {
			writeError(w, http.StatusNotFound, "library not found") // 403 reserved for future sharing
			return
		}
		ctx := context.WithValue(r.Context(), ctxLibraryKey, lib)
		next(w, r.WithContext(ctx))
	}
}

// --- route composition helpers ---

func (s *Server) authd(h http.HandlerFunc) http.HandlerFunc      { return s.session(h) }
func (s *Server) authdCSRF(h http.HandlerFunc) http.HandlerFunc  { return s.session(s.csrf(h)) }
func (s *Server) libRead(h http.HandlerFunc) http.HandlerFunc    { return s.session(s.libraryAccess(PermRead, h)) }
func (s *Server) libWrite(h http.HandlerFunc) http.HandlerFunc {
	return s.session(s.csrf(s.libraryAccess(PermWrite, h)))
}
