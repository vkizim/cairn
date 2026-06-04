package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/vkizim/cairn/repo"
)

const uploadOffsetHeader = "Upload-Offset"
const uploadLengthHeader = "Upload-Length"

type createUploadRequest struct {
	Path     string `json:"path"`     // destination directory (virtual)
	Filename string `json:"filename"`
	Size     *int64 `json:"size"`     // optional declared total size
}

type createUploadResponse struct {
	UploadID string `json:"upload_id"`
	Offset   int64  `json:"offset"`
}

func (s *Server) handleCreateUpload(w http.ResponseWriter, r *http.Request) {
	lib := libraryFromContext(r.Context())
	user := userFromContext(r.Context())

	var req createUploadRequest
	if err := s.decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	dir, err := repo.CleanLibraryPath(req.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}
	if err := repo.ValidateFilename(req.Filename); err != nil {
		writeError(w, http.StatusBadRequest, "invalid filename")
		return
	}
	if req.Size != nil && (*req.Size < 0 || *req.Size > s.cfg.MaxUploadBytes) {
		writeError(w, http.StatusRequestEntityTooLarge, "declared size exceeds maximum upload size")
		return
	}

	tempPath := filepath.Join(s.cfg.UploadTmpDir, uuid.NewString()+".part")
	f, err := os.OpenFile(tempPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	_ = f.Close()

	expires := s.now().Add(s.cfg.UploadTTL)
	up, err := s.db.CreateUploadSession(r.Context(), lib.ID, user.ID, dir, req.Filename, req.Size, tempPath, expires)
	if err != nil {
		_ = os.Remove(tempPath)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, createUploadResponse{UploadID: up.ID.String(), Offset: 0})
}

func (s *Server) handleHeadUpload(w http.ResponseWriter, r *http.Request) {
	lib := libraryFromContext(r.Context())
	up, ok := s.lookupUpload(w, r, lib.ID)
	if !ok {
		return
	}
	w.Header().Set(uploadOffsetHeader, strconv.FormatInt(up.ReceivedBytes, 10))
	if up.DeclaredSize != nil {
		w.Header().Set(uploadLengthHeader, strconv.FormatInt(*up.DeclaredSize, 10))
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handlePatchUpload(w http.ResponseWriter, r *http.Request) {
	lib := libraryFromContext(r.Context())
	uploadID, ok := s.parseUploadID(w, r)
	if !ok {
		return
	}
	clientOffset, err := strconv.ParseInt(r.Header.Get(uploadOffsetHeader), 10, 64)
	if err != nil || clientOffset < 0 {
		writeError(w, http.StatusBadRequest, "missing or invalid Upload-Offset header")
		return
	}

	// Cap a single chunk; AppendUploadChunk additionally enforces the total cap
	// and serializes per session (FOR UPDATE) so concurrent PATCHes can't race.
	body := http.MaxBytesReader(w, r.Body, s.cfg.MaxChunkBytes)
	newOffset, err := s.db.AppendUploadChunk(r.Context(), uploadID, lib.ID, clientOffset, body, s.cfg.MaxUploadBytes)

	var conflict *repo.OffsetConflictError
	var maxErr *http.MaxBytesError
	switch {
	case errors.Is(err, repo.ErrNotFound):
		writeError(w, http.StatusNotFound, "upload not found")
	case errors.As(err, &conflict):
		w.Header().Set(uploadOffsetHeader, strconv.FormatInt(conflict.Current, 10))
		writeError(w, http.StatusConflict, "offset mismatch; resume from Upload-Offset")
	case errors.Is(err, repo.ErrUploadTooLarge), errors.As(err, &maxErr):
		writeError(w, http.StatusRequestEntityTooLarge, "upload exceeds size limit")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal error")
	default:
		w.Header().Set(uploadOffsetHeader, strconv.FormatInt(newOffset, 10))
		w.WriteHeader(http.StatusNoContent)
	}
}

type completeUploadResponse struct {
	CommitHash       string  `json:"commit_hash"`
	Files            int     `json:"files"`
	LogicalBytes     uint64  `json:"logical_bytes"`
	PhysicalNewBytes uint64  `json:"physical_new_bytes"`
	UniqueBlocks     int     `json:"unique_blocks"`
	TotalBlocks      int     `json:"total_blocks"`
	DedupRatio       float64 `json:"dedup_ratio"`
}

func (s *Server) handleCompleteUpload(w http.ResponseWriter, r *http.Request) {
	lib := libraryFromContext(r.Context())
	up, ok := s.lookupUpload(w, r, lib.ID)
	if !ok {
		return
	}
	// A single complete is just a batch of one (merged semantics).
	s.completeSessions(w, r, lib, []repo.UploadSession{up})
}

// maxBatchComplete bounds how many upload sessions one batch-complete may join.
const maxBatchComplete = 256

type completeBatchRequest struct {
	UploadIDs []string `json:"upload_ids"`
}

// handleCompleteBatch joins several finished upload sessions into ONE merged
// commit — a multi-file drag-and-drop lands as a single commit instead of N.
func (s *Server) handleCompleteBatch(w http.ResponseWriter, r *http.Request) {
	lib := libraryFromContext(r.Context())

	var req completeBatchRequest
	if err := s.decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.UploadIDs) == 0 || len(req.UploadIDs) > maxBatchComplete {
		writeError(w, http.StatusBadRequest, "upload_ids must contain between 1 and 256 entries")
		return
	}

	sessions := make([]repo.UploadSession, 0, len(req.UploadIDs))
	seenPath := map[string]bool{}
	for _, raw := range req.UploadIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, http.StatusNotFound, "upload not found")
			return
		}
		up, err := s.db.GetUploadSession(r.Context(), id, lib.ID)
		if errors.Is(err, repo.ErrNotFound) {
			writeError(w, http.StatusNotFound, "upload not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		p := joinVirtual(up.TargetPath, up.Filename)
		if seenPath[p] {
			writeError(w, http.StatusBadRequest, "duplicate target path in batch: "+p)
			return
		}
		seenPath[p] = true
		sessions = append(sessions, up)
	}
	s.completeSessions(w, r, lib, sessions)
}

// completeSessions validates the sessions, runs their assembled temp files
// through the CDC chunker, and lands them all as ONE merged commit
// (repo.CommitFilesMerged): the head tree plus these files, where an existing
// path gets a NEW VERSION (the previous one stays reachable through history —
// no "file(1)" copies). On success the sessions and temp files are cleaned up.
func (s *Server) completeSessions(w http.ResponseWriter, r *http.Request, lib repo.Library, sessions []repo.UploadSession) {
	inputs := make([]repo.FileInput, 0, len(sessions))
	for _, up := range sessions {
		if up.DeclaredSize != nil && *up.DeclaredSize != up.ReceivedBytes {
			writeError(w, http.StatusBadRequest,
				"received bytes do not match declared size for "+up.Filename)
			return
		}
		f, err := os.Open(up.TempPath)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "upload data unavailable for "+up.Filename)
			return
		}
		defer f.Close()
		inputs = append(inputs, repo.FileInput{
			Path:   joinVirtual(up.TargetPath, up.Filename),
			Reader: io.LimitReader(f, up.ReceivedBytes),
		})
	}

	desc := "upload " + sessions[0].Filename
	if len(sessions) > 1 {
		desc = fmt.Sprintf("upload %d files", len(sessions))
	}

	res, err := s.db.CommitFilesMerged(r.Context(), lib.ID, inputs, desc)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "commit failed")
		return
	}

	// Clean up sessions + temp files (tolerant of already-missing files);
	// non-fatal on error — the commit succeeded and the sweep reclaims leftovers.
	for _, up := range sessions {
		_ = s.db.DeleteUploadSession(r.Context(), up)
	}

	writeJSON(w, http.StatusOK, completeUploadResponse{
		CommitHash:       res.Commit.Hash.String(),
		Files:            res.Stats.Files,
		LogicalBytes:     res.Stats.LogicalBytes,
		PhysicalNewBytes: res.Stats.PhysicalNewBytes,
		UniqueBlocks:     res.Stats.UniqueBlocks,
		TotalBlocks:      res.Stats.TotalBlocks,
		DedupRatio:       res.Stats.DedupRatio(),
	})
}

func (s *Server) handleDeleteUpload(w http.ResponseWriter, r *http.Request) {
	lib := libraryFromContext(r.Context())
	uploadID, ok := s.parseUploadID(w, r)
	if !ok {
		return
	}
	up, err := s.db.GetUploadSession(r.Context(), uploadID, lib.ID)
	if errors.Is(err, repo.ErrNotFound) {
		w.WriteHeader(http.StatusNoContent) // idempotent abort
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := s.db.DeleteUploadSession(r.Context(), up); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers ---

func (s *Server) parseUploadID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("uploadId"))
	if err != nil {
		writeError(w, http.StatusNotFound, "upload not found")
		return uuid.UUID{}, false
	}
	return id, true
}

func (s *Server) lookupUpload(w http.ResponseWriter, r *http.Request, libID uuid.UUID) (repo.UploadSession, bool) {
	id, ok := s.parseUploadID(w, r)
	if !ok {
		return repo.UploadSession{}, false
	}
	up, err := s.db.GetUploadSession(r.Context(), id, libID)
	if errors.Is(err, repo.ErrNotFound) {
		writeError(w, http.StatusNotFound, "upload not found")
		return repo.UploadSession{}, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return repo.UploadSession{}, false
	}
	return up, true
}

// joinVirtual joins a cleaned directory ("/", "/docs") and a filename into a
// repository-relative path for CommitFiles.
func joinVirtual(dir, name string) string {
	dir = strings.TrimPrefix(dir, "/")
	if dir == "" {
		return name
	}
	return dir + "/" + name
}
