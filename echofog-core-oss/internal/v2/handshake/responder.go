package handshake

import (
	"crypto/ecdh"
	"crypto/mlkem"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/echofog/echofog-core/internal/v2/noise"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// PassStore finds invitation and personal passes by pass_id (handshake.md §5).
type PassStore interface {
	// LookupPass returns a pass that exists, is not revoked and not expired.
	LookupPass(passID []byte, now time.Time) (Pass, bool)
}

// Pass is an invitation pass or a personal pass (handshake.md §5).
type Pass struct {
	Secret   []byte // 16 bytes
	Personal bool   // a personal pass of a contact (false: an invitation pass)
}

// AccessInfo names the pass that admitted a hidden-node session, so the caller can redeem an
// invitation or renew the contact's personal pass (handshake.md §5, objects.md §10.3). It is set
// on the responder side only; the initiator already knows its own secret.
type AccessInfo struct {
	PassID   [8]byte
	Personal bool
}

// EarlyHandler processes the frames of the 0-RTT stream. It returns ok = false when the request
// is not a 0-RTT safe operation (registries.md §3); the responder then reports early_status 2.
type EarlyHandler func(early []byte) (response []byte, ok bool)

// PuzzleConfig enables HS_RETRY on a public responder (handshake.md §6).
type PuzzleConfig struct {
	Secret     []byte    // R, 32 bytes, rotated daily
	PrevSecret []byte    // the previous R, accepted during the minute after rotation
	RotatedAt  time.Time // when Secret replaced PrevSecret
	// Difficulty returns the current required difficulty for this source; 0 = no puzzle.
	Difficulty func() uint8
}

// ResponderConfig configures a responder.
type ResponderConfig struct {
	Static     *ecdh.PrivateKey // current hs key
	PrevStatic *ecdh.PrivateKey // previous hs key during a rotation (§7.1), or nil
	DeviceID   [32]byte         // own device ID (rid)
	OwnCards   OwnCards         // own cards, for P2
	Suites     []uint16         // supported, preference order; default XK, XX (hidden: XK only)
	Hidden     bool
	Passes     PassStore
	Features   []uint32
	Services   []ServiceOffer // served services and their versions
	Now        func() time.Time
	Offset     time.Duration // learned clock offset, added to Now
	Puzzle     *PuzzleConfig
	Replay     *ReplayCache
	Early      EarlyHandler
	Observed   []byte // the initiator's address as seen (feature observed-addr)
	Cards      CardVerifier
	Bindings   KeyBinder
	Rand       io.Reader

	// testEphemeral and testEncap fix the responder's randomness (vectors only).
	testEphemeral *ecdh.PrivateKey
	testEncap     func(*mlkem.EncapsulationKey768) ([]byte, []byte, error)
}

// OwnCard is one card the node issued.
type OwnCard struct {
	Seq      uint64
	Envelope []byte
}

// OwnCards are the responder's cards for P2 (handshake.md §7, "Card in P2").
type OwnCards struct {
	// Current holds the current card per scope (0 public, 1 peer). XX sends the public card.
	Current map[uint32]OwnCard
	// Issued maps the seq of every card the node issued to its scope and issue time, so
	// known_card_seq names the card the initiator holds. Entries older than 30 days (the longest
	// card lifetime) are ignored; the node may prune them.
	Issued map[uint64]IssuedCard
}

// IssuedCard records one issued card. A zero At never ages out.
type IssuedCard struct {
	Scope uint32
	At    time.Time
}

// IssuedMemory is how long a responder remembers the seq and scope of an issued card (B1).
const IssuedMemory = 30 * 24 * time.Hour

// Action tells the carrier what to do with a Reply.
type Action int

// Actions.
const (
	ActSend         Action = iota + 1 // send the frame; the handshake continues
	ActSendAndClose                   // send the frame, then close the stream
	ActClose                          // close the carrier without a message, now
	ActSilentClose                    // keep the carrier open, close it at CloseAt without a message
)

func (a Action) String() string {
	switch a {
	case ActSend:
		return "send"
	case ActSendAndClose:
		return "send-and-close"
	case ActClose:
		return "close"
	case ActSilentClose:
		return "silent-close"
	}
	return fmt.Sprintf("action(%d)", int(a))
}

// Reply is the responder's answer to HS_HELLO + message 1.
type Reply struct {
	Action    Action
	FrameType byte // FrameMsg, FrameRetry or FrameReject; 0 when nothing is sent
	Payload   []byte
	CloseAt   time.Time
	Err       error // why the handshake ended (for logs; never sent)
}

// Responder runs the responder side on one stream 0.
type Responder struct {
	cfg     ResponderConfig
	hs      *noise.HandshakeState
	suite   uint16
	pending *Session
	early   []byte
	hidden  bool
	access  *AccessInfo

	// test hooks
	timeOverride   uint64
	selectOverride func(*HsResp)
}

// NewResponder returns a responder for one stream.
func NewResponder(cfg ResponderConfig) *Responder {
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
	if cfg.Hidden {
		cfg.Suites = []uint16{SuiteXK} // D22: hidden nodes accept XK only
		cfg.Puzzle = nil               // puzzles are for public nodes
	}
	return &Responder{cfg: cfg, hidden: cfg.Hidden}
}

func (r *Responder) now() time.Time { return r.cfg.Now().Add(r.cfg.Offset) }

// uniformDelay draws uniform(0.5, 3) s.
func (r *Responder) uniformDelay() time.Duration {
	var b [8]byte
	if _, err := io.ReadFull(r.cfg.Rand, b[:]); err != nil {
		return 3 * time.Second
	}
	span := uint64(2500 * time.Millisecond)
	return 500*time.Millisecond + time.Duration(binary.BigEndian.Uint64(b[:])%(span+1))
}

// HandleFirst processes HS_HELLO and message 1, in the order of handshake.md §5. cb is the
// channel binding of this carrier; arrival is when message 1 arrived. It may be called again on
// the same stream after an HS_RETRY.
func (r *Responder) HandleFirst(cb, hello, msg1 []byte, arrival time.Time) Reply {
	r.hs, r.pending, r.early = nil, nil, nil
	r.access = nil
	var closeAt time.Time
	if r.hidden {
		// Drawn first, so the delay never depends on which check fails.
		closeAt = arrival.Add(r.uniformDelay())
	}
	fail := func(err error) Reply {
		r.hs = nil
		if r.hidden {
			return Reply{Action: ActSilentClose, CloseAt: closeAt, Err: err}
		}
		return Reply{Action: ActClose, Err: err}
	}
	if len(cb) != 32 {
		return fail(ErrConfig)
	}

	// 1. HS_HELLO and the suite rule.
	h, err := ParseHello(hello)
	if err != nil {
		return fail(err)
	}
	best := uint16(0)
	for _, s := range r.cfg.Suites {
		if containsSuite(h.Offered, s) {
			best = s
			break
		}
	}
	if best == 0 || h.Chosen != best {
		if r.hidden {
			return fail(errors.New("handshake: suite"))
		}
		rej := &Reject{Reason: ReasonSuite, Detail: suiteDetail(r.cfg.Suites)}
		return Reply{Action: ActSendAndClose, FrameType: FrameReject, Payload: rej.Marshal(), Err: rej}
	}
	suite, _ := SuiteByID(best)

	// 2. Puzzle, before any public-key operation.
	if p := r.cfg.Puzzle; p != nil && p.Difficulty != nil {
		if d := p.Difficulty(); d > 0 {
			if !r.puzzleSolved(h, cb, d, arrival) {
				var rt Retry
				copy(rt.Cookie[:], Cookie(p.Secret, cb, uint64(arrival.Unix())/60))
				rt.Difficulty = min(d, MaxDifficulty)
				return Reply{Action: ActSend, FrameType: FrameRetry, Payload: rt.Marshal()}
			}
		}
	}

	// 3. Message 1 tokens and P1.
	var rid []byte
	if h.Flags&FlagResponderID != 0 {
		rid = r.cfg.DeviceID[:]
	}
	prologue := Prologue(cb, rid, hello)
	keys := []*ecdh.PrivateKey{r.cfg.Static}
	if suite.Pattern.PreResponderStatic && r.cfg.PrevStatic != nil {
		keys = append(keys, r.cfg.PrevStatic)
	}
	var hs *noise.HandshakeState
	var p1b []byte
	for _, k := range keys {
		cand, err := noise.NewHandshake(noise.Config{Pattern: suite.Pattern, Prologue: prologue, Static: k,
			Rand: r.cfg.Rand, Ephemeral: r.cfg.testEphemeral, KEMEncapsulate: r.cfg.testEncap})
		if err != nil {
			return fail(err)
		}
		if p1b, err = cand.ReadMessage(msg1); err == nil {
			hs = cand
			break
		}
	}
	if hs == nil {
		return fail(errors.New("handshake: message 1 rejected"))
	}
	p1, err := ParseHsInit(p1b)
	if err != nil {
		return fail(err)
	}
	if best == SuiteXX && (p1.Access != nil || p1.Early != nil || p1.KnownCardSeq != 0) {
		return fail(fmt.Errorf("%w: access, known_card_seq or early data in XX", ErrPayload))
	}

	// 4. Hidden node: the access proof.
	personal := false
	if r.hidden {
		if p1.Access == nil || r.cfg.Passes == nil {
			return fail(errors.New("handshake: no access proof"))
		}
		pass, ok := r.cfg.Passes.LookupPass(p1.Access.PassID, r.now())
		if !ok || !ctEqual(AccessProof(pass.Secret, hs.HashBeforePayload()), p1.Access.Proof) {
			return fail(errors.New("handshake: access proof"))
		}
		personal = pass.Personal
		var pid [8]byte
		copy(pid[:], p1.Access.PassID)
		r.access = &AccessInfo{PassID: pid, Personal: personal}
	}

	// 5. Early data, P2, message 2.
	p2 := r.buildP2(p1, best, hs, personal)
	if best == SuiteXX && p2.Reject == nil && p2.Card == nil {
		return fail(fmt.Errorf("%w: XX needs a public card", ErrConfig))
	}
	if r.selectOverride != nil {
		r.selectOverride(p2)
	}
	m2, err := hs.WriteMessage(nil, p2.Marshal())
	if err != nil {
		return fail(err)
	}
	if p2.Reject != nil {
		r.hs = nil
		return Reply{Action: ActSendAndClose, FrameType: FrameMsg, Payload: m2, Err: p2.Reject}
	}
	r.hs, r.suite = hs, best
	r.pending = &Session{Features: p2.Features, Services: p2.Services, PeerProtoVer: p1.ProtoVer, PeerTime: p1.Time,
		EarlyRequest: r.early, ResponderCardSent: p2.Card != nil}
	return Reply{Action: ActSend, FrameType: FrameMsg, Payload: m2}
}

func containsSuite(list []uint16, s uint16) bool {
	for _, x := range list {
		if x == s && !IsGrease(x) {
			return true
		}
	}
	return false
}

// puzzleSolved checks the cookie (current or previous minute of the local clock at arrival,
// current or previous R) and the solution.
func (r *Responder) puzzleSolved(h *Hello, cb []byte, d uint8, arrival time.Time) bool {
	if h.Flags&FlagPuzzle == 0 {
		return false
	}
	minute := uint64(arrival.Unix()) / 60
	ok := false
	secrets := [][]byte{r.cfg.Puzzle.Secret}
	if r.cfg.Puzzle.PrevSecret != nil && arrival.Sub(r.cfg.Puzzle.RotatedAt) < time.Minute {
		secrets = append(secrets, r.cfg.Puzzle.PrevSecret)
	}
	for _, sec := range secrets {
		for _, m := range []uint64{minute, minute - 1} {
			if ctEqual(Cookie(sec, cb, m), h.Cookie) {
				ok = true
			}
		}
	}
	return ok && PuzzleOK(h.Cookie, cb, h.Nonce, d)
}

func (r *Responder) buildP2(p1 *HsInit, suite uint16, hs *noise.HandshakeState, personal bool) *HsResp {
	now := r.now()
	t := uint64(now.Unix())
	if r.timeOverride != 0 {
		t = r.timeOverride
	}
	p2 := &HsResp{ProtoVer: ProtoVer, Time: t}
	if p1.ProtoVer < ProtoVer {
		p2.Reject = &Reject{Reason: ReasonVersion, Min: ProtoVer, Max: ProtoVer}
		return p2
	}
	if absDiff(int64(p1.Time), now.Unix()) > MaxClockSkew {
		p2.Reject = &Reject{Reason: ReasonClockSkew, Min: uint32(now.Unix())}
		return p2
	}
	// Features: offered ∩ supported, GREASE removed.
	supported := sortedNoGrease(r.cfg.Features)
	for _, f := range sortedNoGrease(p1.Features) {
		for _, s := range supported {
			if f == s {
				p2.Features = append(p2.Features, f)
			}
		}
	}
	has := func(f uint32) bool {
		for _, x := range p2.Features {
			if x == f {
				return true
			}
		}
		return false
	}
	// Services: the highest common version of each offered service.
	for _, o := range p1.Services {
		if IsGrease32(o.ID) {
			continue
		}
		for _, s := range r.cfg.Services {
			if s.ID != o.ID {
				continue
			}
			var best uint32
			for _, v := range o.Versions {
				for _, w := range s.Versions {
					if v == w && v > best {
						best = v
					}
				}
			}
			if best != 0 {
				p2.Services = append(p2.Services, ServiceChoice{ID: o.ID, Version: best})
			}
		}
	}
	p2.Card = r.cardForP2(p1, suite, personal, now)
	if has(FeatureObservedAddr) && len(r.cfg.Observed) > 0 {
		p2.Observed = r.cfg.Observed
	}
	if len(p1.Early) > 0 {
		p2.EarlyStatus = EarlyIgnored
		if suite == SuiteXK && has(FeatureEarlyData) && r.cfg.Early != nil && len(p1.Early) <= MaxEarlyLen &&
			absDiff(int64(p1.Time), now.Unix()) <= EarlyWindow &&
			(r.cfg.Replay == nil || !r.cfg.Replay.Seen(hs.RemoteEphemeral(), now)) {
			if resp, ok := r.cfg.Early(p1.Early); ok && len(resp) <= MaxEarlyResp {
				p2.EarlyStatus, p2.Early = EarlyProcessed, resp
				r.early = clone(p1.Early)
			}
		}
	}
	return p2
}

// cardForP2 applies the card rule of handshake.md §7 (B1).
func (r *Responder) cardForP2(p1 *HsInit, suite uint16, personal bool, now time.Time) []byte {
	cur := r.cfg.OwnCards.Current
	if suite == SuiteXX {
		return cur[0].Envelope // always the public card
	}
	issued, known := r.cfg.OwnCards.Issued[p1.KnownCardSeq]
	if known && !issued.At.IsZero() && now.Sub(issued.At) > IssuedMemory {
		known = false
	}
	if p1.KnownCardSeq == 0 || !known {
		if !r.hidden {
			return cur[0].Envelope
		}
		if personal { // a hidden node has no public card
			return cur[1].Envelope
		}
		return nil // invitation pass: the contact gets its peer card after redemption
	}
	if issued.Scope == uint32(objects.ScopeInvite) {
		return nil
	}
	if c, ok := cur[issued.Scope]; ok && c.Seq > p1.KnownCardSeq {
		return c.Envelope // exactly when the card of that scope is newer
	}
	return nil
}

// HandleFinish processes message 3 and checks the initiator's card. Admission (bans, limits)
// is the caller's: a refusal is a REJECT frame on the control stream.
//
// The transport keys are computed as soon as message 3 is authentic, before the card is
// checked. If the card is rejected the returned Session is non-nil and carries only the keys
// (Peer is nil), so the caller can report the rejection as a sealed REJECT on the control
// stream (handshake §8, decisions.md T3.1).
func (r *Responder) HandleFinish(msg3 []byte) (*Session, error) {
	hs, pend := r.hs, r.pending
	r.hs, r.pending = nil, nil
	if hs == nil || pend == nil {
		return nil, ErrState
	}
	p3b, err := hs.ReadMessage(msg3)
	if err != nil {
		return nil, err
	}
	p3, err := ParseHsFinish(p3b)
	if err != nil {
		return nil, err
	}
	s, err := newSession(hs, r.suite, false)
	if err != nil {
		return nil, err
	}
	if p3.Card == nil {
		return s, fmt.Errorf("%w: no card", ErrPeerCard)
	}
	info, err := r.cfg.Cards.VerifyCard(p3.Card, r.now())
	if err != nil {
		return s, fmt.Errorf("%w: %v", ErrPeerCard, err)
	}
	if !ctEqual(info.HsKey, s.RemoteStatic) {
		return s, fmt.Errorf("%w: hs_key is not the Noise static key", ErrPeerCard)
	}
	if err := bindCard(r.cfg.Bindings, info); err != nil {
		return s, err
	}
	s.Peer = info
	s.Features, s.Services = pend.Features, pend.Services
	s.PeerProtoVer, s.PeerTime = pend.PeerProtoVer, pend.PeerTime
	s.EarlyRequest, s.ResponderCardSent = pend.EarlyRequest, pend.ResponderCardSent
	s.Access = r.access
	return s, nil
}
