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

	mu    sync.Mutex
	until map[string]time.Time
	order []string
}

// NewNarrowPath builds the marks on an injectable clock (AD13). onMark, when set, is called once
// for each new mark.
func NewNarrowPath(clock Clock, onMark func(netID string, why NarrowReason)) *NarrowPath {
	if clock == nil {
		clock = systemClock{}
	}
	return &NarrowPath{FirstTry: defaultFirstTry, Rung: defaultRung, clock: clock, onMark: onMark,
		until: map[string]time.Time{}}
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
		var connected, expired atomic.Bool
		r2 := req
		r2.DialContext = narrowOpener(req, rung, &connected)
		actx, cancel := context.WithCancel(ctx)
		var timer Timer
		if canLimit {
			budget := n.Rung
			if rung < 0 {
				budget = n.FirstTry
			}
			if rung < len(mssLadder)-1 && budget > 0 {
				timer = n.clock.AfterFunc(budget, func() { expired.Store(true); cancel() })
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
			// Connected, then silent until the deadline: a black hole that cannot be answered
			// here. Say so; do not work around it.
			if errors.Is(err, context.DeadlineExceeded) {
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
func narrowOpener(req DialRequest, rung int, connected *atomic.Bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
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
			nd := net.Dialer{Control: func(netw, _ string, rc syscall.RawConn) error {
				var serr error
				if err := rc.Control(func(fd uintptr) { serr = setTCPMSS(fd, MSSFor(netw == "tcp6", rung)) }); err != nil {
					return err
				}
				return serr
			}}
			c, err = nd.DialContext(ctx, network, addr)
		}
		if err == nil {
			connected.Store(true)
		}
		return c, err
	}
}

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
