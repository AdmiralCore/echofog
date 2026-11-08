package cards

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// Visibility is how the node presents itself on the network.
type Visibility uint8

const (
	// Visible is an ordinary node with a public card in the DHT and PEX.
	Visible Visibility = iota
	// Hidden is a node that is not listed anywhere: it has no public card and marks every card
	// unlisted (wire.md §2.1, K4).
	Hidden
	// Anonymous differs from Hidden only in the node's own policy; cards are built the same way.
	Anonymous
)

func (v Visibility) String() string {
	switch v {
	case Visible:
		return "visible"
	case Hidden:
		return "hidden"
	case Anonymous:
		return "anonymous"
	}
	return "unknown"
}

// RepublishBefore is how long a card may still live before Current reissues it. objects.md §3.2:
// "republishes when less than 2 days remain or anything changes".
const RepublishBefore = 2 * 24 * time.Hour

// Card lifetime bounds (objects.md §3.2, D12).
const (
	cardLifetime   = objects.CardMaxLifetime * time.Second
	inviteLifetime = objects.InviteCardMaxLifetime * time.Second
)

// Processing limits of the card body (objects.md §3.1).
const (
	maxAddrs    = 16
	maxMaskKeys = 8
	maxServices = 16
	maxFeatures = 64
	maxShop     = 3
	maxRelease  = 32
	maxHost     = 253
	maxURL      = 512
	maxSNI      = 253
	inviteIDLen = 8
	// maskKeySlack is MaskKey.not_after − card not_after, at most 7 days (objects.md §3.2).
	maskKeySlack = 7 * 24 * time.Hour
)

// Clock is the node's corrected clock (AD13). *clock.Clock satisfies it.
type Clock interface {
	Now() time.Time
}

// KV is the small persistent key-value view of T4.3 the builder needs. The carrier memory uses
// the same shape; the package does not depend on the store.
type KV interface {
	// Get returns the value for key and ok=false when the key is absent.
	Get(ctx context.Context, key []byte) (value []byte, ok bool, err error)
	// Put stores value under key.
	Put(ctx context.Context, key, value []byte) error
}

// Device is the signing device: the card's envelope key. *identity.Device satisfies it.
type Device interface {
	Public() keys.AKey
	WithSigner(fn func(*keys.Signer) error) error
}

// ExitSource supplies the live exit offer of an exit operator (objects.md §4). It is called on
// every issue, so a price change becomes a new card.
type ExitSource interface {
	ExitOffer() *pb.ExitOffer
}

// Listener is one way the node accepts connections. Mask is the masking public key the address
// refers to; v2 suites require it (objects.md §3.2).
type Listener struct {
	// Suite is the L0 suite (registries.md §6.1): noise3, reality2-quic, reality2-tcp or ws2.
	Suite uint32
	// Host and Port are used by UDP and TCP suites; Host is an IP literal or a DNS name.
	Host string
	Port uint32
	// URL is the wss:// address of a ws2 listener; Host and Port stay empty then.
	URL string
	// SNI is the cover or inner SNI of reality2-* and ws2 (objects.md §3.1, SuiteParams).
	SNI string
	// Shape is the optional traffic-shape profile (wire.md §9).
	Shape *pb.ShapeProfile
	// Bootstrap marks an address that accepts XX handshakes (Addr.flags bit 0).
	Bootstrap bool
	// Mask is the algorithm-tagged X25519 masking public key (akey).
	Mask keys.AKey
	// FollowPublic makes Host follow the node's decided public IP (wire.md §7.2): the runtime
	// calls SetPublicHost when OBSERVED_ADDR peers agree on a new address, and the next Current
	// reissues the card. Host stays the placeholder until then.
	FollowPublic bool
	// PublicPort, when not zero, is the port the card announces instead of Port: the external
	// port a router mapped for a FollowPublic listener (SetPublicPort, T6.5b).
	PublicPort uint32
	// MaskNotAfter is the unix time the key stops being accepted. Zero means the card's not_after;
	// the builder caps the value at card not_after + 7 days.
	MaskNotAfter uint64
}

// Config is the card material of one node.
type Config struct {
	// Account is the algorithm-tagged Ed25519 account public key (akey).
	Account keys.AKey
	// Device signs every card; its device ID names the node in sessions.
	Device Device
	// HsKey is the current Noise static public key (algorithm-tagged X25519).
	HsKey keys.AKey
	// PrevHs is the previous handshake key during a rotation (handshake.md §7.1).
	PrevHs *pb.HsKey
	// L1Suites are the handshake suites the node accepts, in preference order.
	L1Suites []uint32
	// Role is registries.md §11 (user or server).
	Role uint32
	// Flags are the unmanaged card flags (relay, dht-server, mailbox, domestic-only). The builder
	// adds unlisted, invite-scoped and exit itself.
	Flags uint32
	// Features are the L2 feature ids.
	Features []uint32
	// Services are the L3 service offers.
	Services []*pb.ServiceOffer
	// Listeners become the card's addresses and masking keys.
	Listeners []Listener
	// Devices is the account-signed echofog/devices/2 envelope; required.
	Devices []byte
	// PrekeysHash is SHA-256 of the current prekeys/2 envelope; optional, 32 bytes.
	PrekeysHash []byte
	// Release is the build version, at most 32 bytes.
	Release string
	// Shop are links to the operator's page (at most 3, objects.md §3.5).
	Shop []string
	// Visibility selects whether a public card exists and whether cards are unlisted.
	Visibility Visibility
	// Exit supplies the live exit offer; nil for a node that is not an exit.
	Exit ExitSource
	// Clock is the injected clock; nil means time.Now.
	Clock Clock
	// KV keeps the issued-card memory; nil means an in-memory map (tests, ephemeral nodes).
	KV KV
}

// IssuedCard is one card the builder issued.
type IssuedCard struct {
	Scope    objects.Scope
	Seq      uint64
	NotAfter uint64
	Envelope []byte
}

// InviteOptions is the material of one invitation card.
type InviteOptions struct {
	// InviteID is the invitation's pass_id: exactly 8 bytes.
	InviteID []byte
	// Listeners are extra listeners used only by this invitation, for example the separate
	// noise3 masking key of an unredeemed invitation (wire.md §2.1).
	Listeners []Listener
	// OnlyThese replaces the node's listeners with Listeners instead of adding to them. An
	// invitation must carry only its own masking key and listener, never the contacts key or the
	// contacts listener (objects.md §10.3).
	OnlyThese bool
}

// Errors of the builder.
var (
	// ErrNoPublicCard reports a public card request from a node without a camouflaged listener or
	// from a hidden or anonymous node (objects.md §3.2, wire.md §2.1, D37).
	ErrNoPublicCard = errors.New("cards: node has no public card")
	// ErrInviteScope reports Current for the invitation scope: invitations are issued one by one.
	ErrInviteScope = errors.New("cards: invitation cards are issued per invitation")
	// ErrConfig reports invalid card material.
	ErrConfig = errors.New("cards: invalid configuration")
	// ErrMaskKeyExpired reports a masking key that already expired.
	ErrMaskKeyExpired = errors.New("cards: masking key expired")
	// ErrKV reports a store failure.
	ErrKV = errors.New("cards: state store")
)

type currentCard struct {
	fingerprint [32]byte
	envelope    []byte
	seq         uint64
	notAfter    uint64
}

// CardBuilder builds cards of all scopes and remembers what it issued.
type CardBuilder struct {
	mu   sync.Mutex
	cfg  Config
	clk  Clock
	kv   KV
	seq  uint64
	recs map[uint64]issuedRecord
	cur  map[objects.Scope]*currentCard
}

// New validates the configuration, loads the persisted issued-card memory and returns a builder.
func New(cfg Config) (*CardBuilder, error) {
	b := &CardBuilder{
		recs: make(map[uint64]issuedRecord),
		cur:  make(map[objects.Scope]*currentCard),
	}
	if err := b.applyConfig(cfg, true); err != nil {
		return nil, err
	}
	if err := b.load(context.Background()); err != nil {
		return nil, err
	}
	return b, nil
}

// Update replaces the card material after an event (a listener, service, shop, release or device
// list change). The next Current reissues the affected cards with a higher seq. Clock and KV stay
// as they were set by New.
func (b *CardBuilder) Update(cfg Config) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.applyConfig(cfg, false)
}

// SetExit changes the live exit offer: a source that returns an offer makes the cards carry the exit
// flag and the offer, nil takes both away. The next Current reissues the affected cards with a higher
// seq (the desktop turns its exit on and off while it runs).
func (b *CardBuilder) SetExit(src ExitSource) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if src != nil {
		if o := src.ExitOffer(); o != nil {
			if err := validateExitOffer(o); err != nil {
				return err
			}
		}
	}
	cfg := cloneConfig(b.cfg)
	cfg.Exit = src
	return b.applyConfig(cfg, false)
}

// ReplaceMask changes every listener that uses old as its masking key to next and returns the
// number of listeners changed. The runtime calls it when the contacts key rotates (T3.6b): the
// next Current reissues the affected cards with the new key.
func (b *CardBuilder) ReplaceMask(old, next keys.AKey) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(old) == 0 || len(next) == 0 {
		return 0, fmt.Errorf("%w: empty masking key", ErrConfig)
	}
	if bytes.Equal(old, next) {
		return 0, nil
	}
	cfg := cloneConfig(b.cfg)
	n := 0
	for i := range cfg.Listeners {
		if bytes.Equal(cfg.Listeners[i].Mask, old) {
			cfg.Listeners[i].Mask = clone(next)
			n++
		}
	}
	if n == 0 {
		return 0, nil
	}
	if err := b.applyConfig(cfg, false); err != nil {
		return 0, err
	}
	return n, nil
}

// SetPublicHost points every FollowPublic listener at host (the node's public IP) and returns the
// number of listeners changed. A changed host changes the card material, so the next Current
// reissues the cards with a higher seq (the address-change event of T3.4).
func (b *CardBuilder) SetPublicHost(host string) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if host == "" || len(host) > maxHost {
		return 0, fmt.Errorf("%w: public host of %d bytes", ErrConfig, len(host))
	}
	cfg := cloneConfig(b.cfg)
	n := 0
	for i := range cfg.Listeners {
		if cfg.Listeners[i].FollowPublic && cfg.Listeners[i].Host != host {
			cfg.Listeners[i].Host = host
			n++
		}
	}
	if n == 0 {
		return 0, nil
	}
	if err := b.applyConfig(cfg, false); err != nil {
		return 0, err
	}
	return n, nil
}

// SetPublicPort makes the FollowPublic listeners of one transport whose configured port is
// internal announce external instead (a port the router mapped, T6.5b); external 0 restores the
// configured port. It returns the number of listeners changed.
func (b *CardBuilder) SetPublicPort(udp bool, internal, external uint32) (int, error) {
	if external > 65535 {
		return 0, fmt.Errorf("%w: public port %d", ErrConfig, external)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	cfg := cloneConfig(b.cfg)
	n := 0
	for i := range cfg.Listeners {
		l := &cfg.Listeners[i]
		if !l.FollowPublic || l.Port != internal || l.PublicPort == external {
			continue
		}
		switch l.Suite {
		case objects.SuiteNoise3, objects.SuiteReality2QUIC:
			if !udp {
				continue
			}
		case objects.SuiteReality2TCP:
			if udp {
				continue
			}
		default:
			continue
		}
		l.PublicPort = external
		n++
	}
	if n == 0 {
		return 0, nil
	}
	if err := b.applyConfig(cfg, false); err != nil {
		return 0, err
	}
	return n, nil
}

// ClearFlags removes caller-chosen card flags (FlagRelay on a node that may not relay, N6). A changed
// flag set reissues the cards with a higher seq on the next Current.
func (b *CardBuilder) ClearFlags(flags uint32) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cfg.Flags&flags == 0 {
		return nil
	}
	cfg := cloneConfig(b.cfg)
	cfg.Flags &^= flags
	return b.applyConfig(cfg, false)
}

// Visibility returns the configured visibility of the node (objects.md §3.2, wire.md §2.1).
func (b *CardBuilder) Visibility() Visibility {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cfg.Visibility
}

// Issue builds, signs and records a fresh card of the scope.
func (b *CardBuilder) Issue(ctx context.Context, scope objects.Scope) (*IssuedCard, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.issueLocked(ctx, scope, nil, nil, false)
}

// IssueInvite builds an invitation card. The card is recorded for known_card_seq but is not a
// current card: it travels in the invitation link (objects.md §10.3). With OnlyThese the card
// carries exactly opt.Listeners and no listener of the node itself.
func (b *CardBuilder) IssueInvite(ctx context.Context, opt InviteOptions) (*IssuedCard, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(opt.InviteID) != inviteIDLen {
		return nil, fmt.Errorf("%w: invite_id is %d bytes, want %d", ErrConfig, len(opt.InviteID), inviteIDLen)
	}
	if opt.OnlyThese && len(opt.Listeners) == 0 {
		return nil, fmt.Errorf("%w: an invitation needs a listener", ErrConfig)
	}
	return b.issueLocked(ctx, objects.ScopeInvite, opt.InviteID, opt.Listeners, opt.OnlyThese)
}

// Current returns the current card of the scope. It reissues when the card has less than
// RepublishBefore left, expired, or the material changed.
func (b *CardBuilder) Current(ctx context.Context, scope objects.Scope) (*IssuedCard, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.currentLocked(ctx, scope)
}

// OwnCards returns the handshake's view of the node's cards: the current public (for a visible
// node) and peer cards plus the seq and scope of every card issued in the last 30 days.
//
// Restoring the builder from the same KV therefore keeps P2 working for a peer that still names an
// older known_card_seq (handshake.md §7).
func (b *CardBuilder) OwnCards(ctx context.Context) (handshake.OwnCards, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := handshake.OwnCards{
		Current: make(map[uint32]handshake.OwnCard),
		Issued:  make(map[uint64]handshake.IssuedCard),
	}
	if b.cfg.Visibility == Visible {
		c, err := b.currentLocked(ctx, objects.ScopePublic)
		switch {
		case errors.Is(err, ErrNoPublicCard):
			// D37: a visible node without a camouflaged listener has no public card. It still
			// answers handshakes with its peer card, so omit the public entry instead of
			// failing the whole handshake material.
		case err != nil:
			return handshake.OwnCards{}, err
		default:
			out.Current[uint32(objects.ScopePublic)] = handshake.OwnCard{Seq: c.Seq, Envelope: c.Envelope}
		}
	}
	peer, err := b.currentLocked(ctx, objects.ScopePeer)
	if err != nil {
		return handshake.OwnCards{}, err
	}
	out.Current[uint32(objects.ScopePeer)] = handshake.OwnCard{Seq: peer.Seq, Envelope: peer.Envelope}

	b.pruneLocked(b.clk.Now())
	for seq, rec := range b.recs {
		var at time.Time
		if rec.At != 0 {
			at = time.Unix(rec.At, 0).UTC()
		}
		out.Issued[seq] = handshake.IssuedCard{Scope: uint32(rec.Scope), At: at}
	}
	return out, nil
}

func (b *CardBuilder) currentLocked(ctx context.Context, scope objects.Scope) (*IssuedCard, error) {
	switch scope {
	case objects.ScopePublic, objects.ScopePeer:
	case objects.ScopeInvite:
		return nil, ErrInviteScope
	default:
		return nil, fmt.Errorf("%w: unknown scope %d", ErrConfig, scope)
	}
	if scope == objects.ScopePublic && b.cfg.Visibility != Visible {
		return nil, ErrNoPublicCard
	}

	now := b.clk.Now()
	fp, err := b.fingerprintLocked()
	if err != nil {
		return nil, err
	}
	if rec := b.cur[scope]; rec != nil &&
		rec.fingerprint == fp &&
		rec.notAfter > unix(now)+uint64(RepublishBefore/time.Second) {
		return &IssuedCard{Scope: scope, Seq: rec.seq, NotAfter: rec.notAfter, Envelope: clone(rec.envelope)}, nil
	}
	return b.issueLocked(ctx, scope, nil, nil, false)
}

func (b *CardBuilder) issueLocked(ctx context.Context, scope objects.Scope, inviteID []byte, extra []Listener, onlyThese bool) (*IssuedCard, error) {
	switch scope {
	case objects.ScopePublic:
		if b.cfg.Visibility != Visible {
			return nil, ErrNoPublicCard
		}
	case objects.ScopePeer:
	case objects.ScopeInvite:
		if len(inviteID) != inviteIDLen {
			return nil, fmt.Errorf("%w: invite_id is %d bytes, want %d", ErrConfig, len(inviteID), inviteIDLen)
		}
	default:
		return nil, fmt.Errorf("%w: unknown scope %d", ErrConfig, scope)
	}

	now := b.clk.Now()
	nowUnix := unix(now)
	var lifetime time.Duration = cardLifetime
	if scope == objects.ScopeInvite {
		lifetime = inviteLifetime
	}
	notAfter := nowUnix + uint64(lifetime/time.Second)

	devicePub, err := parseDeviceKey(b.cfg.Device)
	if err != nil {
		return nil, err
	}
	if err := checkDeviceList(b.cfg.Devices, b.cfg.Account, devicePub, now); err != nil {
		return nil, err
	}

	listeners := make([]Listener, 0, len(b.cfg.Listeners)+len(extra))
	if len(extra) > 0 {
		if onlyThese {
			if err := validateExtraListeners(nil, extra); err != nil {
				return nil, err
			}
		} else if err := validateExtraListeners(b.cfg.Listeners, extra); err != nil {
			return nil, err
		}
	}
	if onlyThese {
		listeners = append(listeners, cloneListeners(extra)...)
	} else {
		listeners = append(listeners, cloneListeners(b.cfg.Listeners)...)
		listeners = append(listeners, cloneListeners(extra)...)
	}

	var fp [32]byte
	if scope != objects.ScopeInvite {
		var err error
		if fp, err = b.fingerprintLocked(); err != nil {
			return nil, err
		}
	}

	maskNA := func(l Listener) (uint64, error) {
		na := l.MaskNotAfter
		if na == 0 {
			na = notAfter
		}
		if cap := notAfter + uint64(maskKeySlack/time.Second); na > cap {
			na = cap
		}
		if na <= nowUnix {
			return 0, fmt.Errorf("%w: masking key stops at %d", ErrMaskKeyExpired, na)
		}
		return na, nil
	}
	body, err := b.buildBodyLocked(scope, inviteID, listeners, scope == objects.ScopePublic, maskNA)
	if err != nil {
		return nil, err
	}
	if scope == objects.ScopePublic && len(body.GetAddrs()) == 0 {
		return nil, ErrNoPublicCard
	}

	seq := b.seq + 1
	if nowUnix > seq {
		seq = nowUnix
	}
	var envelope []byte
	err = b.cfg.Device.WithSigner(func(signer *keys.Signer) error {
		if !signer.Public().Equal(devicePub) {
			return fmt.Errorf("%w: device key changed while issuing", ErrConfig)
		}
		var err error
		envelope, err = objects.SealCard(signer, seq, notAfter, body)
		return err
	})
	if err != nil {
		return nil, err
	}

	// From here on the card exists and may have been written: never reuse its seq.
	b.seq = seq
	rec := issuedRecord{Scope: scope, At: timeToUnix(now)}
	b.pruneLocked(now)
	b.recs[seq] = rec
	if scope != objects.ScopeInvite {
		b.cur[scope] = &currentCard{fingerprint: fp, envelope: clone(envelope), seq: seq, notAfter: notAfter}
	}

	persistErr := b.persistLocked(ctx, scope, seq, envelope, fp)
	out := &IssuedCard{Scope: scope, Seq: seq, NotAfter: notAfter, Envelope: clone(envelope)}
	if persistErr != nil {
		return out, persistErr
	}
	return out, nil
}

func (b *CardBuilder) applyConfig(cfg Config, init bool) error {
	if init {
		if cfg.Clock != nil {
			b.clk = cfg.Clock
		}
		if b.clk == nil {
			b.clk = systemClock{}
		}
		if cfg.KV != nil {
			b.kv = cfg.KV
		}
		if b.kv == nil {
			b.kv = newMemoryKV()
		}
	}
	prepared := cloneConfig(cfg)
	prepared.Clock = nil
	prepared.KV = nil
	if err := prepareConfig(&prepared, b.clk.Now()); err != nil {
		return err
	}
	b.cfg = prepared
	return nil
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func unix(t time.Time) uint64 {
	s := t.Unix()
	if s < 0 {
		return 0
	}
	return uint64(s)
}

func timeToUnix(t time.Time) int64 { return int64(unix(t)) }

func clone(b []byte) []byte { return append([]byte(nil), b...) }

// SetPrekeysHash sets the SHA-256 of the current prekeys/2 envelope that cards carry (objects.md
// §3); nil clears it. A changed hash reissues the cards with a higher seq on the next Current.
func (b *CardBuilder) SetPrekeysHash(h []byte) error {
	if n := len(h); n != 0 && n != 32 {
		return fmt.Errorf("%w: prekeys_hash of %d bytes", ErrConfig, n)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if bytes.Equal(b.cfg.PrekeysHash, h) {
		return nil
	}
	cfg := cloneConfig(b.cfg)
	cfg.PrekeysHash = clone(h)
	return b.applyConfig(cfg, false)
}
