package identity

import "errors"

// ErrMaster reports a SLIP-0010 master key or chain code that is not 32 bytes.
var ErrMaster = errors.New("identity: bad master key")

// FromMaster restores the account from the SLIP-0010 master key and chain code (the HMAC-SHA512 of
// the BIP-39 seed under "ed25519 seed", split in two). The desktop keeps exactly that pair of the
// v1 identity, never the phrase, and AD7 requires the same account key as v1: the derivation from
// here on is the one FromMnemonicIndex runs. The caller keeps its slices; FromMaster copies them.
func FromMaster(key, chain []byte, index uint32) (*Account, error) {
	return nil, errors.New("identity: FromMaster is not built")
}
