// Package access owns a hidden node's invitations, passes and masking keys (T3.6).
//
// An invitation is a link: a signed invitation card plus a 16-byte access secret S
// (objects.md §10.3). The node keeps the pass (pass_id = HMAC-SHA256(S, "echofog/hs2/pass-id")[:8])
// and its own masking key pair, so a leaked link stops working at L0 as soon as the pass is
// revoked. After the first complete session the contact gets a personal pass and a peer-scope card
// that uses the node's contacts masking key (handshake.md §5).
//
// The manager implements handshake.PassStore; the responder checks the proof, so a pass secret never
// leaves the node. The runtime opens it over one store namespace:
//
//	store.Store(access.Namespace) → access.New
//
// Management API: Mint, Revoke, Extend, List, RotateKeys. The runtime additionally calls Redeem
// when a hidden-node session reports an invitation pass, Renew for a personal pass, and reads
// MaskKeys to build its L0 listeners.
package access

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// Namespace is the store namespace of the access state.
const Namespace = "v2/access"

// Limits and defaults.
const (
	// KeysPerListener is the L0 limit of wire.md §2.1: at most 8 live masking keys per listener.
	// More pending invitations share a key or the runtime opens another listener.
	KeysPerListener = 8
	// DefaultLinkTTL is the invitation link lifetime (objects.md §10.3, D12). It is the card's
	// own lifetime: a link that outlives its card would stop opening before its printed date.
	DefaultLinkTTL = objects.InviteCardLifetime * time.Second
	// DefaultAccessTTL is how long a contact keeps access after the first entry.
	DefaultAccessTTL = 30 * 24 * time.Hour
	// MaxPersonalTTL is the one-year upper bound of a personal pass (handshake.md §5).
	MaxPersonalTTL = 365 * 24 * time.Hour
	// MaxPasses bounds the passes one node keeps: every hidden-node handshake looks a pass up by
	// selector, so the map must stay small (the v1 passbook had the same 10 000 limit).
	MaxPasses = 10000
	// contactSeenWindow is how far back a contact counts as active for retiring the previous
	// contacts key (objects.md §10.3).
	contactSeenWindow = 30 * 24 * time.Hour
	// defaultOverlap is how long the previous contacts key stays accepted while contacts receive
	// the new peer card.
	defaultOverlap = 30 * 24 * time.Hour
)

// Errors of the manager. They are local: none of them changes the wire behaviour.
var (
	// ErrConfig reports invalid configuration or arguments.
	ErrConfig = errors.New("access: invalid configuration")
	// ErrState reports an unreadable or unwritable persistent record.
	ErrState = errors.New("access: state")
	// ErrClosed reports use after Close.
	ErrClosed = errors.New("access: closed")
	// ErrNotFound reports an unknown pass.
	ErrNotFound = errors.New("access: no such pass")
	// ErrRevoked reports a revoked pass.
	ErrRevoked = errors.New("access: pass revoked")
	// ErrExpired reports an expired pass.
	ErrExpired = errors.New("access: pass expired")
	// ErrUsedUp reports an invitation whose entries are spent.
	ErrUsedUp = errors.New("access: invitation used up")
	// ErrKeyLimit reports no free listener slot and no key to share.
	ErrKeyLimit = errors.New("access: masking-key limit reached")
	// ErrPeerCard reports a peer card that does not carry the contacts key or leaks another key.
	ErrPeerCard = errors.New("access: peer card does not use the contacts key")
	// ErrNotPersonal reports an operation that needs a personal pass.
	ErrNotPersonal = errors.New("access: not a personal pass")
	// ErrNotInvitation reports an operation that needs an invitation pass.
	ErrNotInvitation = errors.New("access: not an invitation pass")
	// ErrLimit reports the MaxPasses bound.
	ErrLimit = errors.New("access: too many passes")
)

// Manager implements the handshake's pass lookup (handshake.md §5).
var _ handshake.PassStore = (*Manager)(nil)

// Clock is the node's corrected clock (AD13).
type Clock interface{ Now() time.Time }

// CardIssuer is the part of node.CardBuilder that access uses. *cards.CardBuilder satisfies it.
type CardIssuer interface {
	IssueInvite(ctx context.Context, opt cards.InviteOptions) (*cards.IssuedCard, error)
	Current(ctx context.Context, scope objects.Scope) (*cards.IssuedCard, error)
}

// Config configures a manager.
type Config struct {
	// KV is the store namespace (store.Store(access.Namespace)); required.
	KV store.KV
	// Cards issues the invitation and peer cards; required.
	Cards CardIssuer
	// Listeners are the node's invitation-capable L0 listeners, one entry per listener slot. The
	// Mask of each entry is ignored: access replaces it with the invitation's own key and issues
	// cards with only that listener (cards.InviteOptions.OnlyThese). The contacts listener is not
	// one of them; the runtime installs ContactsKeys() separately.
	Listeners []cards.Listener
	// ContactsKey is the initial contacts masking key pair, usually identity.Device.Masking().
	// It is copied; the caller keeps ownership. A key already in the store wins.
	ContactsKey *identity.KeyPair
	// Clock is the injected clock; nil means time.Now.
	Clock Clock
	// Rand is the randomness source; nil means crypto/rand.
	Rand io.Reader
	// LinkTTL overrides DefaultLinkTTL (capped at the invitation card's lifetime).
	LinkTTL time.Duration
	// AccessTTL overrides DefaultAccessTTL (capped at MaxPersonalTTL).
	AccessTTL time.Duration
	// PersonalTTL is the renewal term of a personal pass; zero means AccessTTL.
	PersonalTTL time.Duration
	// ContactKeyOverlap is how long the previous contacts key stays accepted; zero means 30 days.
	ContactKeyOverlap time.Duration
}

// Kind is the kind of a pass.
type Kind uint8

const (
	// Invitation is an invitation pass from a link.
	Invitation Kind = iota + 1
	// Personal is a contact's personal pass.
	Personal
)

func (k Kind) String() string {
	switch k {
	case Invitation:
		return "invitation"
	case Personal:
		return "personal"
	}
	return "unknown"
}

// State is the life-cycle state of a pass as List reports it.
type State uint8

const (
	// Active means the pass admits now.
	Active State = iota + 1
	// Used means every entry of an invitation was spent.
	Used
	// Expired means the link or the access term passed.
	Expired
	// Revoked means the owner revoked the pass.
	Revoked
)

func (s State) String() string {
	switch s {
	case Active:
		return "active"
	case Used:
		return "used"
	case Expired:
		return "expired"
	case Revoked:
		return "revoked"
	}
	return "unknown"
}

// PassID is the 8-byte pass selector (handshake.md §5).
type PassID [8]byte

// Bytes returns the selector as a fresh slice.
func (id PassID) Bytes() []byte { return append([]byte(nil), id[:]...) }

// String returns the selector in lower-case hex.
func (id PassID) String() string { return hex.EncodeToString(id[:]) }

// Info is the owner-facing view of a pass. Secret is the access secret: it is a credential, so
// callers must never log it; it is present so the owner can resend an invitation link.
type Info struct {
	ID     PassID
	Kind   Kind
	State  State
	Secret []byte
	// Created is when the pass was minted.
	Created time.Time
	// LinkExpires is the deadline for redeeming an invitation.
	LinkExpires time.Time
	// AccessExpires is the access term: after the first entry for an invitation, the expiry of a
	// personal pass.
	AccessExpires time.Time
	MaxEntries    uint32
	Entries       uint32
	// Contact and Device identify the redeemer of an invitation or the owner of a personal pass.
	Contact [32]byte
	Device  [32]byte
	// MaskKey is the invitation's masking public key (nil for a personal pass).
	MaskKey keys.AKey
	// Card is the invitation envelope.
	Card []byte
	// Link is the invitation link (empty for a personal pass).
	Link string
}

// MintOptions are the per-invitation settings.
type MintOptions struct {
	LinkTTL    time.Duration
	AccessTTL  time.Duration
	MaxEntries uint32
	Share      bool
}

// Minted is a freshly minted invitation. Secret is a credential: never log it.
type Minted struct {
	PassID      PassID
	Secret      []byte
	Card        []byte
	Link        string
	LinkExpires time.Time
	MaxEntries  uint32
	MaskKey     keys.AKey
	// Listener is the Config.Listeners slot whose address the card carries.
	Listener int
}

// RedeemRequest identifies the contact that completed the first session.
type RedeemRequest struct {
	PassID PassID
	// Node is the contact's account node ID, Device its device ID.
	Node   [32]byte
	Device [32]byte
}

// Redeemed is the result of redeeming an invitation: a personal pass for later sessions and the
// peer-scope card to deliver over the session (handshake.md §5, objects.md §10.3).
type Redeemed struct {
	// PassID is the personal pass issued or renewed for the contact.
	PassID      PassID
	Secret      []byte
	Expires     time.Time
	PeerCard    []byte
	PeerCardSeq uint64
}

// dropKeysOutsideConfigLocked revokes invitations whose listener slot no longer exists. A card
// that names an address the node does not serve is unreachable; failing closed is better than
// keeping a pass that can never be used and a key the runtime cannot install.
func (m *Manager) dropKeysOutsideConfigLocked() error {
	var errs []error
	for id, r := range m.keys {
		if r.Kind != keyInvitation || int(r.Slot) < len(m.listeners) {
			continue
		}
		for _, inv := range m.invitations {
			if inv.KeyID != id {
				continue
			}
			inv.Revoked = true
			wipe(inv.Secret)
			inv.Secret = nil
			inv.Entries = inv.MaxEntries
			if err := m.saveInvitation(inv); err != nil {
				errs = append(errs, fmt.Errorf("%w: save invitation: %v", ErrState, err))
			}
		}
		if err := m.removeKeyLocked(id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// KeyInfo is one live masking key as the L0 listener and the card builder need it.
type KeyInfo struct {
	Public      keys.AKey
	Contacts    bool
	Slot        int
	NotAfter    time.Time
	RetireAfter time.Time
	Version     uint64
	// Invitations are the active invitation passes that use an invitation key.
	Invitations []PassID
}

// Rotation is the result of RotateKeys.
type Rotation struct {
	ContactsKey keys.AKey
	// PreviousKey is the contacts key kept during the overlap; nil when no contact needs it.
	PreviousKey         keys.AKey
	PreviousRetireAfter time.Time
	Reissued            []Minted
}

// RevokeResult is the result of Revoke.
type RevokeResult struct {
	Pass       Info
	KeyRemoved keys.AKey
	Reissued   []Minted
	// Contacts is set when revoking a contact rotated the contacts key early (objects.md §10.3).
	Contacts *Rotation
}

// Manager owns a node's invitations, passes and masking keys. It is safe for concurrent use.
type Manager struct {
	mu    sync.RWMutex
	kv    store.KV
	cards CardIssuer
	clk   Clock
	rand  io.Reader

	listeners   []cards.Listener
	linkTTL     time.Duration
	accessTTL   time.Duration
	personalTTL time.Duration
	overlap     time.Duration

	contactsKeyID   [8]byte
	contactsVersion uint64
	invitations     map[PassID]*invitationRecord
	personal        map[PassID]*personalRecord
	keys            map[[8]byte]*keyRecord
	live            map[[8]byte]*identity.KeyPair
	closed          bool
}

// New opens the manager over a store namespace and restores its state.
func New(cfg Config) (*Manager, error) {
	if cfg.KV == nil || cfg.Cards == nil {
		return nil, fmt.Errorf("%w: KV and cards are required", ErrConfig)
	}
	if len(cfg.Listeners) == 0 {
		return nil, fmt.Errorf("%w: no invitation listener", ErrConfig)
	}
	for i, l := range cfg.Listeners {
		if err := validateListener(l); err != nil {
			return nil, fmt.Errorf("%w: listener %d: %v", ErrConfig, i, err)
		}
	}
	m := &Manager{
		kv:          cfg.KV,
		cards:       cfg.Cards,
		clk:         cfg.Clock,
		rand:        cfg.Rand,
		listeners:   cloneListeners(cfg.Listeners),
		linkTTL:     cfg.LinkTTL,
		accessTTL:   cfg.AccessTTL,
		personalTTL: cfg.PersonalTTL,
		overlap:     cfg.ContactKeyOverlap,
		invitations: make(map[PassID]*invitationRecord),
		personal:    make(map[PassID]*personalRecord),
		keys:        make(map[[8]byte]*keyRecord),
		live:        make(map[[8]byte]*identity.KeyPair),
	}
	if m.clk == nil {
		m.clk = systemClock{}
	}
	if m.rand == nil {
		m.rand = rand.Reader
	}
	if m.linkTTL == 0 {
		m.linkTTL = DefaultLinkTTL
	}
	if m.linkTTL < 0 || m.linkTTL > objects.InviteCardMaxLifetime*time.Second {
		return nil, fmt.Errorf("%w: link lifetime %v", ErrConfig, m.linkTTL)
	}
	if m.linkTTL > DefaultLinkTTL {
		m.linkTTL = DefaultLinkTTL
	}
	if m.accessTTL == 0 {
		m.accessTTL = DefaultAccessTTL
	}
	if m.accessTTL < 0 || m.accessTTL > MaxPersonalTTL {
		return nil, fmt.Errorf("%w: access lifetime %v", ErrConfig, m.accessTTL)
	}
	if m.personalTTL == 0 {
		m.personalTTL = m.accessTTL
	}
	if m.personalTTL < 0 || m.personalTTL > MaxPersonalTTL {
		return nil, fmt.Errorf("%w: personal lifetime %v", ErrConfig, m.personalTTL)
	}
	if m.overlap == 0 {
		m.overlap = defaultOverlap
	}
	if m.overlap < 0 {
		return nil, fmt.Errorf("%w: contacts overlap %v", ErrConfig, m.overlap)
	}

	if err := m.loadState(); err != nil {
		m.Close()
		return nil, err
	}
	if err := m.initContactsKey(cfg.ContactsKey); err != nil {
		m.Close()
		return nil, err
	}
	if err := m.dropKeysOutsideConfigLocked(); err != nil {
		m.Close()
		return nil, err
	}
	if err := m.pruneInvitationKeysLocked(m.clk.Now()); err != nil {
		m.Close()
		return nil, err
	}
	if err := m.pruneContactKeysLocked(m.clk.Now()); err != nil {
		m.Close()
		return nil, err
	}
	return m, nil
}

// initContactsKey adopts cfg.ContactsKey when the store has no contacts key, otherwise the stored
// key wins (a profile must keep admitting its contacts).
func (m *Manager) initContactsKey(cfgKey *identity.KeyPair) error {
	now := m.clk.Now()
	cur := m.keys[m.contactsKeyID]
	if cur != nil && cur.Kind != keyContacts {
		cur = nil
	}
	if cur == nil {
		// Meta is missing or stale: take the newest contacts key already in the store.
		var best *keyRecord
		for _, r := range m.keys {
			if r.Kind == keyContacts && (best == nil || r.Version > best.Version) {
				best = r
			}
		}
		if best != nil {
			cur = best
			m.contactsKeyID, m.contactsVersion = best.ID, best.Version
			return m.saveMeta()
		}
	}
	if cur != nil {
		if m.contactsVersion < cur.Version {
			m.contactsVersion = cur.Version
		}
		return m.saveMeta()
	}

	if cfgKey != nil {
		var priv []byte
		if err := cfgKey.WithPrivate(func(b []byte) error {
			priv = append([]byte(nil), b...)
			return nil
		}); err != nil {
			return fmt.Errorf("%w: contacts key: %v", ErrConfig, err)
		}
		kp, err := identity.KeyPairFromPrivate(priv)
		if err != nil {
			return fmt.Errorf("%w: contacts key: %v", ErrConfig, err)
		}
		var id [8]byte
		if _, err := io.ReadFull(m.rand, id[:]); err != nil {
			kp.Destroy()
			return fmt.Errorf("%w: key id: %v", ErrState, err)
		}
		rec := &keyRecord{ID: id, Kind: keyContacts, Private: priv, Slot: contactSlot,
			Created: unix(now), Version: 1}
		m.keys[id], m.live[id] = rec, kp
		m.contactsKeyID, m.contactsVersion = id, 1
		if err := m.saveKey(rec); err != nil {
			delete(m.keys, id)
			delete(m.live, id)
			kp.Destroy()
			return fmt.Errorf("%w: save contacts key: %v", ErrState, err)
		}
		return m.saveMeta()
	}

	rec, err := m.newKeyLocked(keyContacts, contactSlot, now, 0, 0, 1)
	if err != nil {
		return err
	}
	m.contactsKeyID, m.contactsVersion = rec.ID, rec.Version
	return m.saveMeta()
}

// Close destroys the private keys the manager holds. It does not close the store.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	for id, kp := range m.live {
		kp.Destroy()
		delete(m.live, id)
	}
	for _, r := range m.invitations {
		wipe(r.Secret)
		r.Secret = nil
	}
	for _, r := range m.personal {
		wipe(r.Secret)
		r.Secret = nil
	}
	for _, r := range m.keys {
		wipe(r.Private)
		r.Private = nil
	}
	m.closed = true
}

// LookupPass implements handshake.PassStore. It performs no I/O: the responder calls it on the
// hidden-node path before any byte leaves the node (handshake.md §5).
func (m *Manager) LookupPass(id []byte, now time.Time) (handshake.Pass, bool) {
	if m == nil || len(id) != 8 {
		return handshake.Pass{}, false
	}
	var pid PassID
	copy(pid[:], id)
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return handshake.Pass{}, false
	}
	if r, ok := m.invitations[pid]; ok && invitationActive(r, now) {
		return handshake.Pass{Secret: append([]byte(nil), r.Secret...)}, true
	}
	if r, ok := m.personal[pid]; ok && personalActive(r, now) {
		return handshake.Pass{Secret: append([]byte(nil), r.Secret...), Personal: true}, true
	}
	return handshake.Pass{}, false
}

// Mint creates an invitation: a fresh masking key (or a shared group key when the listener is
// full), an access secret, an invitation card and its link. The secret is returned only here and
// in List.
func (m *Manager) Mint(ctx context.Context, opt MintOptions) (*Minted, error) {
	linkTTL := opt.LinkTTL
	if linkTTL == 0 {
		linkTTL = m.linkTTL
	}
	if linkTTL <= 0 || linkTTL > objects.InviteCardMaxLifetime*time.Second {
		return nil, fmt.Errorf("%w: link lifetime %v", ErrConfig, linkTTL)
	}
	if linkTTL > DefaultLinkTTL {
		// The card is issued for InviteCardLifetime. A link asked for the protocol's maximum is cut
		// to the card's lifetime instead of promising a date the link cannot reach.
		linkTTL = DefaultLinkTTL
	}
	accessTTL := opt.AccessTTL
	if accessTTL == 0 {
		accessTTL = m.accessTTL
	}
	if accessTTL <= 0 || accessTTL > MaxPersonalTTL {
		return nil, fmt.Errorf("%w: access lifetime %v", ErrConfig, accessTTL)
	}
	maxEntries := opt.MaxEntries
	if maxEntries == 0 {
		maxEntries = 1
	}
	if maxEntries > maxEntriesLimit {
		return nil, fmt.Errorf("%w: %d entries", ErrConfig, maxEntries)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	if len(m.invitations) >= MaxPasses {
		return nil, ErrLimit
	}
	now := m.clk.Now()
	if err := m.pruneInvitationKeysLocked(now); err != nil {
		return nil, err
	}

	secret, pid, err := m.newPassIDLocked()
	if err != nil {
		return nil, err
	}
	key, fresh, err := m.keyForMintLocked(now, opt.Share)
	if err != nil {
		wipe(secret)
		return nil, err
	}
	linkExpires := now.Add(linkTTL)
	if err := m.ensureKeyNotAfterLocked(key, unix(linkExpires)); err != nil {
		if fresh {
			_ = m.removeKeyLocked(key.ID)
		}
		wipe(secret)
		return nil, err
	}
	maskPub := m.publicOf(key.ID)
	ic, card, err := m.issueInvitationCardLocked(ctx, pid, key, unix(linkExpires), now)
	if err != nil {
		if fresh {
			_ = m.removeKeyLocked(key.ID)
		}
		wipe(secret)
		return nil, err
	}

	rec := &invitationRecord{
		PassID: pid, Secret: secret, KeyID: key.ID, Slot: key.Slot,
		Created: unix(now), LinkExpires: unix(linkExpires),
		AccessSecs: int64(accessTTL / time.Second), CardNotAfter: int64(card.Env.NotAfter),
		MaxEntries: maxEntries, Card: append([]byte(nil), ic.Envelope...),
	}
	if err := m.saveInvitation(rec); err != nil {
		if fresh {
			_ = m.removeKeyLocked(key.ID)
		}
		wipe(secret)
		return nil, fmt.Errorf("%w: save invitation: %v", ErrState, err)
	}
	m.invitations[pid] = rec

	link, err := objects.EncodeLink(ic.Envelope, secret)
	if err != nil {
		return nil, fmt.Errorf("%w: link: %v", ErrState, err)
	}
	return &Minted{
		PassID: pid, Secret: append([]byte(nil), secret...), Card: append([]byte(nil), ic.Envelope...),
		Link: link, LinkExpires: linkExpires, MaxEntries: maxEntries,
		MaskKey: append(keys.AKey(nil), maskPub...), Listener: int(key.Slot),
	}, nil
}

// issueInvitationCardLocked builds, signs and checks an invitation card for one key. maskNotAfter
// is the invitation's own masking-key deadline: an extension or a reissue of an expired link must
// put it in the future, otherwise the initiator refuses the card (T3.6 fix 1-2).
func (m *Manager) issueInvitationCardLocked(ctx context.Context, pid PassID, key *keyRecord, maskNotAfter int64, now time.Time) (*cards.IssuedCard, *objects.Card, error) {
	if int(key.Slot) >= len(m.listeners) {
		return nil, nil, fmt.Errorf("%w: listener slot %d is gone", ErrState, key.Slot)
	}
	if maskNotAfter <= unix(now) {
		return nil, nil, fmt.Errorf("%w: mask deadline %d is not in the future", ErrState, maskNotAfter)
	}
	maskPub := m.publicOf(key.ID)
	listener := m.listeners[key.Slot]
	listener.Mask = maskPub
	listener.MaskNotAfter = uint64(maskNotAfter)
	ic, err := m.cards.IssueInvite(ctx, cards.InviteOptions{
		InviteID:  pid.Bytes(),
		Listeners: []cards.Listener{listener},
		OnlyThese: true,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("%w: invitation card: %v", ErrState, err)
	}
	card, err := validateInviteCard(ic.Envelope, pid, maskPub, now)
	if err != nil {
		return nil, nil, err
	}
	return ic, card, nil
}

// newPassIDLocked draws a secret and its selector, avoiding a collision with any known pass.
func (m *Manager) newPassIDLocked() ([]byte, PassID, error) {
	for i := 0; i < 8; i++ {
		secret := make([]byte, secretLen)
		if _, err := io.ReadFull(m.rand, secret); err != nil {
			return nil, PassID{}, fmt.Errorf("%w: secret: %v", ErrState, err)
		}
		var pid PassID
		copy(pid[:], handshake.PassID(secret))
		if _, dup := m.invitations[pid]; dup {
			continue
		}
		if _, dup := m.personal[pid]; dup {
			continue
		}
		return secret, pid, nil
	}
	return nil, PassID{}, fmt.Errorf("%w: pass id collision", ErrState)
}

// Redeem consumes one invitation entry, issues or renews the contact's personal pass and returns
// the peer-scope card the runtime must send over the session (handshake.md §5).
func (m *Manager) Redeem(ctx context.Context, req RedeemRequest) (*Redeemed, error) {
	if req.Node == ([32]byte{}) {
		return nil, fmt.Errorf("%w: no contact node id", ErrConfig)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	now := m.clk.Now()
	inv := m.invitations[req.PassID]
	if inv == nil {
		if _, ok := m.personal[req.PassID]; ok {
			return nil, ErrNotInvitation
		}
		return nil, ErrNotFound
	}
	switch {
	case inv.Revoked:
		return nil, ErrRevoked
	case inv.Entries >= inv.MaxEntries || len(inv.Secret) != secretLen:
		return nil, ErrUsedUp
	case unix(now) >= inv.LinkExpires:
		return nil, ErrExpired
	case inv.AccessExpires != 0 && unix(now) >= inv.AccessExpires:
		return nil, ErrExpired
	}

	peer, err := m.peerCardLocked(ctx, now)
	if err != nil {
		return nil, err
	}

	// One personal pass per contact: a later invitation entry renews it instead of piling up passes.
	// Several records can exist for one node when an old pass expired and a new invitation was
	// redeemed; prefer the active one with the latest expiry.
	var existing *personalRecord
	for _, r := range m.personal {
		if r.Node != req.Node || r.Revoked {
			continue
		}
		if existing == nil || r.Expires > existing.Expires {
			existing = r
		}
	}
	pers := &personalRecord{Node: req.Node, Device: req.Device}
	termEnd := m.capped(now, time.Duration(inv.AccessSecs)*time.Second)
	if existing != nil && unix(now) < existing.Expires {
		*pers = *existing
		pers.Device = req.Device
		pers.LastSeen = unix(now)
		pers.CardVersion = m.contactsVersion
		pers.Expires = laterUnix(existing.Expires, termEnd)
	} else {
		if len(m.personal) >= MaxPasses {
			return nil, ErrLimit
		}
		secret, pid, err := m.newPassIDLocked()
		if err != nil {
			return nil, err
		}
		*pers = personalRecord{
			PassID: pid, Secret: secret, Created: unix(now), Expires: termEnd,
			LastSeen: unix(now), CardVersion: m.contactsVersion, Node: req.Node, Device: req.Device,
		}
	}

	next := *inv
	next.Entries++
	if next.Entries == 1 {
		next.AccessExpires = termEnd
	}
	if next.Entries >= next.MaxEntries {
		wipe(next.Secret)
		next.Secret = nil
	}

	if err := m.savePersonal(pers); err != nil {
		return nil, fmt.Errorf("%w: save personal pass: %v", ErrState, err)
	}
	m.personal[pers.PassID] = pers
	if err := m.saveInvitation(&next); err != nil {
		return nil, fmt.Errorf("%w: save invitation: %v", ErrState, err)
	}
	*inv = next
	if err := m.pruneInvitationKeysLocked(now); err != nil {
		return nil, err
	}
	return &Redeemed{
		PassID: pers.PassID, Secret: append([]byte(nil), pers.Secret...),
		Expires: time.Unix(pers.Expires, 0), PeerCard: append([]byte(nil), peer.Envelope...),
		PeerCardSeq: peer.Seq,
	}, nil
}

// peerCardLocked gets the current peer-scope card and checks that it carries the contacts key and
// never an invitation key (objects.md §10.3).
func (m *Manager) peerCardLocked(ctx context.Context, now time.Time) (*cards.IssuedCard, error) {
	ic, err := m.cards.Current(ctx, objects.ScopePeer)
	if err != nil {
		return nil, fmt.Errorf("%w: peer card: %v", ErrState, err)
	}
	c, err := objects.VerifyCard(ic.Envelope, now)
	if err != nil {
		return nil, fmt.Errorf("%w: peer card: %v", ErrPeerCard, err)
	}
	if c.Scope != objects.ScopePeer {
		return nil, fmt.Errorf("%w: scope %v", ErrPeerCard, c.Scope)
	}
	used := false
	for _, a := range c.Body.GetAddrs() {
		i := int(a.GetMask())
		if i <= 0 || i > len(c.Body.GetMaskKeys()) {
			return nil, fmt.Errorf("%w: address mask index", ErrPeerCard)
		}
		pub := c.Body.GetMaskKeys()[i-1].GetKey()
		if m.contactKeyLiveLocked(pub) {
			used = true
			continue
		}
		if m.invitationKeyLiveLocked(pub, now) {
			return nil, fmt.Errorf("%w: invitation key in a peer card", ErrPeerCard)
		}
	}
	if !used {
		return nil, fmt.Errorf("%w: no contacts key", ErrPeerCard)
	}
	return ic, nil
}

// Renew extends a personal pass from now and records that the contact has the current contacts
// card; the runtime calls it after every completed session (handshake.md §5).
func (m *Manager) Renew(ctx context.Context, id PassID) (Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Info{}, ErrClosed
	}
	now := m.clk.Now()
	r := m.personal[id]
	if r == nil {
		if _, ok := m.invitations[id]; ok {
			return Info{}, ErrNotPersonal
		}
		return Info{}, ErrNotFound
	}
	switch {
	case r.Revoked:
		return Info{}, ErrRevoked
	case unix(now) >= r.Expires:
		return Info{}, ErrExpired
	}
	next := *r
	next.Expires = laterUnix(r.Expires, m.capped(now, m.personalTTL))
	next.LastSeen = unix(now)
	next.CardVersion = m.contactsVersion
	if err := m.savePersonal(&next); err != nil {
		return Info{}, fmt.Errorf("%w: save personal pass: %v", ErrState, err)
	}
	*r = next
	if err := m.pruneContactKeysLocked(now); err != nil {
		return m.infoPersonalLocked(r, now), err
	}
	return m.infoPersonalLocked(r, now), nil
}

// Extend adds term to a pass: the link and access deadlines of an invitation, or the expiry of a
// personal pass. It caps at the invitation card's not_after and at one year for a personal pass.
func (m *Manager) Extend(ctx context.Context, id PassID, d time.Duration) (Info, error) {
	if d <= 0 {
		return Info{}, fmt.Errorf("%w: extension %v", ErrConfig, d)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Info{}, ErrClosed
	}
	now := m.clk.Now()
	delta := int64(d / time.Second)
	if delta <= 0 {
		delta = 1
	}
	if inv := m.invitations[id]; inv != nil {
		switch {
		case inv.Revoked:
			return Info{}, ErrRevoked
		case inv.Entries >= inv.MaxEntries || len(inv.Secret) != secretLen:
			return Info{}, ErrUsedUp
		}
		key := m.keys[inv.KeyID]
		if key == nil {
			return Info{}, fmt.Errorf("%w: invitation key is gone", ErrState)
		}
		next := *inv
		base := inv.LinkExpires
		if unix(now) > base {
			base = unix(now)
		}
		// A renewed card carries a new 30-day lifetime; the old card's not_after must not cap it.
		desired := minInt64(base+delta, unix(now)+int64(DefaultLinkTTL/time.Second))
		ic, card, err := m.issueInvitationCardLocked(ctx, inv.PassID, key, desired, now)
		if err != nil {
			return Info{}, err
		}
		if err := m.ensureKeyNotAfterLocked(key, desired); err != nil {
			return Info{}, err
		}
		next.LinkExpires = desired
		next.Card = append([]byte(nil), ic.Envelope...)
		next.CardNotAfter = int64(card.Env.NotAfter)
		if next.AccessExpires != 0 {
			base = next.AccessExpires
			if unix(now) > base {
				base = unix(now)
			}
			next.AccessExpires = minInt64(base+delta, unix(now)+int64(MaxPersonalTTL/time.Second))
		}
		if err := m.saveInvitation(&next); err != nil {
			return Info{}, fmt.Errorf("%w: save invitation: %v", ErrState, err)
		}
		*inv = next
		return m.infoInvitationLocked(inv, now), nil
	}
	if r := m.personal[id]; r != nil {
		if r.Revoked {
			return Info{}, ErrRevoked
		}
		next := *r
		base := r.Expires
		if unix(now) > base {
			base = unix(now)
		}
		next.Expires = minInt64(base+delta, unix(now)+int64(MaxPersonalTTL/time.Second))
		if err := m.savePersonal(&next); err != nil {
			return Info{}, fmt.Errorf("%w: save personal pass: %v", ErrState, err)
		}
		*r = next
		return m.infoPersonalLocked(r, now), nil
	}
	return Info{}, ErrNotFound
}

// Revoke removes a pass. An invitation's masking key is deleted at once, so its link stops working
// at L0 (objects.md §10.3). If other unredeemed invitations shared the key, they are reissued on a
// fresh key and their new links are returned. Revoking a contact also rotates the contacts key.
func (m *Manager) Revoke(ctx context.Context, id PassID) (*RevokeResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	now := m.clk.Now()
	if r := m.personal[id]; r != nil {
		return m.revokePersonalLocked(r, now)
	}
	if r := m.invitations[id]; r != nil {
		return m.revokeInvitationLocked(ctx, r, now)
	}
	return nil, ErrNotFound
}

// RevokeContact revokes every personal pass of the contact and rotates the contacts key early.
func (m *Manager) RevokeContact(ctx context.Context, node [32]byte) (*RevokeResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	now := m.clk.Now()
	var records []*personalRecord
	for _, r := range m.personal {
		if r.Node != node || r.Revoked {
			continue
		}
		records = append(records, r)
	}
	if len(records) == 0 {
		return nil, ErrNotFound
	}
	// Fail closed for every pass of the contact before any write: a later store error must not
	// leave another device of the same contact active.
	for _, r := range records {
		next := *r
		next.Revoked = true
		wipe(next.Secret)
		next.Secret = nil
		*r = next
	}
	var errs []error
	for _, r := range records {
		if err := m.savePersonal(r); err != nil {
			errs = append(errs, fmt.Errorf("%w: save personal pass: %v", ErrState, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	next, old, err := m.rotateContactsLocked(now)
	if err != nil {
		return nil, err
	}
	return &RevokeResult{Pass: m.infoPersonalLocked(records[0], now), Contacts: m.rotationOf(next, old)}, nil
}

func (m *Manager) revokePersonalLocked(r *personalRecord, now time.Time) (*RevokeResult, error) {
	if r.Revoked {
		return &RevokeResult{Pass: m.infoPersonalLocked(r, now)}, nil
	}
	next := *r
	next.Revoked = true
	wipe(next.Secret)
	next.Secret = nil
	*r = next
	if err := m.savePersonal(&next); err != nil {
		return nil, fmt.Errorf("%w: save personal pass: %v", ErrState, err)
	}
	nextKey, old, err := m.rotateContactsLocked(now)
	if err != nil {
		return nil, err
	}
	return &RevokeResult{Pass: m.infoPersonalLocked(r, now), Contacts: m.rotationOf(nextKey, old)}, nil
}

func (m *Manager) revokeInvitationLocked(ctx context.Context, r *invitationRecord, now time.Time) (*RevokeResult, error) {
	info := m.infoInvitationLocked(r, now)
	if r.Revoked && len(r.Secret) == 0 {
		return &RevokeResult{Pass: info}, nil
	}
	var others []*invitationRecord
	for _, o := range m.holdersLocked(r.KeyID) {
		if o.PassID == r.PassID {
			continue
		}
		others = append(others, o)
	}
	sort.Slice(others, func(i, j int) bool {
		return bytesCompare(others[i].PassID[:], others[j].PassID[:]) < 0
	})
	oldKey := r.KeyID
	oldPub := m.publicOf(oldKey)
	slot := r.Slot

	next := *r
	next.Revoked = true
	wipe(next.Secret)
	next.Secret = nil

	if len(others) == 0 {
		if err := m.removeKeyLocked(oldKey); err != nil {
			*r = next
			return nil, errors.Join(err, m.markRevokedLocked([]*invitationRecord{r}))
		}
		*r = next
		if err := m.saveInvitation(&next); err != nil {
			return nil, fmt.Errorf("%w: save invitation: %v", ErrState, err)
		}
		return &RevokeResult{Pass: m.infoInvitationLocked(r, now), KeyRemoved: oldPub}, nil
	}

	// The old key must die before anything is reissued: a crashed reissue then leaves the other
	// invitations broken (fail closed) rather than reviving the revoked key.
	if err := m.removeKeyLocked(oldKey); err != nil {
		*r = next
		return nil, errors.Join(err, m.markRevokedLocked(append([]*invitationRecord{r}, others...)))
	}
	*r = next
	if err := m.saveInvitation(&next); err != nil {
		return nil, fmt.Errorf("%w: save invitation: %v", ErrState, err)
	}

	fresh, err := m.newKeyLocked(keyInvitation, slot, now, 0, 0, 0)
	if err != nil {
		return nil, errors.Join(err, m.markRevokedLocked(others))
	}
	var reissued []Minted
	for i, o := range others {
		min, err := m.reissueInvitationLocked(ctx, o, fresh, now)
		if err != nil {
			// The old key is already gone; every invitation not moved to the new key must stop
			// admitting rather than linger without a listener.
			return nil, errors.Join(err, m.markRevokedLocked(others[i:]))
		}
		reissued = append(reissued, *min)
	}
	return &RevokeResult{
		Pass:       m.infoInvitationLocked(r, now),
		KeyRemoved: oldPub, Reissued: reissued,
	}, nil
}

// markRevokedLocked fails closed: an invitation that lost its masking key can never be reached,
// so it must not keep admitting through a stale in-memory record. The in-memory change happens
// before the write; a write failure is returned but does not undo it.
func (m *Manager) markRevokedLocked(list []*invitationRecord) error {
	var errs []error
	for _, r := range list {
		next := *r
		next.Revoked = true
		wipe(next.Secret)
		next.Secret = nil
		*r = next
		if err := m.saveInvitation(&next); err != nil {
			errs = append(errs, fmt.Errorf("%w: save invitation: %v", ErrState, err))
		}
	}
	return errors.Join(errs...)
}

// reissueInvitationLocked moves one invitation to a new secret, pass_id and card on the given key.
// The old record is deleted after the new one is written; a crash leaves the invitation broken.
// An expired link gets a future mask deadline so RotateKeys does not fail on it; the link itself
// stays expired until the owner calls Extend (T3.6 fix 2).
func (m *Manager) reissueInvitationLocked(ctx context.Context, r *invitationRecord, key *keyRecord, now time.Time) (*Minted, error) {
	secret, pid, err := m.newPassIDLocked()
	if err != nil {
		return nil, err
	}
	maskNA := r.LinkExpires
	if maskNA <= unix(now) {
		maskNA = unix(now) + int64(DefaultLinkTTL/time.Second)
	}
	ic, card, err := m.issueInvitationCardLocked(ctx, pid, key, maskNA, now)
	if err != nil {
		wipe(secret)
		return nil, errors.Join(err, m.markRevokedLocked([]*invitationRecord{r}))
	}
	if err := m.ensureKeyNotAfterLocked(key, maskNA); err != nil {
		wipe(secret)
		return nil, errors.Join(err, m.markRevokedLocked([]*invitationRecord{r}))
	}
	next := &invitationRecord{
		PassID: pid, Secret: secret, KeyID: key.ID, Slot: key.Slot,
		Created: r.Created, LinkExpires: r.LinkExpires, AccessExpires: r.AccessExpires,
		AccessSecs: r.AccessSecs, CardNotAfter: int64(card.Env.NotAfter),
		MaxEntries: r.MaxEntries, Entries: r.Entries,
		Node: r.Node, Device: r.Device, Card: append([]byte(nil), ic.Envelope...),
	}
	if err := m.saveInvitation(next); err != nil {
		wipe(secret)
		return nil, errors.Join(fmt.Errorf("%w: save invitation: %v", ErrState, err), m.markRevokedLocked([]*invitationRecord{r}))
	}
	if err := m.deleteInvitation(r.PassID); err != nil {
		// The new record is written; leaving the old one would keep the old link alive. Delete the
		// new record and report, so the invitation is lost rather than duplicated.
		_ = m.deleteInvitation(pid)
		wipe(secret)
		return nil, errors.Join(fmt.Errorf("%w: delete old invitation: %v", ErrState, err), m.markRevokedLocked([]*invitationRecord{r}))
	}
	delete(m.invitations, r.PassID)
	m.invitations[pid] = next
	link, err := objects.EncodeLink(ic.Envelope, secret)
	if err != nil {
		return nil, fmt.Errorf("%w: link: %v", ErrState, err)
	}
	return &Minted{
		PassID: pid, Secret: append([]byte(nil), secret...), Card: append([]byte(nil), ic.Envelope...),
		Link: link, LinkExpires: time.Unix(next.LinkExpires, 0), MaxEntries: next.MaxEntries,
		MaskKey: append(keys.AKey(nil), m.publicOf(key.ID)...), Listener: int(key.Slot),
	}, nil
}

// RotateKeys answers a possible masking-key theft (D11): it creates a new contacts key (the old one
// stays accepted during the overlap) and reissues every unredeemed invitation on a fresh key.
func (m *Manager) RotateKeys(ctx context.Context) (*Rotation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	now := m.clk.Now()

	// Snapshot the invitation-key groups before touching the key map. Expired invitations are
	// included too: after D11 they must get a fresh key so an extension keeps working.
	groups := make(map[[8]byte][]*invitationRecord)
	for id := range m.keys {
		r := m.keys[id]
		if r.Kind != keyInvitation {
			continue
		}
		holders := m.holdersLocked(id)
		if len(holders) == 0 {
			continue
		}
		groups[id] = holders
	}

	next, old, err := m.rotateContactsLocked(now)
	if err != nil {
		return nil, err
	}
	rot := m.rotationOf(next, old)

	for keyID, invites := range groups {
		slot := m.keys[keyID].Slot
		if err := m.removeKeyLocked(keyID); err != nil {
			// The key is already gone from memory; the passes that named it must not admit.
			return nil, errors.Join(err, m.markRevokedLocked(invites))
		}
		fresh, err := m.newKeyLocked(keyInvitation, slot, now, 0, 0, 0)
		if err != nil {
			return nil, errors.Join(err, m.markRevokedLocked(invites))
		}
		sort.Slice(invites, func(i, j int) bool {
			return bytesCompare(invites[i].PassID[:], invites[j].PassID[:]) < 0
		})
		for i, inv := range invites {
			min, err := m.reissueInvitationLocked(ctx, inv, fresh, now)
			if err != nil {
				// The old key is gone; fail closed for everything not yet moved.
				return nil, errors.Join(err, m.markRevokedLocked(invites[i:]))
			}
			rot.Reissued = append(rot.Reissued, *min)
		}
	}
	return rot, nil
}

// List returns every pass, newest last. Secrets are included for the owner to resend links.
func (m *Manager) List(ctx context.Context) ([]Info, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil, ErrClosed
	}
	now := m.clk.Now()
	out := make([]Info, 0, len(m.invitations)+len(m.personal))
	for _, r := range m.invitations {
		out = append(out, m.infoInvitationLocked(r, now))
	}
	for _, r := range m.personal {
		out = append(out, m.infoPersonalLocked(r, now))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Created.Equal(out[j].Created) {
			return bytesCompare(out[i].ID[:], out[j].ID[:]) < 0
		}
		return out[i].Created.Before(out[j].Created)
	})
	return out, nil
}

// MaskKeys returns the masking keys L0 must accept now: the current contacts key, previous ones
// during the overlap, and every invitation key that still has an unredeemed invitation.
func (m *Manager) MaskKeys() []KeyInfo {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil
	}
	now := m.clk.Now()
	out := make([]KeyInfo, 0, len(m.keys))
	for id, r := range m.keys {
		k := KeyInfo{
			Public: append(keys.AKey(nil), m.publicOf(id)...), Contacts: r.Kind == keyContacts,
			Slot: -1, Version: r.Version,
		}
		if r.NotAfter != 0 {
			k.NotAfter = time.Unix(r.NotAfter, 0)
		}
		if r.RetireAfter != 0 {
			k.RetireAfter = time.Unix(r.RetireAfter, 0)
		}
		if r.Kind == keyContacts {
			if r.NotAfter != 0 && unix(now) >= r.NotAfter {
				continue
			}
			if r.RetireAfter != 0 && unix(now) >= r.RetireAfter {
				continue
			}
			out = append(out, k)
			continue
		}
		refs := m.refsLocked(id, now)
		if len(refs) == 0 {
			continue
		}
		if r.NotAfter != 0 && unix(now) >= r.NotAfter {
			continue
		}
		k.Slot = int(r.Slot)
		k.Invitations = append([]PassID(nil), refs...)
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Contacts != out[j].Contacts {
			return out[i].Contacts
		}
		if out[i].Version != out[j].Version {
			return out[i].Version > out[j].Version
		}
		return bytesCompare(out[i].Public, out[j].Public) < 0
	})
	return out
}

// Listener returns the configured invitation listener of a slot. Its Mask is not meaningful: the
// runtime installs the live mask keys from MaskKeys (T3.6b). The returned value is a copy.
func (m *Manager) Listener(slot int) (cards.Listener, bool) {
	if m == nil {
		return cards.Listener{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed || slot < 0 || slot >= len(m.listeners) {
		return cards.Listener{}, false
	}
	l := m.listeners[slot]
	l.Mask = append(keys.AKey(nil), l.Mask...)
	return l, true
}

// ContactsKeys returns the contacts masking keys: the current one first, then the previous keys
// still accepted during the overlap.
func (m *Manager) ContactsKeys() []KeyInfo {
	all := m.MaskKeys()
	out := make([]KeyInfo, 0, 2)
	for _, k := range all {
		if k.Contacts {
			out = append(out, k)
		}
	}
	return out
}

// UseMaskKey runs fn with the private key of pub. Callers must not retain the bytes.
func (m *Manager) UseMaskKey(pub keys.AKey, fn func([]byte) error) error {
	if m == nil {
		return ErrNotFound
	}
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return ErrClosed
	}
	var kp *identity.KeyPair
	for id, r := range m.keys {
		_ = r
		if bytesEqual(m.publicOf(id), pub) {
			kp = m.live[id]
			break
		}
	}
	m.mu.RUnlock()
	if kp == nil {
		return ErrNotFound
	}
	return kp.WithPrivate(fn)
}

// info helpers ---------------------------------------------------------------------------------

// rotationOf describes the contacts key change; the previous key is omitted when the overlap
// already ended (no contacts or the grace period passed).
func (m *Manager) rotationOf(next, old *keyRecord) *Rotation {
	rot := &Rotation{}
	if next != nil {
		rot.ContactsKey = append(keys.AKey(nil), m.publicOf(next.ID)...)
	}
	if old != nil && m.keys[old.ID] != nil {
		rot.PreviousKey = append(keys.AKey(nil), m.publicOf(old.ID)...)
		rot.PreviousRetireAfter = time.Unix(old.RetireAfter, 0)
	}
	return rot
}

func (m *Manager) infoInvitationLocked(r *invitationRecord, now time.Time) Info {
	in := Info{
		ID: r.PassID, Kind: Invitation, Secret: append([]byte(nil), r.Secret...),
		Created: time.Unix(r.Created, 0), LinkExpires: time.Unix(r.LinkExpires, 0),
		MaxEntries: r.MaxEntries, Entries: r.Entries, Contact: r.Node, Device: r.Device,
		Card: append([]byte(nil), r.Card...),
	}
	if r.AccessExpires != 0 {
		in.AccessExpires = time.Unix(r.AccessExpires, 0)
	}
	if r.KeyID != ([8]byte{}) {
		in.MaskKey = append(keys.AKey(nil), m.publicOf(r.KeyID)...)
	}
	if len(r.Secret) == secretLen && len(r.Card) > 0 {
		in.Link, _ = objects.EncodeLink(r.Card, r.Secret)
	}
	switch {
	case r.Revoked:
		in.State = Revoked
	case r.Entries >= r.MaxEntries:
		in.State = Used
	case unix(now) >= r.LinkExpires, r.AccessExpires != 0 && unix(now) >= r.AccessExpires:
		in.State = Expired
	default:
		in.State = Active
	}
	return in
}

func (m *Manager) infoPersonalLocked(r *personalRecord, now time.Time) Info {
	in := Info{
		ID: r.PassID, Kind: Personal, Secret: append([]byte(nil), r.Secret...),
		Created: time.Unix(r.Created, 0), AccessExpires: time.Unix(r.Expires, 0),
		Contact: r.Node, Device: r.Device,
	}
	if r.Revoked {
		in.State = Revoked
	} else if unix(now) >= r.Expires {
		in.State = Expired
	} else {
		in.State = Active
	}
	return in
}

// capped returns now+ttl, never beyond one year from now (handshake.md §5).
func (m *Manager) capped(now time.Time, ttl time.Duration) int64 {
	if ttl > MaxPersonalTTL {
		ttl = MaxPersonalTTL
	}
	return unix(now) + int64(ttl/time.Second)
}

func laterUnix(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func validateListener(l cards.Listener) error {
	switch l.Suite {
	case objects.SuiteNoise3, objects.SuiteReality2QUIC, objects.SuiteReality2TCP:
		if l.Host == "" || l.Port == 0 || l.Port > 65535 {
			return fmt.Errorf("%w: host and port required", ErrConfig)
		}
		if len(l.Host) > 253 || len(l.SNI) > 253 {
			return fmt.Errorf("%w: host or sni too long", ErrConfig)
		}
	case objects.SuiteWS2:
		if l.URL == "" || len(l.URL) > 512 {
			return fmt.Errorf("%w: ws2 listener needs an url", ErrConfig)
		}
	default:
		return fmt.Errorf("%w: suite %d", ErrConfig, l.Suite)
	}
	return nil
}

func validateInviteCard(env []byte, pid PassID, pub keys.AKey, now time.Time) (*objects.Card, error) {
	c, err := objects.VerifyCard(env, now)
	if err != nil {
		return nil, fmt.Errorf("%w: invitation card: %v", ErrState, err)
	}
	if c.Scope != objects.ScopeInvite {
		return nil, fmt.Errorf("%w: invitation card scope %v", ErrState, c.Scope)
	}
	if !bytesEqual(c.Body.GetInviteId(), pid[:]) {
		return nil, fmt.Errorf("%w: invitation card names another pass", ErrState)
	}
	if len(c.Body.GetAddrs()) == 0 || len(c.Body.GetMaskKeys()) != 1 {
		return nil, fmt.Errorf("%w: invitation card carries %d addrs and %d keys",
			ErrState, len(c.Body.GetAddrs()), len(c.Body.GetMaskKeys()))
	}
	if !bytesEqual(c.Body.GetMaskKeys()[0].GetKey(), pub) {
		return nil, fmt.Errorf("%w: invitation card uses another masking key", ErrState)
	}
	for _, a := range c.Body.GetAddrs() {
		if a.GetMask() != 1 {
			return nil, fmt.Errorf("%w: invitation address uses mask %d", ErrState, a.GetMask())
		}
	}
	return c, nil
}

func cloneListeners(in []cards.Listener) []cards.Listener {
	out := make([]cards.Listener, len(in))
	for i, l := range in {
		out[i] = l
		out[i].Mask = append(keys.AKey(nil), l.Mask...)
	}
	return out
}

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func bytesCompare(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }
