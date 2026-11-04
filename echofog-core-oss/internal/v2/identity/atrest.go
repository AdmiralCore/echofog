package identity

import (
	"crypto/ed25519"
	"fmt"

	"github.com/echofog/echofog-core/internal/protection/osprotect"
	"github.com/echofog/echofog-core/internal/v2/secret"
)

// Protector seals the local storage key for storage on disk (platforms.md §1.2, flag
// secret_store). It is the app's Android Keystore / iOS Keychain callback, or the operating
// system's protector. The entropy is mixed in, so a blob sealed for one profile does not open for
// another.
type Protector interface {
	Protect(secret, entropy []byte) ([]byte, error)
	Unprotect(blob, entropy []byte) ([]byte, error)
}

// SystemProtector returns the operating system's protector for the current user (DPAPI on
// Windows), or nil where there is none yet.
func SystemProtector() Protector {
	if p := osprotect.System(); p != nil {
		return p
	}
	return nil
}

// WrapStorageKey seals the account's L6 key with p. Store the blob where the profile lives (AD7);
// the plain key never touches the disk.
func (a *Account) WrapStorageKey(p Protector, entropy []byte) ([]byte, error) {
	if p == nil {
		return nil, ErrNoProtector
	}
	var blob []byte
	err := a.storageKey.Use(func(key []byte) error {
		var err error
		blob, err = p.Protect(key, entropy)
		return err
	})
	if err != nil {
		return nil, err
	}
	return blob, nil
}

// UnwrapStorageKey opens a blob made by WrapStorageKey. It returns the key in protected memory.
func UnwrapStorageKey(p Protector, blob, entropy []byte) (*secret.Value, error) {
	if p == nil {
		return nil, ErrNoProtector
	}
	key, err := p.Unprotect(blob, entropy)
	if err != nil {
		return nil, err
	}
	defer secret.Wipe(key)
	if len(key) != ed25519.SeedSize {
		return nil, fmt.Errorf("%w: storage key is %d bytes", ErrKeyLength, len(key))
	}
	return secret.New(key), nil
}
