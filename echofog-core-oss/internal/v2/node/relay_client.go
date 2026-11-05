package node

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/v2/access"
	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/punch"
	"github.com/echofog/echofog-core/internal/v2/relay"
	"github.com/echofog/echofog-core/internal/v2/service"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// PunchNetwork supplies the datagram side of hole punching for one session (T6.2): the addresses to
// offer, the junk sender on the noise3 listener's socket and the noise3 Init dialer.
type PunchNetwork interface {
	Net(peer DeviceID, role punch.Role) punch.Net
}

const (
	// maxIncomingCircuits bounds the circuits a node answers at once (INCOMING from its relays).
	maxIncomingCircuits = 8
	// maxPexRelayHops is how many relay cards from PEX (no session yet) one dial may add.
	maxPexRelayHops = 3
)

// relayClient is the client side of relay/2 in the node (T6.2, N1): choosing relays, building a
// session through a circuit, the answer side of INCOMING, and the punch engines of relayed sessions.
type relayClient struct {
	r   *Runtime
	sem chan struct{}

	mu          sync.Mutex
	engines     map[*mux.Session]*punchState
	pendingAddr map[*mux.Session]netip.AddrPort // PEER_ADDR of an answered circuit until its engine exists
	early       map[*mux.Session][]frame.Frame  // F1: PUNCH frames that beat the session into the table
	lastRelay   map[[32]byte][32]byte           // peer device → relay device that carried the last session
}

// punchState is what the node keeps for the punching of one relayed session.
type punchState struct {
	eng      *punch.Engine
	est      *session.Established
	peerAddr netip.AddrPort // the peer's address as its relay saw it (PEER_ADDR), maybe invalid
	direct   atomic.Bool
	cardOnce sync.Once // F2: the peer card is verified once per session, not per PUNCH frame
	cardIPs  []netip.Addr
}

func newRelayClient(r *Runtime) *relayClient {
	return &relayClient{r: r, sem: make(chan struct{}, maxIncomingCircuits),
		engines:     map[*mux.Session]*punchState{},
		pendingAddr: map[*mux.Session]netip.AddrPort{},
		early:       map[*mux.Session][]frame.Frame{},
		lastRelay:   map[[32]byte][32]byte{}}
}

// punchEnabled: punching needs a datagram side and a profile that may send extra datagrams.
func (r *Runtime) punchEnabled() bool {
	return r.cfg.PunchNet != nil && r.cfg.Profile.UDP != UDPBlocked
}

func hopOf(est *session.Established) relay.Hop {
	return relay.Hop{
		Node: est.Peer.NodeID, Device: est.Peer.DeviceID,
		Open: func(ctx context.Context) (*mux.Stream, error) {
			return est.Session.OpenStream(ctx, service.Relay, 2)
		},
	}
}

// hops returns the relays to try to reach target, best first (relay.Choose): relays that carried the
// last session to the same peer first, then the other connected relays, then relay cards from PEX
// that have no session yet (N2; their Open dials first).
func (c *relayClient) hops(target, targetDevice [32]byte) []relay.Hop {
	r := c.r
	byNode := map[[32]byte]*session.Established{}
	var cands []relay.Candidate
	for _, est := range r.table.Sessions() {
		if est == nil || est.Peer == nil || est.Session == nil || !liveSession(est) {
			continue
		}
		if est.Peer.NodeID == target || est.Peer.DeviceID == targetDevice {
			continue
		}
		byNode[est.Peer.NodeID] = est
		cands = append(cands, relay.Candidate{Node: est.Peer.NodeID, Device: est.Peer.DeviceID,
			Flags: est.Peer.Flags, Connected: true})
	}
	common := map[[32]byte]struct{}{}
	c.mu.Lock()
	if last, ok := c.lastRelay[targetDevice]; ok {
		common[last] = struct{}{}
	}
	c.mu.Unlock()
	chosen := relay.Choose(cands, common, nil, func(n int) int { return mrand.IntN(n) })
	hops := make([]relay.Hop, 0, len(chosen)+maxPexRelayHops)
	for _, cand := range chosen {
		hops = append(hops, hopOf(byNode[cand.Node]))
	}
	if r.peers != nil {
		added := 0
		for _, card := range r.peers.DialCandidates(32, r.clock.Now()) {
			if added == maxPexRelayHops {
				break
			}
			if card.Body == nil || card.Body.GetFlags()&objects.FlagRelay == 0 ||
				card.NodeID == target || card.DeviceID == targetDevice {
				continue
			}
			if _, have := byNode[card.NodeID]; have {
				continue
			}
			card := card
			hops = append(hops, relay.Hop{Node: card.NodeID, Device: card.DeviceID,
				Open: func(ctx context.Context) (*mux.Stream, error) {
					_, sess, err := r.Session(ctx, DeviceID(card.DeviceID), DialOpt{Card: card})
					if err != nil {
						return nil, err
					}
					return sess.OpenStream(ctx, service.Relay, 2)
				}})
			added++
		}
	}
	return hops
}

// dialViaRelay builds the first path of a session to card's device through a relay circuit and
// starts its hole punching. The end-to-end handshake runs inside the circuit; R sees sealed
// records only.
func (r *Runtime) dialViaRelay(ctx context.Context, card *objects.Card, local session.LocalIdentity, opt DialOpt, extra []session.DialOption) (*session.Established, error) {
	c := r.relayc
	hops := c.hops(card.NodeID, card.DeviceID)
	if len(hops) == 0 {
		return nil, relay.ErrNoRelay
	}
	co := relay.ConnectOptions{WantAddr: r.punchEnabled()}
	target := card.NodeID
	if len(local.AccessSecret) > 0 {
		// A hidden or unlisted peer is registered under rendezvous IDs only (services.md §6.3).
		target, co.TargetKind = punch.RendezvousID(local.AccessSecret, punch.Day(r.clock.Now())), relay.TargetRendezvous
	}
	var circ *relay.Circuit
	dial := func(ctx context.Context, cr mux.Carrier) (*session.Established, error) {
		circ, _ = cr.(*relay.Circuit)
		o := opt
		o.Carrier, o.CarrierFunc, o.PathKind, o.Attempts = cr, nil, mux.PathRelay, nil
		return r.dial(ctx, card, local, o, extra)
	}
	est, hop, err := relay.Establish(ctx, hops, target, dial, co)
	if err != nil {
		return nil, fmt.Errorf("node: relay dial: %w", err)
	}
	c.mu.Lock()
	c.lastRelay[card.DeviceID] = hop.Device
	c.mu.Unlock()
	if r.punchEnabled() && est.Session.HasFeature(frame.FeaturePathMigration) {
		var pa netip.AddrPort
		if circ != nil {
			pa = circ.PeerAddr()
		}
		c.startPunch(est, punch.Initiator, pa)
	}
	return est, nil
}

// relayFallback reports whether a failed direct dial may be retried through a relay: the plan had
// no direct suite (D21 rule 4) or no direct carrier could be opened. A refused handshake is not
// retried: the peer answered, and a relay would not change the answer.
func relayFallback(err error, opt DialOpt) bool {
	if opt.Carrier != nil || opt.CarrierFunc != nil {
		return false
	}
	return errors.Is(err, carrier.ErrNeedRelay) || errors.Is(err, errCarrierDial)
}

// errCarrierDial marks "no direct carrier could be opened" (the handshake never started).
var errCarrierDial = errors.New("node: carrier dial")

// onIncoming answers INCOMING from a relay of ours (R opens the stream on the session we dialed).
// Anything arriving on a session the peer dialed is not a relay of ours and is refused: otherwise any
// peer could make this node run handshakes for circuits nobody registered.
func (c *relayClient) onIncoming(ctx context.Context, p relay.Peer, st *mux.Stream, first frame.Frame) {
	r := c.r
	est := r.sessionBySID(p.SID)
	if est == nil || est.Role != session.RoleInitiator {
		st.Reset(frame.CodeRefused)
		return
	}
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	default:
		st.Reset(frame.CodeOverloaded)
		return
	}
	in, err := relay.AnswerFrame(st, first, [32]byte(p.Node), nil, nil,
		func(in relay.Incoming) bool { return in.Mode != relay.ModeDatagram },
		func(relay.Incoming) bool { return r.punchEnabled() })
	if err != nil || in.Stream == nil {
		return
	}
	r.serveCarrierAs(ctx, in.Stream, mux.PathRelay, in.PeerAddr)
}

// notePeerAddr remembers the PEER_ADDR of an answered circuit for the engine of its session.
func (c *relayClient) notePeerAddr(sess *mux.Session, pa netip.AddrPort) {
	if !pa.IsValid() {
		return
	}
	c.mu.Lock()
	c.pendingAddr[sess] = pa
	c.mu.Unlock()
	go func() {
		<-sess.Done()
		c.mu.Lock()
		delete(c.pendingAddr, sess)
		c.mu.Unlock()
	}()
}

// startPunch registers the punch engine of a relayed session. The initiator also runs it.
func (c *relayClient) startPunch(est *session.Established, role punch.Role, peerAddr netip.AddrPort) *punchState {
	r := c.r
	c.mu.Lock()
	if ps, ok := c.engines[est.Session]; ok {
		c.mu.Unlock()
		return ps
	}
	if !peerAddr.IsValid() {
		peerAddr = c.pendingAddr[est.Session]
	}
	ps := &punchState{est: est, peerAddr: peerAddr}
	ps.eng = punch.New(punch.Config{
		Role: role, Link: est.Session, Net: r.cfg.PunchNet.Net(DeviceID(est.Peer.DeviceID), role),
		Clock:     r.carrierClock,
		Rules:     func() punch.Rules { return c.rules(ps) },
		Allowed:   func() bool { return c.allowed(ps) },
		HasDirect: ps.direct.Load,
		Bind: func(ctx context.Context, cr mux.Carrier) error {
			if _, err := r.addPathTo(ctx, est, DialOpt{Carrier: cr}, true); err != nil {
				return err
			}
			ps.direct.Store(true)
			return nil
		},
	})
	c.engines[est.Session] = ps
	c.mu.Unlock()
	sess := est.Session
	go func() {
		<-sess.Done()
		ps.eng.Close()
		c.mu.Lock()
		delete(c.engines, sess)
		c.mu.Unlock()
	}()
	if role == punch.Initiator {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			ctx, cancel := context.WithCancel(r.ctx)
			defer cancel()
			go func() {
				select {
				case <-sess.Done():
					cancel()
				case <-ctx.Done():
				}
			}()
			_ = ps.eng.Run(ctx)
		}()
	}
	return ps
}

// rules are the target rules of services.md §6.4 step 2 for this session.
func (c *relayClient) rules(ps *punchState) punch.Rules {
	var rl punch.Rules
	if ps.peerAddr.IsValid() {
		rl.PeerIP = ps.peerAddr.Addr()
	}
	if own, ok := c.r.PublicAddr(); ok {
		rl.OwnIP = own
	}
	ps.cardOnce.Do(func() {
		if ps.est == nil || ps.est.Peer == nil {
			return
		}
		if card, err := objects.VerifyCard(ps.est.Peer.Raw, c.r.clock.Now()); err == nil {
			for _, a := range card.Addrs() {
				if ip, err := netip.ParseAddr(a.GetHost()); err == nil {
					ps.cardIPs = append(ps.cardIPs, ip)
				}
			}
		}
	})
	rl.CardIPs = ps.cardIPs
	return rl
}

// allowed: punching needs the feature, a datagram side, and the peer in the same country (noise3 is
// confined to one country, D21 rule 2) unless both ends sit behind one NAT.
func (c *relayClient) allowed(ps *punchState) bool {
	r := c.r
	if !r.punchEnabled() || ps.est.Session.GoingAway() || !ps.est.HasFeature(frame.FeaturePathMigration) {
		return false
	}
	rl := c.rules(ps)
	if rl.PeerIP.IsValid() && rl.OwnIP.IsValid() && rl.PeerIP.Unmap() == rl.OwnIP.Unmap() {
		return true
	}
	ip := rl.PeerIP
	if !ip.IsValid() && len(rl.CardIPs) > 0 {
		ip = rl.CardIPs[0]
	}
	if !ip.IsValid() {
		return false
	}
	info := r.planInfo()
	if info.CountryOf == nil {
		return false
	}
	pc, ok := info.CountryOf(ip.Unmap())
	if !ok {
		return false
	}
	for _, la := range info.Addrs {
		if lc, ok := info.CountryOf(la.Unmap()); ok && lc == pc {
			return true
		}
	}
	return false
}

// onPunch routes PUNCH_OFFER and PUNCH_SYNC to the engine of the session; the answering end creates
// its engine on the first frame.
func (c *relayClient) onPunch(sess *mux.Session, f frame.Frame) {
	c.mu.Lock()
	ps := c.engines[sess]
	c.mu.Unlock()
	if ps == nil {
		if !c.r.punchEnabled() {
			return
		}
		est := c.r.estOf(sess)
		if est == nil {
			c.holdEarly(sess, f)
			return
		}
		if est.Role != session.RoleResponder || est.Kind != mux.PathRelay {
			return
		}
		ps = c.startPunch(est, punch.Responder, netip.AddrPort{})
	}
	ps.eng.HandleControl(f)
}

const (
	maxEarlySessions = 64 // sessions with held PUNCH frames at once
	maxEarlyFrames   = 4  // frames held per session
)

// holdEarly keeps a PUNCH frame of a session that is not in the table yet (a hidden node books its
// pass first, F1). takeAccepted replays them through sessionReady.
func (c *relayClient) holdEarly(sess *mux.Session, f frame.Frame) {
	c.mu.Lock()
	defer c.mu.Unlock()
	q, known := c.early[sess]
	if !known {
		if len(c.early) >= maxEarlySessions {
			return
		}
		go func() {
			<-sess.Done()
			c.mu.Lock()
			delete(c.early, sess)
			c.mu.Unlock()
		}()
	}
	if len(q) < maxEarlyFrames {
		c.early[sess] = append(q, f)
	}
}

// sessionReady replays the PUNCH frames held for a session that has just entered the table.
func (c *relayClient) sessionReady(sess *mux.Session) {
	c.mu.Lock()
	q := c.early[sess]
	delete(c.early, sess)
	c.mu.Unlock()
	for _, f := range q {
		c.onPunch(sess, f)
	}
}

// estOf finds the Established of a mux session in the table.
func (r *Runtime) estOf(sess *mux.Session) *session.Established {
	if r.table == nil {
		return nil
	}
	for _, est := range r.table.Sessions() {
		if est != nil && est.Session == sess {
			return est
		}
	}
	return nil
}

// registerPoll is how often a hidden or unlisted node checks that its relays hold its rendezvous IDs
// (jittered ±20 %, AD13). A relay that restarted, a new contact and the change of day are all caught
// within one poll.
const (
	registerPoll  = 30 * time.Second
	registerRetry = 3 * time.Second // while a relay does not hold the IDs yet, or none is connected
)

// registerLoop keeps a hidden or unlisted node registered, under rendezvous IDs only, at every relay
// it holds a session with (services.md §6.3). The node never sends its node ID to a relay.
func (c *relayClient) registerLoop(ctx context.Context) {
	r := c.r
	done := map[*mux.Session]string{} // session → fingerprint of what its relay holds
	fails := 0
	for ctx.Err() == nil {
		poll := registerPoll
		all, failed := c.registerOnce(ctx, done)
		switch {
		case failed:
			poll = registerBackoff(fails)
			fails++
		case !all:
			poll, fails = registerRetry, 0
		default:
			fails = 0
		}
		d := poll - poll/5 + time.Duration(mrand.Int64N(int64(poll)*2/5+1))
		ch := make(chan struct{})
		t := r.carrierClock.AfterFunc(d, func() { close(ch) })
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-ch:
		}
	}
}

// registerOnce registers the current IDs at every relay that does not hold them yet.
func (c *relayClient) registerOnce(ctx context.Context, done map[*mux.Session]string) (all, failed bool) {
	r := c.r
	list, err := r.accessMgr.List(ctx)
	if err != nil {
		return false, false
	}
	var secrets [][]byte
	for _, in := range list {
		if in.Kind == access.Personal && in.State == access.Active && len(in.Secret) > 0 {
			secrets = append(secrets, in.Secret)
		}
	}
	set := punch.IDs(secrets, r.clock.Now(), relay.MaxRegisterIDs)
	h := sha256.New()
	for _, id := range append(append([][32]byte(nil), set.Today...), set.Tomorrow...) {
		h.Write(id[:])
	}
	print := fmt.Sprintf("%d/%x", set.Day, h.Sum(nil))
	live := map[*mux.Session]bool{}
	all = true
	seen := 0
	for _, est := range r.table.Sessions() {
		if est == nil || est.Peer == nil || est.Session == nil || est.Role != session.RoleInitiator ||
			est.Peer.Flags&objects.FlagRelay == 0 || !liveSession(est) {
			continue
		}
		live[est.Session] = true
		seen++
		if len(set.Today) == 0 || done[est.Session] == print {
			continue
		}
		open := hopOf(est).Open
		rctx, cancel := context.WithTimeout(ctx, 2*relay.RegisterTimeout)
		_, err1 := relay.RegisterIDs(rctx, open, set.Today, set.Day)
		_, err2 := relay.RegisterIDs(rctx, open, set.Tomorrow, set.Day+1)
		cancel()
		if err1 == nil && err2 == nil {
			done[est.Session] = print
		} else {
			all, failed = false, true
		}
	}
	for s := range done {
		if !live[s] {
			delete(done, s)
		}
	}
	return all && seen > 0, failed
}

// registerBackoff is the pause after fails consecutive polls with a relay that did not take the IDs.
func registerBackoff(fails int) time.Duration {
	d := registerRetry
	for i := 0; i < fails && d < registerPoll*10; i++ {
		d *= 2
	}
	return min(d, registerPoll*10)
}
