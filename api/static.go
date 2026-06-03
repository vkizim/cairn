package api

import (
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// SPAFileServer serves a single-page app from fsys: if the requested path maps
// to a real file it is served (with correct content-type and caching via
// http.FileServer), otherwise index.html is served so client-side routing can
// handle the path. It is mounted at "/" for non-/api routes.
//
// cmd/cairn-server builds this from the embedded web/dist (web.FS) when compiled
// with -tags embed_spa.
func SPAFileServer(fsys fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(fsys))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Clean the request path to a relative FS path.
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}

		if exists(fsys, name) {
			fileServer.ServeHTTP(w, r)
			return
		}
		serveIndex(w, r, fsys)
	})
}

func exists(fsys fs.FS, name string) bool {
	f, err := fsys.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.IsDir() {
		return false // directories fall back to index.html
	}
	return true
}

func serveIndex(w http.ResponseWriter, r *http.Request, fsys fs.FS) {
	data, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.Error(w, "frontend not built", http.StatusNotFound)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// index.html must not be cached, so new deploys are picked up immediately.
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}
