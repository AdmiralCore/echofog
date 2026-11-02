package keymanager

import (
	"crypto/mlkem"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"log/slog"
	"time"

	"github.com/awnumar/memguard"
)

// The KEM prekey is the post-quantum half of PQXDH: an ML-KEM-768 key in every bundle, signed by
// the identity key like the signed prekey and replaced on the same schedule. It is a "last resort"
// key in Signal's terms: many sessions may start from it until it is replaced, and a replaced one
// is kept as long as a bundle naming it may still be used. Only its 64-byte seed is stored, sealed
// like the other prekeys.

const (
	// KEMPreKeyTTL is how long one KEM prekey is offered before a new one replaces it.
	KEMPreKeyTTL = SignedPreKeyTTL
	// KEMPreKeyRetention keeps a replaced KEM prekey until every bundle that names it expired.
	KEMPreKeyRetention = SignedPreKeyRetention

	kemCurrentKey = "kem:current"
	kemOldPrefix  = "kem:old:"
	// kemSinceKey holds when this account first offered a KEM prekey (unix seconds, not secret):
	// every bundle it signed since then carries one.
	kemSinceKey = "kem:since"

	kemSealedLen = 8 + mlkem.SeedSize
)

var ErrKEMKeyNotFound = errors.New("no KEM prekey of ours has this id")

// KEMKeyID names a KEM prekey in an init packet: the SHA-256 of its encapsulation key.
func KEMKeyID(encapsulationKey []byte) [32]byte { return sha256.Sum256(encapsulationKey) }

// timedSeed is created-at (8 bytes, unix seconds) followed by a KEM seed.
func timedSeed(created time.Time, seed []byte) []byte {
	out := make([]byte, 8+len(seed))
	binary.BigEndian.PutUint64(out[:8], uint64(created.Unix()))
	copy(out[8:], seed)
	return out
}

// encapsulationKey returns the public half of the KEM key with this seed.
func encapsulationKey(seed []byte) ([]byte, error) {
	dk, err := mlkem.NewDecapsulationKey768(seed)
	if err != nil {
		return nil, err
	}
	return dk.EncapsulationKey().Bytes(), nil
}

// KEMPreKey returns the encapsulation key of the current KEM prekey, making a new one when the
// current one is KEMPreKeyTTL old. The replaced one is kept (see KEMPreKeyRetention).
func (am *AccountManager) KEMPreKey() ([]byte, error) {
	am.purgeOld()
	am.kemMu.Lock()
	defer am.kemMu.Unlock()

	name := []byte(kemCurrentKey)
	if data, err := am.storage.Get(name); err == nil {
		plain, err := am.open(name, data, -1)
		if err == nil && len(plain) == kemSealedLen {
			created := time.Unix(int64(binary.BigEndian.Uint64(plain[:8])), 0)
			pub, perr := encapsulationKey(plain[8:])
			if perr == nil && time.Since(created) < KEMPreKeyTTL {
				memguard.WipeBytes(plain)
				return pub, nil
			}
			if perr == nil {
				// retire it: still needed for sessions started from bundles that name it
				id := KEMKeyID(pub)
				oldName := append([]byte(kemOldPrefix), id[:]...)
				if sealed, err := am.seal(oldName, timedSeed(time.Now(), plain[8:])); err == nil {
					_ = am.storage.Set(oldName, sealed)
				}
			}
		}
		memguard.WipeBytes(plain)
	}

	dk, err := mlkem.GenerateKey768()
	if err != nil {
		return nil, err
	}
	seed := dk.Bytes()
	defer memguard.WipeBytes(seed)
	plain := timedSeed(time.Now(), seed)
	defer memguard.WipeBytes(plain)
	sealed, err := am.seal(name, plain)
	if err != nil {
		return nil, err
	}
	// the date first: a bundle carrying the new key must never be older than it
	if !am.storage.Exists([]byte(kemSinceKey)) {
		since := make([]byte, 8)
		binary.BigEndian.PutUint64(since, uint64(time.Now().Unix()))
		if err := am.storage.Set([]byte(kemSinceKey), since); err != nil {
			return nil, err
		}
	}
	if err := am.storage.Set(name, sealed); err != nil {
		return nil, err
	}
	return dk.EncapsulationKey().Bytes(), nil
}

// KEMSince is when this account first offered a KEM prekey (unix seconds), 0 if it never did. A
// bundle it signed at or after that time carries one.
func (am *AccountManager) KEMSince() int64 {
	b, err := am.storage.Get([]byte(kemSinceKey))
	if err != nil || len(b) != 8 {
		return 0
	}
	return int64(binary.BigEndian.Uint64(b))
}

// OpenKEMPreKey returns the seed of our KEM prekey with this id: the current one, or a replaced one
// still kept. It is not used up: the KEM prekey is a last-resort key.
func (am *AccountManager) OpenKEMPreKey(id [32]byte) (*memguard.Enclave, error) {
	am.kemMu.Lock()
	defer am.kemMu.Unlock()
	for i, name := range [][]byte{[]byte(kemCurrentKey), append([]byte(kemOldPrefix), id[:]...)} {
		data, err := am.storage.Get(name)
		if err != nil {
			continue
		}
		plain, err := am.open(name, data, -1)
		if err != nil || len(plain) != kemSealedLen {
			memguard.WipeBytes(plain)
			continue
		}
		pub, err := encapsulationKey(plain[8:])
		if err != nil || KEMKeyID(pub) != id {
			memguard.WipeBytes(plain)
			continue
		}
		if i > 0 { // a replaced one, past its retention: gone
			created := time.Unix(int64(binary.BigEndian.Uint64(plain[:8])), 0)
			if time.Since(created) > KEMPreKeyRetention {
				memguard.WipeBytes(plain)
				return nil, ErrKEMKeyNotFound
			}
		}
		return memguard.NewEnclave(plain[8:]), nil // wipes the seed
	}
	return nil, ErrKEMKeyNotFound
}

// purgeOldKEM drops replaced KEM prekeys that no valid bundle names any more.
func (am *AccountManager) purgeOldKEM() {
	entries, err := am.storage.FindValues([]byte(kemOldPrefix), 0)
	if err != nil {
		return
	}
	for _, e := range entries {
		plain, err := am.open(e.Key, e.Value, -1)
		if err != nil || len(plain) != kemSealedLen {
			memguard.WipeBytes(plain)
			continue
		}
		created := time.Unix(int64(binary.BigEndian.Uint64(plain[:8])), 0)
		memguard.WipeBytes(plain)
		if time.Since(created) > KEMPreKeyRetention {
			if err := am.storage.Delete(e.Key); err != nil {
				slog.Warn("failed to delete an expired KEM prekey", "err", err)
			}
		}
	}
}
