package keymanager

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/awnumar/memguard"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// Secret keys that live in the database (prekeys, ratchet sessions) are sealed with a key derived
// from the account, so a copied profile folder does not reveal them without the identity, which is
// itself stored encrypted with the app password.

const (
	StorageKeyIndex = 4
	// SignedPreKeyRetention keeps a replaced signed prekey long enough for every bundle that names
	// it to expire (bundles are valid for 30 days).
	SignedPreKeyRetention = 35 * 24 * time.Hour
	// OPKRetention: a one-time prekey nobody used in this time was in a bundle that expired.
	OPKRetention = 35 * 24 * time.Hour

	spkCurrentKey = "spk:current"
	spkOldPrefix  = "spk:old:"
	opkPrefix     = "opk:"
)

var ErrSealedOpen = errors.New("sealed value cannot be opened")

// StorageKey is the key that seals secrets in the database.
func (am *AccountManager) StorageKey() (*memguard.Enclave, error) {
	am.mu.Lock()
	defer am.mu.Unlock()
	if am.storageKey != nil {
		return am.storageKey, nil
	}
	seedEnc, _, err := deriveSecureSLIP0010(am.AccountKey, am.AccountCC, HardenedIndexStandard+StorageKeyIndex)
	if err != nil {
		return nil, err
	}
	seed, err := seedEnc.Open()
	if err != nil {
		return nil, err
	}
	defer seed.Destroy()
	key := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, seed.Bytes(), nil, []byte("FOG_STORAGE_KEY_V1")), key); err != nil {
		return nil, err
	}
	am.storageKey = memguard.NewEnclave(key) // wipes key
	return am.storageKey, nil
}

// SealValue encrypts a secret for the database; name is the database key, so a sealed value cannot
// be moved under another name.
func SealValue(keyEnc *memguard.Enclave, name, plain []byte) ([]byte, error) {
	key, err := keyEnc.Open()
	if err != nil {
		return nil, err
	}
	defer key.Destroy()
	aead, err := chacha20poly1305.NewX(key.Bytes())
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plain)+aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plain, name), nil
}

// OpenValue reverses SealValue.
func OpenValue(keyEnc *memguard.Enclave, name, sealed []byte) ([]byte, error) {
	key, err := keyEnc.Open()
	if err != nil {
		return nil, err
	}
	defer key.Destroy()
	aead, err := chacha20poly1305.NewX(key.Bytes())
	if err != nil {
		return nil, err
	}
	if len(sealed) < aead.NonceSize()+aead.Overhead() {
		return nil, ErrSealedOpen
	}
	out, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], name)
	if err != nil {
		return nil, ErrSealedOpen
	}
	return out, nil
}

func (am *AccountManager) seal(name, plain []byte) ([]byte, error) {
	k, err := am.StorageKey()
	if err != nil {
		return nil, err
	}
	return SealValue(k, name, plain)
}

// open reads a sealed value; values written before sealing existed have exactly legacyLen bytes.
func (am *AccountManager) open(name, data []byte, legacyLen int) ([]byte, error) {
	if len(data) == legacyLen {
		return append([]byte(nil), data...), nil
	}
	k, err := am.StorageKey()
	if err != nil {
		return nil, err
	}
	return OpenValue(k, name, data)
}

// timedSecret is created-at (8 bytes, unix seconds) followed by a 32-byte private key.
func timedSecret(created time.Time, priv []byte) []byte {
	out := make([]byte, 40)
	binary.BigEndian.PutUint64(out[:8], uint64(created.Unix()))
	copy(out[8:], priv)
	return out
}

func (am *AccountManager) storeOPK(pub [32]byte, priv []byte) error {
	name := append([]byte(opkPrefix), pub[:]...)
	plain := timedSecret(time.Now(), priv)
	defer memguard.WipeBytes(plain)
	sealed, err := am.seal(name, plain)
	if err != nil {
		return err
	}
	return am.storage.Set(name, sealed)
}

// OpenOPK returns the private half of one of our one-time prekeys without using it up.
func (am *AccountManager) OpenOPK(pub [32]byte) (*memguard.Enclave, error) {
	name := append([]byte(opkPrefix), pub[:]...)
	data, err := am.storage.Get(name)
	if err != nil {
		return nil, ErrOPKNotFound
	}
	plain, err := am.open(name, data, 32)
	memguard.WipeBytes(data)
	if err != nil {
		return nil, err
	}
	defer memguard.WipeBytes(plain)
	if len(plain) == 40 {
		return memguard.NewEnclave(append([]byte(nil), plain[8:]...)), nil
	}
	if len(plain) == 32 {
		return memguard.NewEnclave(append([]byte(nil), plain...)), nil
	}
	return nil, ErrOPKNotFound
}

// DeleteOPK uses up a one-time prekey: a second session with it is impossible.
func (am *AccountManager) DeleteOPK(pub [32]byte) error {
	return am.storage.Delete(append([]byte(opkPrefix), pub[:]...))
}

// purgeOld drops one-time prekeys and replaced signed prekeys that no valid bundle names any more.
// It runs at most once an hour.
func (am *AccountManager) purgeOld() {
	am.mu.Lock()
	if time.Since(am.lastPurge) < time.Hour {
		am.mu.Unlock()
		return
	}
	am.lastPurge = time.Now()
	am.mu.Unlock()

	for _, p := range []struct {
		prefix string
		keep   time.Duration
		legacy int
	}{{opkPrefix, OPKRetention, 32}, {spkOldPrefix, SignedPreKeyRetention, 40}} {
		entries, err := am.storage.FindValues([]byte(p.prefix), 0)
		if err != nil {
			continue
		}
		for _, e := range entries {
			plain, err := am.open(e.Key, e.Value, p.legacy)
			if err != nil || len(plain) != 40 {
				memguard.WipeBytes(plain)
				continue // legacy one-time prekeys carry no date; they go when used
			}
			created := time.Unix(int64(binary.BigEndian.Uint64(plain[:8])), 0)
			memguard.WipeBytes(plain)
			if time.Since(created) > p.keep {
				if err := am.storage.Delete(e.Key); err != nil {
					slog.Warn("failed to delete an expired prekey", "err", err)
				}
			}
		}
	}
}
