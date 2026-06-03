package api

import (
	"errors"

	"github.com/vkizim/cairn/repo"
)

// Permission is the kind of access a request needs to a library.
type Permission int

const (
	PermRead  Permission = iota // GET / list / download
	PermWrite                   // upload / commit / mutate
)

// errNoAccess means the current user may not access the library with the
// requested permission. The middleware maps it to 404 (not 403) so a user
// cannot probe for the existence of other users' libraries. A future 403 is
// reserved for the "visible but not writable" case once sharing exists.
var errNoAccess = errors.New("api: no access")

// checkLibraryAccess is THE single authorization seam for libraries. Every
// library route funnels through it (via libraryAccess middleware), and endpoints
// never make their own ownership checks — they receive an already-authorized
// library from the request context.
//
// CURRENT POLICY: owner-only, for both read and write. This is intentionally the
// one place where sharing and fine-grained ACLs will plug in later (consult a
// memberships/permissions table keyed by user+library+perm) WITHOUT touching any
// endpoint or the middleware wiring.
func checkLibraryAccess(user repo.User, lib repo.Library, perm Permission) error {
	_ = perm // read and write are both owner-only today
	if lib.OwnerID == user.ID {
		return nil
	}
	return errNoAccess
}
