package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"
	"testing"
)

func decode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

func deterministicBytes(n int, seed uint64) []byte {
	b := make([]byte, n)
	s := seed
	for i := range b {
		s ^= s << 13
		s ^= s >> 7
		s ^= s << 17
		b[i] = byte(s)
	}
	return b
}

// --- auth ---

func TestAuthFlow(t *testing.T) {
	e := newTestEnv(t)
	e.seedUser(t, "alice", "correct horse")
	c := e.newClient(t)

	resp := c.login("alice", "correct horse")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = c.req(http.MethodGet, "/api/me", nil, false, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/me status %d", resp.StatusCode)
	}
	var me struct{ Username string }
	decode(t, resp, &me)
	resp.Body.Close()
	if me.Username != "alice" {
		t.Fatalf("/me username %q", me.Username)
	}

	resp = c.req(http.MethodPost, "/api/logout", nil, true, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = c.req(http.MethodGet, "/api/me", nil, false, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/me after logout status %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAbsentSessionUnauthorized(t *testing.T) {
	e := newTestEnv(t)
	c := e.newClient(t)
	resp := c.req(http.MethodGet, "/api/me", nil, false, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", resp.StatusCode)
	}
}

func TestExpiredSessionUnauthorized(t *testing.T) {
	e := newTestEnv(t)
	e.seedUser(t, "alice", "pw")
	c := e.newClient(t)
	c.login("alice", "pw").Body.Close()

	// Force the session to expire.
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE sessions SET expires_at = now() - interval '1 hour'`); err != nil {
		t.Fatalf("expire session: %v", err)
	}

	resp := c.req(http.MethodGet, "/api/me", nil, false, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", resp.StatusCode)
	}
}

func TestBadCredentials(t *testing.T) {
	e := newTestEnv(t)
	e.seedUser(t, "alice", "right")
	c := e.newClient(t)

	for _, tc := range []struct{ user, pass string }{
		{"alice", "wrong"},  // bad password
		{"ghost", "right"},  // unknown user (no enumeration: same 401)
	} {
		resp := c.login(tc.user, tc.pass)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("login %q/%q status %d, want 401", tc.user, tc.pass, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// --- CSRF ---

func TestCSRFRequiredOnMutation(t *testing.T) {
	e := newTestEnv(t)
	e.seedUser(t, "alice", "pw")
	c := e.newClient(t)
	c.login("alice", "pw").Body.Close()

	// Without the CSRF header -> 403.
	resp := c.req(http.MethodPost, "/api/libraries",
		bytes.NewBufferString(`{"name":"x"}`), false, map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no-CSRF status %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// With it -> 201.
	resp = c.req(http.MethodPost, "/api/libraries",
		bytes.NewBufferString(`{"name":"x"}`), true, map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("with-CSRF status %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- access control ---

func TestLibraryAccessIsolation(t *testing.T) {
	e := newTestEnv(t)
	e.seedUser(t, "alice", "pw")
	e.seedUser(t, "bob", "pw")

	alice := e.newClient(t)
	alice.login("alice", "pw").Body.Close()
	libID := mustLibrary(t, alice, "alice-lib")

	bob := e.newClient(t)
	bob.login("bob", "pw").Body.Close()

	// Every cross-user access returns 404 (don't reveal existence).
	paths := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/libraries/" + libID},
		{http.MethodGet, "/api/libraries/" + libID + "/files?path=/"},
		{http.MethodGet, "/api/libraries/" + libID + "/files/download?path=/x"},
		{http.MethodGet, "/api/libraries/" + libID + "/commits"},
	}
	for _, p := range paths {
		resp := bob.req(p.method, p.path, nil, false, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("bob %s %s: status %d, want 404", p.method, p.path, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// And bob cannot start an upload into alice's library.
	resp := bob.req(http.MethodPost, "/api/libraries/"+libID+"/uploads",
		bytes.NewBufferString(`{"path":"/","filename":"x.txt"}`), true,
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("bob upload status %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// alice still sees only her library in the list.
	resp = alice.req(http.MethodGet, "/api/libraries", nil, false, nil)
	var libs []map[string]any
	decode(t, resp, &libs)
	resp.Body.Close()
	if len(libs) != 1 {
		t.Fatalf("alice library count %d, want 1", len(libs))
	}
}

// --- uploads + download ---

// createUpload starts an upload and returns its id.
func (c *client) createUpload(t *testing.T, libID, path, filename string, size *int64) (string, int) {
	t.Helper()
	reqBody := map[string]any{"path": path, "filename": filename}
	if size != nil {
		reqBody["size"] = *size
	}
	b, _ := json.Marshal(reqBody)
	resp := c.req(http.MethodPost, "/api/libraries/"+libID+"/uploads",
		bytes.NewReader(b), true, map[string]string{"Content-Type": "application/json"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", resp.StatusCode
	}
	var out struct {
		UploadID string `json:"upload_id"`
	}
	decode(t, resp, &out)
	return out.UploadID, resp.StatusCode
}

func (c *client) patch(t *testing.T, libID, uploadID string, offset int64, data []byte) *http.Response {
	t.Helper()
	return c.req(http.MethodPatch, "/api/libraries/"+libID+"/uploads/"+uploadID,
		bytes.NewReader(data), true, map[string]string{"Upload-Offset": strconv.FormatInt(offset, 10)})
}

func TestUploadResumeCompleteDownload(t *testing.T) {
	e := newTestEnv(t)
	e.seedUser(t, "alice", "pw")
	c := e.newClient(t)
	c.login("alice", "pw").Body.Close()
	libID := mustLibrary(t, c, "lib")

	content := deterministicBytes(300*1024, 0xA11CE) // 300 KiB -> multiple chunks
	uploadID, st := c.createUpload(t, libID, "/docs", "report.bin", nil)
	if st != http.StatusCreated {
		t.Fatalf("create upload status %d", st)
	}

	// First chunk.
	resp := c.patch(t, libID, uploadID, 0, content[:200*1024])
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("patch#1 status %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Simulate interruption: HEAD to discover the offset before resuming.
	resp = c.req(http.MethodHead, "/api/libraries/"+libID+"/uploads/"+uploadID, nil, false, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("head status %d", resp.StatusCode)
	}
	off, _ := strconv.ParseInt(resp.Header.Get("Upload-Offset"), 10, 64)
	resp.Body.Close()
	if off != 200*1024 {
		t.Fatalf("resume offset %d, want %d", off, 200*1024)
	}

	// Resume from the reported offset.
	resp = c.patch(t, libID, uploadID, off, content[off:])
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("patch#2 status %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Complete -> commit.
	resp = c.req(http.MethodPost, "/api/libraries/"+libID+"/uploads/"+uploadID+"/complete", nil, true, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("complete status %d", resp.StatusCode)
	}
	var cr struct {
		CommitHash string `json:"commit_hash"`
	}
	decode(t, resp, &cr)
	resp.Body.Close()
	if len(cr.CommitHash) != 64 {
		t.Fatalf("bad commit hash %q", cr.CommitHash)
	}

	// File appears in the listing.
	resp = c.req(http.MethodGet, "/api/libraries/"+libID+"/files?path=/docs", nil, false, nil)
	var entries []struct {
		Name  string `json:"name"`
		IsDir bool   `json:"is_dir"`
		Size  int64  `json:"size"`
	}
	decode(t, resp, &entries)
	resp.Body.Close()
	if len(entries) != 1 || entries[0].Name != "report.bin" || entries[0].Size != int64(len(content)) {
		t.Fatalf("listing wrong: %+v", entries)
	}

	// Download returns identical bytes.
	resp = c.req(http.MethodGet, "/api/libraries/"+libID+"/files/download?path=/docs/report.bin", nil, false, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download status %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(got, content) {
		t.Fatalf("downloaded bytes differ (got %d, want %d)", len(got), len(content))
	}
}

func TestUploadOffsetConflict(t *testing.T) {
	e := newTestEnv(t)
	e.seedUser(t, "alice", "pw")
	c := e.newClient(t)
	c.login("alice", "pw").Body.Close()
	libID := mustLibrary(t, c, "lib")

	uploadID, _ := c.createUpload(t, libID, "/", "f.bin", nil)
	data := deterministicBytes(1024, 1)
	c.patch(t, libID, uploadID, 0, data).Body.Close() // received = 1024

	// Re-send at offset 0 (already consumed) -> 409 with current offset.
	resp := c.patch(t, libID, uploadID, 0, data)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status %d, want 409", resp.StatusCode)
	}
	if got := resp.Header.Get("Upload-Offset"); got != "1024" {
		t.Fatalf("conflict Upload-Offset %q, want 1024", got)
	}
}

func TestUploadSizeLimit(t *testing.T) {
	e := newTestEnv(t)
	e.seedUser(t, "alice", "pw")
	c := e.newClient(t)
	c.login("alice", "pw").Body.Close()
	libID := mustLibrary(t, c, "lib")

	// declared size beyond the 1 MiB server cap -> 413 at create.
	big := int64(2 << 20)
	_, st := c.createUpload(t, libID, "/", "huge.bin", &big)
	if st != http.StatusRequestEntityTooLarge {
		t.Fatalf("create with oversized declared size: status %d, want 413", st)
	}

	// Streaming past the total cap -> 413 during PATCH.
	uploadID, _ := c.createUpload(t, libID, "/", "f.bin", nil)
	resp := c.patch(t, libID, uploadID, 0, deterministicBytes(2<<20, 2)) // > 1 MiB cap
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized PATCH status %d, want 413", resp.StatusCode)
	}
}

func TestUploadAbort(t *testing.T) {
	e := newTestEnv(t)
	e.seedUser(t, "alice", "pw")
	c := e.newClient(t)
	c.login("alice", "pw").Body.Close()
	libID := mustLibrary(t, c, "lib")

	uploadID, _ := c.createUpload(t, libID, "/", "f.bin", nil)
	c.patch(t, libID, uploadID, 0, deterministicBytes(1024, 3)).Body.Close()

	resp := c.req(http.MethodDelete, "/api/libraries/"+libID+"/uploads/"+uploadID, nil, true, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()

	resp = c.req(http.MethodHead, "/api/libraries/"+libID+"/uploads/"+uploadID, nil, false, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("head after abort status %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestConcurrentPatchSameOffset(t *testing.T) {
	e := newTestEnv(t)
	e.seedUser(t, "alice", "pw")
	c := e.newClient(t)
	c.login("alice", "pw").Body.Close()
	libID := mustLibrary(t, c, "lib")

	uploadID, _ := c.createUpload(t, libID, "/", "race.bin", nil)

	payloadA := bytes.Repeat([]byte("A"), 4096)
	payloadB := bytes.Repeat([]byte("B"), 4096)

	var wg sync.WaitGroup
	statuses := make([]int, 2)
	payloads := [][]byte{payloadA, payloadB}
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			defer wg.Done()
			resp := c.patch(t, libID, uploadID, 0, payloads[i])
			statuses[i] = resp.StatusCode
			resp.Body.Close()
		}(i)
	}
	wg.Wait()

	// Exactly one 204 and one 409 — never both appended.
	got204, got409 := 0, 0
	for _, s := range statuses {
		switch s {
		case http.StatusNoContent:
			got204++
		case http.StatusConflict:
			got409++
		}
	}
	if got204 != 1 || got409 != 1 {
		t.Fatalf("concurrent PATCH statuses %v, want exactly one 204 and one 409", statuses)
	}

	// received_bytes must equal a single payload (no double append / corruption).
	resp := c.req(http.MethodHead, "/api/libraries/"+libID+"/uploads/"+uploadID, nil, false, nil)
	off, _ := strconv.ParseInt(resp.Header.Get("Upload-Offset"), 10, 64)
	resp.Body.Close()
	if off != 4096 {
		t.Fatalf("after race, offset %d, want 4096 (no corruption)", off)
	}
}

func TestRangeDownload(t *testing.T) {
	e := newTestEnv(t)
	e.seedUser(t, "alice", "pw")
	c := e.newClient(t)
	c.login("alice", "pw").Body.Close()
	libID := mustLibrary(t, c, "lib")

	content := deterministicBytes(50*1024, 0x5151)
	uploadID, _ := c.createUpload(t, libID, "/", "media.bin", nil)
	c.patch(t, libID, uploadID, 0, content).Body.Close()
	c.req(http.MethodPost, "/api/libraries/"+libID+"/uploads/"+uploadID+"/complete", nil, true, nil).Body.Close()

	resp := c.req(http.MethodGet, "/api/libraries/"+libID+"/files/download?path=/media.bin",
		nil, false, map[string]string{"Range": "bytes=100-199"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("range status %d, want 206", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, content[100:200]) {
		t.Fatalf("range bytes mismatch (got %d bytes)", len(got))
	}
}

func TestPathValidationRejectsTraversal(t *testing.T) {
	e := newTestEnv(t)
	e.seedUser(t, "alice", "pw")
	c := e.newClient(t)
	c.login("alice", "pw").Body.Close()
	libID := mustLibrary(t, c, "lib")

	// Traversal / invalid filename at upload-create -> 400.
	for _, tc := range []struct{ path, name string }{
		{"/../etc", "passwd"},   // traversal in path
		{"/", "../escape"},      // separator/traversal in filename
		{"/", "a/b"},            // separator in filename
		{"/", ""},               // empty filename
	} {
		b, _ := json.Marshal(map[string]any{"path": tc.path, "filename": tc.name})
		resp := c.req(http.MethodPost, "/api/libraries/"+libID+"/uploads",
			bytes.NewReader(b), true, map[string]string{"Content-Type": "application/json"})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("create upload path=%q name=%q: status %d, want 400", tc.path, tc.name, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Traversal in a download path -> 400.
	resp := c.req(http.MethodGet, "/api/libraries/"+libID+"/files/download?path=/../secret", nil, false, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("download traversal status %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}
