// Package bootstrap: the signed list of entry nodes (T5.6).
package bootstrap

import (
	"errors"
	"time"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// Signed list limits and errors.
const ListMaxBytes = 64 << 10

var (
	// ErrListSignature reports a list whose signature does not verify or whose signer is not a
	// release key.
	ErrListSignature = errors.New("bootstrap: list signature")
	// ErrListExpired reports a list past its not_after.
	ErrListExpired = errors.New("bootstrap: list expired")
	// ErrListStale reports a list whose seq is not newer than the applied one.
	ErrListStale = errors.New("bootstrap: list seq is not newer")
)

// SignedList is a verified signed list: the official entry nodes of a release.
type SignedList struct {
	Seq      uint64
	NotAfter uint64
	Signer   keys.AKey
	Entries  []Entry
	Raw      []byte
}

// SealList signs text with a release key. seq and not_after are the list's own sequence and term.
func SealList(s *keys.Signer, seq, notAfter uint64, text string) ([]byte, error) {
	return nil, errors.New("not implemented")
}

// OpenList verifies raw against the release key set and parses its entries.
func OpenList(raw []byte, release []keys.AKey, now time.Time) (*SignedList, error) {
	return nil, errors.New("not implemented")
}

// ManagerConfig configures a Manager.
type ManagerConfig struct {
	// Builtin is the signed list embedded at build time. Empty in the OSS source build.
	Builtin []byte
	// Keys is the release public key set. Empty: no signed list is trusted.
	Keys []keys.AKey
	// Custom is the user's own entries (node.json "bootstrap").
	Custom []string
	// KV persists the applied list and the disabled built-in entries. Nil: memory only.
	KV store.KV
	// Now is the clock.
	Now func() time.Time
	// SaveCustom persists the user's entries (node.json). Called under the manager lock.
	SaveCustom func([]string) error
}

// Manager holds the official list and the user's entries.
type Manager struct{}

// OpenManager loads the built-in list, the applied update and the user's entries.
func OpenManager(cfg ManagerConfig) *Manager { return &Manager{} }

// Builtin returns the official entries with their enabled state and 1-based numbers.
func (m *Manager) Builtin() []Origin { return nil }

// Custom returns the user's entries.
func (m *Manager) Custom() []Origin { return nil }

// Entries returns what is dialed: enabled official entries followed by the user's.
func (m *Manager) Entries() []Entry { return nil }

// AddCustom parses and saves one user entry.
func (m *Manager) AddCustom(line string) (Entry, error) { return Entry{}, errors.New("not implemented") }

// RemoveCustom forgets one user entry.
func (m *Manager) RemoveCustom(line string) error { return errors.New("not implemented") }

// SetBuiltinEnabled switches one official entry on or off and persists the choice.
func (m *Manager) SetBuiltinEnabled(index int, on bool) error { return errors.New("not implemented") }

// Offer applies a signed list from a release update. The signature, the term and the seq are
// checked; a rejected list changes nothing.
func (m *Manager) Offer(raw []byte) error { return errors.New("not implemented") }

// OfferFile applies a signed list from path, deleting it on success.
func (m *Manager) OfferFile(path string) (bool, error) { return false, nil }

// Seq returns the sequence of the effective official list, NotAfter its term.
func (m *Manager) Seq() uint64     { return 0 }
func (m *Manager) NotAfter() uint64 { return 0 }
func (m *Manager) Err() error      { return nil }

// Origin is one list entry with where it came from.
type Origin struct {
	Entry Entry
	// Builtin is true for an entry of the signed list; Index is then its 1-based number.
	Builtin bool
	Index   int
	Enabled bool
}
