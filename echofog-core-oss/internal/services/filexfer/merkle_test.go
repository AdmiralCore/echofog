package filexfer

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mth is the RFC 6962 tree hash written out recursively, the reference the level-wise tree must match.
func mth(leaves []Hash) Hash {
	if len(leaves) == 1 {
		return leaves[0]
	}
	k := 1
	for k*2 < len(leaves) {
		k *= 2
	}
	return nodeHash(mth(leaves[:k]), mth(leaves[k:]))
}

func leavesN(n int) []Hash {
	out := make([]Hash, n)
	for i := range out {
		out[i] = LeafHash([]byte{byte(i), byte(i >> 8)})
	}
	return out
}

func TestTreeMatchesRFC6962AndEveryProofVerifies(t *testing.T) {
	for n := 1; n <= 70; n++ {
		leaves := leavesN(n)
		tree := NewTree(leaves)
		require.Equal(t, mth(leaves), tree.Root(), "n=%d", n)
		for i := 0; i < n; i++ {
			p := tree.Proof(i)
			assert.True(t, VerifyProof(tree.Root(), i, n, leaves[i], p), "n=%d i=%d", n, i)
			// the same proof does not hold for another position, another size or another leaf
			if n > 1 {
				assert.False(t, VerifyProof(tree.Root(), (i+1)%n, n, leaves[i], p), "n=%d i=%d other index", n, i)
				assert.False(t, VerifyProof(tree.Root(), i, n, leaves[(i+1)%n], p), "n=%d i=%d other leaf", n, i)
			}
			if len(p) > 0 {
				bad := append([]Hash(nil), p...)
				bad[len(bad)-1][0] ^= 1
				assert.False(t, VerifyProof(tree.Root(), i, n, leaves[i], bad), "n=%d i=%d flipped proof", n, i)
				assert.False(t, VerifyProof(tree.Root(), i, n, leaves[i], p[:len(p)-1]), "n=%d i=%d short proof", n, i)
			}
		}
	}
}

func TestProofRejectsOutOfRange(t *testing.T) {
	tree := NewTree(leavesN(5))
	assert.Nil(t, tree.Proof(5))
	assert.Nil(t, tree.Proof(-1))
	assert.False(t, VerifyProof(tree.Root(), 5, 5, LeafHash(nil), nil))
	assert.False(t, VerifyProof(tree.Root(), -1, 5, LeafHash(nil), nil))
}

func TestInnerNodeCannotPassAsChunk(t *testing.T) {
	leaves := leavesN(4)
	tree := NewTree(leaves)
	// the bytes behind an inner node, sent as a chunk, hash as a leaf (other prefix) and do not
	// verify: a sender cannot shorten the file by passing two chunks off as one
	preimage := append(append([]byte(nil), leaves[0][:]...), leaves[1][:]...)
	assert.False(t, VerifyProof(tree.Root(), 0, 2, LeafHash(preimage), []Hash{nodeHash(leaves[2], leaves[3])}))
	assert.True(t, VerifyProof(tree.Root(), 0, 2, nodeHash(leaves[0], leaves[1]), []Hash{nodeHash(leaves[2], leaves[3])}),
		"only the leaf prefix stands between the two; the chunk count comes from the size in the signed offer")
}

func TestChunking(t *testing.T) {
	assert.Equal(t, 1, NumChunks(0))
	assert.Equal(t, 1, NumChunks(1))
	assert.Equal(t, 1, NumChunks(ChunkSize))
	assert.Equal(t, 2, NumChunks(ChunkSize+1))
	assert.Equal(t, 0, chunkLen(0, 0))
	assert.Equal(t, 1, chunkLen(ChunkSize+1, 1))
	assert.Equal(t, ChunkSize, chunkLen(3*ChunkSize, 2))

	for _, size := range []int64{0, 1, ChunkSize - 1, ChunkSize, ChunkSize + 1, 5*ChunkSize + 77} {
		data := make([]byte, size)
		_, _ = rand.Read(data)
		tree, err := HashFile(bytes.NewReader(data), size)
		require.NoError(t, err)
		require.Equal(t, NumChunks(size), tree.Leaves())
		leaves := make([]Hash, NumChunks(size))
		for i := range leaves {
			off := int64(i) * ChunkSize
			leaves[i] = LeafHash(data[off : off+int64(chunkLen(size, i))])
		}
		assert.Equal(t, mth(leaves), tree.Root(), "size %d", size)
	}

	// a file shorter than claimed is an error, not a silently different root
	_, err := HashFile(bytes.NewReader(make([]byte, 10)), 20)
	assert.Error(t, err)
}

func TestFrameSealing(t *testing.T) {
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	k, err := deriveKeys(secret)
	require.NoError(t, err)
	var id [idLen]byte
	_, _ = rand.Read(id[:])

	f := seal(k.toSender, kindWant, id, encodeWant([]int{1, 2, 3}))
	kind, gotID, ok := parseHeader(f)
	require.True(t, ok)
	assert.Equal(t, kindWant, kind)
	assert.Equal(t, id, gotID)
	body, err := open(k.toSender, f)
	require.NoError(t, err)
	idx, err := decodeWant(body)
	require.NoError(t, err)
	assert.Equal(t, []int{1, 2, 3}, idx)

	// the other direction's key does not open it: a relay cannot bounce it back
	_, err = open(k.toReceiver, f)
	assert.Error(t, err)
	// the kind and the transfer ID are authenticated
	g := append([]byte(nil), f...)
	g[1] = byte(kindDone)
	_, err = open(k.toSender, g)
	assert.Error(t, err)
	g = append([]byte(nil), f...)
	g[5] ^= 1
	_, err = open(k.toSender, g)
	assert.Error(t, err)
	// so is every byte of the body
	g = append([]byte(nil), f...)
	g[len(g)-3] ^= 1
	_, err = open(k.toSender, g)
	assert.Error(t, err)
	// another key of the same shape opens nothing
	other := make([]byte, 32)
	_, _ = rand.Read(other)
	k2, _ := deriveKeys(other)
	_, err = open(k2.toSender, f)
	assert.Error(t, err)

	_, _, ok = parseHeader(f[:overhead-1])
	assert.False(t, ok)
	_, err = deriveKeys(secret[:16])
	assert.Error(t, err)

	_, err = decodeWant(encodeWant(make([]int, maxWant+1)))
	assert.Error(t, err)
	_, _, _, err = decodeChunk([]byte{0, 0, 0, 1, 3, 1, 2})
	assert.Error(t, err)
}
