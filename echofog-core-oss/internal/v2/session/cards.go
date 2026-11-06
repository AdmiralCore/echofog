package session

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// Record type bytes of the persistent card book. The keys are local; the values are the exact
// signed envelopes, so a restart re-verifies them instead of trusting the disk.
const (
	cardRecordType  byte = 'c' // 'c' ‖ device ID(32) ‖ scope(1) ‖ invite_id
	listRecordType  byte = 'l' // 'l' ‖ node ID(32)
	badRecordType   byte = 'b' // 'b' ‖ signer ID(32)
	cardRecordFixed      = 1 + 32 + 1
)

func cardRecord(c *objects.Card) []byte {
	invite := c.Body.GetInviteId()
	out := make([]byte, 0, cardRecordFixed+len(invite))
	out = append(out, cardRecordType)
	out = append(out, c.DeviceID[:]...)
	out = append(out, byte(c.Scope))
	return append(out, invite...)
}

func listRecord(nodeID [32]byte) []byte {
	return append([]byte{listRecordType}, nodeID[:]...)
}

func badRecord(signerID [32]byte) []byte {
	return append([]byte{badRecordType}, signerID[:]...)
}

// CardBook is objects.CardBook with every accepted object written to the store (AD13): cards
// arrive on CARD_UPDATE and in handshakes, and their seq must not be forgotten across a
// restart. A damaged or expired record is skipped on load, never trusted.
type CardBook struct {
	mu   sync.Mutex
	book *objects.CardBook
	kv   store.KV
}

// OpenCardBook opens the card book over a store namespace and restores cards, device lists and
// the misbehaving markers. kv is required: the handshake memory S15 is not complete without
// persistence.
func OpenCardBook(kv store.KV) (*CardBook, error) {
	if kv == nil {
		return nil, fmt.Errorf("%w: no KV for the card book", ErrConfig)
	}
	b := &CardBook{book: objects.NewCardBook(), kv: kv}
	if err := b.load(); err != nil {
		return nil, err
	}
	return b, nil
}

// load replays the store through the normal verification rules. A record that no longer
// verifies (expired, damaged, written by another build) is dropped from memory; the store
// keeps it until the next write replaces it.
func (b *CardBook) load() error {
	now := time.Now()

	lists, err := b.scan(listRecordType)
	if err != nil {
		return err
	}
	for _, e := range lists {
		dl, err := objects.VerifyDeviceList(e.Value, now)
		if err != nil {
			continue
		}
		_ = b.book.AddDeviceList(dl)
	}

	cards, err := b.scan(cardRecordType)
	if err != nil {
		return err
	}
	for _, e := range cards {
		c, err := objects.VerifyCard(e.Value, now)
		if err != nil {
			continue
		}
		_, _ = b.book.Add(c)
	}

	bad, err := b.scan(badRecordType)
	if err != nil {
		return err
	}
	for _, e := range bad {
		if len(e.Key) != 1+32 {
			continue
		}
		var id [32]byte
		copy(id[:], e.Key[1:])
		b.book.MarkMisbehaving(id)
	}
	return nil
}

func (b *CardBook) scan(typ byte) ([]store.Entry, error) {
	var out []store.Entry
	var start []byte
	for {
		entries, err := b.kv.ScanFrom([]byte{typ}, start, store.MaxScanLimit)
		if err != nil {
			return nil, err
		}
		out = append(out, entries...)
		if len(entries) < store.MaxScanLimit {
			return out, nil
		}
		start = entries[len(entries)-1].Key
	}
}

// Add applies the objects.md §3.2 rules and persists what the book keeps. Equivocation is
// returned (the caller may act on it) but the preferred card and the misbehaving marker stay.
func (b *CardBook) Add(c *objects.Card) (objects.AddResult, error) {
	if b == nil || c == nil || c.Env == nil {
		return objects.CardIgnored, fmt.Errorf("%w: no card", ErrConfig)
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	res, err := b.book.Add(c)
	if err != nil && !errors.Is(err, objects.ErrEquivocation) {
		return res, err
	}
	if res != objects.CardIgnored {
		if perr := b.kv.Put(cardRecord(c), c.Env.Raw); perr != nil {
			return res, perr
		}
	}
	if err != nil {
		b.persistMisbehavingLocked()
	}
	return res, err
}

// AddDeviceList applies and persists a device list.
func (b *CardBook) AddDeviceList(dl *objects.DeviceList) error {
	if b == nil || dl == nil || dl.Env == nil {
		return fmt.Errorf("%w: no device list", ErrConfig)
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	err := b.book.AddDeviceList(dl)
	if err != nil && !errors.Is(err, objects.ErrEquivocation) {
		return err
	}
	kept := b.book.DeviceList(dl.NodeID)
	if kept != nil {
		if perr := b.kv.Put(listRecord(dl.NodeID), kept.Env.Raw); perr != nil {
			return perr
		}
	}
	if err != nil {
		b.persistMisbehavingLocked()
	}
	return err
}

func (b *CardBook) persistMisbehavingLocked() {
	for _, id := range b.book.MisbehavingIDs() {
		_ = b.kv.Put(badRecord(id), nil)
	}
}

// Get returns the stored card of a slot, or nil.
func (b *CardBook) Get(deviceID [32]byte, scope objects.Scope, inviteID []byte) *objects.Card {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.book.Get(deviceID, scope, inviteID)
}

// Latest returns the device's newest card of any scope, or nil.
func (b *CardBook) Latest(deviceID [32]byte) *objects.Card {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.book.Latest(deviceID)
}

// DeviceList returns the newest device list of an account, or nil.
func (b *CardBook) DeviceList(nodeID [32]byte) *objects.DeviceList {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.book.DeviceList(nodeID)
}

// Misbehaving reports whether a signer was seen equivocating.
func (b *CardBook) Misbehaving(signerID [32]byte) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.book.Misbehaving(signerID)
}

// ControlConfig wires the session layer's control-frame handling.
type ControlConfig struct {
	// Cards is the card book that receives CARD_UPDATE (objects.md §3.6).
	Cards *CardBook
	// Abuse counts a validly signed card that claims another device (optional).
	Abuse *AbuseLog
	// Clock checks card lifetimes; nil is the system clock.
	Clock carrier.Clock
	// OnCard is called after every CARD_UPDATE that verified, with the book's result. The
	// runtime uses it to refresh the card it serves to services.
	OnCard func(*objects.Card, objects.AddResult, error)
	// Observer receives OBSERVED_ADDR frames and the address of HsResp.observed (wire.md §7.2).
	// Nil drops them.
	Observer *Observer
	// OnPunch receives PUNCH_OFFER and PUNCH_SYNC with the session they arrived on (T6.2). Nil drops
	// them.
	OnPunch func(*mux.Session, frame.Frame)
}

// observed feeds one OBSERVED_ADDR payload or HsResp.observed value to the Observer.
func (c *Control) observed(src controlSource, payload []byte) {
	if c.cfg.Observer == nil {
		return
	}
	ap, ok := ParseObserved(payload)
	if !ok {
		return
	}
	c.cfg.Observer.Observe(Observation{Peer: src.peer, PeerAddr: src.remote, Reported: ap, Socket: src.socket})
}

// Control handles session control frames that belong to this layer. One Control may be shared
// by every session of a node; the authenticated peer is passed with each frame.
type Control struct {
	cfg ControlConfig
}

// NewControl returns a CARD_UPDATE handler. A nil Cards book makes it a no-op (CARD_UPDATE is
// still accepted on the wire when the feature was negotiated).
func NewControl(cfg ControlConfig) *Control { return &Control{cfg: cfg} }

// controlSource says who sent a control frame and over what: the authenticated device, the
// peer's network address and the local socket. Peer address and socket stay empty on a relayed
// path, where they describe the relay and not the peer.
type controlSource struct {
	peer   [32]byte
	remote netip.Addr
	socket string
	// sess is the mux session the frame arrived on; it is set right after the session exists, a frame
	// that beats it is dropped (PUNCH frames are retried by their sender).
	sess *atomic.Pointer[mux.Session]
}

func (c *Control) handle(src controlSource, f frame.Frame) {
	if c == nil {
		return
	}
	if f.Type == frame.TypeObservedAddr {
		c.observed(src, f.Payload)
		return
	}
	if f.Type == frame.TypePunchOffer || f.Type == frame.TypePunchSync {
		if c.cfg.OnPunch != nil && src.sess != nil {
			if s := src.sess.Load(); s != nil {
				c.cfg.OnPunch(s, f)
			}
		}
		return
	}
	peer := src.peer
	if c.cfg.Cards == nil || f.Type != frame.TypeCardUpdate {
		return
	}
	now := clockOr(c.cfg.Clock).Now()
	card, err := objects.VerifyCard(f.Payload, now)
	if err != nil {
		// A forged or damaged card proves nothing about its sender and is not counted as
		// abuse; the session stays up (objects.md §3.6 says only that the update is applied
		// by the normal rules).
		return
	}
	if card.DeviceID != peer {
		// Only a card of the authenticated device itself is a valid update. A validly signed
		// card of another device is misbehaviour of the sender.
		if c.cfg.Abuse != nil {
			_ = c.cfg.Abuse.Report(peer)
		}
		return
	}
	res, err := c.cfg.Cards.Add(card)
	if errors.Is(err, objects.ErrHsKeyBound) && c.cfg.Abuse != nil {
		// A signed card that tries to take an hs key another device owns is abuse (handshake
		// §7, S15).
		_ = c.cfg.Abuse.Report(peer)
	}
	if c.cfg.OnCard != nil {
		c.cfg.OnCard(card, res, err)
	}
}

// PushCard sends a newer card of this identity on the control stream (objects.md §3.6). It
// fails with mux.ErrFeatureNotSelected when card-push was not negotiated.
func (e *Established) PushCard(envelope []byte) error {
	if e == nil || e.Session == nil {
		return fmt.Errorf("%w: no session", ErrConfig)
	}
	return e.Session.SendControl(frame.TypeCardUpdate, 0, envelope)
}

// GoAway sends GOAWAY and waits for the session to drain: no new streams, and the session ends
// when the streams of both sides have ended or after the drain time (15 s, wire.md §7.3).
func (e *Established) GoAway(ctx context.Context, code uint64, reason string, retryAfter uint32) error {
	if e == nil || e.Session == nil {
		return fmt.Errorf("%w: no session", ErrConfig)
	}
	if err := e.Session.GoAway(code, reason, retryAfter); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-e.Session.Done():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
