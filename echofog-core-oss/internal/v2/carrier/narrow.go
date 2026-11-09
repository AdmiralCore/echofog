package carrier

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/echofog/echofog-core/internal/v2/mux"
)

// Narrow path (T3.12, wire.md section 2.4 "Path requirement").
//
// noise3 needs a path of 1271 bytes; a path below that (PMTU 1280 and 1200 are the cases the
// polygon of T15.0c builds) drops its first packet and says nothing. The first packet is never
// made smaller and nothing is fragmented. Instead the carrier layer
//
//   - starts the camouflaged suites within a second (happy eyeballs already does, 300-500 ms) and,
//     when one of them wins while noise3 has been silent for NoAnswerAfter, marks the network as a
//     narrow path for 24 h: noise3 is not tried there;
//   - on a TCP path that swallows the first big segments (the SYN passed, the ClientHello did not,
//     no ICMP) repeats the TCP dial with a limited MSS (TCP_MAXSEG), and marks the network.
//
// A limited MSS in a SYN is a fingerprint (p0f; Chrome sets none), so the first attempt on an
// unmarked network always carries the system MSS; the limit is only for the retry and for a network
// that is already marked.

const (
	// NoAnswerAfter is how long a noise3 Init may stay unanswered, while another suite connects,
	// before the network is taken for a narrow path.
	NoAnswerAfter = time.Second
	// NarrowTTL is how long a network stays marked.
	NarrowTTL = 24 * time.Hour

	defaultFirstTry = 2 * time.Second
	defaultRung     = 1200 * time.Millisecond
	maxNarrowNets   = 64
)

// mssLadder is the MSS of each retry: rung 0 fits a path of 1280 bytes (1220 for IPv6, 1240 for
// IPv4, wire.md section 2.4 / T3.12); rung 1 fits 1200, which the ТЕО numbers alone do not
// (1240 + 40 = 1280 > 1200): it is the same fingerprint-on-retry-only idea, one step further.
var mssLadder = [][2]int{{1240, 1220}, {1160, 1140}} // {IPv4, IPv6}

// ErrMSSUnsupported reports that a TCP retry with a limited MSS was needed but the socket cannot
// take one (a proxy opens it, or the OS has no TCP_MAXSEG).
var ErrMSSUnsupported = errors.New("carrier: TCP MSS cannot be limited on this socket")

// NarrowReason says why a network was marked.
type NarrowReason int

const (
	// ReasonNoise3Silent: noise3 got no answer for NoAnswerAfter while another suite connected.
	ReasonNoise3Silent NarrowReason = iota + 1
	// ReasonMSS: a TCP carrier connected only after the MSS was limited.
	ReasonMSS
)

// NarrowPath is the per-network mark of a narrow path. It is safe for concurrent use and bounded:
// at most 64 networks. The marks follow the T6.4d freeze marks: 24 h, per network identifier,
// dropped with Forget when the network is left.
type NarrowPath struct {
	// FirstTry bounds the first, unrestricted TCP attempt and Rung each limited retry but the last
	// (which gets what is left of the context).
	FirstTry, Rung time.Duration

	clock  Clock
	onMark func(netID string, why NarrowReason)

	mu     sync.Mutex
	until  map[string]time.Time
	order  []string
	silent map[string]map[string]time.Time // network -> peer host -> when noise3 was last silent
}

// NewNarrowPath builds the marks on an injectable clock (AD13). onMark, when set, is called once
// for each new mark.
func NewNarrowPath(clock Clock, onMark func(netID string, why NarrowReason)) *NarrowPath {
	if clock == nil {
		clock = systemClock{}
	}
	return &NarrowPath{FirstTry: defaultFirstTry, Rung: defaultRung, clock: clock, onMark: onMark,
		until: map[string]time.Time{}, silent: map[string]map[string]time.Time{}}
}

const (
	// SilentWindow and SilentPeers: a network is marked for a silent noise3 only when noise3 stayed
	// silent to this many different peers within this window while another carrier won to each. One
	// peer proves nothing about the network (its UDP port may be closed, its noise3 down).
	SilentWindow = 10 * time.Minute
	SilentPeers  = 2
	maxSilentPer = 8
)

// NoteNoise3Silent records that noise3 stayed silent to peer (a host) on the network while another
// carrier won to the same peer. When SilentPeers different peers did so within SilentWindow, the
// network is marked (ReasonNoise3Silent).
func (n *NarrowPath) NoteNoise3Silent(netID, peer string) {
	if n == nil || netID == "" || peer == "" {
		return
	}
	now := n.clock.Now()
	n.mu.Lock()
	peers := n.silent[netID]
	if peers == nil {
		if len(n.silent) >= maxNarrowNets {
			for id := range n.silent { // a bounded table: drop an arbitrary old network
				delete(n.silent, id)
				break
			}
		}
		peers = map[string]time.Time{}
		n.silent[netID] = peers
	}
	for p, at := range peers {
		if now.Sub(at) > SilentWindow {
			delete(peers, p)
		}
	}
	if _, ok := peers[peer]; !ok && len(peers) >= maxSilentPer {
		for p := range peers {
			delete(peers, p)
			break
		}
	}
	peers[peer] = now
	enough := len(peers) >= SilentPeers
	if enough {
		delete(n.silent, netID)
	}
	n.mu.Unlock()
	if enough {
		n.Mark(netID, ReasonNoise3Silent)
	}
}

// Mark marks a network for NarrowTTL. Marking a marked network only extends it and is not reported.
func (n *NarrowPath) Mark(netID string, why NarrowReason) {
	if n == nil || netID == "" {
		return
	}
	now := n.clock.Now()
	n.mu.Lock()
	_, was := n.until[netID]
	fresh := !was || !n.until[netID].After(now)
	if !was {
		if len(n.order) >= maxNarrowNets {
			delete(n.until, n.order[0])
			n.order = n.order[1:]
		}
		n.order = append(n.order, netID)
	}
	n.until[netID] = now.Add(NarrowTTL)
	cb := n.onMark
	n.mu.Unlock()
	if fresh && cb != nil {
		cb(netID, why)
	}
}

// Marked reports whether the network is marked now.
func (n *NarrowPath) Marked(netID string) bool {
	if n == nil || netID == "" {
		return false
	}
	now := n.clock.Now()
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.until[netID].After(now)
}

// Forget drops the mark of a network (it was left).
func (n *NarrowPath) Forget(netID string) {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.until[netID]; !ok {
		return
	}
	delete(n.until, netID)
	delete(n.silent, netID)
	for i, id := range n.order {
		if id == netID {
			n.order = append(n.order[:i], n.order[i+1:]...)
			break
		}
	}
}

// MSSFor is the MSS of retry rung (0-based) for IPv6 or IPv4; 0 past the ladder.
func MSSFor(v6 bool, rung int) int {
	if rung < 0 || rung >= len(mssLadder) {
		return 0
	}
	if v6 {
		return mssLadder[rung][1]
	}
	return mssLadder[rung][0]
}

// WithNarrowRetry runs attempt (a whole dial of a TCP suite) and, on a black-holed TCP path, runs
// it again with a limited MSS. The attempt must open its socket through req.DialContext: the
// request handed to it carries an opener that applies the MSS of the current rung.
//
// A black hole is: the socket connected, the attempt then ran out of its time budget. Any other
// failure is returned as it is. A retry that succeeds marks the network. Without Narrow or
// NetworkID in the request it only calls attempt.
func WithNarrowRetry(ctx context.Context, req DialRequest, attempt func(ctx context.Context, req DialRequest) (mux.Carrier, error)) (mux.Carrier, error) {
	n := req.Narrow
	if n == nil || req.NetworkID == nil {
		return attempt(ctx, req)
	}
	netID := req.NetworkID()
	canLimit := req.DialMSS != nil || (req.DialContext == nil && mssSupported)
	marked := n.Marked(netID)
	rung := -1
	if marked && canLimit {
		rung = 0
	}
	for {
		var connected, expired, progress atomic.Bool
		r2 := req
		r2.DialContext = narrowOpener(req, rung, &connected, &progress)
		actx, cancel := context.WithCancel(ctx)
		var timer Timer
		if canLimit {
			budget := n.Rung
			if rung < 0 {
				budget = n.FirstTry
			}
			if rung < len(mssLadder)-1 && budget > 0 {
				timer = n.clock.AfterFunc(budget, func() {
					if progress.Load() {
						return // the server has answered: slow, not black-holed, let it finish
					}
					expired.Store(true)
					cancel()
				})
			}
		}
		res, err := attempt(actx, r2)
		if timer != nil {
			timer.Stop()
		}
		cancel()
		if err == nil {
			if rung >= 0 && !marked {
				n.Mark(netID, ReasonMSS)
			}
			return res, nil
		}
		if !connected.Load() {
			return nil, err
		}
		if !canLimit {
			// Connected, then not one byte from the server until the deadline: a black hole that
			// cannot be answered here. Say so; do not work around it. A path that answered and
			// was only slow is an ordinary timeout.
			if errors.Is(err, context.DeadlineExceeded) && !progress.Load() {
				return nil, fmt.Errorf("%w: %w", err, ErrMSSUnsupported)
			}
			return nil, err
		}
		if ctx.Err() != nil || !expired.Load() || rung+1 >= len(mssLadder) {
			return nil, err
		}
		rung++
	}
}

// narrowOpener returns the socket opener for one rung (-1 = unlimited). It records that a socket
// connected: only then is a timeout a black hole and not a lost SYN.
func narrowOpener(req DialRequest, rung int, connected, progress *atomic.Bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		var c net.Conn
		var err error
		v6 := hostIsV6(addr)
		switch {
		case rung < 0 && req.DialMSS != nil:
			c, err = req.DialMSS(ctx, network, addr, 0)
		case rung < 0 && req.DialContext != nil:
			c, err = req.DialContext(ctx, network, addr)
		case rung < 0:
			var nd net.Dialer
			c, err = nd.DialContext(ctx, network, addr)
		case req.DialMSS != nil:
			c, err = req.DialMSS(ctx, network, addr, MSSFor(v6, rung))
		case req.DialContext != nil:
			return nil, ErrMSSUnsupported
		default:
			c, err = dialTCPRung(ctx, network, addr, rung)
		}
		if err == nil {
			connected.Store(true)
			c = &countConn{Conn: c, progress: progress}
		}
		return c, err
	}
}

// countConn notes the first byte the server sends: any incoming byte is handshake progress, and a
// path that delivers bytes is not a black hole (T3.12).
type countConn struct {
	net.Conn
	progress *atomic.Bool
}

func (c *countConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.progress.Store(true)
	}
	return n, err
}

// NetConn returns the socket under the counter.
func (c *countConn) NetConn() net.Conn { return c.Conn }

// hostIsV6 reports whether addr (host:port) names an IPv6 literal; a DNS name is taken as IPv4
// here (the default opener decides by the resolved family itself).
func hostIsV6(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Unmap().Is6()
}

// DialTCPMSS opens a direct TCP socket with its MSS limited before the SYN: mss is the value for
// IPv4 literals and hosts (and the IPv6 one is taken from the ladder when mss names a rung's IPv4
// value). It exists for the node's own opener; mss 0 is a plain dial. Where the OS has no
// TCP_MAXSEG it returns ErrMSSUnsupported.
func DialTCPMSS(ctx context.Context, network, addr string, mss int) (net.Conn, error) {
	if mss == 0 {
		var nd net.Dialer
		return nd.DialContext(ctx, network, addr)
	}
	for rung := range mssLadder {
		if mss == mssLadder[rung][0] || mss == mssLadder[rung][1] {
			return dialTCPRung(ctx, network, addr, rung)
		}
	}
	return nil, fmt.Errorf("carrier: %d is not a rung of the MSS ladder", mss)
}

// dialTCPRung dials with the MSS of the rung chosen by the family the socket ends up with.
func dialTCPRung(ctx context.Context, network, addr string, rung int) (net.Conn, error) {
	if !mssSupported {
		return nil, ErrMSSUnsupported
	}
	nd := net.Dialer{Control: func(netw, _ string, rc syscall.RawConn) error {
		var serr error
		if err := rc.Control(func(fd uintptr) { serr = setTCPMSS(fd, MSSFor(netw == "tcp6", rung)) }); err != nil {
			return err
		}
		return serr
	}}
	return nd.DialContext(ctx, network, addr)
}
