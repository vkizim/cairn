package chunker

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"testing"

	"github.com/vkizim/cairn/blockstore"
)

// deterministicBytes fills a buffer with a reproducible pseudo-random stream
// using xorshift64* seeded by seed. It deliberately does NOT use math/rand: this
// generator's output must be identical across Go versions, platforms, and
// (critically) when this package is compiled to WASM, so the golden vectors
// below form a stable cross-environment contract.
func deterministicBytes(n int, seed uint64) []byte {
	b := make([]byte, n)
	s := seed
	var word [8]byte
	for i := 0; i < n; i += 8 {
		s ^= s >> 12
		s ^= s << 25
		s ^= s >> 27
		v := s * 0x2545F4914F6CDD1D
		binary.LittleEndian.PutUint64(word[:], v)
		copy(b[i:], word[:])
	}
	return b
}

// collectChunks runs Split and returns every chunk (with a copy of its data,
// since Split reuses its buffer).
func collectChunks(t *testing.T, data []byte, cfg Config) []Chunk {
	t.Helper()
	var out []Chunk
	err := Split(bytes.NewReader(data), cfg, func(c Chunk) error {
		cp := append([]byte(nil), c.Data...)
		out = append(out, Chunk{Hash: c.Hash, Data: cp, Offset: c.Offset, Length: c.Length})
		return nil
	})
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	return out
}

func TestSplitInvariants(t *testing.T) {
	cfg := DefaultConfig()
	tests := []struct {
		name       string
		size       int
		wantChunks int // -1 means "don't assert exact count"
	}{
		{"empty", 0, 0},
		{"one byte", 1, 1},
		{"below min", 100 << 10, 1},          // 100 KiB < 2 MiB min -> single chunk
		{"just below min", MinSize - 1, 1},   // still one chunk
		{"at min", MinSize, 1},               // exactly min -> one chunk (boundary not forced until > min)
		{"above max single span", MaxSize, -1},
		{"large multi-chunk", 40 << 20, -1},  // 40 MiB -> several chunks
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := deterministicBytes(tc.size, 0xC0FFEE)
			chunks := collectChunks(t, data, cfg)

			if tc.wantChunks >= 0 && len(chunks) != tc.wantChunks {
				t.Fatalf("got %d chunks, want %d", len(chunks), tc.wantChunks)
			}

			// Reassembly: concatenated chunks must equal the input exactly,
			// offsets must be contiguous, and each hash must address its bytes.
			var reassembled bytes.Buffer
			var expectOffset uint64
			for i, c := range chunks {
				if c.Offset != expectOffset {
					t.Fatalf("chunk %d offset = %d, want %d", i, c.Offset, expectOffset)
				}
				if c.Length != uint(len(c.Data)) {
					t.Fatalf("chunk %d Length = %d, len(Data) = %d", i, c.Length, len(c.Data))
				}
				if want := blockstore.Hash(sha256.Sum256(c.Data)); want != c.Hash {
					t.Fatalf("chunk %d hash mismatch", i)
				}
				if int(c.Length) > cfg.Max {
					t.Fatalf("chunk %d length %d exceeds Max %d", i, c.Length, cfg.Max)
				}
				// Every chunk except the last must be at least Min.
				if i < len(chunks)-1 && int(c.Length) < cfg.Min {
					t.Fatalf("non-final chunk %d length %d below Min %d", i, c.Length, cfg.Min)
				}
				reassembled.Write(c.Data)
				expectOffset += uint64(c.Length)
			}
			if !bytes.Equal(reassembled.Bytes(), data) {
				t.Fatal("reassembled chunks do not equal input")
			}
		})
	}
}

// TestSplitDeterminism asserts that chunking the same input twice — through two
// independent chunker instances (each Split call creates a fresh one) — yields
// byte-identical boundaries and hashes. This is the foundation of stable block
// addressing.
func TestSplitDeterminism(t *testing.T) {
	data := deterministicBytes(40<<20, 0xABCDEF)
	cfg := DefaultConfig()

	a := collectChunks(t, data, cfg)
	b := collectChunks(t, data, cfg)

	if len(a) != len(b) {
		t.Fatalf("chunk counts differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Offset != b[i].Offset || a[i].Length != b[i].Length || a[i].Hash != b[i].Hash {
			t.Fatalf("chunk %d differs between runs: %+v vs %+v",
				i, a[i].Hash.String(), b[i].Hash.String())
		}
	}
}

// goldenVector is one expected chunk in the reference set.
type goldenVector struct {
	offset uint64
	length uint
	hash   string
}

// goldenInputSize / goldenSeed define the fixed reference input.
const (
	goldenInputSize = 40 << 20 // 40 MiB
	goldenSeed      = 0x5EED1234
)

// goldenChunks pins the exact chunk boundaries and hashes produced for the
// fixed reference input by the pinned FastCDC parameters. It is a
// CROSS-ENVIRONMENT CONTRACT: the same input must yield these same boundaries
// and hashes regardless of where the code runs — native or compiled to WASM —
// because client (browser) and server must agree on block addresses bit-for-bit.
//
// If this test fails, chunk boundaries shifted. That is a BREAKING change to
// block addressing (every previously stored address is invalidated) and must be
// intentional, versioned, and migrated — never silently accepted.
var goldenChunks = []goldenVector{
	{offset: 0, length: 9364828, hash: "54652f22ba767099364184c4809d39d42b84c564f4a2f9d8f607f4a9c06c78d0"},
	{offset: 9364828, length: 10727117, hash: "4162a1c2d768d999747a58cc62cf625013e601422aa2ee3d4a828317fb68968b"},
	{offset: 20091945, length: 8579647, hash: "5dfa8dcbe28379f1cc33830862c3c6709f9a65d2b458dd5525ddeda8b1cb636a"},
	{offset: 28671592, length: 8480285, hash: "554c3adf8a420271da0d9b61352e2ad34ec366d1a43610a3ef969eb40dc52739"},
	{offset: 37151877, length: 4791163, hash: "0727f28240a2be28df1ce9fc99be01068a27f1fbbb50ae522b01072697f7c980"},
}

// Set to true, run `go test -run TestChunkerGolden -v`, copy the printed
// literals into goldenChunks, then set back to false.
const regenerateGolden = false

func TestChunkerGolden(t *testing.T) {
	data := deterministicBytes(goldenInputSize, goldenSeed)
	chunks := collectChunks(t, data, DefaultConfig())

	if regenerateGolden || len(goldenChunks) == 0 {
		var sb bytes.Buffer
		fmt.Fprintf(&sb, "\nvar goldenChunks = []goldenVector{\n")
		for _, c := range chunks {
			fmt.Fprintf(&sb, "\t{offset: %d, length: %d, hash: %q},\n", c.Offset, c.Length, c.Hash.String())
		}
		fmt.Fprintf(&sb, "}\n")
		t.Fatalf("golden vectors not set (regenerate): %s", sb.String())
	}

	if len(chunks) != len(goldenChunks) {
		t.Fatalf("got %d chunks, golden has %d", len(chunks), len(goldenChunks))
	}
	for i, c := range chunks {
		g := goldenChunks[i]
		if c.Offset != g.offset || c.Length != g.length || c.Hash.String() != g.hash {
			t.Errorf("chunk %d mismatch:\n  got  offset=%d length=%d hash=%s\n  want offset=%d length=%d hash=%s",
				i, c.Offset, c.Length, c.Hash, g.offset, g.length, g.hash)
		}
	}
}

// TestSplitCallbackError verifies an error from the callback aborts Split.
func TestSplitCallbackError(t *testing.T) {
	data := deterministicBytes(40<<20, 1)
	sentinel := io.ErrUnexpectedEOF
	err := Split(bytes.NewReader(data), DefaultConfig(), func(Chunk) error {
		return sentinel
	})
	if err != sentinel {
		t.Fatalf("got %v, want %v", err, sentinel)
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"default ok", DefaultConfig(), false},
		{"zero min", Config{0, 8, 16}, true},
		{"normal below min", Config{8, 4, 16}, true},
		{"max below normal", Config{2, 8, 4}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("validate() err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}
