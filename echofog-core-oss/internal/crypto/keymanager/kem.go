package keymanager

import (
	"crypto/mlkem"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"log/slog"
	"reflect"
	"time"
	"unsafe"

	"github.com/awnumar/memguard"
)

// The KEM prekey is the post-quantum half of PQXDH: an ML-KEM-768 key in every bundle, signed by
// the identity key like the signed prekey and replaced on the same schedule. It is a "last resort"
// key in Signal's terms: many sessions may start from it until it is replaced, and a replaced one
// is kept as long as a bundle naming it may still be used. Only its 64-byte seed is stored, sealed
// like the other prekeys, with the key's id; its public half is kept in the open next to it.
//
// The private key is expanded from the seed only to decapsulate (Decapsulate), once per init, and
// wiped right after: Go's mlkem key cannot be wiped through its API, and every expansion used to
// leave a copy of the seed and the secret vector on the heap (one per bundle handed out).

const (
	// KEMPreKeyTTL is how long one KEM prekey is offered before a new one replaces it.
	KEMPreKeyTTL = SignedPreKeyTTL
	// KEMPreKeyRetention keeps a replaced KEM prekey until every bundle that names it expired.
	KEMPreKeyRetention = SignedPreKeyRetention

	kemCurrentKey = "kem:current"
	// kemPubKey holds the public half of the current KEM prekey (created-at, 8 bytes, then the
	// encapsulation key): not secret, and bundles need it without the seed.
	kemPubKey    = "kem:pub"
	kemOldPrefix = "kem:old:"
	// kemSinceKey holds when this account first offered a KEM prekey (unix seconds, not secret):
	// every bundle it signed since then carries one. It never lies in the future (see noteSinceLocked).
	kemSinceKey = "kem:since"

	// a sealed KEM prekey: created-at (8 bytes) and the seed; the current one also has its id after
	// them (kemSealedLen), a replaced one is named by its id (kem:old:<id>)
	kemSeedLen   = 8 + mlkem.SeedSize
	kemSealedLen = kemSeedLen + 32
	kemPubLen    = 8 + mlkem.EncapsulationKeySize768
)

var (
	ErrKEMKeyNotFound = errors.New("no KEM prekey of ours has this id")
	ErrKEMDecapsulate = errors.New("the KEM ciphertext cannot be decapsulated")
)

// KEMKeyID names a KEM prekey in an init packet: the SHA-256 of its encapsulation key.
func KEMKeyID(encapsulationKey []byte) [32]byte { return sha256.Sum256(encapsulationKey) }

// timedSeed is created-at (8 bytes, unix seconds) followed by a KEM seed.
func timedSeed(created time.Time, seed []byte) []byte {
	out := make([]byte, 8+len(seed))
	binary.BigEndian.PutUint64(out[:8], uint64(created.Unix()))
	copy(out[8:], seed)
	return out
}

func createdAt(b []byte) time.Time { return time.Unix(int64(binary.BigEndian.Uint64(b[:8])), 0) }

// encapsulationKey returns the public half of the KEM key with this seed. It expands the private
// key, so it is used only for a key stored before its public half was kept.
func encapsulationKey(seed []byte) ([]byte, error) {
	dk, err := mlkem.NewDecapsulationKey768(seed)
	if err != nil {
		return nil, err
	}
	defer wipeDecapsulationKey(dk)
	return dk.EncapsulationKey().Bytes(), nil
}

// decapsulate opens a ciphertext with the KEM key of this seed; the expanded key is wiped before it
// returns.
func decapsulate(seed, ct []byte) ([]byte, error) {
	dk, err := mlkem.NewDecapsulationKey768(seed)
	if err != nil {
		return nil, err
	}
	defer wipeDecapsulationKey(dk)
	return dk.Decapsulate(ct)
}

// wipeDecapsulationKey zeroes an expanded ML-KEM key in place: the seed (d, z) and the secret
// vector it holds. The standard library offers no way to; the key is a pointer to a struct of
// fixed-size arrays, which is checked before it is zeroed (a struct that held pointers is left
// alone, as zeroing those behind the garbage collector's back is not safe).
func wipeDecapsulationKey(dk *mlkem.DecapsulationKey768) {
	if dk == nil {
		return
	}
	v := reflect.ValueOf(dk).Elem()
	if v.Kind() != reflect.Struct || v.NumField() != 1 {
		return
	}
	inner := v.Field(0)
	if inner.Kind() != reflect.Pointer || inner.IsNil() || !pointerFree(inner.Type().Elem()) {
		return
	}
	memguard.WipeBytes(unsafe.Slice((*byte)(inner.UnsafePointer()), inner.Type().Elem().Size()))
}

// pointerFree reports whether a value of type t holds only plain numbers.
func pointerFree(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return true
	case reflect.Array:
		return pointerFree(t.Elem())
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if !pointerFree(t.Field(i).Type) {
				return false
			}
		}
		return true
	}
	return false
}

// KEMPreKey returns the encapsulation key of the current KEM prekey, making a new one when the
// current one is KEMPreKeyTTL old. The replaced one is kept (see KEMPreKeyRetention). It reads the
// public half kept in the open: the seed is not touched.
func (am *AccountManager) KEMPreKey() ([]byte, error) {
	am.purgeOld()
	am.kemMu.Lock()
	defer am.kemMu.Unlock()
	now := time.Now()
	if err := am.noteSinceLocked(now); err != nil {
		return nil, err
	}
	if b, err := am.storage.Get([]byte(kemPubKey)); err == nil && len(b) == kemPubLen && now.Sub(createdAt(b)) < KEMPreKeyTTL {
		return b[8:], nil
	}

	// the current key is due, or its public half is not kept (stored by an older build, or lost
	// to a crash right after a new key was made)
	name := []byte(kemCurrentKey)
	if data, err := am.storage.Get(name); err == nil {
		plain, err := am.open(name, data, -1)
		if err == nil && (len(plain) == kemSealedLen || len(plain) == kemSeedLen) {
			pub, id, ok := am.currentKEMLocked(plain)
			created := createdAt(plain)
			switch {
			case ok && now.Sub(created) < KEMPreKeyTTL:
				memguard.WipeBytes(plain)
				return pub, nil
			case ok:
				// retire it: still needed for sessions started from bundles that name it
				oldName := append([]byte(kemOldPrefix), id[:]...)
				if sealed, err := am.seal(oldName, timedSeed(now, plain[8:kemSeedLen])); err == nil {
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
	pub := dk.EncapsulationKey().Bytes()
	wipeDecapsulationKey(dk)
	defer memguard.WipeBytes(seed)
	if err := am.storeCurrentKEMLocked(now, seed, pub); err != nil {
		return nil, err
	}
	return pub, nil
}

// currentKEMLocked returns the public half and the id of the current KEM prekey, whose sealed record
// is plain. A record of an older build carries no id: the key is expanded once to find it, and the
// record is stored again with it. The public half is kept in the open from then on. am.kemMu is
// held.
func (am *AccountManager) currentKEMLocked(plain []byte) (pub []byte, id [32]byte, ok bool) {
	created, seed := createdAt(plain), plain[8:kemSeedLen]
	if len(plain) == kemSealedLen {
		copy(id[:], plain[kemSeedLen:])
	}
	if b, err := am.storage.Get([]byte(kemPubKey)); err == nil && len(b) == kemPubLen && KEMKeyID(b[8:]) == id {
		return b[8:], id, true
	}
	pub, err := encapsulationKey(seed)
	if err != nil {
		return nil, id, false
	}
	if len(plain) == kemSealedLen && KEMKeyID(pub) != id {
		return nil, id, false
	}
	if err := am.storeCurrentKEMLocked(created, seed, pub); err != nil {
		slog.Warn("failed to keep the public half of the KEM prekey", "err", err)
	}
	return pub, KEMKeyID(pub), true
}

// storeCurrentKEMLocked stores a KEM prekey as the current one: the sealed seed with its id, then its
// public half. A crash between the two leaves a public half that does not match the seed's id,
// which currentKEMLocked repairs. am.kemMu is held.
func (am *AccountManager) storeCurrentKEMLocked(created time.Time, seed, pub []byte) error {
	id := KEMKeyID(pub)
	plain := append(timedSeed(created, seed), id[:]...)
	defer memguard.WipeBytes(plain)
	name := []byte(kemCurrentKey)
	sealed, err := am.seal(name, plain)
	if err != nil {
		return err
	}
	if err := am.storage.Set(name, sealed); err != nil {
		return err
	}
	pubRec := make([]byte, 8, kemPubLen)
	binary.BigEndian.PutUint64(pubRec, uint64(created.Unix()))
	return am.storage.Set([]byte(kemPubKey), append(pubRec, pub...))
}

// noteSinceLocked sets kem:since to now when it is not set yet (a KEM prekey is about to be offered:
// the date goes first, so a bundle with the key is never older than it) or lies after now. A clock
// that ran ahead when the first KEM prekey was made (say, in 2030) would otherwise leave every later
// bundle "older" than our first KEM prekey for good; corrected, it only moves back. am.kemMu is held.
func (am *AccountManager) noteSinceLocked(now time.Time) error {
	if since := am.KEMSince(); since != 0 && since <= now.Unix() {
		return nil
	}
	return am.storage.Set([]byte(kemSinceKey), binary.BigEndian.AppendUint64(nil, uint64(now.Unix())))
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
// still kept. It is not used up: the KEM prekey is a last-resort key. The key is found by its id,
// without expanding it.
func (am *AccountManager) OpenKEMPreKey(id [32]byte) (*memguard.Enclave, error) {
	am.kemMu.Lock()
	defer am.kemMu.Unlock()
	name := []byte(kemCurrentKey)
	if data, err := am.storage.Get(name); err == nil {
		plain, err := am.open(name, data, -1)
		switch {
		case err != nil:
		case len(plain) == kemSealedLen && subtle.ConstantTimeCompare(plain[kemSeedLen:], id[:]) == 1:
			defer memguard.WipeBytes(plain)
			return memguard.NewEnclave(append([]byte(nil), plain[8:kemSeedLen]...)), nil
		case len(plain) == kemSeedLen: // stored by an older build, without its id
			if pub, _, ok := am.currentKEMLocked(plain); ok && KEMKeyID(pub) == id {
				defer memguard.WipeBytes(plain)
				return memguard.NewEnclave(append([]byte(nil), plain[8:]...)), nil
			}
		}
		memguard.WipeBytes(plain)
	}
	// a replaced one: its name carries the id, and the seal is bound to the name
	name = append([]byte(kemOldPrefix), id[:]...)
	data, err := am.storage.Get(name)
	if err != nil {
		return nil, ErrKEMKeyNotFound
	}
	plain, err := am.open(name, data, -1)
	if err != nil || len(plain) != kemSeedLen {
		memguard.WipeBytes(plain)
		return nil, ErrKEMKeyNotFound
	}
	if time.Since(createdAt(plain)) > KEMPreKeyRetention { // past its retention: gone
		memguard.WipeBytes(plain)
		return nil, ErrKEMKeyNotFound
	}
	return memguard.NewEnclave(plain[8:]), nil // wipes the seed
}

// Decapsulate opens a KEM ciphertext with our KEM prekey of this id and returns the shared secret.
// This is the only place the private key is expanded, and it is wiped before Decapsulate returns.
// A ciphertext made for another key gives another secret (ML-KEM's implicit rejection).
func (am *AccountManager) Decapsulate(id [32]byte, ct []byte) ([]byte, error) {
	seedEnc, err := am.OpenKEMPreKey(id)
	if err != nil {
		return nil, err
	}
	seed, err := seedEnc.Open()
	if err != nil {
		return nil, ErrEnclaveOpenFailed
	}
	defer seed.Destroy()
	ss, err := decapsulate(seed.Bytes(), ct)
	if err != nil {
		return nil, ErrKEMDecapsulate
	}
	return ss, nil
}

// purgeOldKEM drops replaced KEM prekeys that no valid bundle names any more.
func (am *AccountManager) purgeOldKEM() {
	entries, err := am.storage.FindValues([]byte(kemOldPrefix), 0)
	if err != nil {
		return
	}
	for _, e := range entries {
		plain, err := am.open(e.Key, e.Value, -1)
		if err != nil || len(plain) != kemSeedLen {
			memguard.WipeBytes(plain)
			continue
		}
		created := createdAt(plain)
		memguard.WipeBytes(plain)
		if time.Since(created) > KEMPreKeyRetention {
			if err := am.storage.Delete(e.Key); err != nil {
				slog.Warn("failed to delete an expired KEM prekey", "err", err)
			}
		}
	}
}
