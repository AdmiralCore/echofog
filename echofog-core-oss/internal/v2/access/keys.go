package access

import (
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/keys"
)

// identityKeyPair rebuilds a masking key pair from the 32-byte X25519 private key kept in the store.
func identityKeyPair(priv []byte) (*identity.KeyPair, error) {
	if len(priv) != privateKeyLen {
		return nil, fmt.Errorf("%w: private key of %d bytes", ErrState, len(priv))
	}
	return identity.KeyPairFromPrivate(priv)
}

func (m *Manager) publicOf(id [8]byte) keys.AKey {
	kp := m.live[id]
	if kp == nil {
		return nil
	}
	return kp.Public()
}

// newKeyLocked makes, records and persists one masking key pair. The caller holds m.mu.
func (m *Manager) newKeyLocked(kind uint8, slot uint32, now time.Time, notAfter, retireAfter int64, version uint64) (*keyRecord, error) {
	var id [8]byte
	for {
		if _, err := rand.Read(id[:]); err != nil {
			return nil, fmt.Errorf("%w: key id: %v", ErrState, err)
		}
		if _, dup := m.keys[id]; !dup {
			break
		}
	}
	kp, err := identity.NewKeyPair()
	if err != nil {
		return nil, fmt.Errorf("%w: masking key: %v", ErrState, err)
	}
	var priv []byte
	if err := kp.WithPrivate(func(b []byte) error {
		priv = append([]byte(nil), b...)
		return nil
	}); err != nil {
		kp.Destroy()
		return nil, fmt.Errorf("%w: masking key: %v", ErrState, err)
	}
	rec := &keyRecord{
		ID: id, Kind: kind, Private: priv, Slot: slot,
		Created: unix(now), NotAfter: notAfter, RetireAfter: retireAfter, Version: version,
	}
	m.keys[id] = rec
	m.live[id] = kp
	if err := m.saveKey(rec); err != nil {
		delete(m.keys, id)
		delete(m.live, id)
		kp.Destroy()
		return nil, fmt.Errorf("%w: save masking key: %v", ErrState, err)
	}
	return rec, nil
}

func (m *Manager) removeKeyLocked(id [8]byte) error {
	kp := m.live[id]
	delete(m.live, id)
	delete(m.keys, id)
	if kp != nil {
		kp.Destroy()
	}
	if err := m.deleteKey(id); err != nil {
		return fmt.Errorf("%w: delete masking key: %v", ErrState, err)
	}
	return nil
}

// invitationActive reports whether the invitation pass may admit an entry now (handshake.md §5,
// objects.md §10.3). The access term starts on the first entry and then also bounds later entries
// of a multi-use invitation.
func invitationActive(r *invitationRecord, now time.Time) bool {
	if r == nil || r.Revoked || len(r.Secret) != secretLen || r.Entries >= r.MaxEntries {
		return false
	}
	t := unix(now)
	if t >= r.LinkExpires {
		return false
	}
	if r.AccessExpires != 0 && t >= r.AccessExpires {
		return false
	}
	return true
}

func personalActive(r *personalRecord, now time.Time) bool {
	return r != nil && !r.Revoked && len(r.Secret) == secretLen && unix(now) < r.Expires
}

// refsLocked returns the active invitations that use the key. The caller holds m.mu.
func (m *Manager) refsLocked(id [8]byte, now time.Time) []PassID {
	var out []PassID
	for _, r := range m.invitations {
		if r.KeyID == id && invitationActive(r, now) {
			out = append(out, r.PassID)
		}
	}
	return out
}

// holdersLocked returns every invitation that still has an access secret: an expired link can be
// extended by the owner, so its masking key must stay until the record is revoked or used up.
func (m *Manager) holdersLocked(id [8]byte) []*invitationRecord {
	var out []*invitationRecord
	for _, r := range m.invitations {
		if r.KeyID == id && !r.Revoked && len(r.Secret) == secretLen {
			out = append(out, r)
		}
	}
	return out
}

// keyCountsLocked counts invitation keys per listener slot.
func (m *Manager) keyCountsLocked() map[uint32]int {
	counts := make(map[uint32]int, len(m.listeners))
	for _, r := range m.keys {
		if r.Kind == keyInvitation {
			counts[r.Slot]++
		}
	}
	return counts
}

// freeSlotLocked returns a listener slot that may hold one more invitation key (wire.md §2.1: at
// most 8 live masking keys per listener).
func (m *Manager) freeSlotLocked() (uint32, bool) {
	counts := m.keyCountsLocked()
	best := -1
	for i := range m.listeners {
		slot := uint32(i)
		if counts[slot] >= KeysPerListener {
			continue
		}
		if best < 0 || counts[slot] < counts[uint32(best)] {
			best = i
		}
	}
	if best < 0 {
		return 0, false
	}
	return uint32(best), true
}

// keyForMintLocked chooses the masking key of a new invitation: a fresh key on a free listener
// slot when possible, otherwise the least-loaded existing invitation key (wire.md §2.1 allows
// sharing among a small group of unredeemed invitations, objects.md §10.3).
func (m *Manager) keyForMintLocked(now time.Time, share bool) (*keyRecord, bool, error) {
	if !share {
		if slot, ok := m.freeSlotLocked(); ok {
			rec, err := m.newKeyLocked(keyInvitation, slot, now, 0, 0, 0)
			return rec, true, err
		}
	}
	var best *keyRecord
	bestRefs := 0
	for id, r := range m.keys {
		if r.Kind != keyInvitation || int(r.Slot) >= len(m.listeners) {
			continue
		}
		n := len(m.refsLocked(id, now))
		if n == 0 {
			continue
		}
		if best == nil || n < bestRefs || (n == bestRefs && bytesCompare(id[:], best.ID[:]) < 0) {
			best, bestRefs = r, n
		}
	}
	if best != nil {
		return best, false, nil
	}
	if slot, ok := m.freeSlotLocked(); ok {
		rec, err := m.newKeyLocked(keyInvitation, slot, now, 0, 0, 0)
		return rec, true, err
	}
	return nil, false, fmt.Errorf("%w: all %d listeners hold %d keys", ErrKeyLimit, len(m.listeners), KeysPerListener)
}

// pruneInvitationKeysLocked removes keys no invitation can use any more. An expired invitation
// still holds its secret and can be extended, so its key stays until the pass is revoked or spent.
func (m *Manager) pruneInvitationKeysLocked(now time.Time) error {
	var errs []error
	for id, r := range m.keys {
		if r.Kind != keyInvitation {
			continue
		}
		if len(m.holdersLocked(id)) > 0 {
			continue
		}
		if err := m.removeKeyLocked(id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// rotateContactsLocked creates a new contacts key and keeps the old one accepted during the
// overlap (objects.md §10.3). The caller holds m.mu.
func (m *Manager) rotateContactsLocked(now time.Time) (*keyRecord, *keyRecord, error) {
	old := m.keys[m.contactsKeyID]
	next, err := m.newKeyLocked(keyContacts, contactSlot, now, 0, 0, m.contactsVersion+1)
	if err != nil {
		return nil, nil, err
	}
	m.contactsKeyID, m.contactsVersion = next.ID, next.Version
	if err := m.saveMeta(); err != nil {
		return nil, nil, fmt.Errorf("%w: save meta: %v", ErrState, err)
	}
	if old != nil {
		old.RetireAfter = unix(now.Add(m.overlap))
		if err := m.saveKey(old); err != nil {
			return next, old, fmt.Errorf("%w: save previous contacts key: %v", ErrState, err)
		}
	}
	if err := m.pruneContactKeysLocked(now); err != nil {
		return next, old, err
	}
	return next, old, nil
}

// pruneContactKeysLocked drops previous contacts keys once the overlap passed or every contact
// seen in the last 30 days has the current card (objects.md §10.3).
func (m *Manager) pruneContactKeysLocked(now time.Time) error {
	var errs []error
	seen := unix(now.Add(-contactSeenWindow))
	for id, r := range m.keys {
		if r.Kind != keyContacts || id == m.contactsKeyID {
			continue
		}
		remove := r.RetireAfter != 0 && unix(now) >= r.RetireAfter
		if !remove {
			covered := true
			for _, p := range m.personal {
				if p.Revoked || p.LastSeen < seen {
					continue
				}
				if p.CardVersion < m.contactsVersion {
					covered = false
					break
				}
			}
			remove = covered
		}
		if !remove {
			continue
		}
		if err := m.removeKeyLocked(id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// contactKeyLiveLocked reports whether pub is a contacts key the node still accepts.
func (m *Manager) contactKeyLiveLocked(pub keys.AKey) bool {
	for id, r := range m.keys {
		if r.Kind != keyContacts {
			continue
		}
		if bytesEqual(m.publicOf(id), pub) {
			return true
		}
	}
	return false
}

// invitationKeyLiveLocked reports whether pub is an active invitation key.
func (m *Manager) invitationKeyLiveLocked(pub keys.AKey, now time.Time) bool {
	for id, r := range m.keys {
		if r.Kind != keyInvitation {
			continue
		}
		if bytesEqual(m.publicOf(id), pub) {
			return len(m.refsLocked(id, now)) > 0
		}
	}
	return false
}
