package handshake

import (
	"crypto/sha256"
	"encoding/binary"
	"math/big"
)

type PuzzleProtocol struct {
	NoncePrefix []byte
	Difficulty  uint32
}

// maxPuzzleDifficulty bounds the work a remote responder can demand (2^24 hashes, well under
// a second); without it a hostile peer could pin our CPU forever.
const maxPuzzleDifficulty = 24

func SolvePuzzle(prefix []byte, difficulty uint32) uint64 {
	var nonce uint64 = 0
	target := big.NewInt(1)
	target.Lsh(target, 256-uint(difficulty))

	hashBuf := make([]byte, len(prefix)+8)
	copy(hashBuf, prefix)

	for {
		binary.BigEndian.PutUint64(hashBuf[len(prefix):], nonce)

		hash := sha256.Sum256(hashBuf)

		hashInt := new(big.Int).SetBytes(hash[:])
		if hashInt.Cmp(target) == -1 {
			return nonce
		}

		nonce++
	}
}

func VerifyPuzzle(prefix []byte, difficulty uint32, nonce uint64) bool {
	target := big.NewInt(1)
	target.Lsh(target, 256-uint(difficulty))

	hashBuf := make([]byte, len(prefix)+8)
	copy(hashBuf, prefix)
	binary.BigEndian.PutUint64(hashBuf[len(prefix):], nonce)

	hash := sha256.Sum256(hashBuf)
	hashInt := new(big.Int).SetBytes(hash[:])

	return hashInt.Cmp(target) == -1
}
