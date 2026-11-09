package node

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/routepolicy"
)

// The freeze of a network (T6.4d). A network that freezes TCP stops a flow to a foreign address once
// the server has sent 12 to 20 KiB. The watcher below sees that on the sockets of the carriers; two
// such stops at one address mark the network, by its identifier; and from then on the row of a
// mobile network towards abroad applies to every new path on it.
//
// The mark costs the direct foreign paths, and a peer chooses which addresses the client dials, so
// three limits keep a hostile or slow server from earning it: only a young flow counts (the
// handshake and the first data, not a long request), a stop does not count while the same address
// answers on another flow, and without a verified entry the mark lasts an hour, not a day.

const (
	// defaultFreezeStall is how long a flow that wrote and heard nothing back counts as stopped.
	defaultFreezeStall = 10 * time.Second
	// defaultFreezeFlowAge is the oldest a flow may be, from its open, for its stop to count.
	defaultFreezeFlowAge = 30 * time.Second
)

// SetNetworkID tells the runtime which network the client is on now. A change forgets what was
// learned about the old one and about the new one: a freeze belongs to a network, not to the device.
func (r *Runtime) SetNetworkID(id string) {
	r.netMu.Lock()
	old := r.netID
	r.netID = id
	r.netMu.Unlock()
	if old != id {
		r.freeze.Forget(old)
		r.freeze.Forget(id)
		r.narrow.Forget(old)
		r.narrow.Forget(id)
	}
}

// NetworkID returns the identifier of the current network.
func (r *Runtime) NetworkID() string {
	r.netMu.Lock()
	defer r.netMu.Unlock()
	return r.netID
}

// frozen reports whether the current network is marked frozen.
func (r *Runtime) frozen() bool { return r.freeze.Frozen(r.NetworkID()) }

// decide is the table row for the own country, the network class, the destination and the freeze
// mark of the current network.
func (r *Runtime) decide(own string, dest routepolicy.DestClass) routepolicy.Decision {
	return r.rules.DecideFrozen(own, r.NetClass(), dest, r.frozen())
}

// freezeTTL is how long a mark lasts: a day when the client knows a verified entry to go through,
// an hour when it does not.
func (r *Runtime) freezeTTL() time.Duration {
	if r.rules.HasVerifiedEntries() {
		return routepolicy.FreezeTTL
	}
	return routepolicy.FreezeTTLNoEntry
}

// noteStall records a stalled flow to ip on the current network, with no flow identity.
func (r *Runtime) noteStall(ip netip.Addr, bytes int) bool {
	return r.noteStallFlow(r.NetworkID(), ip, bytes, 0, time.Time{})
}

// noteStallFlow records a stalled flow of network netID. It returns true when it marked the
// network, and then tells the application.
func (r *Runtime) noteStallFlow(netID string, ip netip.Addr, bytes int, flow uint64, waitingSince time.Time) bool {
	if !r.freeze.NoteStallFlow(netID, ip, bytes, flow, waitingSince, r.freezeTTL()) {
		return false
	}
	r.events.Emit(Event{Kind: EventNetworkFrozen, At: time.Now(),
		Message: "Сеть замораживает соединения с зарубежными узлами после нескольких килобайт: " +
			"новые пути строятся через вход в своей стране или в соседней."})
	return true
}

// watchedDialContext wraps the node's socket opener so that TCP flows to foreign addresses are
// watched for the freeze.
func (r *Runtime) watchedDialContext(base func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	if base == nil {
		base = (&net.Dialer{}).DialContext
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		c, err := base(ctx, network, address)
		if err != nil || !strings.HasPrefix(network, "tcp") {
			return c, err
		}
		host, _, serr := net.SplitHostPort(address)
		if serr != nil {
			return c, nil
		}
		if ip, perr := netip.ParseAddr(host); perr == nil {
			return r.freezeConn(c, ip.Unmap()), nil
		}
		return c, nil
	}
}

// watchedDialMSS is watchedDialContext for the node's own direct sockets: it can limit the MSS of
// the socket before the SYN (T3.12). A proxy opens its own sockets, and then there is none.
func (r *Runtime) watchedDialMSS() func(context.Context, string, string, int) (net.Conn, error) {
	return func(ctx context.Context, network, address string, mss int) (net.Conn, error) {
		c, err := carrier.DialTCPMSS(ctx, network, address, mss)
		if err != nil || !strings.HasPrefix(network, "tcp") {
			return c, err
		}
		host, _, serr := net.SplitHostPort(address)
		if serr != nil {
			return c, nil
		}
		if ip, perr := netip.ParseAddr(host); perr == nil {
			return r.freezeConn(c, ip.Unmap()), nil
		}
		return c, nil
	}
}

// freezeConn watches a TCP flow to ip for the freeze: a domestic address, or a network that is
// already marked, is returned as it is.
func (r *Runtime) freezeConn(c net.Conn, ip netip.Addr) net.Conn {
	own := r.ownCountry()
	country, _ := r.Country(ip)
	if r.rules.Classify(own, country) == routepolicy.DestDomestic || r.frozen() {
		return c
	}
	stall := r.cfg.FreezeStall
	if stall <= 0 {
		stall = defaultFreezeStall
	}
	maxAge := r.cfg.FreezeFlowAge
	if maxAge <= 0 {
		maxAge = defaultFreezeFlowAge
	}
	return &watchConn{Conn: c, r: r, ip: ip, stall: stall, maxAge: maxAge,
		id: r.flowSeq.Add(1), netID: r.NetworkID(), opened: r.carrierClock.Now()}
}

// watchConn counts the bytes a flow heard. When it has written something and heard nothing for the
// stall time, it says the flow stopped, with the bytes heard so far; the tracker decides whether
// that is the freeze. A flow that is only quiet, with nothing waiting for an answer, says nothing;
// neither does one older than maxAge, or one that outlived its network. Every byte heard is told to
// the tracker, because an address that answers one flow has not frozen the others.
type watchConn struct {
	net.Conn
	r      *Runtime
	ip     netip.Addr
	stall  time.Duration
	maxAge time.Duration
	id     uint64
	netID  string
	opened time.Time

	mu       sync.Mutex
	heard    int
	waiting  bool      // something was written and no answer has come since
	waitFrom time.Time // when that wait began
	timer    carrier.Timer
	noted    bool
	closed   bool
}

func (c *watchConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.mu.Lock()
		c.heard += n
		c.waiting = false
		if c.timer != nil {
			c.timer.Stop()
			c.timer = nil
		}
		c.mu.Unlock()
		c.r.freeze.NoteData(c.netID, c.ip, c.id)
	}
	return n, err
}

func (c *watchConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.mu.Lock()
		if !c.waiting {
			c.waiting, c.waitFrom = true, c.r.carrierClock.Now()
		}
		if c.timer == nil && !c.noted && !c.closed {
			c.timer = c.r.carrierClock.AfterFunc(c.stall, c.fire)
		}
		c.mu.Unlock()
	}
	return n, err
}

func (c *watchConn) fire() {
	c.mu.Lock()
	if !c.waiting || c.noted || c.closed {
		c.mu.Unlock()
		return
	}
	c.noted = true
	heard, from := c.heard, c.waitFrom
	c.mu.Unlock()
	if c.r.carrierClock.Now().Sub(c.opened) > c.maxAge || c.r.NetworkID() != c.netID {
		return // an old flow is a long request, and a flow of another network says nothing about this one
	}
	c.r.noteStallFlow(c.netID, c.ip, heard, c.id, from)
}

func (c *watchConn) Close() error {
	c.mu.Lock()
	c.closed = true
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.mu.Unlock()
	return c.Conn.Close()
}

// narrowMarked tells the user that the current network cannot carry noise3 (T3.12). The message
// says what the connection does instead; on a path of 1200 bytes only TCP with a limited MSS works.
func (r *Runtime) narrowMarked(_ string, why carrier.NarrowReason) {
	msg := "UDP-носитель noise3 на этой сети не отвечает нескольким узлам, поэтому пропускается: " +
		"соединение идёт через другие носители."
	if why == carrier.ReasonMSS {
		msg = "Путь сети узкий (MTU 1280 или меньше): TCP-соединение установлено только с ограниченным MSS. " +
			"На пути с MTU 1200 работает только TCP с ограничением MSS."
	}
	r.events.Emit(Event{Kind: EventNarrowPath, At: time.Now(), Message: msg})
}
