package access

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/echofog/echofog-core/internal/v2/store"
)

// State keys inside the access namespace. The whole namespace is encrypted by the store (T4.3);
// the keys themselves carry only pass selectors (the 8-byte HMAC of handshake.md §5) and random
// key ids, never a contact's node id or a secret.
const (
	passPrefix    = "p/"
	contactPrefix = "c/"
	keyPrefix     = "k/"
	metaName      = "m"
)

// stateVersion is the local state format. It is not a protocol version.
const stateVersion = 1

// Bounds of a decoded record. They keep a damaged or hostile store value from asking for a huge
// allocation and bound the pass list the node will hold in memory.
const (
	maxInvitationCard = 4096 // objects.md §3, envelope limit
	maxEntriesLimit   = 10000
	maxRecords        = 1 << 16
	secretLen         = 16
	privateKeyLen     = 32

	keyInvitation uint8 = 1
	keyContacts   uint8 = 2
	// contactSlot is the sentinel slot of the contacts key: it belongs to the contacts listener
	// rather than to one of Config.Listeners.
	contactSlot uint32 = ^uint32(0)
)

// invitationRecord is one invitation pass (objects.md §10.3, handshake.md §5).
type invitationRecord struct {
	PassID PassID
	// Secret is the 16-byte access secret S; nil once the pass is revoked or used up.
	Secret []byte
	KeyID  [8]byte
	Slot   uint32
	// Created, LinkExpires, AccessExpires and CardNotAfter are unix seconds. AccessExpires is set
	// on the first entry; AccessSecs is the term granted then. CardNotAfter bounds later
	// extensions.
	Created       int64
	LinkExpires   int64
	AccessExpires int64
	AccessSecs    int64
	CardNotAfter  int64
	MaxEntries    uint32
	Entries       uint32
	Revoked       bool
	// Node and Device identify the contact after redemption. Zero before.
	Node   [32]byte
	Device [32]byte
	// Card is the invitation envelope as delivered in the link.
	Card []byte
}

// personalRecord is one contact's personal pass (handshake.md §5).
type personalRecord struct {
	PassID      PassID
	Secret      []byte
	Created     int64
	Expires     int64
	LastSeen    int64
	CardVersion uint64
	Revoked     bool
	Node        [32]byte
	Device      [32]byte
}

// keyRecord is one masking key pair: an invitation's own key or a version of the contacts key.
type keyRecord struct {
	ID          [8]byte
	Kind        uint8
	Private     []byte
	Slot        uint32
	Created     int64
	NotAfter    int64 // 0: no time limit (contacts key lives until rotated)
	RetireAfter int64
	Version     uint64 // contacts key version; 0 for invitation keys
}

// metaRecord is the store-level pointer to the current contacts key.
type metaRecord struct {
	CurrentKey      [8]byte
	ContactsVersion uint64
}

func passKey(id PassID) []byte { return append([]byte(passPrefix), id[:]...) }
func contactKey(id PassID) []byte {
	return append([]byte(contactPrefix), id[:]...)
}
func keyKey(id [8]byte) []byte { return append([]byte(keyPrefix), id[:]...) }

// --- encoding ---------------------------------------------------------------------------------

func putBytes(out []byte, b []byte) []byte {
	out = binary.BigEndian.AppendUint32(out, uint32(len(b)))
	return append(out, b...)
}

func encodeInvitation(r *invitationRecord) []byte {
	out := make([]byte, 0, 1+8+1+secretLen+8+4+6*8+2*4+1+64+4+len(r.Card))
	out = append(out, stateVersion)
	out = append(out, r.PassID[:]...)
	out = append(out, byte(len(r.Secret)))
	out = append(out, r.Secret...)
	out = append(out, r.KeyID[:]...)
	out = binary.BigEndian.AppendUint32(out, r.Slot)
	out = binary.BigEndian.AppendUint64(out, uint64(r.Created))
	out = binary.BigEndian.AppendUint64(out, uint64(r.LinkExpires))
	out = binary.BigEndian.AppendUint64(out, uint64(r.AccessExpires))
	out = binary.BigEndian.AppendUint64(out, uint64(r.AccessSecs))
	out = binary.BigEndian.AppendUint64(out, uint64(r.CardNotAfter))
	out = binary.BigEndian.AppendUint32(out, r.MaxEntries)
	out = binary.BigEndian.AppendUint32(out, r.Entries)
	var flags byte
	if r.Revoked {
		flags = 1
	}
	out = append(out, flags)
	out = append(out, r.Node[:]...)
	out = append(out, r.Device[:]...)
	return putBytes(out, r.Card)
}

func decodeInvitation(b []byte) (*invitationRecord, error) {
	d := decoder{b: b}
	if d.u8() != stateVersion {
		return nil, errState("version")
	}
	r := &invitationRecord{}
	copy(r.PassID[:], d.n(8))
	n := int(d.u8())
	if n != 0 && n != secretLen {
		return nil, errState("secret length")
	}
	r.Secret = d.n(n)
	copy(r.KeyID[:], d.n(8))
	r.Slot = d.u32()
	r.Created = int64(d.u64())
	r.LinkExpires = int64(d.u64())
	r.AccessExpires = int64(d.u64())
	r.AccessSecs = int64(d.u64())
	r.CardNotAfter = int64(d.u64())
	r.MaxEntries = d.u32()
	r.Entries = d.u32()
	r.Revoked = d.u8()&1 != 0
	copy(r.Node[:], d.n(32))
	copy(r.Device[:], d.n(32))
	card, ok := d.bytes(maxInvitationCard)
	if !ok {
		return nil, errState("card")
	}
	r.Card = card
	if err := d.done(); err != nil {
		return nil, err
	}
	if r.MaxEntries == 0 || r.MaxEntries > maxEntriesLimit || r.Entries > r.MaxEntries {
		return nil, errState("entries")
	}
	return r, nil
}

func encodePersonal(r *personalRecord) []byte {
	out := make([]byte, 0, 1+8+1+secretLen+5*8+1+64)
	out = append(out, stateVersion)
	out = append(out, r.PassID[:]...)
	out = append(out, byte(len(r.Secret)))
	out = append(out, r.Secret...)
	out = binary.BigEndian.AppendUint64(out, uint64(r.Created))
	out = binary.BigEndian.AppendUint64(out, uint64(r.Expires))
	out = binary.BigEndian.AppendUint64(out, uint64(r.LastSeen))
	out = binary.BigEndian.AppendUint64(out, r.CardVersion)
	var flags byte
	if r.Revoked {
		flags = 1
	}
	out = append(out, flags)
	out = append(out, r.Node[:]...)
	return append(out, r.Device[:]...)
}

func decodePersonal(b []byte) (*personalRecord, error) {
	d := decoder{b: b}
	if d.u8() != stateVersion {
		return nil, errState("version")
	}
	r := &personalRecord{}
	copy(r.PassID[:], d.n(8))
	n := int(d.u8())
	if n != 0 && n != secretLen {
		return nil, errState("secret length")
	}
	r.Secret = d.n(n)
	r.Created = int64(d.u64())
	r.Expires = int64(d.u64())
	r.LastSeen = int64(d.u64())
	r.CardVersion = d.u64()
	r.Revoked = d.u8()&1 != 0
	copy(r.Node[:], d.n(32))
	copy(r.Device[:], d.n(32))
	if err := d.done(); err != nil {
		return nil, err
	}
	return r, nil
}

func encodeKey(r *keyRecord) []byte {
	out := make([]byte, 0, 1+8+1+privateKeyLen+4+9*8)
	out = append(out, stateVersion)
	out = append(out, r.ID[:]...)
	out = append(out, r.Kind)
	out = binary.BigEndian.AppendUint32(out, uint32(len(r.Private)))
	out = append(out, r.Private...)
	out = binary.BigEndian.AppendUint32(out, r.Slot)
	out = binary.BigEndian.AppendUint64(out, uint64(r.Created))
	out = binary.BigEndian.AppendUint64(out, uint64(r.NotAfter))
	out = binary.BigEndian.AppendUint64(out, uint64(r.RetireAfter))
	return binary.BigEndian.AppendUint64(out, r.Version)
}

func decodeKey(b []byte) (*keyRecord, error) {
	d := decoder{b: b}
	if d.u8() != stateVersion {
		return nil, errState("version")
	}
	r := &keyRecord{}
	copy(r.ID[:], d.n(8))
	r.Kind = d.u8()
	n := int(d.u32())
	if n != privateKeyLen {
		return nil, errState("private key length")
	}
	r.Private = d.n(n)
	r.Slot = d.u32()
	r.Created = int64(d.u64())
	r.NotAfter = int64(d.u64())
	r.RetireAfter = int64(d.u64())
	r.Version = d.u64()
	if err := d.done(); err != nil {
		return nil, err
	}
	if r.Kind != keyInvitation && r.Kind != keyContacts {
		return nil, errState("key kind")
	}
	if r.Kind == keyContacts && r.Slot != contactSlot {
		return nil, errState("contacts key slot")
	}
	if r.Kind == keyInvitation && r.Slot == contactSlot {
		return nil, errState("invitation key slot")
	}
	return r, nil
}

func encodeMeta(m *metaRecord) []byte {
	out := make([]byte, 0, 1+8+8)
	out = append(out, stateVersion)
	out = append(out, m.CurrentKey[:]...)
	return binary.BigEndian.AppendUint64(out, m.ContactsVersion)
}

func decodeMeta(b []byte) (*metaRecord, error) {
	d := decoder{b: b}
	if d.u8() != stateVersion {
		return nil, errState("version")
	}
	m := &metaRecord{}
	copy(m.CurrentKey[:], d.n(8))
	m.ContactsVersion = d.u64()
	if err := d.done(); err != nil {
		return nil, err
	}
	return m, nil
}

var errBadState = errors.New("access: bad state record")

func errState(what string) error { return fmt.Errorf("%w: %s", errBadState, what) }

// decoder is a strict reader: any short read marks the record bad, and done rejects trailing bytes.
type decoder struct {
	b   []byte
	off int
	bad bool
}

func (d *decoder) n(n int) []byte {
	if d.bad || n < 0 || d.off+n > len(d.b) {
		d.bad = true
		return nil
	}
	out := d.b[d.off : d.off+n]
	d.off += n
	return out
}

func (d *decoder) u8() byte {
	b := d.n(1)
	if b == nil {
		return 0
	}
	return b[0]
}

func (d *decoder) u32() uint32 {
	b := d.n(4)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint32(b)
}

func (d *decoder) u64() uint64 {
	b := d.n(8)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint64(b)
}

func (d *decoder) bytes(limit int) ([]byte, bool) {
	n := int(d.u32())
	if d.bad || n < 0 || n > limit {
		d.bad = true
		return nil, false
	}
	b := d.n(n)
	if d.bad {
		return nil, false
	}
	return append([]byte(nil), b...), true
}

func (d *decoder) done() error {
	if d.bad || d.off != len(d.b) {
		return errState("length")
	}
	return nil
}

// --- loading and persistence ------------------------------------------------------------------

// scanAll reads every record under a prefix, paging through the store's bounded scans.
func scanAll(kv store.KV, prefix []byte) ([]store.Entry, error) {
	out := make([]store.Entry, 0, 16)
	var start []byte
	for {
		page, err := kv.ScanFrom(prefix, start, store.MaxScanLimit)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if len(out) > maxRecords {
			return nil, fmt.Errorf("%w: more than %d records", ErrState, maxRecords)
		}
		if len(page) < store.MaxScanLimit {
			return out, nil
		}
		start = page[len(page)-1].Key
	}
}

// loadState restores passes, keys and the contacts pointer. A record this version cannot parse is
// skipped: the pass stops admitting, which is the safe direction; the value stays on disk for an
// operator to inspect. A store error (including a damaged value) is returned.
func (m *Manager) loadState() error {
	entries, err := scanAll(m.kv, []byte(keyPrefix))
	if err != nil {
		return fmt.Errorf("%w: keys: %v", ErrState, err)
	}
	for _, e := range entries {
		if len(e.Key) != len(keyPrefix)+8 {
			continue
		}
		rec, err := decodeKey(e.Value)
		if err != nil || !bytesEqual(e.Key[len(keyPrefix):], rec.ID[:]) {
			continue
		}
		if _, dup := m.keys[rec.ID]; dup {
			continue
		}
		kp, err := identityKeyPair(rec.Private)
		if err != nil {
			continue
		}
		m.keys[rec.ID] = rec
		m.live[rec.ID] = kp
	}

	entries, err = scanAll(m.kv, []byte(passPrefix))
	if err != nil {
		return fmt.Errorf("%w: invitations: %v", ErrState, err)
	}
	for _, e := range entries {
		if len(e.Key) != len(passPrefix)+8 {
			continue
		}
		rec, err := decodeInvitation(e.Value)
		if err != nil || !bytesEqual(e.Key[len(passPrefix):], rec.PassID[:]) {
			continue
		}
		if _, dup := m.invitations[rec.PassID]; dup {
			continue
		}
		if _, ok := m.keys[rec.KeyID]; !ok {
			// The invitation's masking key is gone: it cannot be reached any more. Fail closed
			// and keep the record visible as revoked.
			rec.Revoked = true
			rec.Secret = nil
			rec.Entries = rec.MaxEntries
		}
		m.invitations[rec.PassID] = rec
	}

	entries, err = scanAll(m.kv, []byte(contactPrefix))
	if err != nil {
		return fmt.Errorf("%w: personal passes: %v", ErrState, err)
	}
	for _, e := range entries {
		if len(e.Key) != len(contactPrefix)+8 {
			continue
		}
		rec, err := decodePersonal(e.Value)
		if err != nil || !bytesEqual(e.Key[len(contactPrefix):], rec.PassID[:]) {
			continue
		}
		if _, dup := m.personal[rec.PassID]; dup {
			continue
		}
		m.personal[rec.PassID] = rec
	}

	v, err := m.kv.Get([]byte(metaName))
	switch {
	case err == nil:
		meta, err := decodeMeta(v)
		if err == nil {
			m.contactsKeyID = meta.CurrentKey
			m.contactsVersion = meta.ContactsVersion
		}
	case errors.Is(err, store.ErrNotFound):
	default:
		return fmt.Errorf("%w: meta: %v", ErrState, err)
	}
	return nil
}

func (m *Manager) saveInvitation(r *invitationRecord) error {
	return m.kv.Put(passKey(r.PassID), encodeInvitation(r))
}

func (m *Manager) deleteInvitation(id PassID) error {
	return m.kv.Delete(passKey(id))
}

func (m *Manager) savePersonal(r *personalRecord) error {
	return m.kv.Put(contactKey(r.PassID), encodePersonal(r))
}

func (m *Manager) saveKey(r *keyRecord) error {
	return m.kv.Put(keyKey(r.ID), encodeKey(r))
}

func (m *Manager) deleteKey(id [8]byte) error {
	return m.kv.Delete(keyKey(id))
}

func (m *Manager) saveMeta() error {
	return m.kv.Put([]byte(metaName), encodeMeta(&metaRecord{
		CurrentKey:      m.contactsKeyID,
		ContactsVersion: m.contactsVersion,
	}))
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func unix(t time.Time) int64 { return t.Unix() }
