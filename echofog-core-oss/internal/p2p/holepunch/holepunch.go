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
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
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
	// sent: a hidden node answers (and so gives out its addresses) only to the peers it invited.
	Admit func(from types.PeerID, ticket []byte) bool
	// Ticket, when set, gives the access ticket to send with a CONNECT to peer (nil: none).
	Ticket func(peer types.PeerID) []byte
}

type message struct {
	T     string   `json:"t"` // "connect" (request) | "answer" (reply to connect) | "sync"
	N     uint64   `json:"n"`
	Addr  []string `json:"a,omitempty"`
	Local []string `json:"l,omitempty"` // LAN addresses, for a peer behind the same NAT
	Both  bool     `json:"b,omitempty"` // sync: both sides dial
	// Ticket proves an invitation to a hidden node (connect only; see package access).
	Ticket []byte `json:"k,omitempty"`
}

type sessKey struct {
	peer types.PeerID
	n    uint64
}

type incoming struct {
	addrs []netip.AddrPort
	timer *time.Timer
}

// Service runs both roles.
type Service struct {
	cfg Config

	mu         sync.Mutex
	waiting    map[sessKey]chan message
	expecting  map[sessKey]*incoming
	active     map[types.PeerID]struct{}
	lastAccept map[types.PeerID][]time.Time
	responders int
	closed     bool
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

// withLAN puts the peer's LAN addresses in front of its public ones when both of us are behind the
// same NAT, that is, when its public address has the same IP as one of ours.
func withLAN(own, remote []netip.AddrPort, remoteLocal []string) []netip.AddrPort {
	for _, o := range own {
		for _, r := range remote {
			if o.Addr() == r.Addr() {
				return append(lan(remoteLocal), remote...)
			}
		}
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
		if errors.Is(lastErr, ErrNoOwnAddr) || errors.Is(lastErr, ErrNoRemoteAddr) {
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

	start := time.Now()
	req := message{T: "connect", N: key.n, Addr: addrStrings(own), Local: s.ownLocal()}
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
	remote = withLAN(own, remote, answer.Local)
	half := rtt / 2
	if half > maxHalfRTT {
		return fmt.Errorf("the path through the relay is too slow to synchronise (rtt %v)", rtt)
	}

	if err := s.send(ctx, peer, message{T: "sync", N: key.n, Both: both}); err != nil {
		return fmt.Errorf("send SYNC: %w", err)
	}
	// SYNC takes about half a round trip to arrive; start when it does
	select {
	case <-time.After(half):
	case <-ctx.Done():
		return ctx.Err()
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
	remote = withLAN(own, remote, m.Local)
	key := sessKey{from, m.N}
	in := &incoming{addrs: remote}
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
	if err := s.send(ctx, from, message{T: "answer", N: m.N, Addr: addrStrings(own), Local: s.ownLocal()}); err != nil {
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
	// the initiator is already starting: dial now. In "both" mode it dials too, otherwise it
	// punches toward us while we dial it.
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

func addrStrings(in []netip.AddrPort) []string {
	out := make([]string, len(in))
	for i, a := range in {
		out[i] = a.String()
	}
	return out
}
