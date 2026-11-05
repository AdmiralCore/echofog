package noise3

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/mux"
)

// SendRaw sends one datagram to to from the listener's socket. It opens this end's NAT mapping
// towards to (hole punching, services.md §6.4 step 4) and is the junk sender.
func (l *Listener) SendRaw(to netip.AddrPort, p []byte) error {
	if !to.IsValid() || to.Port() == 0 {
		return fmt.Errorf("%w: bad target %v", ErrBadConfig, to)
	}
	select {
	case <-l.done:
		return errClosed
	default:
	}
	_, err := l.conn.WriteTo(p, net.UDPAddrFromAddrPort(to))
	return err
}

// routeConn is the dialing side of one DialFrom as a net.Conn on the listener's socket: reads are the
// datagrams the listener's read loop routed from the target, writes go out of the listener's socket,
// counted against the budget.
type routeConn struct {
	l      *Listener
	remote netip.AddrPort
	q      *queueConn
	budget atomic.Int64
}

func (c *routeConn) Read(p []byte) (int, error) {
	n, _, err := c.q.ReadFrom(p)
	return n, err
}

// Write counts datagrams against the Init budget until DialFrom lifts it after the handshake.
func (c *routeConn) Write(p []byte) (int, error) {
	if c.budget.Add(-1) < 0 {
		return len(p), nil // beyond the budget: dropped, not an error (the dial just runs out)
	}
	return c.l.conn.WriteTo(p, net.UDPAddrFromAddrPort(c.remote))
}

// Close leaves the route in place: the dialer restarts a dead flow on the same connection, and the
// route ends with the carrier (routedCarrier.Close) or a failed dial.
func (c *routeConn) Close() error                      { return nil }
func (c *routeConn) LocalAddr() net.Addr               { return c.l.conn.LocalAddr() }
func (c *routeConn) RemoteAddr() net.Addr              { return net.UDPAddrFromAddrPort(c.remote) }
func (c *routeConn) SetDeadline(t time.Time) error     { return c.q.SetDeadline(t) }
func (c *routeConn) SetReadDeadline(t time.Time) error { return c.q.SetReadDeadline(t) }
func (c *routeConn) SetWriteDeadline(time.Time) error  { return nil }

// routedCarrier ends the route when the carrier closes.
type routedCarrier struct {
	mux.Carrier
	end func()
}

func (c *routedCarrier) Close() error {
	c.end()
	return c.Carrier.Close()
}

// route returns the dial that expects datagrams from src, if any. Routes are keyed by the unmapped
// address: a dual-stack socket reports IPv4 peers as 4in6.
func (l *Listener) route(src netip.AddrPort) *routeConn {
	src = netip.AddrPortFrom(src.Addr().Unmap(), src.Port())
	l.rmu.Lock()
	defer l.rmu.Unlock()
	return l.routes[src]
}

// DialFrom dials like Dialer.Dial but sends from the listener's own socket, so the NAT mapping the
// peer has to hit is the listener's. At most budget datagrams leave for the target in all (Init
// retransmissions included); later ones are dropped silently. Replies from the target are routed to
// this dial instead of the listener's responder side while it lives.
func (l *Listener) DialFrom(ctx context.Context, cfg DialerConfig, req carrier.DialRequest, budget int) (mux.Carrier, error) {
	if req.Addr == nil || budget <= 0 {
		return nil, fmt.Errorf("%w: address or budget missing", ErrBadConfig)
	}
	host := strings.TrimPrefix(strings.TrimSuffix(req.Addr.GetHost(), "]"), "[")
	ip, err := netip.ParseAddr(host)
	if err != nil || req.Addr.GetPort() == 0 || req.Addr.GetPort() > 65535 {
		return nil, fmt.Errorf("%w: DialFrom needs an IP literal and a port", ErrBadConfig)
	}
	remote := netip.AddrPortFrom(ip.Unmap(), uint16(req.Addr.GetPort()))
	rc := &routeConn{l: l, remote: remote}
	rc.q = newQueueConn(l.conn.LocalAddr(), func([]byte, net.Addr) (int, error) { return 0, errClosed })
	rc.budget.Store(int64(budget))

	l.rmu.Lock()
	if l.routes == nil {
		l.routes = map[netip.AddrPort]*routeConn{}
	}
	if _, busy := l.routes[remote]; busy {
		l.rmu.Unlock()
		return nil, errors.New("noise3: a dial to this address is already running")
	}
	l.routes[remote] = rc
	l.rmu.Unlock()
	var once sync.Once
	end := func() {
		once.Do(func() {
			l.rmu.Lock()
			delete(l.routes, remote)
			l.rmu.Unlock()
			_ = rc.q.Close()
		})
	}

	cfg.DialContext = func(context.Context, string, string) (net.Conn, error) { return rc, nil }
	req.DialContext = nil
	c, err := NewDialer(cfg).Dial(ctx, req)
	if err != nil {
		end()
		return nil, err
	}
	// The budget bounds the Init handshake only; the carrier's own data is not an Init.
	rc.budget.Store(math.MaxInt64)
	return &routedCarrier{Carrier: c, end: end}, nil
}
