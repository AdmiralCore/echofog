package store

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/echofog/echofog-core/internal/v2/secret"
	"golang.org/x/crypto/hkdf"
)

// idInfo separates the identifier key from the value key and the key-check key derived from the
// same storage key.
const idInfo = "echofog/store2/id"

// Domain is the type of an identifier. The domain is part of the blinded value, so the same raw id
// used as a peer, a chat, a group or a device yields four unrelated values: a namespace cannot be
// walked with another type's keys.
//
// A domain is a stable part of the on-disk key format. Renaming one changes every key of that type.
type Domain string

const (
	// DomainPeer blinds account (node) identifiers and other identifiers of people.
	DomainPeer Domain = "peer"
	// DomainChat blinds conversation identifiers.
	DomainChat Domain = "chat"
	// DomainGroup blinds group identifiers.
	DomainGroup Domain = "group"
	// DomainDevice blinds device identifiers.
	DomainDevice Domain = "device"
)

// ErrBadDomain is returned for an empty, over-long or NUL-containing domain.
var ErrBadDomain = errors.New("store: bad id domain")

// ID is a blinded identifier: the full 32-byte HMAC-SHA256 of its domain and raw value under the
// store's identifier key. It is deterministic and not reversible; it is not derived from the raw
// value in any way a reader of the disk could invert.
type ID [sha256.Size]byte

// Bytes returns the id as a byte slice of its own. The slice is a copy of the id.
func (id ID) Bytes() []byte {
	out := make([]byte, len(id))
	copy(out, id[:])
	return out
}

// String returns the id in lower-case hex, for logs and text protocols that cannot carry bytes.
// The blinded id is not a secret, but callers must not log the raw identifier it came from.
func (id ID) String() string {
	return hex.EncodeToString(id[:])
}

// IDs blinds identifiers with the account's storage key before a service builds a database key
// (versioning.md §3.7, security.md §4.8). A service must put only a blinded ID into a key: keys
// are visible to a local reader of the disk, while values are encrypted.
//
// NewIDs derives its own key from the storage key, so the blinder can live as long as the service
// that uses it. Close destroys the derived key.
type IDs struct {
	key *secret.Value
}

// NewIDs derives the identifier key from the account's 32-byte storage key (T4.1).
func NewIDs(key *secret.Value) (*IDs, error) {
	if key == nil {
		return nil, fmt.Errorf("%w: no key", ErrBadKey)
	}
	if key.Size() != sha256.Size {
		return nil, fmt.Errorf("%w: got %d", ErrBadKey, key.Size())
	}

	var derived *secret.Value
	err := key.Use(func(keyBytes []byte) error {
		subkey := make([]byte, sha256.Size)
		defer secret.Wipe(subkey)
		if _, err := io.ReadFull(hkdf.New(sha256.New, keyBytes, nil, []byte(idInfo)), subkey); err != nil {
			return fmt.Errorf("store: deriving the id key: %w", err)
		}
		derived = secret.New(subkey)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &IDs{key: derived}, nil
}

// Close destroys the derived key. It is idempotent; later Blind calls return secret.ErrDestroyed.
func (ids *IDs) Close() {
	if ids == nil || ids.key == nil {
		return
	}
	ids.key.Destroy()
}

// Blind returns the opaque id of raw in domain. The result is deterministic for one storage key and
// domain, and different under another key or domain. raw is not validated and is never logged or
// stored: only the result goes into a database key.
func (ids *IDs) Blind(domain Domain, raw []byte) (ID, error) {
	var id ID
	if !validDomain(domain) {
		return id, ErrBadDomain
	}
	if ids == nil || ids.key == nil {
		return id, fmt.Errorf("%w: blinded ids are closed", secret.ErrDestroyed)
	}

	err := ids.key.Use(func(keyBytes []byte) error {
		mac := hmac.New(sha256.New, keyBytes)
		// The NUL terminates the domain, which never contains one, so a domain and a raw value
		// cannot be shifted into each other.
		mac.Write([]byte(domain))
		mac.Write([]byte{0})
		mac.Write(raw)
		copy(id[:], mac.Sum(nil))
		return nil
	})
	if err != nil {
		return ID{}, fmt.Errorf("store: blinding an id: %w", err)
	}
	return id, nil
}

func validDomain(domain Domain) bool {
	if domain == "" || len(domain) > 64 {
		return false
	}
	return strings.IndexByte(string(domain), 0) < 0
}
