package dht

import (
	"math/bits"

	"github.com/echofog/echofog-core/pkg/api/types"
)

const IDLength = 32

func getBucketIndex(localID, remoteID types.PeerID) int {
	for i := 0; i < IDLength; i++ {
		xor := localID[i] ^ remoteID[i]
		if xor != 0 {
			return i*8 + bits.LeadingZeros8(xor)
		}
	}
	return IDLength*8 - 1
}
