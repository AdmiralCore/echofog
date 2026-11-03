package handshake

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/v2/noise"
)

// InitiatorConfig configures an initiator.
type InitiatorConfig struct {
	Static *ecdh.PrivateKey // own hs key
	Card   []byte           // own card for P3 (a throwaway card for bare-address XX)
	// Remote is the verified card of the responder. With it the initiator runs XK (or XX if the
	// card lists only XX); without it, XX.
	Remote *CardInfo
	// ExpectedDeviceID is the responder device ID for XX without a card (bootstrap lists).
	ExpectedDeviceID *[32]byte
	Suites           []uint16 // supported, preference order; default XK, XX
	Features         []uint32
	Services         []ServiceOffer
	AccessSecret     []byte // 16-byte pass secret for a hidden responder (XK only)
	Early            []byte // frames of one 0-RTT stream (XK, feature early-data), ≤ 1200 bytes
	Now              func() time.Time
	Offset           time.Duration
	Grease           bool // add one GREASE value to offered, features and services (§3.4)
	Cards            CardVerifier
	Bindings         KeyBinder
	Rand             io.Reader
}

// Initiator runs the initiator side on one stream 0.
type Initiator struct {
	cfg       InitiatorConfig
	cb        []byte
	rid       []byte
	offered   []uint16 // fixed for the whole connection attempt (D4)
	chosen    uint16
	hs        *noise.HandshakeState
	retries   int // HS_RETRY answered
	suiteRetr int // REJECT{suite} answered
	cookie    []byte
	nonce     uint64
	started   bool
	init      *HsInit

	greaseSuite, greaseFeature, greaseService uint16

	// test hooks
	chosenOverride uint16
	timeOverride   uint64
	protoOverride  uint32
	forceAccess    *Access
	lastInit       *HsInit
	lastResp       *HsResp
}

// NewInitiator prepares an initiator for a carrier with channel binding cb.
func NewInitiator(cfg InitiatorConfig, cb []byte) (*Initiator, error) {
	if cfg.Static == nil || len(cfg.Card) == 0 || len(cb) != 32 {
		return nil, fmt.Errorf("%w: static key, own card and cb are required", ErrConfig)
	}
	if cfg.Cards == nil {
		cfg.Cards = DefaultCardVerifier{}
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Reader
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if len(cfg.Suites) == 0 {
		cfg.Suites = DefaultSuites
	}
	i := &Initiator{cfg: cfg, cb: clone(cb)}
	if cfg.Remote != nil {
		for _, s := range cfg.Suites {
			if _, ok := SuiteByID(s); ok && slices.Contains(cfg.Remote.L1Suites, s) {
				i.offered = append(i.offered, s)
			}
		}
		if len(i.offered) == 0 {
			return nil, fmt.Errorf("%w: no common suite with the card", ErrConfig)
		}
		i.rid = cfg.Remote.DeviceID[:]
	} else {
		i.offered = []uint16{SuiteXX}
		if cfg.ExpectedDeviceID != nil {
			i.rid = cfg.ExpectedDeviceID[:]
		}
	}
	i.chosen = i.offered[0]
	if i.chosen != SuiteXK && (cfg.AccessSecret != nil || cfg.Early != nil) {
		return nil, fmt.Errorf("%w: access and early data are XK only", ErrConfig)
	}
	if len(cfg.Early) > MaxEarlyLen || (cfg.Early != nil && !slices.Contains(cfg.Features, FeatureEarlyData)) {
		return nil, fmt.Errorf("%w: early data needs feature early-data and ≤ 1200 bytes", ErrConfig)
	}
	if cfg.AccessSecret != nil && len(cfg.AccessSecret) != 16 {
		return nil, fmt.Errorf("%w: access secret must be 16 bytes", ErrConfig)
	}
	if cfg.Grease {
		var b [3]byte
		if _, err := io.ReadFull(cfg.Rand, b[:]); err != nil {
			return nil, err
		}
		g := func(x byte) uint16 { n := uint16(x%15)<<4 | 0x0a; return n<<8 | n }
		i.greaseSuite, i.greaseFeature, i.greaseService = g(b[0]), g(b[1]), g(b[2])
		if len(i.offered) < 8 {
			i.offered = insertSorted(i.offered, i.greaseSuite)
		}
	}
	return i, nil
}

// insertSorted puts g before the first element greater than it, keeping the others in order.
func insertSorted(list []uint16, g uint16) []uint16 {
	out := make([]uint16, 0, len(list)+1)
	done := false
	for _, x := range list {
		if !done && x > g {
			out = append(out, g)
			done = true
		}
		out = append(out, x)
	}
	if !done {
		out = append(out, g)
	}
	return out
}

func (i *Initiator) now() time.Time { return i.cfg.Now().Add(i.cfg.Offset) }

// Start returns HS_HELLO and message 1, sent in one flight on stream 0.
func (i *Initiator) Start() (hello, msg1 []byte, err error) {
	i.started = true
	return i.flight()
}

func (i *Initiator) flight() ([]byte, []byte, error) {
	chosen := i.chosen
	if i.chosenOverride != 0 {
		chosen = i.chosenOverride
	}
	h := &Hello{Offered: i.offered, Chosen: chosen}
	if i.rid != nil {
		h.Flags |= FlagResponderID
	}
	if i.cookie != nil {
		h.Flags |= FlagPuzzle
		h.Cookie, h.Nonce = i.cookie, i.nonce
	}
	hello, err := h.Marshal()
	if err != nil {
		return nil, nil, err
	}
	suite, _ := SuiteByID(chosen)
	nc := noise.Config{Pattern: suite.Pattern, Initiator: true, Prologue: Prologue(i.cb, i.rid, hello),
		Static: i.cfg.Static, Rand: i.cfg.Rand}
	if suite.Pattern.PreResponderStatic {
		if i.cfg.Remote == nil {
			return nil, nil, ErrConfig
		}
		nc.RemoteStatic = i.cfg.Remote.HsKey // initiators always use hs_key (§7.1)
	}
	hs, err := noise.NewHandshake(nc)
	if err != nil {
		return nil, nil, err
	}
	p1 := i.buildP1(chosen)
	msg1, err := hs.WriteMessageFunc(nil, func(h1 []byte) ([]byte, error) {
		if chosen == SuiteXK && i.cfg.AccessSecret != nil {
			p1.Access = &Access{PassID: PassID(i.cfg.AccessSecret), Proof: AccessProof(i.cfg.AccessSecret, h1)}
		}
		if i.forceAccess != nil {
			p1.Access = i.forceAccess
		}
		return p1.Marshal(), nil
	})
	if err != nil {
		return nil, nil, err
	}
	i.hs, i.init, i.lastInit = hs, p1, p1
	return hello, msg1, nil
}

func (i *Initiator) buildP1(suite uint16) *HsInit {
	p := &HsInit{ProtoVer: ProtoVer, Time: uint64(i.now().Unix())}
	if i.protoOverride != 0 {
		p.ProtoVer = i.protoOverride
	}
	if i.timeOverride != 0 {
		p.Time = i.timeOverride
	}
	p.Features = sortedNoGrease(i.cfg.Features)
	services := slices.Clone(i.cfg.Services)
	if i.cfg.Grease {
		p.Features = append(p.Features, uint32(i.greaseFeature))
		slices.Sort(p.Features)
		services = append(services, ServiceOffer{ID: uint32(i.greaseService), Versions: []uint32{1}})
	}
	slices.SortFunc(services, func(a, b ServiceOffer) int { return int(a.ID) - int(b.ID) })
	p.Services = services
	if suite == SuiteXK {
		if i.cfg.Remote != nil {
			p.KnownCardSeq = i.cfg.Remote.Seq
		}
		p.Early = i.cfg.Early
	}
	return p
}

// HandleRetry answers HS_RETRY: it solves the puzzle and returns a new flight with fresh
// ephemerals. A difficulty above 24 is refused.
func (i *Initiator) HandleRetry(payload []byte) (hello, msg1 []byte, err error) {
	if !i.started {
		return nil, nil, ErrState
	}
	r, err := ParseRetry(payload)
	if err != nil {
		return nil, nil, err
	}
	if i.retries >= 2 {
		return nil, nil, ErrRetries
	}
	i.retries++
	n, err := SolvePuzzle(r.Cookie[:], i.cb, r.Difficulty)
	if err != nil {
		return nil, nil, err
	}
	i.cookie, i.nonce = clone(r.Cookie[:]), n
	return i.flight()
}

// HandleReject answers a REJECT frame received before message 2. For reason suite it retries
// once with the same offered list and the best suite of the intersection (§3.1, D4); any other
// reason ends the handshake with the Reject as error.
func (i *Initiator) HandleReject(payload []byte) (hello, msg1 []byte, err error) {
	if !i.started {
		return nil, nil, ErrState
	}
	r, err := ParseReject(payload)
	if err != nil {
		return nil, nil, err
	}
	if r.Reason != ReasonSuite {
		return nil, nil, r
	}
	if i.suiteRetr >= 1 {
		return nil, nil, ErrRetries
	}
	i.suiteRetr++
	theirs := map[uint16]bool{}
	for _, f := range strings.Split(r.Detail, ",") {
		if len(f) != 4 {
			return nil, nil, fmt.Errorf("%w: suite list %q", ErrPayload, r.Detail)
		}
		v, err := strconv.ParseUint(f, 16, 16)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: suite list %q", ErrPayload, r.Detail)
		}
		theirs[uint16(v)] = true
	}
	for _, s := range i.offered { // our preference order; the offered list is never narrowed
		if !IsGrease(s) && theirs[s] {
			i.chosen, i.chosenOverride = s, 0
			if s != SuiteXK && (i.cfg.AccessSecret != nil || i.cfg.Early != nil) {
				return nil, nil, fmt.Errorf("%w: hidden responder offers no XK", ErrConfig)
			}
			return i.flight()
		}
	}
	return nil, nil, r
}

// HandleMsg2 processes message 2 and returns message 3 with the session. On any error the
// initiator sends nothing more: its identity is never revealed to an unverified responder.
func (i *Initiator) HandleMsg2(msg2 []byte) (msg3 []byte, s *Session, err error) {
	hs := i.hs
	i.hs = nil
	if hs == nil {
		return nil, nil, ErrState
	}
	p2b, err := hs.ReadMessage(msg2)
	if err != nil {
		return nil, nil, err
	}
	p2, err := ParseHsResp(p2b)
	if err != nil {
		return nil, nil, err
	}
	i.lastResp = p2
	if p2.Reject != nil {
		return nil, nil, p2.Reject
	}
	if p2.ProtoVer < ProtoVer {
		return nil, nil, ErrVersion
	}
	if absDiff(int64(p2.Time), i.now().Unix()) > MaxClockSkew {
		return nil, nil, ErrClockSkew
	}
	for _, f := range p2.Features {
		if IsGrease32(f) || !slices.Contains(i.init.Features, f) {
			return nil, nil, fmt.Errorf("%w: feature %d", ErrSelection, f)
		}
	}
	for _, c := range p2.Services {
		ok := false
		for _, o := range i.init.Services {
			if o.ID == c.ID && !IsGrease32(o.ID) && slices.Contains(o.Versions, c.Version) {
				ok = true
			}
		}
		if !ok {
			return nil, nil, fmt.Errorf("%w: service %d version %d", ErrSelection, c.ID, c.Version)
		}
	}
	// The responder's card: required in XX, optional (newer) in XK.
	peer := i.cfg.Remote
	suite, _ := SuiteByID(i.chosen)
	if i.chosenOverride != 0 {
		suite, _ = SuiteByID(i.chosenOverride)
	}
	if p2.Card != nil || suite.ID == SuiteXX {
		if p2.Card == nil {
			return nil, nil, fmt.Errorf("%w: no card in XX", ErrPeerCard)
		}
		info, err := i.cfg.Cards.VerifyCard(p2.Card, i.now())
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %v", ErrPeerCard, err)
		}
		if !bytes.Equal(info.HsKey, hs.RemoteStatic()) {
			return nil, nil, fmt.Errorf("%w: hs_key is not the Noise static key", ErrPeerCard)
		}
		if i.rid != nil && !bytes.Equal(info.DeviceID[:], i.rid) {
			return nil, nil, fmt.Errorf("%w: unexpected device ID", ErrPeerCard)
		}
		if err := bindCard(i.cfg.Bindings, info); err != nil {
			return nil, nil, err
		}
		peer = info
	}
	msg3, err = hs.WriteMessage(nil, (&HsFinish{Card: i.cfg.Card}).Marshal())
	if err != nil {
		return nil, nil, err
	}
	s, err = newSession(hs, suite.ID, true)
	if err != nil {
		return nil, nil, err
	}
	s.Peer = peer
	s.Features, s.Services = p2.Features, p2.Services
	s.PeerProtoVer, s.PeerTime = p2.ProtoVer, p2.Time
	s.Observed = p2.Observed
	s.EarlyStatus, s.EarlyResponse = p2.EarlyStatus, p2.Early
	s.Unverified = suite.ID == SuiteXX && i.rid == nil
	s.ResponderCardSent = p2.Card != nil
	return msg3, s, nil
}
