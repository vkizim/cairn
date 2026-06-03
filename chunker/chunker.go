// Package chunker performs content-defined chunking (CDC) of a byte stream and
// computes the SHA-256 address of each chunk. It wraps the FastCDC
// implementation in github.com/PlakarKorp/go-cdc-chunkers behind a small,
// stable API so the third-party type never leaks into the rest of Cairn.
//
// # Reproducibility contract
//
// Block addressing depends on chunk boundaries being identical everywhere. The
// algorithm name, normalization level, gear table, and size parameters are all
// pinned (the constants below plus the library's hardcoded NC=2 normalization
// and fixed gear table). Identical input therefore produces identical chunk
// boundaries and hashes across runs and across machines. The golden-vector test
// locks this down; changing any pinned value is a breaking change to addressing.
//
// # Portability
//
// This package imports ONLY the chunker library, the standard library, and
// blockstore (for the Hash type). It contains no cgo and no server-only
// dependency, so it compiles unchanged for GOOS=js GOARCH=wasm:
//
//	CGO_ENABLED=0 GOOS=js GOARCH=wasm go build ./chunker/
//
// This is intentional: a future encrypted-library mode will chunk in the
// browser (this code compiled to WASM) so plaintext never reaches the server,
// and client and server MUST produce identical boundaries by running the same
// implementation. Do not add server-only imports here.
package chunker

import (
	"crypto/sha256"
	"fmt"
	"io"

	chunkers "github.com/PlakarKorp/go-cdc-chunkers"
	// Registers the "fastcdc" algorithm with the chunkers framework via init().
	_ "github.com/PlakarKorp/go-cdc-chunkers/chunkers/fastcdc"

	"github.com/vkizim/cairn/blockstore"
)

// Pinned chunking parameters. These define block addressing and MUST NOT change
// without a deliberate migration — any change shifts chunk boundaries and
// invalidates every previously stored address.
const (
	// Algorithm is the CDC algorithm name registered with go-cdc-chunkers.
	// FastCDC uses two-mask normalized chunking; the library hardcodes
	// normalization level 2 (NC=2 from the FastCDC paper).
	//
	// We use the version-pinned "fastcdc-v1.0.0" rather than the bare "fastcdc"
	// on purpose. The bare name registers go-cdc-chunkers' *legacy* FastCDC,
	// whose Setup() hardcodes masks tuned for an 8 KiB NormalSize and silently
	// ignores the NormalSize we pass — collapsing every chunk to ~MinSize. The
	// "-v1.0.0" variant derives its masks from our NormalSize (so we actually
	// get the 8 MiB average), and the version suffix pins the mask derivation:
	// a future library change to the algorithm ships under a new name, leaving
	// our boundaries stable. The golden-vector test guards this.
	Algorithm = "fastcdc-v1.0.0"

	MinSize    = 2 << 20  // 2 MiB  — chunks are never smaller (except the final one)
	NormalSize = 8 << 20  // 8 MiB  — target/average chunk size
	MaxSize    = 16 << 20 // 16 MiB — chunks are never larger
)

// Config holds the chunking parameters. Use DefaultConfig unless a test needs
// smaller chunks; production paths must use the pinned defaults.
type Config struct {
	Min    int // minimum chunk size in bytes
	Normal int // target/average chunk size in bytes
	Max    int // maximum chunk size in bytes
}

// DefaultConfig is the pinned production configuration (2/8/16 MiB).
func DefaultConfig() Config {
	return Config{Min: MinSize, Normal: NormalSize, Max: MaxSize}
}

func (c Config) validate() error {
	switch {
	case c.Min <= 0:
		return fmt.Errorf("chunker: Min must be > 0, got %d", c.Min)
	case c.Normal < c.Min:
		return fmt.Errorf("chunker: Normal (%d) must be >= Min (%d)", c.Normal, c.Min)
	case c.Max < c.Normal:
		return fmt.Errorf("chunker: Max (%d) must be >= Normal (%d)", c.Max, c.Normal)
	}
	return nil
}

// Chunk is a single content-defined chunk and its content address.
type Chunk struct {
	Hash   blockstore.Hash // SHA-256 of Data
	Data   []byte          // raw chunk bytes — see lifetime note on Split/Chunk
	Offset uint64          // byte offset of this chunk within the stream
	Length uint            // len(Data)
}

// Split reads r to EOF, splitting it into content-defined chunks and invoking
// fn once per chunk in stream order. It never buffers the whole input; memory
// use is bounded by the max chunk size.
//
// Lifetime: Chunk.Data may alias an internal buffer that the chunker reuses on
// the next read. It is valid only for the duration of the fn call. fn must copy
// the bytes if it needs to retain them beyond the call (Chunk.Hash is always
// safe to keep). Ingest hashes and stores synchronously inside fn, so it does
// not copy.
//
// If fn returns an error, Split stops and returns that error. A read error from
// r is wrapped and returned. An empty stream produces zero chunks and nil.
func Split(r io.Reader, cfg Config, fn func(Chunk) error) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	c, err := chunkers.NewChunker(Algorithm, r, &chunkers.ChunkerOpts{
		MinSize:    cfg.Min,
		NormalSize: cfg.Normal,
		MaxSize:    cfg.Max,
	})
	if err != nil {
		return fmt.Errorf("chunker: init %q: %w", Algorithm, err)
	}

	var offset uint64
	for {
		data, err := c.Next()
		if len(data) > 0 {
			ck := Chunk{
				Hash:   blockstore.Hash(sha256.Sum256(data)),
				Data:   data,
				Offset: offset,
				Length: uint(len(data)),
			}
			if cbErr := fn(ck); cbErr != nil {
				return cbErr
			}
			offset += uint64(len(data))
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("chunker: read at offset %d: %w", offset, err)
		}
	}
}
