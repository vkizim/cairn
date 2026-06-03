package blockstore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// HashSize is the length in bytes of a block address (SHA-256).
const HashSize = sha256.Size // 32

// Hash is the content address of a block: the SHA-256 of its raw bytes.
//
// It is a *defined* type (not an alias for [32]byte) so the compiler treats it
// as distinct from an arbitrary byte array — you cannot accidentally pass a
// random [32]byte where a Hash is expected. This is deliberately stronger than
// the `type Hash = [32]byte` alias originally sketched in the plan.
type Hash [HashSize]byte

// HashData computes the address of a block from its raw bytes.
func HashData(data []byte) Hash {
	return Hash(sha256.Sum256(data))
}

// String returns the lowercase hex encoding of the hash (64 chars).
func (h Hash) String() string {
	return hex.EncodeToString(h[:])
}

// ParseHash decodes a 64-character lowercase hex string into a Hash.
func ParseHash(s string) (Hash, error) {
	var h Hash
	if len(s) != hex.EncodedLen(HashSize) {
		return h, fmt.Errorf("blockstore: invalid hash length %d, want %d", len(s), hex.EncodedLen(HashSize))
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return h, fmt.Errorf("blockstore: invalid hash %q: %w", s, err)
	}
	copy(h[:], b)
	return h, nil
}

// ShardPath returns the sharded relative path for filesystem storage:
// the first two hex chars, then the next two, then the full hex hash, e.g.
//
//	ab/cd/abcdef...   (for hash starting ab cd)
//
// Sharding keeps directory fan-out bounded (256 * 256 leaf dirs) so no single
// directory holds millions of entries.
func (h Hash) ShardPath() string {
	s := h.String()
	return fmt.Sprintf("%s/%s/%s", s[0:2], s[2:4], s)
}
