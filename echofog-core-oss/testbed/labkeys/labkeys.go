// Package labkeys derives the masking keys of the live lab from the run's seed, so the servers in
// the containers, their clients and the runner (which builds the prober's first flights) agree
// without a card file. Lab only: the keys are a function of a number that the report prints.
package labkeys

import (
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/binary"
)

// Mask is the X25519 masking key of one suite for a seed.
func Mask(seed int64, suite string) *ecdh.PrivateKey {
	h := sha256.New()
	h.Write([]byte("echofog launchpad live mask key " + suite))
	var s [8]byte
	binary.BigEndian.PutUint64(s[:], uint64(seed))
	h.Write(s[:])
	k, err := ecdh.X25519().NewPrivateKey(h.Sum(nil))
	if err != nil {
		panic(err) // any 32 bytes are a valid X25519 key
	}
	return k
}
