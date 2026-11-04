package keymanager

import "github.com/awnumar/memguard"

// V1StorageKeyInfo is the HKDF info of the key that seals secrets at rest (L6) in v1. It must
// not change: an account sealed with it must open with it.
const V1StorageKeyInfo = "FOG_STORAGE_KEY_V1"

// IdentityKeySource gives the X3DH identity DH key. *AccountManager satisfies it: v1 derives the
// key at index 1' of the account. v2 passes its own source, which keeps a random key per device
// and leaves that derivation path unused (objects §5).
type IdentityKeySource interface {
	GetEncryptionKey() (*memguard.Enclave, []byte, error)
}

// Options tune one account. The zero value is v1.
type Options struct {
	// StorageKeyInfo is the HKDF info of the key that seals secrets at rest. Empty means the v1
	// label; v2 renames it to "echofog/storage2/key" (services §4.0).
	StorageKeyInfo string
	// IdentityKeySource replaces the account derivation of the X3DH identity DH key; nil means
	// the v1 derivation at index 1'.
	IdentityKeySource IdentityKeySource
}

// storageKeyInfo is the label actually used.
func (o Options) storageKeyInfo() string {
	if o.StorageKeyInfo == "" {
		return V1StorageKeyInfo
	}
	return o.StorageKeyInfo
}
