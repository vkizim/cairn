package repo

import (
	"fmt"
	"strings"
)

// Limits on virtual paths/names, to bound path_index rows and reject abuse.
const (
	MaxNameLen = 255
	MaxPathLen = 4096
)

// CleanLibraryPath normalizes a virtual library path to a canonical, rooted,
// forward-slash form ("/", "/a", "/a/b.pdf"), rejecting anything that must never
// reach path_index or the object tree. It is the SINGLE place path handling
// lives — used by upload-create, download, and listing — so traversal and
// invalid-name rules can't drift between call sites.
//
// Rejected: ".." or "." segments, empty/whitespace-only segments (incl. double
// slashes), control characters, and over-long names/paths. Backslashes are
// treated as separators and normalized. The root "/" (or "") is allowed and
// returns "/".
func CleanLibraryPath(p string) (string, error) {
	p = strings.ReplaceAll(p, `\`, "/")
	trimmed := strings.Trim(p, "/")
	if trimmed == "" {
		return "/", nil
	}
	if len(p) > MaxPathLen {
		return "", fmt.Errorf("repo: path too long (%d > %d)", len(p), MaxPathLen)
	}

	segs := strings.Split(trimmed, "/")
	for _, s := range segs {
		if err := validateSegment(s); err != nil {
			return "", err
		}
	}
	return "/" + strings.Join(segs, "/"), nil
}

// ValidateFilename checks a single file/dir name (no separators allowed).
func ValidateFilename(name string) error {
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("repo: filename %q must not contain a path separator", name)
	}
	return validateSegment(name)
}

// SplitLibraryPath cleans p and splits it into the parent directory (path_index
// "path", rooted, e.g. "/docs") and the final name. The path must not be the
// root (a file/dir must have a name).
func SplitLibraryPath(p string) (dir, name string, err error) {
	clean, err := CleanLibraryPath(p)
	if err != nil {
		return "", "", err
	}
	if clean == "/" {
		return "", "", fmt.Errorf("repo: path %q has no name component", p)
	}
	i := strings.LastIndex(clean, "/")
	dir, name = clean[:i], clean[i+1:]
	if dir == "" {
		dir = "/"
	}
	return dir, name, nil
}

func validateSegment(s string) error {
	switch {
	case s == "":
		return fmt.Errorf("repo: empty path segment")
	case strings.TrimSpace(s) == "":
		return fmt.Errorf("repo: whitespace-only path segment")
	case s == "." || s == "..":
		return fmt.Errorf("repo: path segment %q not allowed", s)
	case len(s) > MaxNameLen:
		return fmt.Errorf("repo: name too long (%d > %d)", len(s), MaxNameLen)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("repo: control character in path segment")
		}
	}
	return nil
}
