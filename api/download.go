package api

import (
	"errors"
	"net/http"

	"github.com/vkizim/cairn/repo"
)

// handleDownload streams a file's reassembled bytes. It delegates to
// http.ServeContent over repo.OpenFile's io.ReadSeeker, which gives HTTP Range
// requests (206 Partial Content), If-Range, and content-type sniffing for free —
// important for seeking large media.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	lib := libraryFromContext(r.Context())

	// Validate the virtual path up front (centralized rules); OpenFile also
	// splits/cleans it, but rejecting here yields a clean 400.
	if _, err := repo.CleanLibraryPath(r.URL.Query().Get("path")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}

	fr, err := s.db.OpenFile(r.Context(), lib.ID, r.URL.Query().Get("path"))
	switch {
	case errors.Is(err, repo.ErrNotFound):
		writeError(w, http.StatusNotFound, "file not found")
		return
	case errors.Is(err, repo.ErrIsDirectory):
		writeError(w, http.StatusBadRequest, "path is a directory")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	http.ServeContent(w, r, fr.Name(), fr.ModTime(), fr)
}
