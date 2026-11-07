package node

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/routepolicy"
)

// The freeze of a network (T6.4d). A network that freezes TCP stops a flow to a foreign address once
// the server has sent 12 to 20 KiB. The watcher below sees that on the sockets of the carriers; two
// such stops at one address mark the network, by its identifier, for a day; and from then on the
// row of a mobile network towards abroad applies to every new path on it.

// defaultFreezeStall is how long a flow that wrote and heard nothing back counts as stopped.
const defaultFreezeStall = 10 * time.Second

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

// noteStall records a stalled flow to ip. It returns true when it marked the network, and then
// tells the application.
func (r *Runtime) noteStall(ip netip.Addr, bytes int) bool {
	if !r.freeze.NoteStall(r.NetworkID(), ip, bytes) {
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
	return &watchConn{Conn: c, r: r, ip: ip, stall: stall}
}

// watchConn counts the bytes a flow heard. When it has written something and heard nothing for the
// stall time, it says the flow stopped, with the bytes heard so far; the tracker decides whether
// that is the freeze. A flow that is only quiet, with nothing waiting for an answer, says nothing.
type watchConn struct {
	net.Conn
	r     *Runtime
	ip    netip.Addr
	stall time.Duration

	mu      sync.Mutex
	heard   int
	waiting bool // something was written and no answer has come since
	timer   *time.Timer
	noted   bool
	closed  bool
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
	}
	return n, err
}

func (c *watchConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.mu.Lock()
		c.waiting = true
		if c.timer == nil && !c.noted && !c.closed {
			c.timer = time.AfterFunc(c.stall, c.fire)
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
	heard := c.heard
	c.mu.Unlock()
	c.r.noteStall(c.ip, heard)
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
