package objects

import (
	"errors"
	"sync"
)

// CardBook keeps verified cards by the rules of objects.md §3.2: per (device ID, scope, invite_id)
// only the card with the highest seq (equivocation: the smaller object_id), never merging fields;
// per account only the newest device list; and each handshake key bound to one device ID.
type CardBook struct {
	mu          sync.Mutex
	cards       map[cardSlot]*Card
	hsOwner     map[string][32]byte      // hs akey → device ID
	lists       map[[32]byte]*DeviceList // node ID → newest device list
	misbehaving map[[32]byte]bool        // signer IDs seen equivocating
}

type cardSlot struct {
	device [32]byte
	scope  Scope
	invite string
}

// AddResult says what CardBook.Add did with a card.
type AddResult int

const (
	CardAdded    AddResult = iota // first card of its slot
	CardReplaced                  // newer than the stored card
	CardIgnored                   // older than, or identical to, the stored card
)

var (
	// ErrHsKeyBound reports a card whose handshake key is already bound to another device (S15).
	ErrHsKeyBound = errors.New("objects: handshake key bound to another device")
	// ErrDeviceRemoved reports a card of a device missing from its account's newest device list.
	ErrDeviceRemoved = errors.New("objects: device not in the account's current device list")
	// ErrEquivocation reports two different objects with the same (signer, label, seq).
	ErrEquivocation = errors.New("objects: equivocation")
)

// NewCardBook returns an empty card book.
func NewCardBook() *CardBook {
	return &CardBook{
		cards:       make(map[cardSlot]*Card),
		hsOwner:     make(map[string][32]byte),
		lists:       make(map[[32]byte]*DeviceList),
		misbehaving: make(map[[32]byte]bool),
	}
}

// Add stores a verified card. With ErrEquivocation the result still says which card was kept.
func (b *CardBook) Add(c *Card) (AddResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	var equivocated bool
	dl := b.lists[c.NodeID]
	switch {
	case dl == nil:
		dl = c.Devices
	case Equivocation(dl.Env, c.Devices.Env):
		equivocated = true
		b.misbehaving[c.NodeID] = true
		if Prefer(dl.Env, c.Devices.Env) == c.Devices.Env {
			dl = c.Devices
		}
	case c.Devices.Env.Seq > dl.Env.Seq:
		dl = c.Devices
	}
	b.lists[c.NodeID] = dl
	if !dl.Contains(c.DeviceKey) {
		return CardIgnored, ErrDeviceRemoved
	}

	hs := []string{string(c.HsKey)}
	if p := c.Body.GetPrevHs(); p != nil {
		hs = append(hs, string(p.GetKey()))
	}
	for _, k := range hs {
		if owner, ok := b.hsOwner[k]; ok && owner != c.DeviceID {
			return CardIgnored, ErrHsKeyBound
		}
	}

	slot := cardSlot{c.DeviceID, c.Scope, string(c.Body.GetInviteId())}
	old := b.cards[slot]
	res := CardAdded
	if old != nil {
		if Equivocation(old.Env, c.Env) {
			equivocated = true
			b.misbehaving[c.DeviceID] = true
		}
		if Prefer(old.Env, c.Env) != c.Env || old.Env.ID() == c.Env.ID() {
			res = CardIgnored
		} else {
			res = CardReplaced
		}
	}
	if res != CardIgnored {
		b.cards[slot] = c
		for _, k := range hs {
			b.hsOwner[k] = c.DeviceID
		}
	}
	if equivocated {
		return res, ErrEquivocation
	}
	return res, nil
}

// Get returns the stored card of a slot, or nil. A card of a device that the account's newest
// device list no longer lists is not returned.
func (b *CardBook) Get(deviceID [32]byte, scope Scope, inviteID []byte) *Card {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.live(b.cards[cardSlot{deviceID, scope, string(inviteID)}])
}

// Latest returns the device's most recent card of any scope (the one to dial with), or nil.
func (b *CardBook) Latest(deviceID [32]byte) *Card {
	b.mu.Lock()
	defer b.mu.Unlock()
	var best *Card
	for s, c := range b.cards {
		if s.device == deviceID && b.live(c) != nil && (best == nil || c.Env.Seq > best.Env.Seq) {
			best = c
		}
	}
	return best
}

func (b *CardBook) live(c *Card) *Card {
	if c == nil {
		return nil
	}
	if dl := b.lists[c.NodeID]; dl != nil && !dl.Contains(c.DeviceKey) {
		return nil
	}
	return c
}

// DeviceList returns the newest device list of an account, or nil.
func (b *CardBook) DeviceList(nodeID [32]byte) *DeviceList {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lists[nodeID]
}

// AddDeviceList stores a verified device list if it is newer than the stored one.
func (b *CardBook) AddDeviceList(dl *DeviceList) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	old := b.lists[dl.NodeID]
	if old != nil && Equivocation(old.Env, dl.Env) {
		b.misbehaving[dl.NodeID] = true
		b.lists[dl.NodeID] = deviceListOf(Prefer(old.Env, dl.Env), old, dl)
		return ErrEquivocation
	}
	if old == nil || dl.Env.Seq > old.Env.Seq {
		b.lists[dl.NodeID] = dl
	}
	return nil
}

func deviceListOf(e *Envelope, a, b *DeviceList) *DeviceList {
	if e == a.Env {
		return a
	}
	return b
}

// Misbehaving reports whether a signer (device or account ID) was seen equivocating.
func (b *CardBook) Misbehaving(signerID [32]byte) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.misbehaving[signerID]
}

// MisbehavingIDs returns the signers seen equivocating, in no particular order. A persistent
// card book stores the markers and restores them with MarkMisbehaving.
func (b *CardBook) MisbehavingIDs() [][32]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	ids := make([][32]byte, 0, len(b.misbehaving))
	for id := range b.misbehaving {
		ids = append(ids, id)
	}
	return ids
}

// MarkMisbehaving records a signer as having equivocated. It exists for restoring a persisted
// card book: the marker cannot be derived from the surviving cards alone.
func (b *CardBook) MarkMisbehaving(signerID [32]byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.misbehaving[signerID] = true
}
