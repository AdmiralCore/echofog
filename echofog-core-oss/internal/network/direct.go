package network

import (
	"context"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// While whole-computer mode is on, the TUN captures every route. The node's own connections
// must still leave through the real network: its peers (QUIC is pinned by BindToInterface), the
// TCP and WebSocket fallbacks, its own DNS lookups and its HTTP calls (the TON wallet, which has
// to work to pay an exit that is refusing us). Each of these dials through DirectDialer, which
// pins the socket to the physical interface while one is set.

var directIf atomic.Int32

// directDNS are the resolvers the node asks itself while the TUN is up: the system resolver then
// points at the TUN's fake DNS, whose answers are only meaningful inside the tunnel.
// The list is spread over operators, so one blocked in a country does not leave the node blind.
var directDNS = []string{"1.1.1.1:53", "8.8.8.8:53", "9.9.9.9:53", "77.88.8.8:53", "223.5.5.5:53"}

// SetDirectInterface pins the node's own new sockets to ifIndex (0: no pin, normal routing).
func SetDirectInterface(ifIndex int) {
	installDirectHooks()
	directIf.Store(int32(ifIndex))
}

// DirectInterface is the interface the node's own traffic is pinned to, 0 when none.
func DirectInterface() int { return int(directIf.Load()) }

// directControl is a net.Dialer Control that applies the pin.
func directControl(network, _ string, raw syscall.RawConn) error {
	if idx := int(directIf.Load()); idx != 0 {
		return bindDialSocket(network, raw, idx)
	}
	return nil
}

// DirectDialer is a dialer for the node's own connections.
func DirectDialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, Control: directControl}
}

var hooksOnce sync.Once

// installDirectHooks routes the process-wide resolver and HTTP client through DirectDialer, so
// code that uses net.DefaultResolver or http.DefaultClient (bootstrap names, STUN, the wallet)
// keeps working while the TUN is up. With no pin set they behave exactly as before.
func installDirectHooks() {
	hooksOnce.Do(func() {
		net.DefaultResolver.PreferGo = true
		net.DefaultResolver.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			d := DirectDialer(5 * time.Second)
			if directIf.Load() == 0 {
				return d.DialContext(ctx, network, address)
			}
			var last error
			for _, srv := range directDNS {
				c, err := d.DialContext(ctx, network, srv)
				if err == nil {
					return c, nil
				}
				last = err
			}
			return nil, last
		}
		if tr, ok := http.DefaultTransport.(*http.Transport); ok {
			tr.DialContext = DirectDialer(30 * time.Second).DialContext
		}
	})
}
