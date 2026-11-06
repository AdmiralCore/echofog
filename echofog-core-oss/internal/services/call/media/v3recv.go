package media

import (
	"crypto/ed25519"
	"errors"
	"sync"
	"time"
)

const (
	// MaxCandidates3 is how many different units with one number a receiver keeps.
	MaxCandidates3 = 4
	// LateVerify3 is how long after a CALL_AUTH a late unit is still checked against it.
	LateVerify3 = time.Second
	// CandidateTTL3 is how long a unit waits for its CALL_AUTH.
	CandidateTTL3 = 500 * time.Millisecond
	// ForgeryMute3 is how long a sender's unverified units are not played after a forgery.
	ForgeryMute3 = 2 * time.Second

	// Bounds of the receiver state (an insider can invent numbers freely).
	maxSources3   = 1024
	maxPerSource3 = 512
	sourceIdle3   = 30 * time.Second
	// maxSourceBytes3 bounds the ciphertext one source keeps as candidates.
	maxSourceBytes3 = 2 << 20
)

var (
	ErrUnknownMediaKey = errors.New("media v3: no media key for sender")
	ErrForgedAuth      = errors.New("media v3: CALL_AUTH signature fails")
	ErrForgedUnit      = errors.New("media v3: unit does not match its authenticator")
)

// ReceiverConfig configures one Receiver.
type ReceiverConfig struct {
	Now    func() time.Time
	Strict bool
	// MediaKey returns the media key a sender announced in its signed join.
	MediaKey func(sender, gen byte) (ed25519.PublicKey, bool)
	// Report tells the coordinator about a forgery; it must not block.
	Report func(sender, gen byte, reason error)
}

// UnitResult says what to do with a unit that just arrived.
type UnitResult struct {
	// Play: decode and play it now. Forward: a relay forwards it now. Verified: it matched an
	// authenticator already. Duplicate: dropped.
	Play, Forward, Verified, Duplicate bool
}

// VerifiedUnit is a unit that matched a verified authenticator.
type VerifiedUnit struct {
	Header     Header3
	Ciphertext []byte
}

// AuthResult is the outcome of one CALL_AUTH.
type AuthResult struct {
	Verified []VerifiedUnit
	Forged   bool
	// Need lists units this authenticator vouches for whose copy was dropped because forged
	// candidates filled the slot: the caller asks the feeder to send them again (NACK), and the
	// retransmission is verified against the same authenticator.
	Need []NeedUnit
}

// NeedUnit names a unit to ask for again.
type NeedUnit struct {
	Sender, Gen, Stream, Kind byte
	Seq                       uint32
}

type srcKey struct{ sender, gen, stream, kind byte }

type cand struct {
	h    Header3
	ct   []byte
	hash [UnitHash3Len]byte
}

type candSet struct {
	list []cand
	at   time.Time
	// overflow: a copy was dropped because the slots were full. If no kept candidate matches
	// the authenticator, the real unit may be the dropped one and must be asked for again.
	overflow bool
}

type knownHash struct {
	hash [UnitHash3Len]byte
	at   time.Time
}

type source struct {
	bytes int // ciphertext held as candidates
	win   replayWindow
	cands map[uint32]*candSet
	known map[uint32]knownHash
}

type report struct {
	sender, gen byte
	reason      error
}

// Receiver authenticates units by batch (services.md 9.3). It works on sealed units, so it needs
// no sender keys: the CALL_AUTH hashes cover the ciphertext. Only verified units enter the replay
// window (P1). Safe for concurrent use; Config.Report runs after the lock is released.
type Receiver struct {
	cfg  ReceiverConfig
	mu   sync.Mutex
	src  map[srcKey]*source
	mute map[byte]time.Time

	lastSweep time.Time
}

// NewReceiver returns a Receiver.
func NewReceiver(cfg ReceiverConfig) *Receiver {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Receiver{cfg: cfg, src: map[srcKey]*source{}, mute: map[byte]time.Time{}}
}

func (r *Receiver) source(k srcKey) *source {
	s := r.src[k]
	if s == nil {
		if len(r.src) >= maxSources3 {
			return nil
		}
		s = &source{cands: map[uint32]*candSet{}, known: map[uint32]knownHash{}}
		r.src[k] = s
	}
	return s
}

func (r *Receiver) flag(sender, gen byte, reason error, now time.Time, out *[]report) {
	r.mute[sender] = now.Add(ForgeryMute3)
	*out = append(*out, report{sender, gen, reason})
}

func (r *Receiver) fire(reports []report) {
	if r.cfg.Report == nil {
		return
	}
	for _, rp := range reports {
		r.cfg.Report(rp.sender, rp.gen, rp.reason)
	}
}

// OnUnit handles one reassembled sealed unit.
func (r *Receiver) OnUnit(h Header3, ct []byte) UnitResult {
	var reports []report
	res := r.onUnit(h, ct, &reports)
	r.fire(reports)
	return res
}

func (r *Receiver) onUnit(h Header3, ct []byte, reports *[]report) UnitResult {
	// No state before authentication: only a sender whose signed join announced a media key
	// gets a source, so unannounced numbers cannot fill the table or the memory.
	if r.cfg.MediaKey == nil {
		return UnitResult{}
	}
	if _, ok := r.cfg.MediaKey(h.Sender, h.Gen); !ok {
		return UnitResult{}
	}
	now := r.cfg.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.maybeSweepLocked(now)
	s := r.source(srcKey{h.Sender, h.Gen, h.Stream, h.Kind})
	if s == nil {
		return UnitResult{}
	}
	if s.win.seen(h.Seq) {
		return UnitResult{Duplicate: true}
	}
	hash := UnitHash3(h, ct)
	if k, ok := s.known[h.Seq]; ok && now.Sub(k.at) < LateVerify3 {
		if k.hash != hash {
			r.flag(h.Sender, h.Gen, ErrForgedUnit, now, reports)
			return UnitResult{}
		}
		s.win.mark(h.Seq, now)
		return UnitResult{Play: true, Forward: true, Verified: true}
	}
	cs := s.cands[h.Seq]
	if cs != nil && now.Sub(cs.at) > CandidateTTL3 {
		s.removeCands(h.Seq)
		cs = nil
	}
	if cs == nil {
		if len(s.cands) >= maxPerSource3 {
			s.makeRoom(now)
		}
		cs = &candSet{at: now}
		s.cands[h.Seq] = cs
	}
	for _, c := range cs.list {
		if c.hash == hash {
			return UnitResult{Duplicate: true}
		}
	}
	if len(cs.list) >= MaxCandidates3 || s.bytes+len(ct) > maxSourceBytes3 {
		cs.overflow = true
		return UnitResult{}
	}
	s.bytes += len(ct)
	first := len(cs.list) == 0
	cs.list = append(cs.list, cand{h: h, ct: ct, hash: hash})
	if !first {
		return UnitResult{}
	}
	return UnitResult{Forward: true, Play: !r.cfg.Strict && !r.mutedLocked(h.Sender, now)}
}

// makeRoom drops expired candidates, and the oldest one if none expired.
func (s *source) makeRoom(now time.Time) {
	var oldest uint32
	var oldestAt time.Time
	first := true
	for seq, cs := range s.cands {
		if now.Sub(cs.at) > CandidateTTL3 {
			s.removeCands(seq)
			continue
		}
		if first || cs.at.Before(oldestAt) {
			oldest, oldestAt, first = seq, cs.at, false
		}
	}
	if len(s.cands) >= maxPerSource3 && !first {
		s.removeCands(oldest)
	}
}

func (s *source) removeCands(seq uint32) {
	cs := s.cands[seq]
	if cs == nil {
		return
	}
	for _, c := range cs.list {
		s.bytes -= len(c.ct)
	}
	delete(s.cands, seq)
}

// OnAuth handles one CALL_AUTH payload.
func (r *Receiver) OnAuth(payload []byte) (AuthResult, error) {
	var reports []report
	res, err := r.onAuth(payload, &reports)
	r.fire(reports)
	return res, err
}

func (r *Receiver) onAuth(payload []byte, reports *[]report) (AuthResult, error) {
	a, err := DecodeAuth(payload)
	if err != nil {
		return AuthResult{}, err
	}
	if r.cfg.MediaKey == nil {
		return AuthResult{}, ErrUnknownMediaKey
	}
	pub, ok := r.cfg.MediaKey(a.Sender, a.Gen)
	if !ok {
		return AuthResult{}, ErrUnknownMediaKey
	}
	now := r.cfg.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.maybeSweepLocked(now)
	if !a.Verify(pub) {
		r.flag(a.Sender, a.Gen, ErrForgedAuth, now, reports)
		return AuthResult{Forged: true}, ErrForgedAuth
	}
	s := r.source(srcKey{a.Sender, a.Gen, a.Stream, a.Kind})
	if s == nil {
		return AuthResult{}, nil
	}
	var res AuthResult
	base := a.FirstSeq - uint32(len(a.Prev))
	hashes := append(append([][UnitHash3Len]byte{}, a.Prev...), a.Cur...)
	for i, hash := range hashes {
		seq := base + uint32(i)
		if k, ok := s.known[seq]; ok && now.Sub(k.at) < LateVerify3 && k.hash != hash {
			continue // the signer contradicts itself; the first signed list stands
		}
		if len(s.known) >= maxPerSource3 {
			s.purgeKnown(now)
		}
		s.known[seq] = knownHash{hash: hash, at: now}
		cs := s.cands[seq]
		if cs == nil {
			continue
		}
		s.removeCands(seq)
		if now.Sub(cs.at) > CandidateTTL3 {
			continue
		}
		matched := false
		for _, c := range cs.list {
			if c.hash != hash {
				res.Forged = true
				continue
			}
			matched = true
			if s.win.mark(seq, now) {
				res.Verified = append(res.Verified, VerifiedUnit{Header: c.h, Ciphertext: c.ct})
			}
		}
		if !matched && cs.overflow {
			res.Need = append(res.Need, NeedUnit{Sender: a.Sender, Gen: a.Gen, Stream: a.Stream, Kind: a.Kind, Seq: seq})
		}
	}
	if res.Forged {
		r.flag(a.Sender, a.Gen, ErrForgedUnit, now, reports)
	}
	return res, nil
}

func (s *source) purgeKnown(now time.Time) {
	for seq, k := range s.known {
		if now.Sub(k.at) >= LateVerify3 {
			delete(s.known, seq)
		}
	}
	if len(s.known) >= maxPerSource3 { // a flood of authenticators: start over rather than grow
		s.known = map[uint32]knownHash{}
	}
}

func (r *Receiver) mutedLocked(sender byte, now time.Time) bool {
	until, ok := r.mute[sender]
	return ok && now.Before(until)
}

// Muted reports whether the sender's unverified units are held back.
func (r *Receiver) Muted(sender byte) bool {
	now := r.cfg.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mutedLocked(sender, now)
}

// Sweep drops expired candidates, authenticators, mutes and idle sources.
func (r *Receiver) Sweep() {
	now := r.cfg.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked(now)
}

// maybeSweepLocked ages the state at most every 100 ms, so a caller that never calls Sweep
// still has bounded state.
func (r *Receiver) maybeSweepLocked(now time.Time) {
	if now.Sub(r.lastSweep) >= BatchMaxAge {
		r.sweepLocked(now)
	}
}

func (r *Receiver) sweepLocked(now time.Time) {
	r.lastSweep = now
	for sender, until := range r.mute {
		if !now.Before(until) {
			delete(r.mute, sender)
		}
	}
	for k, s := range r.src {
		for seq, cs := range s.cands {
			if now.Sub(cs.at) > CandidateTTL3 {
				s.removeCands(seq)
			}
		}
		for seq, kh := range s.known {
			if now.Sub(kh.at) >= LateVerify3 {
				delete(s.known, seq)
			}
		}
		if len(s.cands) == 0 && len(s.known) == 0 && now.Sub(s.win.lastSeen) > sourceIdle3 {
			delete(r.src, k)
		}
	}
}
