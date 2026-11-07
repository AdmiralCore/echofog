package routepolicy

import "crypto/ed25519"

// Parse reads a rules file without a clock: no expiry. Tests only; a deployed file goes through
// ParseAt.
func Parse(data []byte, pub ed25519.PublicKey, minSeq uint64) (*Rules, error) {
	r, _, err := parse(data, pub, minSeq)
	return r, err
}
