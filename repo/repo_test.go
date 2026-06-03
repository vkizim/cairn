package repo

import (
	"context"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vkizim/cairn/blockstore"
)

func strReader(s string) io.Reader { return strings.NewReader(s) }

func mustLibrary(t *testing.T, db *DB) Library {
	t.Helper()
	lib, err := db.CreateLibrary(context.Background(), "lib", "owner")
	if err != nil {
		t.Fatalf("CreateLibrary: %v", err)
	}
	return lib
}

func commit(t *testing.T, db *DB, libID uuid.UUID, desc string, inputs ...FileInput) CommitResult {
	t.Helper()
	res, err := db.CommitFiles(context.Background(), libID, inputs, desc)
	if err != nil {
		t.Fatalf("CommitFiles(%q): %v", desc, err)
	}
	return res
}

func file(path, content string) FileInput { return FileInput{Path: path, Reader: strReader(content)} }

// listNames returns the entry names of a directory, sorted.
func listNames(t *testing.T, db *DB, libID uuid.UUID, dir string) []string {
	t.Helper()
	entries, err := db.ListDir(context.Background(), libID, dir)
	if err != nil {
		t.Fatalf("ListDir(%q): %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	sort.Strings(names)
	return names
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestCommitAndListDir(t *testing.T) {
	db := newTestDB(t)
	lib := mustLibrary(t, db)

	commit(t, db, lib.ID, "initial", file("a.txt", "hello"), file("docs/b.txt", "world"))

	root, err := db.ListDir(context.Background(), lib.ID, "/")
	if err != nil {
		t.Fatalf("ListDir root: %v", err)
	}
	byName := map[string]PathEntry{}
	for _, e := range root {
		byName[e.Name] = e
	}
	if len(byName) != 2 {
		t.Fatalf("root has %d entries, want 2: %+v", len(byName), root)
	}
	if a, ok := byName["a.txt"]; !ok || a.IsDir || a.Size != 5 || a.FileObjHash == nil {
		t.Fatalf("a.txt entry wrong: %+v", a)
	}
	if d, ok := byName["docs"]; !ok || !d.IsDir || d.FileObjHash != nil {
		t.Fatalf("docs entry wrong: %+v", d)
	}

	sub := listNames(t, db, lib.ID, "/docs")
	if !eqStrings(sub, []string{"b.txt"}) {
		t.Fatalf("/docs = %v, want [b.txt]", sub)
	}
}

func TestHistory(t *testing.T) {
	db := newTestDB(t)
	lib := mustLibrary(t, db)

	c1 := commit(t, db, lib.ID, "one", file("a.txt", "a"))
	c2 := commit(t, db, lib.ID, "two", file("a.txt", "a"), file("b.txt", "b"))
	c3 := commit(t, db, lib.ID, "three", file("a.txt", "a"), file("b.txt", "b"), file("c.txt", "c"))

	hist, err := db.History(context.Background(), lib.ID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(hist) != 3 {
		t.Fatalf("history len %d, want 3", len(hist))
	}
	// Newest first, with parent links forming the chain.
	if hist[0].Hash != c3.Commit.Hash || hist[1].Hash != c2.Commit.Hash || hist[2].Hash != c1.Commit.Hash {
		t.Fatalf("history order wrong")
	}
	if hist[0].Parent == nil || *hist[0].Parent != c2.Commit.Hash {
		t.Fatalf("c3 parent should be c2")
	}
	if hist[2].Parent != nil {
		t.Fatalf("root commit should have nil parent")
	}
	if hist[0].Description != "three" {
		t.Fatalf("description mismatch: %q", hist[0].Description)
	}
}

func TestRollbackRestoresState(t *testing.T) {
	db := newTestDB(t)
	lib := mustLibrary(t, db)

	cA := commit(t, db, lib.ID, "A", file("a.txt", "hello"))
	commit(t, db, lib.ID, "B", file("a.txt", "hello"), file("b.txt", "world"))

	if got := listNames(t, db, lib.ID, "/"); !eqStrings(got, []string{"a.txt", "b.txt"}) {
		t.Fatalf("after B root = %v, want [a.txt b.txt]", got)
	}

	if err := db.RollbackHead(context.Background(), lib.ID, &cA.Commit.Hash); err != nil {
		t.Fatalf("RollbackHead: %v", err)
	}

	if got := listNames(t, db, lib.ID, "/"); !eqStrings(got, []string{"a.txt"}) {
		t.Fatalf("after rollback root = %v, want [a.txt]", got)
	}
	lib2, _ := db.GetLibrary(context.Background(), lib.ID)
	if lib2.HeadCommit == nil || *lib2.HeadCommit != cA.Commit.Hash {
		t.Fatalf("head not rolled back to A")
	}
}

func TestRebuildPathIndexMatchesWalk(t *testing.T) {
	db := newTestDB(t)
	lib := mustLibrary(t, db)

	res := commit(t, db, lib.ID, "nested",
		file("top.txt", "t"),
		file("docs/a.txt", "aa"),
		file("docs/sub/b.txt", "bbb"),
	)

	// Snapshot the path_index as built by the commit.
	before := snapshotPathIndex(t, db, lib.ID)

	// Independently walk the committed tree and derive the expected entries.
	want := map[string]bool{}
	ctx := context.Background()
	err := walkTree(ctx, db.pool, res.Commit.RootTree, func(parentPath string, e TreeEntry) error {
		want[parentPath+"|"+e.Name+"|"+boolStr(e.Type == ObjTypeTree)] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walkTree: %v", err)
	}
	if !sameSet(before, want) {
		t.Fatalf("path_index != tree walk\n index=%v\n walk =%v", before, want)
	}

	// RebuildPathIndex (the fsck/repair entry point) must reproduce it exactly.
	if err := db.RebuildPathIndex(ctx, lib.ID, res.Commit.Hash); err != nil {
		t.Fatalf("RebuildPathIndex: %v", err)
	}
	after := snapshotPathIndex(t, db, lib.ID)
	if !sameSet(before, after) {
		t.Fatalf("rebuild changed path_index\n before=%v\n after =%v", before, after)
	}
}

func TestRefcountAcrossCommits(t *testing.T) {
	db := newTestDB(t)
	lib := mustLibrary(t, db)
	ctx := context.Background()

	hShared := blockstore.HashData([]byte("hello"))
	hUnique := blockstore.HashData([]byte("world"))

	cA := commit(t, db, lib.ID, "A", file("a.txt", "hello"))
	if n, _ := db.BlockRefcount(ctx, hShared); n != 1 {
		t.Fatalf("after A: shared refcount %d, want 1", n)
	}

	// B shares the "hello" block and adds a unique "world" block.
	commit(t, db, lib.ID, "B", file("a.txt", "hello"), file("c.txt", "world"))
	if n, _ := db.BlockRefcount(ctx, hShared); n != 2 {
		t.Fatalf("after B: shared refcount %d, want 2", n)
	}
	if n, _ := db.BlockRefcount(ctx, hUnique); n != 1 {
		t.Fatalf("after B: unique refcount %d, want 1", n)
	}

	// Roll back to A, orphaning B, then GC.
	if err := db.RollbackHead(ctx, lib.ID, &cA.Commit.Hash); err != nil {
		t.Fatalf("RollbackHead: %v", err)
	}
	gc, err := db.GarbageCollect(ctx, lib.ID)
	if err != nil {
		t.Fatalf("GarbageCollect: %v", err)
	}
	if gc.OrphanCommitsDeleted != 1 {
		t.Fatalf("GC deleted %d orphan commits, want 1", gc.OrphanCommitsDeleted)
	}

	// Shared block survives (still referenced by A); unique block is gone.
	if n, _ := db.BlockRefcount(ctx, hShared); n != 1 {
		t.Fatalf("after GC: shared refcount %d, want 1", n)
	}
	if n, _ := db.BlockRefcount(ctx, hUnique); n != 0 {
		t.Fatalf("after GC: unique refcount %d, want 0", n)
	}
	if ok, _ := db.store.Exists(db.ns, hUnique); ok {
		t.Fatalf("unique block should have been deleted from the store")
	}
	if ok, _ := db.store.Exists(db.ns, hShared); !ok {
		t.Fatalf("shared block must remain in the store")
	}
}

func TestFsckDetectsMissingHeadAndRecovers(t *testing.T) {
	db := newTestDB(t)
	lib := mustLibrary(t, db)
	ctx := context.Background()

	cA := commit(t, db, lib.ID, "A", file("a.txt", "hello"))
	cB := commit(t, db, lib.ID, "B", file("a.txt", "hello"), file("b.txt", "world"))

	// Inject corruption: a head pointing at a commit that no longer exists.
	// In normal operation the head FK makes this impossible, so we drop it to
	// simulate a bug/disk fault that bypassed the invariant, then delete B.
	if _, err := db.pool.Exec(ctx, `ALTER TABLE libraries DROP CONSTRAINT fk_head_commit`); err != nil {
		t.Fatalf("drop fk: %v", err)
	}
	if _, err := db.pool.Exec(ctx, `DELETE FROM commits WHERE commit_hash = $1`, cB.Commit.Hash.String()); err != nil {
		t.Fatalf("delete commit B: %v", err)
	}

	report, err := db.Fsck(ctx, lib.ID)
	if err != nil {
		t.Fatalf("Fsck: %v", err)
	}
	if report.HeadConsistent {
		t.Fatal("Fsck reported head consistent, but head points at a deleted commit")
	}
	if report.LastConsistentCommit == nil || *report.LastConsistentCommit != cA.Commit.Hash {
		t.Fatalf("LastConsistentCommit = %v, want A (%s)", report.LastConsistentCommit, cA.Commit.Hash)
	}
	if len(report.Problems) == 0 {
		t.Fatal("expected problems to be reported")
	}

	// Recover should roll the head back to A and rebuild path_index.
	if _, err := db.Recover(ctx, lib.ID); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	lib2, _ := db.GetLibrary(ctx, lib.ID)
	if lib2.HeadCommit == nil || *lib2.HeadCommit != cA.Commit.Hash {
		t.Fatalf("head not recovered to A")
	}
	if got := listNames(t, db, lib.ID, "/"); !eqStrings(got, []string{"a.txt"}) {
		t.Fatalf("after recovery root = %v, want [a.txt]", got)
	}
	// A clean library now verifies.
	report2, err := db.Fsck(ctx, lib.ID)
	if err != nil {
		t.Fatalf("Fsck after recover: %v", err)
	}
	if !report2.HeadConsistent {
		t.Fatalf("library still inconsistent after recovery: %v", report2.Problems)
	}
}

// TestWriteOrderInvariant simulates a crash between tx1 (objects + commit row)
// and tx2 (head advance + path_index). The head must NOT have moved, and the
// commit must exist only as an orphan — never reachable as the head.
func TestWriteOrderInvariant(t *testing.T) {
	db := newTestDB(t)
	lib := mustLibrary(t, db)
	ctx := context.Background()

	prep, err := db.prepareCommit(ctx, lib, []FileInput{file("a.txt", "hello")}, "crash-between")
	if err != nil {
		t.Fatalf("prepareCommit: %v", err)
	}
	// Run ONLY tx1; do not publish (the "crash").
	if err := db.commitObjects(ctx, prep); err != nil {
		t.Fatalf("commitObjects: %v", err)
	}

	// Head must still be nil — it never moved.
	lib2, _ := db.GetLibrary(ctx, lib.ID)
	if lib2.HeadCommit != nil {
		t.Fatalf("head advanced despite no publish: %v", lib2.HeadCommit)
	}
	// The commit row exists (durable orphan), reachable by hash but not as head.
	if _, err := db.getCommit(ctx, db.pool, prep.commit.Hash); err != nil {
		t.Fatalf("orphan commit row should exist: %v", err)
	}
	// path_index was never populated (head never advanced).
	if got := listNames(t, db, lib.ID, "/"); len(got) != 0 {
		t.Fatalf("path_index populated without head advance: %v", got)
	}

	// And GC should reclaim the orphan commit.
	gc, err := db.GarbageCollect(ctx, lib.ID)
	if err != nil {
		t.Fatalf("GarbageCollect: %v", err)
	}
	if gc.OrphanCommitsDeleted != 1 {
		t.Fatalf("GC deleted %d orphan commits, want 1", gc.OrphanCommitsDeleted)
	}
}

// --- small helpers ---

func boolStr(b bool) string {
	if b {
		return "d"
	}
	return "f"
}

func snapshotPathIndex(t *testing.T, db *DB, libID uuid.UUID) map[string]bool {
	t.Helper()
	rows, err := db.pool.Query(context.Background(),
		`SELECT path, name, is_dir FROM path_index WHERE library_id = $1`, libID)
	if err != nil {
		t.Fatalf("query path_index: %v", err)
	}
	defer rows.Close()

	set := map[string]bool{}
	for rows.Next() {
		var path, name string
		var isDir bool
		if err := rows.Scan(&path, &name, &isDir); err != nil {
			t.Fatalf("scan: %v", err)
		}
		set[path+"|"+name+"|"+boolStr(isDir)] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return set
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}
