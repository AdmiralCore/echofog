// Package identity implements the key hierarchy of protocol v2 (README.md §5.1–5.2,
// objects.md §1, §6).
//
// The account key is Ed25519 and is derived from the BIP-39 mnemonic exactly as v1's identity key:
// SLIP-0010, path m/44'/999'/account'/0'. Its algorithm-tagged form (0x01 ‖ pub) hashed with
// SHA-256 is the node ID, which contacts, links and chats use and which survives restoring the
// phrase on another installation.
//
// Everything else is random and never derived from the account key or the node ID (K1): the device
// key (Ed25519), the Noise static key hs and the masking keys (X25519). The device key is what the
// network names (device ID); it signs the device's cards and prekeys. A new install of the same
// phrase is a new device in the same account, and the account key signs the device list
// (echofog/devices/2, objects.md §6).
//
// Private keys live in package secret: locked pages where the platform allows it, ordinary memory
// wiped on destroy otherwise. A keys.Signer built for one signature is a temporary copy inside the
// secret callback and is not kept.
package identity

import (
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"github.com/tyler-smith/go-bip39"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/secret"
)

// DefaultAccount is the BIP-44 account index of the main identity.
const DefaultAccount uint32 = 0

// StorageKeyLabel is the HKDF info of the local storage key (services.md §4.0); it replaces v1's
// FOG_STORAGE_KEY_V1.
const StorageKeyLabel = "echofog/storage2/key"

// storageKeyIndex is the SLIP-0010 child index of the local storage key; v1 used the same index
// and changed only the HKDF label.
const storageKeyIndex = 4

var (
	// ErrPhrase reports a recovery phrase that is not a 12- or 24-word BIP-39 phrase.
	ErrPhrase = errors.New("identity: invalid recovery phrase")
	// ErrNoProtector reports that the platform has no way to wrap the storage key at rest.
	ErrNoProtector = errors.New("identity: no protector for the storage key")
	// ErrKeyLength reports a private key of the wrong length.
	ErrKeyLength = errors.New("identity: key of the wrong length")
)

// Account is a person's long-term identity: the account key and the node ID derived from it.
type Account struct {
	index      uint32
	seed       *secret.Value // Ed25519 seed at m/44'/999'/index'/0'
	storageKey *secret.Value // L6 key (services.md §4.0)
	pub        keys.AKey
	nodeID     [32]byte
}

// NewMnemonic returns a new BIP-39 recovery phrase of 12 or 24 words.
func NewMnemonic(words int) (string, error) {
	var bits int
	switch words {
	case 12:
		bits = 128
	case 24:
		bits = 256
	default:
		return "", fmt.Errorf("%w: %d words", ErrPhrase, words)
	}
	entropy, err := bip39.NewEntropy(bits)
	if err != nil {
		return "", err
	}
	defer secret.Wipe(entropy)
	return bip39.NewMnemonic(entropy)
}

// FromMnemonic restores the main account from a recovery phrase and an optional BIP-39 passphrase.
// The phrase is lowercased and its whitespace is normalised before use, so a phrase typed with
// capitals or line breaks gives the same identity.
func FromMnemonic(phrase, passphrase string) (*Account, error) {
	return FromMnemonicIndex(phrase, passphrase, DefaultAccount)
}

// FromMnemonicIndex is FromMnemonic for a non-default BIP-44 account index.
func FromMnemonicIndex(phrase, passphrase string, account uint32) (*Account, error) {
	norm, err := normalizePhrase(phrase)
	if err != nil {
		return nil, err
	}
	masterSeed := bip39.NewSeed(norm, passphrase)
	defer secret.Wipe(masterSeed)
	return newAccount(masterSeed, account)
}

// normalizePhrase accepts a phrase as typed and returns its canonical BIP-39 form.
func normalizePhrase(phrase string) (string, error) {
	words := strings.Fields(strings.ToLower(phrase))
	switch len(words) {
	case 12, 24:
	default:
		return "", fmt.Errorf("%w: %d words", ErrPhrase, len(words))
	}
	norm := strings.Join(words, " ")
	if !bip39.IsMnemonicValid(norm) {
		return "", fmt.Errorf("%w: unknown word or wrong checksum", ErrPhrase)
	}
	return norm, nil
}

// newAccount derives the account key, its node ID and the local storage key from a 64-byte
// BIP-39 seed. A random seed gives a throwaway account (see NewExitIdentity).
func newAccount(masterSeed []byte, index uint32) (*Account, error) {
	key, chain := slip10Master(masterSeed)
	return accountFromMasterOwned(key, chain, index)
}

// accountFromMasterOwned derives the account from a master pair it owns: both slices are wiped.
func accountFromMasterOwned(key, chain []byte, index uint32) (*Account, error) {
	key, chain = slip10Path(key, chain, 44, 999, index)
	defer secret.Wipe(key)
	defer secret.Wipe(chain)

	idSeed, _ := slip10Child(key, chain, 0)
	defer secret.Wipe(idSeed)
	storageSeed, _ := slip10Child(key, chain, storageKeyIndex)
	defer secret.Wipe(storageSeed)

	a, err := accountFromSeeds(index, idSeed, storageSeed)
	if err != nil {
		return nil, err
	}
	return a, nil
}

func accountFromSeeds(index uint32, idSeed, storageSeed []byte) (*Account, error) {
	signer, err := keys.NewEd25519Signer(idSeed)
	if err != nil {
		return nil, err
	}
	pub := signer.Public()

	key, err := hkdf.Key(sha256.New, storageSeed, nil, StorageKeyLabel, ed25519.SeedSize)
	if err != nil {
		return nil, err
	}
	defer secret.Wipe(key)

	return &Account{
		index:      index,
		seed:       secret.New(idSeed),
		storageKey: secret.New(key),
		pub:        append(keys.AKey(nil), pub...),
		nodeID:     pub.ID(),
	}, nil
}

// Index returns the BIP-44 account index.
func (a *Account) Index() uint32 { return a.index }

// Public returns the algorithm-tagged account key. The caller may keep the copy.
func (a *Account) Public() keys.AKey { return append(keys.AKey(nil), a.pub...) }

// NodeID returns SHA-256 of the algorithm-tagged account key (README.md §5.2).
func (a *Account) NodeID() [32]byte { return a.nodeID }

// WithSigner runs fn with a signer for the account key. The signer lives only for the call.
func (a *Account) WithSigner(fn func(*keys.Signer) error) error {
	if fn == nil {
		return nil
	}
	return a.seed.Use(func(seed []byte) error {
		signer, err := keys.NewEd25519Signer(seed)
		if err != nil {
			return err
		}
		return fn(signer)
	})
}

// Sign signs msg with the account key.
func (a *Account) Sign(msg []byte) ([]byte, error) {
	var sig []byte
	err := a.WithSigner(func(s *keys.Signer) error {
		sig = s.Sign(msg)
		return nil
	})
	return sig, err
}

// StorageKey returns the L6 key that seals stored values (services.md §4.0). The Account owns it:
// the caller must not destroy it.
func (a *Account) StorageKey() *secret.Value { return a.storageKey }

// Destroy wipes the account secrets. It is idempotent.
func (a *Account) Destroy() {
	a.seed.Destroy()
	a.storageKey.Destroy()
}
