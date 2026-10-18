// Package filexfer sends files of any size between two peers: content-addressed, chunk by chunk,
// directly over QUIC or through a relay circuit.
//
// The design takes from BitTorrent v2 (BEP 52: a SHA-256 Merkle tree over fixed-size blocks, the
// root names the content) and from verified streaming as in Bao/iroh (every chunk travels with the
// proof that ties it to the root, so the receiver checks each chunk on its own the moment it
// arrives, needs no piece list up front and never keeps a byte it has not verified). Like IPFS
// Bitswap the receiver pulls: it asks for the chunks it wants, a window at a time, and the sender
// only answers. The root, the size and a fresh key reach the receiver inside an end-to-end
// encrypted chat message; every transfer frame is sealed with keys derived from that key, so a
// relay sees nothing and cannot inject or alter a frame.
package filexfer

import (
	"crypto/sha256"
	"errors"
	"io"
)

// ChunkSize is the unit of hashing, requesting and sending. 64 KiB keeps one frame short enough
// to cross a slow relay hop well within a stream's write deadline, and the proof that travels with
// it (log2 of the chunk count, 32 bytes each) under 1% of the data.
const ChunkSize = 64 << 10

// Hash is a SHA-256 digest.
type Hash [32]byte

// NumChunks is how many chunks a file of size bytes has. An empty file still has one (empty) chunk,
// so every file has a root.
func NumChunks(size int64) int {
	if size <= 0 {
		return 1
	}
	return int((size + ChunkSize - 1) / ChunkSize)
}

// chunkLen is the length of chunk i of a file of size bytes.
func chunkLen(size int64, i int) int {
	off := int64(i) * ChunkSize
	if off >= size {
		return 0
	}
	if size-off < ChunkSize {
		return int(size - off)
	}
	return ChunkSize
}

// LeafHash hashes one chunk. Leaves and inner nodes get different prefixes (as in RFC 6962), so an
// inner node can never be passed off as a chunk.
func LeafHash(data []byte) Hash {
	h := sha256.New()
	h.Write([]byte{0})
	h.Write(data)
	var out Hash
	h.Sum(out[:0])
	return out
}

func nodeHash(l, r Hash) Hash {
	var buf [65]byte
	buf[0] = 1
	copy(buf[1:], l[:])
	copy(buf[33:], r[:])
	return sha256.Sum256(buf[:])
}

// Tree is the Merkle tree over a file's chunks, kept level by level: levels[0] are the leaves, the
// last level holds the root. A node without a partner at the end of a level moves up unchanged,
// which gives exactly the RFC 6962 tree hash (the left subtree always covers the largest power of
// two of leaves).
type Tree struct {
	levels [][]Hash
}

// NewTree builds the tree over leaves (at least one).
func NewTree(leaves []Hash) *Tree {
	if len(leaves) == 0 {
		leaves = []Hash{LeafHash(nil)}
	}
	levels := [][]Hash{leaves}
	for cur := leaves; len(cur) > 1; {
		next := make([]Hash, 0, (len(cur)+1)/2)
		for i := 0; i < len(cur); i += 2 {
			if i+1 < len(cur) {
				next = append(next, nodeHash(cur[i], cur[i+1]))
			} else {
				next = append(next, cur[i])
			}
		}
		levels = append(levels, next)
		cur = next
	}
	return &Tree{levels: levels}
}

// Root names the content.
func (t *Tree) Root() Hash { return t.levels[len(t.levels)-1][0] }

// Leaves is the number of chunks.
func (t *Tree) Leaves() int { return len(t.levels[0]) }

// Proof is the audit path of chunk i: the sibling hashes from the leaf up to the root, leaving out
// the levels where the node has no partner.
func (t *Tree) Proof(i int) []Hash {
	if i < 0 || i >= t.Leaves() {
		return nil
	}
	var path []Hash
	for _, level := range t.levels[:len(t.levels)-1] {
		sib := i ^ 1
		if sib < len(level) {
			path = append(path, level[sib])
		}
		i /= 2
	}
	return path
}

// VerifyProof checks that leaf is chunk i of a tree of n chunks with the given root (RFC 9162,
// 2.1.3.2).
func VerifyProof(root Hash, i, n int, leaf Hash, proof []Hash) bool {
	if i < 0 || n <= 0 || i >= n {
		return false
	}
	fn, sn := i, n-1
	r := leaf
	for _, p := range proof {
		if sn == 0 {
			return false
		}
		if fn&1 == 1 || fn == sn {
			r = nodeHash(p, r)
			if fn&1 == 0 {
				for fn&1 == 0 && fn != 0 {
					fn >>= 1
					sn >>= 1
				}
			}
		} else {
			r = nodeHash(r, p)
		}
		fn >>= 1
		sn >>= 1
	}
	return sn == 0 && r == root
}

// errShortRead: the file is shorter than it claims to be.
var errShortRead = errors.New("file shorter than expected")

// readChunk fills buf from off; an io.EOF that comes with a full buffer is no error.
func readChunk(r io.ReaderAt, buf []byte, off int64) error {
	n, err := r.ReadAt(buf, off)
	if n == len(buf) {
		return nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		return errShortRead
	}
	return err
}

// HashFile reads size bytes of r chunk by chunk and builds their tree.
func HashFile(r io.ReaderAt, size int64) (*Tree, error) {
	n := NumChunks(size)
	leaves := make([]Hash, n)
	buf := make([]byte, ChunkSize)
	for i := 0; i < n; i++ {
		l := chunkLen(size, i)
		if err := readChunk(r, buf[:l], int64(i)*ChunkSize); err != nil {
			return nil, err
		}
		leaves[i] = LeafHash(buf[:l])
	}
	return NewTree(leaves), nil
}
