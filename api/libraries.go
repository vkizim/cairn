package api

import (
	"net/http"
	"time"

	"github.com/vkizim/cairn/repo"
)

type libraryResponse struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	OwnerID    string  `json:"owner_id"`
	HeadCommit *string `json:"head_commit"`
	Encrypted  bool    `json:"encrypted"`
	CreatedAt  string  `json:"created_at"`
}

func toLibraryResponse(l repo.Library) libraryResponse {
	resp := libraryResponse{
		ID:        l.ID.String(),
		Name:      l.Name,
		OwnerID:   l.OwnerID.String(),
		Encrypted: l.Encrypted,
		CreatedAt: l.CreatedAt.UTC().Format(time.RFC3339),
	}
	if l.HeadCommit != nil {
		h := l.HeadCommit.String()
		resp.HeadCommit = &h
	}
	return resp
}

func (s *Server) handleListLibraries(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	libs, err := s.db.ListLibrariesByOwner(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]libraryResponse, 0, len(libs))
	for _, l := range libs {
		out = append(out, toLibraryResponse(l))
	}
	writeJSON(w, http.StatusOK, out)
}

type createLibraryRequest struct {
	Name string `json:"name"`
}

func (s *Server) handleCreateLibrary(w http.ResponseWriter, r *http.Request) {
	var req createLibraryRequest
	if err := s.decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	user := userFromContext(r.Context())
	lib, err := s.db.CreateLibrary(r.Context(), req.Name, user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, toLibraryResponse(lib))
}

func (s *Server) handleGetLibrary(w http.ResponseWriter, r *http.Request) {
	lib := libraryFromContext(r.Context())
	writeJSON(w, http.StatusOK, toLibraryResponse(lib))
}

type pathEntryResponse struct {
	Name  string  `json:"name"`
	IsDir bool    `json:"is_dir"`
	Size  int64   `json:"size"`
	Mtime *string `json:"mtime"`
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	lib := libraryFromContext(r.Context())
	dir, err := repo.CleanLibraryPath(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}
	entries, err := s.db.ListDir(r.Context(), lib.ID, dir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]pathEntryResponse, 0, len(entries))
	for _, e := range entries {
		pe := pathEntryResponse{Name: e.Name, IsDir: e.IsDir, Size: e.Size}
		if e.Mtime != nil {
			m := e.Mtime.UTC().Format(time.RFC3339)
			pe.Mtime = &m
		}
		out = append(out, pe)
	}
	writeJSON(w, http.StatusOK, out)
}

type commitResponse struct {
	Hash        string  `json:"hash"`
	Parent      *string `json:"parent"`
	RootTree    string  `json:"root_tree"`
	Ctime       string  `json:"ctime"`
	Description string  `json:"description"`
}

func (s *Server) handleCommits(w http.ResponseWriter, r *http.Request) {
	lib := libraryFromContext(r.Context())
	history, err := s.db.History(r.Context(), lib.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]commitResponse, 0, len(history))
	for _, c := range history {
		cr := commitResponse{
			Hash:        c.Hash.String(),
			RootTree:    c.RootTree.String(),
			Ctime:       c.Ctime.UTC().Format(time.RFC3339),
			Description: c.Description,
		}
		if c.Parent != nil {
			p := c.Parent.String()
			cr.Parent = &p
		}
		out = append(out, cr)
	}
	writeJSON(w, http.StatusOK, out)
}

type fsckResponse struct {
	HeadCommit           *string  `json:"head_commit"`
	HeadConsistent       bool     `json:"head_consistent"`
	LastConsistentCommit *string  `json:"last_consistent_commit"`
	Problems             []string `json:"problems"`
}

func (s *Server) handleFsck(w http.ResponseWriter, r *http.Request) {
	lib := libraryFromContext(r.Context())
	report, err := s.db.Fsck(r.Context(), lib.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	resp := fsckResponse{HeadConsistent: report.HeadConsistent, Problems: report.Problems}
	if resp.Problems == nil {
		resp.Problems = []string{}
	}
	if report.HeadCommit != nil {
		h := report.HeadCommit.String()
		resp.HeadCommit = &h
	}
	if report.LastConsistentCommit != nil {
		h := report.LastConsistentCommit.String()
		resp.LastConsistentCommit = &h
	}
	writeJSON(w, http.StatusOK, resp)
}
