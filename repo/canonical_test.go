package repo

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vkizim/cairn/blockstore"
)

// These golden vectors pin the canonical serialization of file, tree, and commit
// objects: fixed inputs -> exact canonical bytes AND object hashes. Object hashes
// are a PERMANENT addressing contract, exactly like the chunker's golden chunk
// boundaries: any future change to the canonical encoding shifts these bytes and
// silently breaks cross-version dedup of identical objects. Such a change must
// fail this test loudly and be treated as a versioned, migrated breaking change —
// never silently accepted.
//
// To regenerate after an INTENTIONAL format change: set regenerateGolden = true,
// run `go test ./repo -run TestCanonicalGolden -v`, copy the printed literals,
// then set it back to false.
const regenerateGolden = false

// fixed, content-free hashes used as stand-ins for block/file/tree addresses.
func fixedHash(b byte) blockstore.Hash {
	var h blockstore.Hash
	for i := range h {
		h[i] = b
	}
	return h
}

func TestCanonicalGolden(t *testing.T) {
	fileObj := FileObject{
		Size: 4096,
		Blocks: []FileBlock{
			{Hash: fixedHash(0x11), Size: 3000},
			{Hash: fixedHash(0x22), Size: 1096},
		},
	}
	// Entries intentionally out of name order to prove canonical sorting.
	treeObj := TreeObject{
		Entries: []TreeEntry{
			{Name: "zeta.txt", Type: ObjTypeFile, Hash: fixedHash(0xaa), Size: 10},
			{Name: "alpha", Type: ObjTypeTree, Hash: fixedHash(0xbb), Size: 20},
			{Name: "beta.bin", Type: ObjTypeFile, Hash: fixedHash(0xcc), Size: 30},
		},
	}
	parent := fixedHash(0x42)
	commit := Commit{
		LibraryID:   uuid.MustParse("00000000-0000-0000-0000-0000000000ff"),
		Parent:      &parent,
		RootTree:    fixedHash(0x99),
		Ctime:       time.Unix(0, 1_700_000_000_000_000_000).UTC(),
		Description: "first commit",
	}

	fileBytes, fileH, err := encodeFileObject(fileObj)
	if err != nil {
		t.Fatalf("encodeFileObject: %v", err)
	}
	treeBytes, treeH, err := encodeTreeObject(treeObj)
	if err != nil {
		t.Fatalf("encodeTreeObject: %v", err)
	}
	commitBytes, commitH, err := encodeCommit(commit)
	if err != nil {
		t.Fatalf("encodeCommit: %v", err)
	}

	if regenerateGolden || goldenFileBytes == "" {
		t.Fatalf("REGENERATE golden vectors:\n"+
			"const (\n"+
			"\tgoldenFileBytes   = %q\n\tgoldenFileHash    = %q\n"+
			"\tgoldenTreeBytes   = %q\n\tgoldenTreeHash    = %q\n"+
			"\tgoldenCommitBytes = %q\n\tgoldenCommitHash  = %q\n)\n",
			fileBytes, fileH, treeBytes, treeH, commitBytes, commitH)
	}

	check := func(name string, gotBytes []byte, gotHash blockstore.Hash, wantBytes, wantHash string) {
		if string(gotBytes) != wantBytes {
			t.Errorf("%s canonical bytes changed:\n got:  %s\n want: %s", name, gotBytes, wantBytes)
		}
		if gotHash.String() != wantHash {
			t.Errorf("%s hash changed: got %s want %s", name, gotHash, wantHash)
		}
		// Self-verification (rule C): the bytes must hash to the address.
		if !VerifyContent(gotBytes, gotHash) {
			t.Errorf("%s: VerifyContent failed", name)
		}
	}

	check("file", fileBytes, fileH, goldenFileBytes, goldenFileHash)
	check("tree", treeBytes, treeH, goldenTreeBytes, goldenTreeHash)
	check("commit", commitBytes, commitH, goldenCommitBytes, goldenCommitHash)
}

// Golden constants — populated by running with regenerateGolden = true.
const (
	goldenFileBytes   = "{\"size\":4096,\"blocks\":[{\"hash\":\"1111111111111111111111111111111111111111111111111111111111111111\",\"size\":3000},{\"hash\":\"2222222222222222222222222222222222222222222222222222222222222222\",\"size\":1096}]}"
	goldenFileHash    = "f4202dcc05f24a914a6a10a9787d50c949a3fde98cf7dc9be28138bf549cc700"
	goldenTreeBytes   = "{\"entries\":[{\"name\":\"alpha\",\"type\":\"tree\",\"hash\":\"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\",\"size\":20},{\"name\":\"beta.bin\",\"type\":\"file\",\"hash\":\"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc\",\"size\":30},{\"name\":\"zeta.txt\",\"type\":\"file\",\"hash\":\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"size\":10}]}"
	goldenTreeHash    = "2593adf7e28337fe089e66532ab44f9b4c8c7510379f034449779bc9eb64325d"
	goldenCommitBytes = "{\"library_id\":\"00000000-0000-0000-0000-0000000000ff\",\"parent\":\"4242424242424242424242424242424242424242424242424242424242424242\",\"root_tree\":\"9999999999999999999999999999999999999999999999999999999999999999\",\"ctime_unix_nano\":1700000000000000000,\"description\":\"first commit\"}"
	goldenCommitHash  = "b3abf7e279284752c7d58e24335ac749b6f9a15210cc695e33f9e016fd39e2bc"
)
