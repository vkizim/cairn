package api

import (
	"encoding/json"
	"errors"
	"net/http"
)

// errorResponse is the uniform JSON error envelope.
type errorResponse struct {
	Error string `json:"error"`
}

// writeJSON writes v as JSON with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// writeError writes a JSON error envelope. Messages are intentionally generic
// for auth/access failures to avoid leaking information.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

// decodeJSON decodes the request body into dst, capping the body size to guard
// against memory-exhaustion. Unknown fields are rejected to catch client bugs.
func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxJSONBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	// Reject trailing garbage / multiple JSON values.
	if dec.More() {
		return errors.New("unexpected trailing data in request body")
	}
	return nil
}
