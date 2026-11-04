// Package holepunch connects two peers that are both behind NATs without a relay carrying their
// traffic. The peers cannot reach each other yet, so they coordinate over a relay and then start
// punching at the same moment.
//
// The protocol is DCUtR (Direct Connection Upgrade through Relay) as implemented by go-libp2p's
// p2p/protocol/holepunch (MIT License, Copyright (c) 2014 Juan Batiz-Benet): the initiator sends
// CONNECT with its public addresses and times the round trip, the responder answers with its own,
// the initiator sends SYNC and starts RTT/2 later, so that both begin at the same instant. Over
// three attempts the roles are tried as "initiator punches, responder dials", twice, and then
// "both dial". The differences from the original: messages are small JSON frames carried by our
// relay circuits, addresses are validated and the amount of work a peer can request is capped.
// See THIRD_PARTY_NOTICES.md.
//
// It works through cone NATs (the mapping is the same for every destination). Through a symmetric
// NAT the address learned from the relay is not the one the other peer will be seen at, and the
// attempt fails; callers should check the NAT report first and use a relay instead.
package holepunch

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

var (
	ErrNoOwnAddr    = errors.New("no public address known yet, cannot hole punch")
	ErrBusy         = errors.New("another hole punch to this peer is running")
	ErrNoRemoteAddr = errors.New("the peer sent no usable address")
	ErrFailed       = errors.New("hole punching did not produce a direct connection")
)

const (
	maxAttempts   = 3
	maxAddrs      = 8
	maxMessage    = 2048
	connectWait   = 8 * time.Second  // how long the initiator waits for the answer to CONNECT
	syncWait      = 10 * time.Second // how long the responder waits for SYNC after answering
	maxHalfRTT    = 2 * time.Second  // a slower path cannot be synchronised: give up
	acceptWindow  = 10 * time.Second // per peer, we answer at most maxAttempts CONNECTs in this window
	maxResponders = 8                // simultaneous incoming attempts
	wantWindow    = 10 * time.Minute // how long a peer we tried to reach counts as wanted
	maxWanted     = 1024
)

// Transport is the network side: punch with random packets, or dial.
type Transport interface {
	PunchHole(ctx context.Context, addr string, peer types.PeerID) error
	DialDirect(ctx context.Context, addr string) error
}

// Signaler carries coordination messages to a peer, over a relay if need be.
type Signaler interface {
	Send(ctx context.Context, to types.PeerID, payload []byte) error
}

type Config struct {
	Transport Transport
	Signal    Signaler
	// OwnAddrs returns our public "ip:port" candidates (observed by peers, mapped by the router).
	OwnAddrs func() []string
	// Connected reports whether a direct connection to the peer exists.
	Connected func(types.PeerID) bool
	// LocalAddrs returns our LAN "ip:port" addresses. They are offered to a peer, who tries them only
	// when it sits behind the same NAT as we do (the same public address): there the public address
	// cannot be reached from inside, the LAN address can.
	LocalAddrs func() []string
	// AllowPrivate accepts loopback and LAN addresses (tests, closed networks).
	AllowPrivate bool
	// ConnectWait overrides how long the initiator waits for the answer to CONNECT (default 8 s).
	ConnectWait time.Duration
	// Admit, when set, decides whether to answer a CONNECT from a peer, given the access ticket it
	// sent. The answer gives out this node's public, IPv6 and LAN addresses, so it goes only to
	// peers the node has reason to reach (contacts, peers it dialed itself, see Wanted); a hidden
	// node answers only the peers it invited. Nil answers everyone (tests).
	Admit func(from types.PeerID, ticket []byte) bool
	// Ticket, when set, gives the access ticket to send with a CONNECT to peer (nil: none).
	Ticket func(peer types.PeerID) []byte
	// Refuse, when set, reports whether a punch or dial toward addr must not be made: the node's
	// noise-dial rule (a server across a border with no known cover is not reached by noise).
	// Punching bypasses the swarm's dial path, so it applies the rule itself.
	Refuse func(peer types.PeerID, addr string) bool
	// OwnNAT, when set, reports our NAT so a birthday punch can be chosen for a symmetric NAT (see
	// package network, BirthdayDial). Nil, or a transport without the birthday half, means only the
	// ordinary DCUtR punch is ever run.
	OwnNAT func() NATInfo
	// OwnID is this node's peer ID. It decides, deterministically on both sides, which is the dialer
	// in the both-symmetric case (the lower ID dials, the higher listens).
	OwnID types.PeerID
}

// NATInfo describes this node's NAT for the purpose of choosing a punch strategy.
type NATInfo struct {
	// Sym: behind a symmetric NAT with no forwarded port, so the ordinary punch cannot reach us and
	// only a birthday punch, or a relay, can.
	Sym bool
	// Unknown: the NAT check has not classified us yet (or could not). A birthday role needs to know
	// whether our mapping is stable, so we advertise this instead of pretending to be cone.
	Unknown bool
	// Lo, Hi and Ports describe the public IPv4 port block the NAT was seen to use (natcheck). A
	// narrow block lets a peer aim a birthday spray; a wide one, or too few observations, does not.
	Lo, Hi uint16
	Ports  int
}

const (
	ntCone    = "cone"
	ntSym     = "sym"
	ntUnknown = "unknown"
	// birthdayNarrowRange is the widest observed port block still treated as narrow enough for a
	// birthday punch to be worth it (mirrors the transport's own figure).
	birthdayNarrowRange = 8000
	// maxBirthdaySockets caps the sockets the symmetric side opens.
	maxBirthdaySockets = 256
	// maxBirthdayNode caps birthday punches running on this node at once.
	maxBirthdayNode = 2
)

// ErrNoPunchPath: the NAT pair has no direct path a punch can make (both symmetric, or a wide
// symmetric range), so the caller should use a relay. Connect stops trying on it.
var ErrNoPunchPath = errors.New("no direct path for this NAT pair; use a relay")

// The birthday roles carried in SYNC. The initiator chooses the role from both NAT reports (its own
// and the responder's ANSWER), so the two ends always run complementary halves. Recomputing the mode
// on the responder made it act on older information and run the wrong half of the punch.
const (
	roleDial   = "dial"   // open the birthday sockets and dial the peer's stable address
	roleSpray  = "spray"  // spray the peer's observed port block
	roleMutual = "mutual" // both sides open sockets and spray each other's block
)

// punchMode is the strategy chosen for a peer from the pair of NATs.
type punchMode int

const (
	modeOrdinary       punchMode = iota // cone/cone (or an older peer we can punch): DCUtR
	modeBirthdayDial                    // we are symmetric, the peer is cone: we open the sockets
	modeBirthdaySpray                   // the peer is symmetric, we are cone: we spray its range
	modeBirthdayMutual                  // both symmetric with narrow ranges: both open sockets and spray
	modeNone                            // no direct path: relay
)

// birthdayTransport is the transport's birthday-punch half. A transport without it never runs one.
type birthdayTransport interface {
	BirthdayDial(ctx context.Context, remoteAddr string, peer types.PeerID, n int) error
	BirthdaySpray(ctx context.Context, ip string, lo, hi uint16, m int, peer types.PeerID) error
	// BirthdayMutual is the both-symmetric case: open n sockets, spray the peer's range from each,
	// and — as the dialer — dial whichever socket the peer reaches; as the listener, accept on them.
	BirthdayMutual(ctx context.Context, ip string, lo, hi uint16, n int, peer types.PeerID, asDialer bool) error
}

type message struct {
	T     string   `json:"t"` // "connect" (request) | "answer" (reply to connect) | "sync"
	N     uint64   `json:"n"`
	Addr  []string `json:"a,omitempty"`
	Local []string `json:"l,omitempty"` // LAN addresses, for a peer behind the same NAT
	Both  bool     `json:"b,omitempty"` // sync: both sides dial
	// Ticket proves an invitation to a hidden node (connect only; see package access).
	Ticket []byte `json:"k,omitempty"`
	// Birthday-punch fields, carried on connect and answer. A peer running v0.2.0 sends none of
	// them (and ignores them, since the decoder does not reject unknown fields), so a birthday punch
	// is only ever chosen when both sides sent NT: an old peer is treated as cone, never sprayed to.
	NT string    `json:"nt,omitempty"` // our NAT: "cone" (public/cone/mapped), "sym" or "unknown"
	PR [2]uint16 `json:"pr,omitempty"` // the public port block our NAT was seen to use [lo, hi]
	PN int       `json:"pn,omitempty"` // how many distinct ports were observed (confidence)
	BD bool      `json:"bd,omitempty"` // sync: run a birthday punch rather than the ordinary one
	BR string    `json:"br,omitempty"` // sync: the birthday role the responder must run (roleDial/…)
	NS uint16    `json:"ns,omitempty"` // sync: how many sockets the symmetric side opens (informational)
}

type sessKey struct {
	peer types.PeerID
	n    uint64
}

type incoming struct {
	addrs []netip.AddrPort
	timer *time.Timer
	// the peer's NAT, learned from its CONNECT, so onSync can choose the birthday role.
	peerNT         string
	peerLo, peerHi uint16
	peerPorts      int
}

// Service runs both roles.
type Service struct {
	cfg Config

	mu         sync.Mutex
	waiting    map[sessKey]chan message
	expecting  map[sessKey]*incoming
	active     map[types.PeerID]struct{}
	lastAccept map[types.PeerID][]time.Time
	wanted     map[types.PeerID]time.Time // peers this node set out to reach, and when
	responders int
	birthdays  int // birthday punches running on this node now (both roles)
	closed     bool

	bdTried, bdOK atomic.Int64 // birthday punches started and ones that made a direct link
}

// BirthdayStats reports how many birthday punches this node has started and how many produced a
// direct link, for the connection report.
func (s *Service) BirthdayStats() (tried, ok int64) {
	return s.bdTried.Load(), s.bdOK.Load()
}

// Want notes that this node is trying to reach peer: for wantWindow, a CONNECT from that peer is
// one this node has reason to answer (see Wanted). Connect notes it by itself.
func (s *Service) Want(peer types.PeerID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if len(s.wanted) >= maxWanted {
		for id, at := range s.wanted {
			if now.Sub(at) > wantWindow {
				delete(s.wanted, id)
			}
		}
		if len(s.wanted) >= maxWanted {
			s.wanted = make(map[types.PeerID]time.Time)
		}
	}
	s.wanted[peer] = now
}

// Wanted reports whether this node tried to reach peer in the last wantWindow: two peers that both
// want a direct link (a call, a chat both sides opened) answer each other's CONNECT.
func (s *Service) Wanted(peer types.PeerID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.wanted[peer]
	return ok && time.Since(at) <= wantWindow
}

func New(cfg Config) *Service {
	if cfg.ConnectWait <= 0 {
		cfg.ConnectWait = connectWait
	}
	return &Service{
		cfg:        cfg,
		waiting:    make(map[sessKey]chan message),
		expecting:  make(map[sessKey]*incoming),
		active:     make(map[types.PeerID]struct{}),
		lastAccept: make(map[types.PeerID][]time.Time),
		wanted:     make(map[types.PeerID]time.Time),
	}
}

func (s *Service) Close() {
	s.mu.Lock()
	s.closed = true
	for _, in := range s.expecting {
		in.timer.Stop()
	}
	s.expecting = map[sessKey]*incoming{}
	s.mu.Unlock()
}

// usable keeps the addresses worth punching toward: public unicast ones, a handful.
func (s *Service) usable(in []string) []netip.AddrPort {
	var out []netip.AddrPort
	seen := map[netip.AddrPort]bool{}
	for _, a := range in {
		ap, err := netip.ParseAddrPort(a)
		if err != nil || ap.Port() == 0 || seen[ap] {
			continue
		}
		ip := ap.Addr().Unmap()
		if ip.IsUnspecified() || ip.IsMulticast() {
			continue
		}
		if !s.cfg.AllowPrivate && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || isCGNAT(ip)) {
			continue
		}
		seen[ap] = true
		out = append(out, netip.AddrPortFrom(ip, ap.Port()))
		if len(out) == maxAddrs {
			break
		}
	}
	return out
}

// lan keeps the private addresses of a list (a few): what a peer on the same network can reach.
func lan(in []string) []netip.AddrPort {
	var out []netip.AddrPort
	seen := map[netip.AddrPort]bool{}
	for _, a := range in {
		ap, err := netip.ParseAddrPort(a)
		if err != nil || ap.Port() == 0 || seen[ap] {
			continue
		}
		ip := ap.Addr().Unmap()
		if !ip.IsPrivate() {
			continue
		}
		seen[ap] = true
		out = append(out, netip.AddrPortFrom(ip, ap.Port()))
		if len(out) == 4 {
			break
		}
	}
	return out
}

func (s *Service) ownLocal() []string {
	if s.cfg.LocalAddrs == nil {
		return nil
	}
	return addrStrings(lan(s.cfg.LocalAddrs()))
}

// allowed drops the addresses the node's noise-dial rule refuses for peer.
func (s *Service) allowed(peer types.PeerID, in []netip.AddrPort) []netip.AddrPort {
	if s.cfg.Refuse == nil {
		return in
	}
	out := in[:0:0]
	for _, a := range in {
		if !s.cfg.Refuse(peer, a.String()) {
			out = append(out, a)
		}
	}
	return out
}

// sameNAT reports whether the peer's public addresses share an IP with ours: we are behind the same
// router, the only case in which our LAN addresses are of any use to it.
func sameNAT(own, remote []netip.AddrPort) bool {
	for _, o := range own {
		for _, r := range remote {
			if o.Addr() == r.Addr() {
				return true
			}
		}
	}
	return false
}

// withLAN puts the peer's LAN addresses in front of its public ones when both of us are behind the
// same NAT, that is, when its public address has the same IP as one of ours.
func withLAN(own, remote []netip.AddrPort, remoteLocal []string) []netip.AddrPort {
	if sameNAT(own, remote) {
		return append(lan(remoteLocal), remote...)
	}
	return remote
}

func isCGNAT(ip netip.Addr) bool {
	return ip.Is4() && ip.As4()[0] == 100 && ip.As4()[1]&0xC0 == 64
}

func (s *Service) send(ctx context.Context, to types.PeerID, m message) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.cfg.Signal.Send(ctx, to, raw)
}

// Connect tries to establish a direct connection to peer, coordinating through a relay.
func (s *Service) Connect(ctx context.Context, peer types.PeerID) error {
	if s.cfg.Connected(peer) {
		return nil
	}
	s.Want(peer)
	s.mu.Lock()
	if _, busy := s.active[peer]; busy || s.closed {
		s.mu.Unlock()
		return ErrBusy
	}
	s.active[peer] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.active, peer)
		s.mu.Unlock()
	}()

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		// the last attempt swaps the roles: both dial (a peer that cannot punch can still dial)
		both := attempt == maxAttempts
		lastErr = s.attempt(ctx, peer, both)
		if lastErr == nil || s.cfg.Connected(peer) {
			return nil
		}
		if errors.Is(lastErr, ErrNoOwnAddr) || errors.Is(lastErr, ErrNoRemoteAddr) || errors.Is(lastErr, ErrNoPunchPath) {
			return lastErr
		}
		slog.Debug("hole punch attempt failed", "peer", peer.String()[:8], "attempt", attempt, "err", lastErr)
	}
	return fmt.Errorf("%w: %v", ErrFailed, lastErr)
}

// sessionNonce names an attempt; an answer is only taken for it if it carries the number back, so
// it must not be guessable by somebody who cannot see our messages.
func sessionNonce() uint64 {
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		panic(err) // the system has no randomness: nothing here can be trusted
	}
	return binary.BigEndian.Uint64(b[:])
}

func (s *Service) attempt(ctx context.Context, peer types.PeerID, both bool) error {
	own := s.usable(s.cfg.OwnAddrs())
	if len(own) == 0 {
		return ErrNoOwnAddr
	}
	key := sessKey{peer: peer, n: sessionNonce()}
	reply := make(chan message, 1)
	s.mu.Lock()
	s.waiting[key] = reply
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.waiting, key)
		s.mu.Unlock()
	}()

	ownNAT := s.ownNAT()
	start := time.Now()
	req := message{T: "connect", N: key.n, Addr: addrStrings(own), Local: s.ownLocal()}
	ownNAT.fill(&req)
	if s.cfg.Ticket != nil {
		req.Ticket = s.cfg.Ticket(peer)
	}
	if err := s.send(ctx, peer, req); err != nil {
		return fmt.Errorf("send CONNECT: %w", err)
	}

	var answer message
	wait := time.NewTimer(s.cfg.ConnectWait)
	defer wait.Stop()
	select {
	case answer = <-reply:
	case <-wait.C:
		return errors.New("the peer did not answer CONNECT")
	case <-ctx.Done():
		return ctx.Err()
	}
	rtt := time.Since(start)
	remote := s.usable(answer.Addr)
	if len(remote) == 0 {
		return ErrNoRemoteAddr
	}
	remote = s.allowed(peer, withLAN(own, remote, answer.Local))
	if len(remote) == 0 {
		return ErrNoRemoteAddr // every address is one the noise-dial rule holds back
	}
	half := rtt / 2
	if half > maxHalfRTT {
		return fmt.Errorf("the path through the relay is too slow to synchronise (rtt %v)", rtt)
	}

	mode := s.chooseMode(ownNAT, answer.NT, answer.PR[0], answer.PR[1], answer.PN)
	if mode == modeNone {
		// no direct path for this NAT pair (both symmetric, or a wide symmetric range): do not send
		// SYNC, which would only make the peer dial an address it cannot reach. The caller relays.
		return ErrNoPunchPath
	}
	// A birthday punch is an IPv4 mechanism: its sockets and its spray target IPv4 only. When the
	// pair has no public IPv4 candidate (an IPv6-only network, or the peer's IPv4 was filtered), the
	// ordinary punch over IPv6 is the direct path, exactly as it is over a cone NAT.
	if mode != modeOrdinary && !firstPublicV4(remote).IsValid() {
		mode = modeOrdinary
	}
	sync := message{T: "sync", N: key.n, Both: both}
	switch mode {
	case modeBirthdayDial:
		// We are the symmetric side and open the sockets; the peer's cone side sprays our block.
		sync.BD = true
		sync.BR = roleSpray
		sync.NS = uint16(s.birthdaySockets(ownNAT))
	case modeBirthdaySpray:
		// The peer is symmetric and opens the sockets; we spray its block.
		sync.BD = true
		sync.BR = roleDial
	case modeBirthdayMutual:
		sync.BD = true
		sync.BR = roleMutual
	}
	if err := s.send(ctx, peer, sync); err != nil {
		return fmt.Errorf("send SYNC: %w", err)
	}
	// SYNC takes about half a round trip to arrive; start when it does
	select {
	case <-time.After(half):
	case <-ctx.Done():
		return ctx.Err()
	}

	switch mode {
	case modeBirthdayDial:
		return s.birthdayDial(ctx, peer, remote, int(sync.NS))
	case modeBirthdaySpray:
		return s.birthdaySpray(ctx, peer, remote, answer.PR[0], answer.PR[1], answer.PN)
	case modeBirthdayMutual:
		return s.birthdayMutual(ctx, peer, remote, answer.PR[0], answer.PR[1], answer.PN)
	case modeNone:
		return ErrNoPunchPath
	}
	if both {
		return s.dialAll(ctx, peer, remote)
	}
	return s.punchAll(ctx, peer, remote)
}

// Handle processes a coordination message from peer. Messages from peers that are not who they
// claim cannot hurt: an address is only ever punched or dialled, and the QUIC handshake then
// proves who answers.
func (s *Service) Handle(from types.PeerID, payload []byte) {
	if len(payload) == 0 || len(payload) > maxMessage {
		return
	}
	var m message
	if json.Unmarshal(payload, &m) != nil {
		return
	}
	switch m.T {
	case "connect":
		s.onConnect(from, m)
	case "answer":
		s.onAnswer(from, m)
	case "sync":
		s.onSync(from, m)
	}
}

// onAnswer delivers the reply to a CONNECT we sent. A reply with nobody waiting for it (the attempt
// timed out) is dropped: replies are never answered, so two nodes cannot keep each other talking.
func (s *Service) onAnswer(from types.PeerID, m message) {
	s.mu.Lock()
	ch, ok := s.waiting[sessKey{from, m.N}]
	s.mu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- m:
	default:
	}
}

func (s *Service) onConnect(from types.PeerID, m message) {
	if s.cfg.Admit != nil && !s.cfg.Admit(from, m.Ticket) {
		return
	}
	// a new request: rate-limited per peer and in total
	s.mu.Lock()
	now := time.Now()
	recent := s.lastAccept[from][:0:0]
	for _, t := range s.lastAccept[from] {
		if now.Sub(t) < acceptWindow {
			recent = append(recent, t)
		}
	}
	// a peer legitimately needs one CONNECT per attempt, three attempts in a row
	if s.closed || s.responders >= maxResponders || len(recent) >= maxAttempts {
		s.mu.Unlock()
		return
	}
	if len(s.lastAccept) > 4096 {
		s.lastAccept = make(map[types.PeerID][]time.Time)
	}
	s.lastAccept[from] = append(recent, now)
	remote := s.usable(m.Addr)
	own := s.usable(s.cfg.OwnAddrs())
	if len(remote) == 0 || len(own) == 0 {
		s.mu.Unlock()
		return
	}
	// our LAN addresses only to a peer behind the same router: to anyone else they are a map of
	// our home network and of no use
	var ownLocal []string
	if sameNAT(own, remote) {
		ownLocal = s.ownLocal()
	}
	remote = s.allowed(from, withLAN(own, remote, m.Local))
	if len(remote) == 0 {
		// nothing we may dial or punch toward: no answer, and so no address of ours either
		s.mu.Unlock()
		return
	}
	key := sessKey{from, m.N}
	in := &incoming{addrs: remote, peerNT: m.NT, peerLo: m.PR[0], peerHi: m.PR[1], peerPorts: m.PN}
	s.responders++
	in.timer = time.AfterFunc(syncWait, func() {
		s.mu.Lock()
		if cur, ok := s.expecting[key]; ok && cur == in {
			delete(s.expecting, key)
			s.responders--
		}
		s.mu.Unlock()
	})
	s.expecting[key] = in
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ans := message{T: "answer", N: m.N, Addr: addrStrings(own), Local: ownLocal}
	s.ownNAT().fill(&ans)
	if err := s.send(ctx, from, ans); err != nil {
		slog.Debug("hole punch: could not answer CONNECT", "err", err)
	}
}

func (s *Service) onSync(from types.PeerID, m message) {
	key := sessKey{from, m.N}
	s.mu.Lock()
	in, ok := s.expecting[key]
	if ok {
		delete(s.expecting, key)
		in.timer.Stop()
		s.responders--
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	// The initiator is already starting. For a birthday punch it names our role in SYNC: it chose the
	// mode from both NAT reports, and deriving it again here on older information made the two ends
	// run the wrong halves (both spraying, or neither). A birthday SYNC without a role comes from a
	// build that predates the field; derive it as that build did, as a compatibility fallback.
	if m.BD {
		role := m.BR
		if role == "" {
			switch s.chooseMode(s.ownNAT(), in.peerNT, in.peerLo, in.peerHi, in.peerPorts) {
			case modeBirthdayDial:
				role = roleDial
			case modeBirthdaySpray:
				role = roleSpray
			case modeBirthdayMutual:
				role = roleMutual
			}
		}
		if role == "" {
			slog.Debug("birthday punch: no role for this NAT pair", "peer", from.String()[:8])
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			var err error
			switch role {
			case roleDial:
				err = s.birthdayDial(ctx, from, in.addrs, s.birthdaySockets(s.ownNAT()))
			case roleSpray:
				err = s.birthdaySpray(ctx, from, in.addrs, in.peerLo, in.peerHi, in.peerPorts)
			case roleMutual:
				err = s.birthdayMutual(ctx, from, in.addrs, in.peerLo, in.peerHi, in.peerPorts)
			default:
				slog.Debug("birthday punch: unknown role in SYNC", "peer", from.String()[:8], "role", role)
				return
			}
			if err != nil {
				slog.Debug("birthday punch failed", "peer", from.String()[:8], "role", role, "err", err)
			}
		}()
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := s.dialAll(ctx, from, in.addrs); err != nil {
			slog.Debug("hole punch: dial failed", "peer", from.String()[:8], "err", err)
		}
	}()
}

// punchAll punches toward every address at once; the first connection from the peer wins.
func (s *Service) punchAll(ctx context.Context, peer types.PeerID, addrs []netip.AddrPort) error {
	return s.race(ctx, peer, addrs, func(ctx context.Context, a netip.AddrPort) error {
		return s.cfg.Transport.PunchHole(ctx, a.String(), peer)
	})
}

// dialAll dials every address at once; the first connection wins.
func (s *Service) dialAll(ctx context.Context, peer types.PeerID, addrs []netip.AddrPort) error {
	return s.race(ctx, peer, addrs, func(ctx context.Context, a netip.AddrPort) error {
		// the packets to a node are obfuscated with a key derived from its ID
		if e, ok := s.cfg.Transport.(interface {
			ExpectPeer(addr string, id types.PeerID)
		}); ok {
			e.ExpectPeer(a.String(), peer)
		}
		return s.cfg.Transport.DialDirect(ctx, a.String())
	})
}

func (s *Service) race(ctx context.Context, peer types.PeerID, addrs []netip.AddrPort, do func(context.Context, netip.AddrPort) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, len(addrs))
	for _, a := range addrs {
		go func() { results <- do(ctx, a) }()
	}
	var last error
	for range addrs {
		err := <-results
		if err == nil || s.cfg.Connected(peer) {
			return nil
		}
		last = err
	}
	return last
}

// ownNAT reports this node's NAT (the zero value when no source is wired: then NT is "cone" and no
// birthday punch is ever chosen for us).
func (s *Service) ownNAT() NATInfo {
	if s.cfg.OwnNAT == nil {
		return NATInfo{}
	}
	return s.cfg.OwnNAT()
}

// nt is the wire NAT type of a NATInfo.
func (ni NATInfo) nt() string {
	switch {
	case ni.Unknown:
		return ntUnknown
	case ni.Sym:
		return ntSym
	default:
		return ntCone
	}
}

// fill writes this NAT into a message (connect or answer).
func (ni NATInfo) fill(m *message) {
	m.NT = ni.nt()
	m.PR = [2]uint16{ni.Lo, ni.Hi}
	m.PN = ni.Ports
}

// narrowRange reports whether a symmetric NAT's observed port block is tight enough, and seen often
// enough, for a birthday punch to be worth aiming at it.
func narrowRange(lo, hi uint16, ports int) bool {
	return lo >= 1024 && hi >= lo && ports >= 2 && int(hi-lo) <= birthdayNarrowRange
}

// chooseMode picks the punch strategy from this node's NAT and the peer's (as it reported it). An
// older peer that sent no NT counts as cone: it is punched the ordinary way if we can, never
// sprayed to (it would not spray back). Symmetric↔cone is tried whatever the range (a narrow block
// is swept, a wide one sprayed at random over the dynamic range). Both-symmetric is tried only when
// both ranges are narrow, where a double birthday has a chance; otherwise it falls to a relay.
func (s *Service) chooseMode(own NATInfo, peerNT string, peerLo, peerHi uint16, peerPorts int) punchMode {
	peerSym := peerNT == ntSym
	peerUnknown := peerNT == "" || peerNT == ntUnknown
	switch {
	case !own.Sym && !peerSym:
		// Both sides have (or may have) a stable mapping: the ordinary punch is the safe try. That
		// includes an unclassified NAT, where a birthday role would be a guess.
		return modeOrdinary
	case own.Unknown && peerSym:
		// Our NAT is not classified yet. If it turns out to be cone, spraying the peer's range is the
		// only direct path; if it is symmetric, the attempt fails and the pair falls back to a relay.
		return modeBirthdaySpray
	case own.Sym && peerUnknown:
		// We are symmetric and the peer cannot spray back (old build, or its own NAT is unknown):
		// nothing direct can be arranged.
		return modeNone
	case own.Sym && !peerSym:
		// we are symmetric, the peer is cone and knows birthday: it sprays our range
		return modeBirthdayDial
	case !own.Sym && peerSym:
		// the peer is symmetric, we are cone: we spray its range
		return modeBirthdaySpray
	default: // own.Sym && peerSym
		// both symmetric: a double birthday, worth it only when both ranges are narrow
		if narrowRange(own.Lo, own.Hi, own.Ports) && narrowRange(peerLo, peerHi, peerPorts) {
			return modeBirthdayMutual
		}
		return modeNone
	}
}

// birthdaySockets is how many sockets we open as the symmetric side. More sockets mean more open
// mappings for the cone's spray to hit, so we use the cap: the extra mappings cost little and the
// per-attempt success is what makes a direct link form inside a call's short window.
func (s *Service) birthdaySockets(own NATInfo) int {
	return maxBirthdaySockets
}

// spraysFor is how many packets the cone side sends: enough to sweep a narrow block, widened, a few
// times over; the whole dynamic range (lo below 1024, our marker for "wide or unknown") takes the
// cap. Always within the transport's own limit.
func spraysFor(lo, hi uint16) int {
	if lo < 1024 || hi < lo {
		return maxBirthdaySpray
	}
	m := (int(hi-lo) + 1) * 6
	if m < 256 {
		m = 256
	}
	if m > maxBirthdaySpray {
		m = maxBirthdaySpray
	}
	return m
}

// maxBirthdaySpray mirrors the transport's cap so the Service never asks for more.
const maxBirthdaySpray = 900

// firstPublicV4 is the first IPv4 address of a list (birthday punching is an IPv4-NAT affair).
func firstPublicV4(addrs []netip.AddrPort) netip.AddrPort {
	for _, a := range addrs {
		if a.Addr().Unmap().Is4() {
			return a
		}
	}
	return netip.AddrPort{}
}

// beginBirthday reserves one of the node's birthday-punch slots; end releases it.
func (s *Service) beginBirthday() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.birthdays >= maxBirthdayNode {
		return false
	}
	s.birthdays++
	return true
}

func (s *Service) endBirthday() {
	s.mu.Lock()
	if s.birthdays > 0 {
		s.birthdays--
	}
	s.mu.Unlock()
}

// birthdayDial runs our symmetric-side half: open n sockets and dial the peer from whichever one it
// reaches. remote is already filtered by the noise-dial rule, so its first IPv4 address is a target
// we are allowed to punch toward.
func (s *Service) birthdayDial(ctx context.Context, peer types.PeerID, remote []netip.AddrPort, n int) error {
	bt, ok := s.cfg.Transport.(birthdayTransport)
	if !ok {
		return ErrFailed
	}
	target := firstPublicV4(remote)
	if !target.IsValid() {
		return ErrNoRemoteAddr
	}
	if !s.beginBirthday() {
		return ErrBusy
	}
	defer s.endBirthday()
	s.bdTried.Add(1)
	err := bt.BirthdayDial(ctx, target.String(), peer, n)
	if err == nil {
		s.bdOK.Add(1)
	}
	return err
}

// birthdaySpray runs our cone-side half: spray the peer's IP across the port block its NAT uses. A
// range that is not narrow enough to trust (wide, or too few observations) is zeroed, which the
// transport reads as "spray the whole dynamic range".
func (s *Service) birthdaySpray(ctx context.Context, peer types.PeerID, remote []netip.AddrPort, lo, hi uint16, ports int) error {
	bt, ok := s.cfg.Transport.(birthdayTransport)
	if !ok {
		return ErrFailed
	}
	target := firstPublicV4(remote)
	if !target.IsValid() {
		return ErrNoRemoteAddr
	}
	if !narrowRange(lo, hi, ports) {
		lo, hi = 0, 0
	}
	if !s.beginBirthday() {
		return ErrBusy
	}
	defer s.endBirthday()
	s.bdTried.Add(1)
	err := bt.BirthdaySpray(ctx, target.Addr().String(), lo, hi, spraysFor(lo, hi), peer)
	if err == nil {
		s.bdOK.Add(1)
	}
	return err
}

// amDialer decides, the same way on both sides, which end dials in the both-symmetric case: the
// lower peer ID dials, the higher listens. Without an OwnID the node never dials (it listens),
// which still lets a peer that does have one drive the rendezvous.
func (s *Service) amDialer(peer types.PeerID) bool {
	return bytes.Compare(s.cfg.OwnID[:], peer[:]) < 0
}

// birthdayMutual runs the both-symmetric case: both ends open sockets and spray each other's narrow
// range; the dialer dials the socket the peer reaches, the listener accepts on its own.
func (s *Service) birthdayMutual(ctx context.Context, peer types.PeerID, remote []netip.AddrPort, lo, hi uint16, ports int) error {
	bt, ok := s.cfg.Transport.(birthdayTransport)
	if !ok {
		return ErrFailed
	}
	target := firstPublicV4(remote)
	if !target.IsValid() {
		return ErrNoRemoteAddr
	}
	if !narrowRange(lo, hi, ports) {
		return ErrNoPunchPath // both-narrow only
	}
	if !s.beginBirthday() {
		return ErrBusy
	}
	defer s.endBirthday()
	s.bdTried.Add(1)
	err := bt.BirthdayMutual(ctx, target.Addr().String(), lo, hi, maxBirthdayMutualSockets, peer, s.amDialer(peer))
	if err == nil {
		s.bdOK.Add(1)
	}
	return err
}

// maxBirthdayMutualSockets is how many sockets each side opens in the both-symmetric case; kept
// well below the single-side cap because both ends run listeners on them at once.
const maxBirthdayMutualSockets = 64

func addrStrings(in []netip.AddrPort) []string {
	out := make([]string, len(in))
	for i, a := range in {
		out[i] = a.String()
	}
	return out
}
