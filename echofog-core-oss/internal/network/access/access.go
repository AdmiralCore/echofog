// Package access is what keeps a hidden or isolated node to the people it invited. Its owner
// shares a secret in a link; a peer proves it knows the secret in the handshake (an HMAC bound to
// the connection, so it cannot be replayed on another one) or in a hole punch request (a ticket
// bound to the peer and the minute). The node stays silent to everybody else: it sends nothing,
// not even its public key, to a peer that proves nothing.
//
// A peer that proved the secret once is remembered (joined), so rotating the secret keeps the
// people already in and only stops the old link from letting new ones in. Remove takes one out.
package access

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sync"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// SecretLen is the length of an access secret: 128 bits, 22 characters in a link.
const SecretLen = 16

const (
	labelHandshake = "echofog/access/v1/handshake"
	labelTicket    = "echofog/access/v1/punch"
	ticketLen      = 8 + sha256.Size
	// ticketSkew is how far a ticket's minute may be from ours: clocks differ, relays are slow
	ticketSkew = 2
	// maxJoined bounds the remembered peers, so that a leaked link cannot grow the list forever
	maxJoined = 10000
)

var ErrBadSecret = errors.New("access secret must be 16 bytes")

// NewSecret returns a fresh random secret.
func NewSecret() []byte {
	s := make([]byte, SecretLen)
	if _, err := rand.Read(s); err != nil {
		panic(err) // no randomness: nothing here can be trusted
	}
	return s
}

// HandshakeProof is what an initiator sends to show it knows secret: bound to the connection
// (binding), the initiator's nonce and its public key.
func HandshakeProof(secret, binding, nonce, initiatorPub []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(labelHandshake))
	m.Write(binding)
	m.Write(nonce)
	m.Write(initiatorPub)
	return m.Sum(nil)
}

// Ticket is what a peer sends with a hole punch request: the hole punch starts before any
// handshake, and the node behind NAT may well be the side that dials, so it cannot wait for one.
func Ticket(secret []byte, peer types.PeerID, now time.Time) []byte {
	out := make([]byte, 8, ticketLen)
	binary.BigEndian.PutUint64(out, uint64(now.Unix()/60))
	return append(out, ticketMAC(secret, peer, out[:8])...)
}

func ticketMAC(secret []byte, peer types.PeerID, minute []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(labelTicket))
	m.Write(peer[:])
	m.Write(minute)
	return m.Sum(nil)
}

// Gate is the exit side: whether to talk to a peer at all.
type Gate struct {
	mu      sync.RWMutex
	enabled bool
	secret  []byte
	joined  map[types.PeerID]int64 // unix time of joining
	allow   func(types.PeerID) bool
	onJoin  func(types.PeerID)
	passes  *Passbook
	now     func() time.Time
}

// SetPasses lets the devices of active passes in, and lets a stranger in who proves an active
// pass's secret (binding it to that pass). nil: no passes.
func (g *Gate) SetPasses(b *Passbook) {
	g.mu.Lock()
	g.passes = b
	g.mu.Unlock()
}

// NewGate returns an open gate: it lets everybody in until Enable.
func NewGate() *Gate {
	return &Gate{joined: make(map[types.PeerID]int64), now: time.Now}
}

// Enable closes the gate to everybody who does not prove the secret, has not joined before and
// is not allowed otherwise; Disable opens it.
func (g *Gate) Enable(on bool) {
	g.mu.Lock()
	g.enabled = on
	g.mu.Unlock()
}

func (g *Gate) Enabled() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.enabled
}

// SetSecret sets the secret new peers must prove. A nil secret lets no new peer in.
func (g *Gate) SetSecret(s []byte) error {
	if s != nil && len(s) != SecretLen {
		return ErrBadSecret
	}
	g.mu.Lock()
	g.secret = append([]byte(nil), s...)
	if s == nil {
		g.secret = nil
	}
	g.mu.Unlock()
	return nil
}

// Secret returns a copy of the current secret (nil: none).
func (g *Gate) Secret() []byte {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.secret == nil {
		return nil
	}
	return append([]byte(nil), g.secret...)
}

// SetAllow adds peers that get in without the secret (the owner's contacts). nil: none.
func (g *Gate) SetAllow(f func(types.PeerID) bool) {
	g.mu.Lock()
	g.allow = f
	g.mu.Unlock()
}

// OnJoin is called (outside the lock) when a peer joins for the first time: the owner saves the list.
func (g *Gate) OnJoin(f func(types.PeerID)) {
	g.mu.Lock()
	g.onJoin = f
	g.mu.Unlock()
}

// SetJoined replaces the remembered peers (loaded from storage), with when each joined.
func (g *Gate) SetJoined(m map[types.PeerID]int64) {
	g.mu.Lock()
	g.joined = make(map[types.PeerID]int64, len(m))
	for id, at := range m {
		g.joined[id] = at
	}
	g.mu.Unlock()
}

// Joined returns the remembered peers with when each joined.
func (g *Gate) Joined() map[types.PeerID]int64 {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make(map[types.PeerID]int64, len(g.joined))
	for id, at := range g.joined {
		out[id] = at
	}
	return out
}

// Remove forgets a peer: it needs the current secret to come back.
func (g *Gate) Remove(id types.PeerID) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.joined[id]
	delete(g.joined, id)
	return ok
}

// Allowed reports whether peer may use this node without proving anything now: the gate is
// open, the peer joined before, or it is allowed otherwise.
func (g *Gate) Allowed(id types.PeerID) bool {
	g.mu.RLock()
	_, joined := g.joined[id]
	enabled, allow, passes := g.enabled, g.allow, g.passes
	g.mu.RUnlock()
	if !enabled {
		return true
	}
	if passes != nil && passes.Bound(id) {
		return passes.Allows(id) // a device of a pass follows its pass, even if it joined otherwise
	}
	return joined || (allow != nil && allow(id))
}

// Invited reports whether the peer came in with this node's link or a pass of it, whether or not
// the gate is on: the owner asked for it to be let in.
func (g *Gate) Invited(id types.PeerID) bool {
	if g == nil {
		return false
	}
	g.mu.RLock()
	_, joined := g.joined[id]
	passes := g.passes
	g.mu.RUnlock()
	if passes != nil && passes.Bound(id) {
		return passes.Allows(id)
	}
	return joined
}

func (g *Gate) passbook() *Passbook {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.passes
}

// AdmitHandshake decides an inbound handshake: the peer is allowed already, or proof shows it
// knows the secret for this very connection, which makes it join.
func (g *Gate) AdmitHandshake(initiatorPub, binding, nonce, proof []byte) bool {
	id, ok := peerIDOf(initiatorPub)
	if !ok {
		return false
	}
	if g.Allowed(id) {
		return true
	}
	if len(proof) != sha256.Size {
		return false
	}
	proves := func(secret []byte) bool {
		return hmac.Equal(proof, HandshakeProof(secret, binding, nonce, initiatorPub))
	}
	if pb := g.passbook(); pb != nil && pb.Bound(id) {
		return pb.admit(id, proves) // a device of a pass gets in only through that pass
	}
	if secret := g.Secret(); secret != nil && proves(secret) {
		g.join(id)
		return true
	}
	if pb := g.passbook(); pb != nil {
		return pb.admit(id, proves)
	}
	return false
}

// AdmitTicket decides a hole punch request from peer. A relay can replay a ticket it carried, but
// only for the peer it was made for, which then still has to prove its key in the handshake.
func (g *Gate) AdmitTicket(id types.PeerID, ticket []byte) bool {
	if g.Allowed(id) {
		return true
	}
	if len(ticket) != ticketLen {
		return false
	}
	minute := int64(binary.BigEndian.Uint64(ticket[:8]))
	if d := g.now().Unix()/60 - minute; d > ticketSkew || d < -ticketSkew {
		return false
	}
	proves := func(secret []byte) bool { return hmac.Equal(ticket[8:], ticketMAC(secret, id, ticket[:8])) }
	if pb := g.passbook(); pb != nil && pb.Bound(id) {
		return pb.admit(id, proves)
	}
	if secret := g.Secret(); secret != nil && proves(secret) {
		g.join(id)
		return true
	}
	if pb := g.passbook(); pb != nil {
		return pb.admit(id, proves)
	}
	return false
}

func (g *Gate) join(id types.PeerID) {
	g.mu.Lock()
	if _, ok := g.joined[id]; ok || len(g.joined) >= maxJoined {
		g.mu.Unlock()
		return
	}
	g.joined[id] = g.now().Unix()
	cb := g.onJoin
	g.mu.Unlock()
	if cb != nil {
		cb(id)
	}
}

func peerIDOf(pub []byte) (types.PeerID, bool) {
	var k types.PeerPublicKey
	if len(pub) != len(k) {
		return types.PeerID{}, false
	}
	copy(k[:], pub)
	return types.PeerPubKeyToID(k), true
}

// Keyring is the client side: the secrets of the hidden nodes this node was invited to.
type Keyring struct {
	mu   sync.RWMutex
	keys map[types.PeerID][]byte
}

func NewKeyring() *Keyring { return &Keyring{keys: make(map[types.PeerID][]byte)} }

// Set stores the secret for a node; a nil secret forgets it.
func (k *Keyring) Set(id types.PeerID, secret []byte) error {
	if secret != nil && len(secret) != SecretLen {
		return ErrBadSecret
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if secret == nil {
		delete(k.keys, id)
		return nil
	}
	k.keys[id] = append([]byte(nil), secret...)
	return nil
}

// Get returns the secret for a node, or nil.
func (k *Keyring) Get(id types.PeerID) []byte {
	if k == nil {
		return nil
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.keys[id]
}

// All returns a copy of every stored secret.
func (k *Keyring) All() map[types.PeerID][]byte {
	k.mu.RLock()
	defer k.mu.RUnlock()
	out := make(map[types.PeerID][]byte, len(k.keys))
	for id, s := range k.keys {
		out[id] = append([]byte(nil), s...)
	}
	return out
}

// Ticket returns a hole punch ticket for a node this keyring has a secret for, else nil.
func (k *Keyring) Ticket(target, self types.PeerID) []byte {
	s := k.Get(target)
	if s == nil {
		return nil
	}
	return Ticket(s, self, time.Now())
}
